package invariant_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
	"github.com/serverplumber/charpy/internal/oracle/oracletest"
	"github.com/serverplumber/charpy/internal/transcript"
)

func req(id string, method string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{}}`
}
func res(id string) string { return `{"jsonrpc":"2.0","id":` + id + `,"result":{}}` }
func errf(id string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-1,"message":"x"}}`
}

func findings(rep oracle.Report, check string) []oracle.Finding {
	var out []oracle.Finding
	for _, f := range rep.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestCleanExchangeHasNoFindings(t *testing.T) {
	got := invariant.Check(oracletest.New(t, transcript.ClassServer).
		ToSubject(req("1", "initialize")).FromSubject(res("1")).
		ToSubject(req("2", "tools/call")).FromSubject(res("2")).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("a clean exchange produced findings: %+v", got.Findings)
	}
}

// I1. The three arms fail differently and are reported differently.
func TestIDResolvesOnce(t *testing.T) {
	t.Run("answered twice", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			FromSubject(res("7")).FromSubject(res("7")).
			Done())

		f := findings(got, "id-resolves-once")
		if len(f) != 1 {
			t.Fatalf("findings: %+v", f)
		}
		if f[0].Verdict != oracle.Observed {
			t.Errorf("verdict = %q; charpy never issues a behavioural MUST", f[0].Verdict)
		}
		if !strings.Contains(f[0].Summary, "answered 2 times") {
			t.Errorf("summary = %q", f[0].Summary)
		}
	})

	// "Answered twice" is a claim about the subject. The duplicate that a
	// vary_type or duplicate_id case puts on the wire is charpy's, and
	// reporting it would be quoting our own injection back as evidence -- the
	// verdict someone pastes into an issue tracker.
	t.Run("a duplicate charpy injected is not the subject answering twice", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			FromSubject(res("7")).Corrupted(res("7")).
			Done())

		if f := findings(got, "id-resolves-once"); len(f) != 0 {
			t.Errorf("charpy's own duplicate was held against the subject: %+v", f)
		}
	})

	t.Run("two answers charpy touched are still not the subject's", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			Corrupted(res("7")).Corrupted(res("7")).
			Done())

		if f := findings(got, "id-resolves-once"); len(f) != 0 {
			t.Errorf("findings: %+v", f)
		}
	})

	// The malformed_json shape: the subject answered, charpy replaced the
	// answer with bytes that parse to no id, and the request looks
	// outstanding. Blaming the subject there is charpy grading its own
	// interference -- and INCONCLUSIVE rather than silence, because a reader
	// still needs to know the exchange was never completed.
	t.Run("an answer charpy replaced is not an answer the subject withheld", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			Replacing(envelope.NumberID(7), `{"jsonrpc":"2.0","id":7,"resu`).
			Done())

		f := findings(got, "id-resolves-once")
		if len(f) != 1 {
			t.Fatalf("findings: %+v", f)
		}
		if f[0].Verdict != oracle.Inconclusive {
			t.Errorf("verdict = %q, want INCONCLUSIVE", f[0].Verdict)
		}
		if f[0].Reason != "charpy-replaced-the-answer" {
			t.Errorf("reason = %q", f[0].Reason)
		}
		if strings.Contains(f[0].Summary, "never") {
			t.Errorf("summary blames the subject: %q", f[0].Summary)
		}
	})

	t.Run("result and error both count as answers", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			FromSubject(res("7")).FromSubject(errf("7")).
			Done())
		if len(findings(got, "id-resolves-once")) != 1 {
			t.Error("result plus error is still resolving twice")
		}
	})

	t.Run("never answered", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			Done())

		f := findings(got, "id-resolves-once")
		if len(f) != 1 || f[0].Verdict != oracle.Observed {
			t.Fatalf("findings: %+v", f)
		}
		if !strings.Contains(f[0].Detail, "tools/call") {
			t.Errorf("detail does not name the method: %q", f[0].Detail)
		}
	})

	// An id outstanding when the subject died supports no conclusion. Calling
	// that a failure would blame the subject for a transcript that stopped.
	t.Run("outstanding when the subject exited is inconclusive", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).SubjectExit(1).
			Done())

		f := findings(got, "id-resolves-once")
		if len(f) != 1 {
			t.Fatalf("findings: %+v", f)
		}
		if f[0].Verdict != oracle.Inconclusive {
			t.Errorf("verdict = %q, want INCONCLUSIVE", f[0].Verdict)
		}
		if f[0].Reason == "" {
			t.Error("a non-verdict with no reason is indistinguishable from a pass")
		}
	})
}

// I2 is split from I1 because a pending-map leak needs a different reproducer.
func TestNoUnsolicitedResponse(t *testing.T) {
	got := invariant.Check(oracletest.New(t, transcript.ClassServer).
		FromSubject(res("99")).
		Done())

	f := findings(got, "no-unsolicited-response")
	if len(f) != 1 {
		t.Fatalf("findings: %+v", f)
	}
	if !strings.Contains(f[0].Summary, "never requested") {
		t.Errorf("summary = %q", f[0].Summary)
	}
	// It must not also be reported as an unresolved request.
	if len(findings(got, "id-resolves-once")) != 0 {
		t.Error("an answer to nothing was also counted as a request")
	}
}

// A frame charpy fabricated is charpy's. Reporting it would be charpy finding
// its own fault and attributing it to the subject.
func TestChapysOwnFramesAreNotFindings(t *testing.T) {
	got := invariant.Check(oracletest.New(t, transcript.ClassServer).
		Corrupted(res("99")).
		Done())

	if len(findings(got, "no-unsolicited-response")) != 0 {
		t.Errorf("charpy's own injected frame was reported: %+v", got.Findings)
	}
}

// I3: overlap is the fault, not reuse.
func TestNoDuplicateInflightID(t *testing.T) {
	t.Run("two requests in flight at once", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).
			ToSubject(req("7", "tools/list")).
			FromSubject(res("7")).
			Done())

		if len(findings(got, "no-duplicate-inflight-id")) != 1 {
			t.Errorf("findings: %+v", got.Findings)
		}
	})

	t.Run("reuse after the first was answered is not a duplicate", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassServer).
			ToSubject(req("7", "tools/call")).FromSubject(res("7")).
			ToSubject(req("7", "tools/list")).FromSubject(res("7")).
			Done())

		if n := len(findings(got, "no-duplicate-inflight-id")); n != 0 {
			t.Errorf("%d findings for sequential reuse of an id", n)
		}
	})
}

// envelope.ID.Key is type-qualified, and the invariants inherit that: the
// number 7 and the string "7" are two exchanges. An oracle that merged them
// would report a phantom double-answer on every vary_type case.
func TestNumberAndStringIDsAreDifferentExchanges(t *testing.T) {
	got := invariant.Check(oracletest.New(t, transcript.ClassServer).
		ToSubject(req("7", "tools/call")).FromSubject(res("7")).
		ToSubject(req(`"7"`, "tools/list")).FromSubject(res(`"7"`)).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("7 and \"7\" were treated as one exchange: %+v", got.Findings)
	}
}

// An id is unique within the exchange that issued it, so pairing across
// connections would manufacture findings.
func TestConnectionsAreSeparate(t *testing.T) {
	b := oracletest.New(t, transcript.ClassServer)
	b.Conn("c-1").ToSubject(req("7", "tools/call")).FromSubject(res("7"))
	b.Conn("c-2").ToSubject(req("7", "tools/call")).FromSubject(res("7"))

	if got := invariant.Check(b.Done()); len(got.Findings) != 0 {
		t.Errorf("the same id on two connections collided: %+v", got.Findings)
	}
}

// The registry names thirteen invariants; v0 evaluates three. A report that
// omitted the rest silently would read as ten passes.
func TestImplementedIsASubsetOfTheRegistry(t *testing.T) {
	for _, name := range invariant.Implemented() {
		if !invariant.Known(name) {
			t.Errorf("%q is evaluated but not registered", name)
		}
	}
	if len(invariant.Implemented()) >= len(invariant.All()) {
		t.Error("Implemented should be the subset v0 actually evaluates")
	}
}
