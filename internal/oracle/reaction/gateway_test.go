package reaction_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/oracletest"
	"github.com/serverplumber/charpy/internal/transcript"
)

// The three questions charpy puts to a gateway after a fault (ADR-015), and
// their answers, on the downstream face.
const (
	gwInit     = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	gwInitOK   = `{"jsonrpc":"2.0","id":1,"result":{}}`
	gwPing     = `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	gwPingOK   = `{"jsonrpc":"2.0","id":7,"result":{}}`
	gwPath     = `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"u0_echo","arguments":{}}}`
	gwPathOK   = `{"jsonrpc":"2.0","id":8,"result":{"content":[]}}`
	gwPathErr  = `{"jsonrpc":"2.0","id":8,"error":{"code":-32603,"message":"upstream u0 unavailable"}}`
	gwOther    = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"u1_echo","arguments":{}}}`
	gwOtherOK  = `{"jsonrpc":"2.0","id":9,"result":{"content":[]}}`
	gwOtherErr = `{"jsonrpc":"2.0","id":9,"error":{"code":-32603,"message":"busy"}}`
)

// gatewayAfterUpstreamFault starts a gateway transcript whose fault reached
// it on its upstream face, from upstream u0, and turns to the downstream face
// where the questions are asked, on the connection charpy's client opened
// before it.
func gatewayAfterUpstreamFault(t *testing.T) *oracletest.Builder {
	return faultOn(t, "u0-1")
}

// faultOn is gatewayAfterUpstreamFault with the fault on upstream conn.
func faultOn(t *testing.T, conn string) *oracletest.Builder {
	return oracletest.New(t, transcript.ClassGateway).
		Conn("d-1").ToSubject(gwInit).FromSubject(gwInitOK).
		Face(transcript.Upstream).Conn(conn).FaultToSubject().
		Face(transcript.Downstream).Conn("d-1")
}

// asked drops the findings for questions charpy never asked, for tests about
// the ones it did.
func asked(fs []oracle.Finding) []oracle.Finding {
	return slices.DeleteFunc(fs, func(f oracle.Finding) bool { return f.Reason == "question-not-asked" })
}

// summaries is each finding's summary, in order.
func summaries(fs []oracle.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Summary)
	}
	return out
}

// Each question gets a finding of its own, named by its role: whether the
// gateway lives, whether the path the fault took works, whether the rest of
// it was spared.
func TestEachQuestionToAGatewayIsReportedByRole(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		ToSubject(gwPing).FromSubject(gwPingOK).
		ToSubject(gwPath).FromSubject(gwPathErr).
		ToSubject(gwOther).FromSubject(gwOtherOK).
		Done()

	fs := reactions(t, tr)
	want := []string{
		"the gateway answered a ping after the fault",
		"the gateway answered a call through u0 (the upstream the fault reached) with an error after the fault",
		"the gateway answered a call through u1 (an upstream the fault did not reach) after the fault",
	}
	got := summaries(fs)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summaries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range fs {
		if f.Verdict != oracle.Observed {
			t.Errorf("%q: verdict %s, want OBSERVED", f.Summary, f.Verdict)
		}
	}
	if !strings.Contains(fs[1].Detail, "-32603 upstream u0 unavailable") {
		t.Errorf("error detail = %q", fs[1].Detail)
	}
}

// Which upstream the fault reached is the connection it applied on, so the
// same calls swap roles when the fault was u1's.
func TestTheFaultedUpstreamIsTheFaultsConnection(t *testing.T) {
	tr := faultOn(t, "u1-1").
		ToSubject(gwPath).FromSubject(gwPathOK).
		ToSubject(gwOther).FromSubject(gwOtherOK).
		Done()

	got := summaries(asked(reactions(t, tr)))
	if len(got) != 2 ||
		!strings.Contains(got[0], "u0 (an upstream the fault did not reach)") ||
		!strings.Contains(got[1], "u1 (the upstream the fault reached)") {
		t.Errorf("summaries = %q", got)
	}
}

// A fault on the downstream face reached no upstream in particular, so calls
// are named by their upstream alone.
func TestADownstreamFaultNamesNoUpstreamAsReached(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassGateway).
		Conn("d-1").FaultToSubject().
		ToSubject(gwPing).FromSubject(gwPingOK).
		ToSubject(gwPath).FromSubject(gwPathOK).
		ToSubject(gwOther).FromSubject(gwOtherOK).
		Done()

	got := summaries(reactions(t, tr))
	want := []string{
		"the gateway answered a ping after the fault",
		"the gateway answered a call through u0 after the fault",
		"the gateway answered a call through u1 after the fault",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("summaries = %q", got)
	}
}

// A gateway with one upstream is asked two questions, and two are reported.
func TestASingleUpstreamGatewayIsAskedNoHealthyCall(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		ToSubject(gwPing).FromSubject(gwPingOK).
		ToSubject(gwPath).FromSubject(gwPathOK).
		Done()

	if got := summaries(reactions(t, tr)); len(got) != 2 {
		t.Errorf("summaries = %q, want two", got)
	}
}

// A question still open when the run ends is a hang, and says so per role:
// the gateway answered the ping but hung the call through the faulted path.
func TestAnUnansweredQuestionToAGatewayIsAHang(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		ToSubject(gwPing).FromSubject(gwPingOK).
		ToSubject(gwPath).
		ToSubject(gwOther).FromSubject(gwOtherOK).
		SubjectKilled().
		Done()

	fs := reactions(t, tr)
	if len(fs) != 3 {
		t.Fatalf("findings = %q", summaries(fs))
	}
	if fs[1].Summary != "the gateway did not answer a call through u0 (the upstream the fault reached) after the fault" {
		t.Errorf("summary = %q", fs[1].Summary)
	}
	if fs[1].Verdict != oracle.Observed || !strings.Contains(fs[1].Detail, "still unanswered") {
		t.Errorf("finding = %+v", fs[1])
	}
}

// An answer is matched within its connection: the gateway's answer on another
// downstream connection with the same id answers nothing asked here.
func TestAGatewaysAnswerOnAnotherConnectionDoesNotCount(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		ToSubject(gwPath).
		Conn("d-2").FromSubject(gwPathOK).
		Done()

	if f := only(t, asked(reactions(t, tr))); !strings.Contains(f.Summary, "did not answer") {
		t.Errorf("summary = %q", f.Summary)
	}
}

// Frames on the upstream face are the gateway's own traffic, not questions
// charpy put to it, so every question is reported as not asked.
func TestUpstreamTrafficIsNotAQuestionToTheGateway(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		Face(transcript.Upstream).Conn("u0-1").
		FromSubject(gwPath).ToSubject(gwPathOK).
		Done()

	wantUnasked(t, reactions(t, tr),
		"charpy did not ask the gateway a ping after the fault",
		"charpy did not ask the gateway a call through u0 (the upstream the fault reached) after the fault")
}

// wantUnasked asserts fs is exactly the not-asked findings for want, in order.
func wantUnasked(t *testing.T, fs []oracle.Finding, want ...string) {
	t.Helper()
	if got := summaries(fs); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summaries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range fs {
		if f.Verdict != oracle.Inconclusive || f.Reason != "question-not-asked" {
			t.Errorf("finding = %+v", f)
		}
	}
}

// A ping on a connection opened after the fault is charpy's liveness probe,
// not a question: it cannot stand in for the ping the questions never sent.
func TestAPingOnALaterConnectionIsNoQuestion(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		Conn("d-2").ToSubject(gwInit).FromSubject(gwInitOK).
		ToSubject(gwPing).FromSubject(gwPingOK).
		Done()

	wantUnasked(t, reactions(t, tr),
		"charpy did not ask the gateway a ping after the fault",
		"charpy did not ask the gateway a call through u0 (the upstream the fault reached) after the fault")
}

// The driver's note on a question that failed without crossing is cited, so
// the report says why it was not asked.
func TestAnUnaskedQuestionCitesTheDriversNote(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		QuestionFailed("ping", "connection closed").
		QuestionFailed("call through u1", "connection closed").
		ToSubject(gwPath).FromSubject(gwPathOK).
		Done()

	fs := reactions(t, tr)
	wantUnasked(t, fs[1:], "charpy did not ask the gateway a ping after the fault")
	if !strings.HasSuffix(fs[1].Detail, "charpy's client: connection closed") || strings.Contains(fs[1].Detail, "u1") {
		t.Errorf("detail = %q", fs[1].Detail)
	}
}

// With two upstreams, an unasked call through the spared one is named.
func TestAnUnaskedCallThroughTheSparedUpstreamIsNamed(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		Face(transcript.Upstream).Conn("u1-1").ToSubject(gwInit).
		Face(transcript.Downstream).Conn("d-1").
		ToSubject(gwPing).FromSubject(gwPingOK).
		ToSubject(gwPath).FromSubject(gwPathOK).
		Done()

	fs := reactions(t, tr)
	if len(fs) != 3 {
		t.Fatalf("findings = %q", summaries(fs))
	}
	wantUnasked(t, fs[2:],
		"charpy did not ask the gateway a call through u1 (an upstream the fault did not reach) after the fault")
}

// A gateway that exits on its own after the fault is reported, beside what
// it left unanswered.
func TestAGatewayThatExitsIsReported(t *testing.T) {
	tr := gatewayAfterUpstreamFault(t).
		ToSubject(gwPing).
		SubjectExit(2).
		Done()

	got := summaries(asked(reactions(t, tr)))
	if len(got) != 2 || got[0] != "the gateway exited after the fault" ||
		!strings.Contains(got[1], "did not answer a ping") {
		t.Errorf("summaries = %q", got)
	}
}

// The gateway's upstream sends it requests; a fault on one of those, put to
// it upstream as client to server, reached charpy's own server instead.
func TestAFaultOnTheGatewaysOwnUpstreamRequestReachedCharpy(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassGateway).
		Face(transcript.Upstream).Conn("u0-1").FaultToCharpy().
		Face(transcript.Downstream).Conn("d-1").
		ToSubject(gwPing).FromSubject(gwPingOK).
		Done()

	if f := only(t, reactions(t, tr)); f.Verdict != oracle.Skipped {
		t.Errorf("verdict = %s, want SKIPPED", f.Verdict)
	}
}

const (
	carriedCall   = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"u0_add_numbers","arguments":{"a":1,"b":0}}}`
	carriedErr    = `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"upstream u0 idle"}}`
	carriedCancel = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`
	otherCancel   = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"2"}}`
)

// carrying starts a gateway transcript in which charpy's call, joined j1
// under via, is in flight downstream when a fault on the same join lands on
// u0's answer to its forwarded copy.
func carrying(t *testing.T, via transcript.Via) *oracletest.Builder {
	return oracletest.New(t, transcript.ClassGateway).
		Conn("d-1").Joined("j1", via).ToSubject(carriedCall).
		Face(transcript.Upstream).Conn("u0-1").FaultToSubject().
		Joined("", "").Face(transcript.Downstream).Conn("d-1")
}

// The call the fault was carrying is judged by the join the fault records:
// here the gateway gave up on the upstream and said so.
func TestTheCarriedCallIsJudgedByItsJoin(t *testing.T) {
	tr := carrying(t, transcript.ViaTraced).
		FromSubject(carriedErr).
		ToSubject(gwPing).FromSubject(gwPingOK).
		Done()

	fs := asked(reactions(t, tr))
	got := summaries(fs)
	if len(got) != 2 || got[0] != "the gateway answered the call the fault was carrying (through u0) with an error" {
		t.Fatalf("summaries = %q", got)
	}
	if fs[0].Verdict != oracle.Observed || !strings.Contains(fs[0].Detail, "-32603 upstream u0 idle") {
		t.Errorf("finding = %+v", fs[0])
	}
}

// A carried call left unanswered until charpy's client cancelled it is a
// hang, and the detail says who ended the wait.
func TestACarriedCallCancelledUnansweredIsAHang(t *testing.T) {
	tr := carrying(t, transcript.ViaTraced).
		ToSubject(carriedCancel).
		ToSubject(gwPing).FromSubject(gwPingOK).
		Done()

	f := reactions(t, tr)[0]
	if f.Summary != "the gateway did not answer the call the fault was carrying (through u0)" {
		t.Errorf("summary = %q", f.Summary)
	}
	if !strings.Contains(f.Detail, "cancelled it at seq") {
		t.Errorf("detail = %q", f.Detail)
	}
}

// A cancellation naming "2" is not one naming 2.
func TestACancellationOfAnotherIDIsNotTheCarriedCalls(t *testing.T) {
	tr := carrying(t, transcript.ViaTraced).
		ToSubject(otherCancel).
		Done()

	f := only(t, asked(reactions(t, tr)))
	if !strings.Contains(f.Detail, "still unanswered") {
		t.Errorf("detail = %q", f.Detail)
	}
}

// A join by content is no fact, and the carried call is said to be joined
// that way rather than judged.
func TestACarriedCallJoinedByContentIsInconclusive(t *testing.T) {
	tr := carrying(t, transcript.ViaInferred).
		FromSubject(carriedErr).
		Done()

	f := only(t, asked(reactions(t, tr)))
	if f.Verdict != oracle.Inconclusive || f.Reason != "carried-call-join-inferred" {
		t.Errorf("finding = %+v", f)
	}
}

// A judged carried call does not stand in for the questions: with none asked
// after it, each is still reported as not asked.
func TestACarriedCallDoesNotHideTheUnaskedQuestions(t *testing.T) {
	tr := carrying(t, transcript.ViaTraced).
		FromSubject(carriedErr).
		Done()

	fs := reactions(t, tr)
	if len(fs) == 0 || fs[0].Verdict != oracle.Observed {
		t.Fatalf("findings = %+v", fs)
	}
	wantUnasked(t, fs[1:],
		"charpy did not ask the gateway a ping after the fault",
		"charpy did not ask the gateway a call through u0 (the upstream the fault reached) after the fault")
}
