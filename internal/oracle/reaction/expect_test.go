package reaction_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/oracletest"
	"github.com/serverplumber/charpy/internal/oracle/reaction"
	"github.com/serverplumber/charpy/internal/transcript"
)

// The builder's faults belong to frame/malformed-request; these say what that
// case expects, as the catalogue would.
func expects(e reaction.Expectation) func(string) reaction.Expectation {
	return func(id string) reaction.Expectation {
		if id == "frame/malformed-request" {
			return e
		}
		return reaction.Expectation{}
	}
}

// destroyed writes the request a malformed_json fault leaves without its id,
// and the fault_applied that follows it.
func destroyed(b *oracletest.Builder) *oracletest.Builder {
	b.Raw(transcript.C2S, `{"jsonrpc":"2.0","id":3,"meth`, &transcript.Fault{
		CaseID: "frame/malformed-request", Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
		Kind: "malformed_json", Replaced: envelope.NumberID(3), ReplacedKind: envelope.KindRequest,
	})
	return b.FaultToSubject()
}

func expectation(t *testing.T, tr *transcript.Transcript, e reaction.Expectation) oracle.Finding {
	t.Helper()
	got := reaction.Expected(tr, expects(e)).Findings
	if len(got) != 1 {
		t.Fatalf("findings: %d, want 1: %+v", len(got), got)
	}
	if got[0].Verdict != oracle.Observed || got[0].Check != "expectation" {
		t.Errorf("got %s %s, want an OBSERVED expectation", got[0].Verdict, got[0].Check)
	}
	if got[0].Citation != "frame/malformed-request@2025-11-25#seed=8f2c1a" {
		t.Errorf("citation = %q", got[0].Citation)
	}
	return got[0]
}

const parseError = `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`

// A request too broken to carry an id is answered with a null id, as JSON-RPC
// says, and that answer is the one the expectation is about.
func TestAnExpectedErrorIsReported(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).FromSubject(parseError).Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "as the case expects") {
		t.Errorf("summary = %q", f.Summary)
	}
}

func TestAnotherErrorIsReportedAgainstTheExpectation(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).
		FromSubject(`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"no"}}`).Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "error -32601; the case expects -32700") {
		t.Errorf("summary = %q", f.Summary)
	}
}

// The subject answered the faulted request's id with a result: it took the
// broken bytes for a request it could serve.
func TestAResultWhereAnErrorIsExpectedIsReported(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).
		FromSubject(`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`).Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "answered with a result") {
		t.Errorf("summary = %q", f.Summary)
	}
}

func TestAnExitWhereAnErrorIsExpectedIsReported(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).SubjectExit(1).Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "exited without answering") {
		t.Errorf("summary = %q", f.Summary)
	}
}

func TestSilenceWhereAnErrorIsExpectedIsReported(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).SubjectKilled().Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "did not answer with an error") {
		t.Errorf("summary = %q", f.Summary)
	}
}

// An answer to some other id is not the answer to the faulted request.
func TestAnErrorForAnotherIDIsNotTheAnswer(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).
		FromSubject(`{"jsonrpc":"2.0","id":9,"error":{"code":-32700,"message":"x"}}`).
		SubjectKilled().Done()
	f := expectation(t, tr, reaction.Expectation{ErrorCode: -32700})
	if !strings.Contains(f.Summary, "did not answer with an error") {
		t.Errorf("summary = %q", f.Summary)
	}
}

func TestAnHTTPStatusIsComparedWithTheExpectation(t *testing.T) {
	for status, want := range map[int]string{400: "as the case expects", 500: "HTTP 500; the case expects 400"} {
		b := destroyed(oracletest.New(t, transcript.ClassServer))
		tr := b.RawHTTP(transcript.S2C, "malformed payload", status).Done()
		f := expectation(t, tr, reaction.Expectation{HTTPStatus: 400})
		if !strings.Contains(f.Summary, want) {
			t.Errorf("status %d: summary = %q, want %q", status, f.Summary, want)
		}
	}
}

// A case that declares nothing is judged by the generic tier alone, and a
// fault that reached charpy's peer asked the subject nothing to expect of.
func TestNothingIsExpectedOfWhatWasNotAsked(t *testing.T) {
	undeclared := destroyed(oracletest.New(t, transcript.ClassServer)).FromSubject(parseError).Done()
	if got := reaction.Expected(undeclared, expects(reaction.Expectation{})).Findings; len(got) != 0 {
		t.Errorf("a case declaring nothing produced %+v", got)
	}
	toCharpy := oracletest.New(t, transcript.ClassServer).FaultToCharpy().FromSubject(parseError).Done()
	if got := reaction.Expected(toCharpy, expects(reaction.Expectation{ErrorCode: -32700})).Findings; len(got) != 0 {
		t.Errorf("a fault that reached charpy produced %+v", got)
	}
}
