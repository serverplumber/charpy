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
	// The subject allocates the ids of the requests it sends: a client
	// subject's are c2s.
	t.Run("two requests in flight at once", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassClient).
			FromSubject(req("7", "tools/call")).
			FromSubject(req("7", "tools/list")).
			ToSubject(res("7")).
			Done())

		if len(findings(got, "no-duplicate-inflight-id")) != 1 {
			t.Errorf("findings: %+v", got.Findings)
		}
	})

	t.Run("reuse after the first was answered is not a duplicate", func(t *testing.T) {
		got := invariant.Check(oracletest.New(t, transcript.ClassClient).
			FromSubject(req("7", "tools/call")).ToSubject(res("7")).
			FromSubject(req("7", "tools/list")).ToSubject(res("7")).
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

// A request charpy destroyed is not an answer charpy replaced. The subject was
// never asked with that id, so it owes the id nothing -- and when the same id
// then does cross intact and goes unanswered, that silence is the subject's,
// not "charpy replaced the answer".
func TestAReplacedRequestIsNotAReplacedAnswer(t *testing.T) {
	b := oracletest.New(t, transcript.ClassServer)
	b.Raw(transcript.C2S, `{"jsonrpc":"2.0","id":7,"meth`, &transcript.Fault{
		CaseID:       "stream/truncate-mid-frame",
		Citation:     "stream/truncate-mid-frame@2025-11-25#seed=8f2c1a",
		Kind:         "truncate",
		Replaced:     envelope.NumberID(7),
		ReplacedKind: envelope.KindRequest,
	})
	tr := b.ToSubject(req("7", "tools/call")).Done()

	var neverAnswered bool
	for _, f := range invariant.Check(tr).Findings {
		if f.Reason == "charpy-replaced-the-answer" {
			t.Errorf("a destroyed request was reported as a replaced answer: %s", f.Summary)
		}
		if f.Verdict == oracle.Observed && strings.Contains(f.Summary, "never answered") {
			neverAnswered = true
		}
	}
	if !neverAnswered {
		t.Error("the intact request the subject left unanswered was not reported")
	}
}

// A destroyed request on its own is owed nothing: no finding at all.
func TestADestroyedRequestAloneOwesNothing(t *testing.T) {
	b := oracletest.New(t, transcript.ClassServer)
	b.Raw(transcript.C2S, `{"jsonrpc":"2.0","id":7,"meth`, &transcript.Fault{
		CaseID:       "stream/truncate-mid-frame",
		Citation:     "stream/truncate-mid-frame@2025-11-25#seed=8f2c1a",
		Kind:         "truncate",
		Replaced:     envelope.NumberID(7),
		ReplacedKind: envelope.KindRequest,
	})
	if fs := invariant.Check(b.Done()).Findings; len(fs) != 0 {
		t.Errorf("findings for a request the subject never received: %+v", fs)
	}
}

// A server handed the same id twice at once -- charpy's duplicate beside the
// original -- did not allocate either, and answering each is what it was asked
// to do. Neither I3 nor I1 may file that against it.
func TestADuplicateTheSubjectReceivedIsNotItsFinding(t *testing.T) {
	b := oracletest.New(t, transcript.ClassServer).ToSubject(req("7", "tools/call"))
	b.Raw(transcript.C2S, req("7", "tools/call"), &transcript.Fault{
		CaseID: "id/duplicate-request-inflight", Citation: "id/duplicate-request-inflight@2025-11-25#seed=8f2c1a",
		Kind: "duplicate_id",
	})
	got := invariant.Check(b.FromSubject(res("7")).FromSubject(res("7")).Done())

	for _, f := range got.Findings {
		t.Errorf("a finding against a server for a duplicate it received: %s %s", f.Check, f.Summary)
	}
}

// A line cut before its newline is whole JSON still carrying its id, and the
// recipient never read it. Neither an answer nor a request cut that way may
// count as having arrived under the id it still parses to.
func TestAFrameCutShortOfItsDelimiterDidNotArrive(t *testing.T) {
	cut := func(id envelope.ID, kind envelope.Kind) *transcript.Fault {
		return &transcript.Fault{
			CaseID: "stream/truncate-mid-line", Citation: "stream/truncate-mid-line@2025-11-25#seed=8f2c1a",
			Kind: "truncate", Replaced: id, ReplacedKind: kind,
		}
	}

	t.Run("an answer", func(t *testing.T) {
		b := oracletest.New(t, transcript.ClassServer).ToSubject(req("7", "tools/call"))
		b.Raw(transcript.S2C, res("7"), cut(envelope.NumberID(7), envelope.KindResponse))
		var replaced bool
		for _, f := range invariant.Check(b.Done()).Findings {
			if f.Reason == "charpy-replaced-the-answer" {
				replaced = true
			}
		}
		if !replaced {
			t.Error("an answer cut before its newline was counted as having arrived")
		}
	})

	t.Run("a request", func(t *testing.T) {
		b := oracletest.New(t, transcript.ClassServer)
		b.Raw(transcript.C2S, req("7", "tools/call"), cut(envelope.NumberID(7), envelope.KindRequest))
		if fs := invariant.Check(b.Done()).Findings; len(fs) != 0 {
			t.Errorf("a request the subject never read was held against it: %+v", fs)
		}
	})
}

// JSON-RPC ids are the sender's. A client's request 1 and a server's request
// 1 are two exchanges, and an answer to one is no answer to the other: here
// the server answered the client's initialize, and the client never answered
// the server's ping, which shares its id.
func TestEachSendersIDsAreTheirOwn(t *testing.T) {
	got := invariant.Check(oracletest.New(t, transcript.ClassClient).
		FromSubject(req("1", "initialize")).
		ToSubject(res("1")).
		ToSubject(req("1", "ping")).
		Done())

	var neverAnswered bool
	for _, f := range got.Findings {
		if f.Check == "id-resolves-once" && strings.Contains(f.Summary, "never answered") &&
			strings.Contains(f.Detail, "ping") {
			neverAnswered = true
		}
	}
	if !neverAnswered {
		t.Errorf("the client's unanswered ping was hidden by the answer to its initialize: %+v", got.Findings)
	}
}
