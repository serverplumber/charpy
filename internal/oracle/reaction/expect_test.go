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

// A case that declares nothing is judged by the generic tier alone.
func TestNothingIsExpectedOfWhatWasNotAsked(t *testing.T) {
	undeclared := destroyed(oracletest.New(t, transcript.ClassServer)).FromSubject(parseError).Done()
	if got := reaction.Expected(undeclared, expects(reaction.Expectation{})).Findings; len(got) != 0 {
		t.Errorf("a case declaring nothing produced %+v", got)
	}
}

// narrowed writes a client's handshake with charpy's answer rewritten to
// advertise only logging, as capability_flip leaves it, and the fault_applied
// that follows.
func narrowed(b *oracletest.Builder) *oracletest.Builder {
	b.Face(transcript.Upstream)
	b.FromSubject(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	b.Raw(transcript.S2C, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"logging":{}},"serverInfo":{"name":"s","version":"1"}}}`,
		&transcript.Fault{
			CaseID: "frame/malformed-request", Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
			Kind: "capability_flip",
		})
	return b.FaultToSubject()
}

// The narrowed answer is what the client negotiated, so a tools/call after it
// uses a capability that was not, whatever the server offered before.
func TestARequestOutsideTheNegotiatedCapabilitiesIsReported(t *testing.T) {
	tr := narrowed(oracletest.New(t, transcript.ClassClient)).
		FromSubject(`{"jsonrpc":"2.0","method":"notifications/initialized"}`).
		FromSubject(`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info"}}`).
		FromSubject(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`).
		Done()
	f := expectation(t, tr, reaction.Expectation{NegotiatedOnly: true})
	if !strings.Contains(f.Summary, "tools/call (needs tools)") || strings.Contains(f.Summary, "logging") {
		t.Errorf("summary = %q, want tools/call alone", f.Summary)
	}
}

func TestKeepingToTheNegotiatedCapabilitiesIsReported(t *testing.T) {
	tr := narrowed(oracletest.New(t, transcript.ClassClient)).
		FromSubject(`{"jsonrpc":"2.0","id":2,"method":"ping"}`).
		FromSubject(`{"jsonrpc":"2.0","id":3,"method":"logging/setLevel","params":{"level":"info"}}`).
		Done()
	f := expectation(t, tr, reaction.Expectation{NegotiatedOnly: true})
	if !strings.Contains(f.Summary, "sent only what the handshake") {
		t.Errorf("summary = %q", f.Summary)
	}
}

// A fault that rewrote something other than a handshake gives the expectation
// nothing negotiated to judge against, and says so rather than passing.
func TestNoHandshakeIsNothingToJudgeAgainst(t *testing.T) {
	tr := destroyed(oracletest.New(t, transcript.ClassServer)).FromSubject(parseError).Done()
	f := expectation(t, tr, reaction.Expectation{NegotiatedOnly: true})
	if !strings.Contains(f.Summary, "did not land on a handshake") {
		t.Errorf("summary = %q", f.Summary)
	}
}
