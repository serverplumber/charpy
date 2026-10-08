package reaction_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/oracletest"
	"github.com/serverplumber/charpy/internal/oracle/reaction"
	"github.com/serverplumber/charpy/internal/transcript"
)

// reactions returns the findings of the reaction check alone, so a test about
// one fault is not also a test about probes.
func reactions(t *testing.T, tr *transcript.Transcript) []oracle.Finding {
	t.Helper()
	var out []oracle.Finding
	for _, f := range reaction.Check(tr).Findings {
		if f.Check == "reaction" {
			out = append(out, f)
		}
	}
	return out
}

func only(t *testing.T, fs []oracle.Finding) oracle.Finding {
	t.Helper()
	if len(fs) != 1 {
		t.Fatalf("findings: %d, want 1: %+v", len(fs), fs)
	}
	return fs[0]
}

const (
	callReq  = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`
	callResp = `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`
	pingReq  = `{"jsonrpc":"2.0","id":3,"method":"ping"}`
	pingResp = `{"jsonrpc":"2.0","id":3,"result":{}}`
)

// The subject answered the next question after the fault reached it, and that
// is reported: still serving is the observation worth making, not a silence.
func TestASubjectThatAnswersAfterTheFaultIsReported(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		FromSubject(pingResp).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed {
		t.Errorf("verdict = %s, want OBSERVED", f.Verdict)
	}
	if !strings.Contains(f.Summary, "answered after the fault") {
		t.Errorf("summary = %q", f.Summary)
	}
	// It belongs to the case that caused it, or a report cannot say which
	// fault the subject survived.
	if f.Citation != "frame/malformed-request@2025-11-25#seed=8f2c1a" {
		t.Errorf("citation = %q", f.Citation)
	}
}

// The fault reached the subject and then nothing asked it anything. That is a
// run which did not put the question, not a subject that failed it.
func TestNothingAskedAfterTheFaultIsInconclusive(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		ToSubject(callReq).
		FaultToSubject().
		FromSubject(callResp).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Inconclusive {
		t.Errorf("verdict = %s, want INCONCLUSIVE", f.Verdict)
	}
	if f.Reason != "nothing-asked-after-fault" {
		t.Errorf("reason = %q", f.Reason)
	}
}

// A request asked before the fault and answered after says nothing about
// whether the subject still serves: that exchange was already under way.
func TestAnExchangeBegunBeforeTheFaultIsNotTheAnswer(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		ToSubject(callReq).
		FaultToSubject().
		FromSubject(callResp).
		ToSubject(pingReq).
		SubjectKilled().
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed || !strings.Contains(f.Summary, "did not answer") {
		t.Errorf("got %s %q, want the unanswered ping reported", f.Verdict, f.Summary)
	}
}

// A question left open when charpy ended the run is reported as the fact it
// is, with how long it was open, rather than as a crash or as silence.
func TestAnUnansweredQuestionIsReported(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		SubjectKilled().
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed {
		t.Errorf("verdict = %s, want OBSERVED", f.Verdict)
	}
	if !strings.Contains(f.Summary, "did not answer") {
		t.Errorf("summary = %q", f.Summary)
	}
	if !strings.Contains(f.Detail, "ping") {
		t.Errorf("detail does not name the unanswered request: %q", f.Detail)
	}
}

// Under fault injection a subject that goes away is the finding.
func TestASubjectThatExitsAfterTheFaultIsReported(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		SubjectExit(2).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed || !strings.Contains(f.Summary, "exited") {
		t.Errorf("got %s %q, want the exit reported", f.Verdict, f.Summary)
	}
	if !strings.Contains(f.Detail, "exit code 2") {
		t.Errorf("detail = %q", f.Detail)
	}
}

// charpy's own shutdown is not the subject going away.
func TestCharpyEndingTheRunIsNotAnExit(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		SubjectKilled().
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Inconclusive || f.Reason != "nothing-asked-after-fault" {
		t.Errorf("charpy's kill was read as the subject's doing: %s %q", f.Verdict, f.Summary)
	}
}

// The reaction is the subject's to give on the connection the fault reached.
// An answer on another connection is a different conversation.
func TestAnAnswerOnAnotherConnectionDoesNotCount(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		Conn("c-2").
		ToSubject(pingReq).
		FromSubject(pingResp).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Inconclusive {
		t.Errorf("verdict = %s, want INCONCLUSIVE: nothing was asked on the faulted connection", f.Verdict)
	}
}

// A follow-up charpy itself damaged is not a question the subject can be held
// to answering.
func TestATamperedQuestionDoesNotCount(t *testing.T) {
	b := oracletest.New(t, transcript.ClassServer).FaultToSubject()
	b.Raw(transcript.C2S, pingReq, &transcript.Fault{
		CaseID: "frame/malformed-request", Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
		Kind: "malformed_json",
	})
	tr := b.SubjectKilled().Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Inconclusive {
		t.Errorf("verdict = %s, want INCONCLUSIVE", f.Verdict)
	}
}

// The same judgement from the other side: a client receives s2c, so charpy's
// server asks it (a ping) and the client answers c2s.
func TestAClientSubjectIsJudgedFromItsSide(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassClient).
		FaultToSubject().
		ToSubject(pingReq).
		FromSubject(pingResp).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed || !strings.Contains(f.Summary, "answered after the fault") {
		t.Errorf("got %s %q", f.Verdict, f.Summary)
	}
}

// A fault_applied written before ADR-013 records no direction, so it cannot
// say who the fault was put to. The layer does not guess.
func TestAFaultWithNoRecordedDirectionYieldsNothing(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		Raw(transcript.C2S, pingReq, nil).
		Done()
	// Rewrite the only fault-free transcript into one with a bare
	// fault_applied, as an older writer produced it.
	tr.Entries = append(tr.Entries, transcript.Entry{
		Common: transcript.Common{Seq: int64(len(tr.Entries))},
		Event: &transcript.EventLine{
			EventKind: transcript.FaultApplied,
			Detail:    map[string]any{"verb": "rewrite"},
		},
	})

	if fs := reactions(t, tr); len(fs) != 0 {
		t.Errorf("a direction-less fault produced findings: %+v", fs)
	}
}

// A subject that answers the question after a fault with a frame that never
// ends has reacted, and the reaction is that frame -- not a silence, which is
// what reading on to the end of the run would call it.
func TestAFrameThatNeverEndsIsTheReaction(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		CappedFromSubject().
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Observed || !strings.Contains(f.Summary, "past charpy's size cap") {
		t.Errorf("got %s %q, want the capped frame as the reaction", f.Verdict, f.Summary)
	}
	if !strings.Contains(f.Detail, "5242880 bytes") {
		t.Errorf("detail = %q, want how much charpy read", f.Detail)
	}
}

func frameCaps(t *testing.T, tr *transcript.Transcript) []oracle.Finding {
	t.Helper()
	var out []oracle.Finding
	for _, f := range reaction.Check(tr).Findings {
		if f.Check == "frame-cap" {
			out = append(out, f)
		}
	}
	return out
}

// Every capped frame is reported in its own right, and belongs to the fault
// before it on its connection.
func TestACappedFrameCitesTheFaultBeforeIt(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		CappedFromSubject().
		Done()

	f := only(t, frameCaps(t, tr))
	// MCP sets no frame size: the cap is charpy's, and never a MUST.
	if f.Verdict != oracle.Observed {
		t.Errorf("verdict = %s, want OBSERVED", f.Verdict)
	}
	if f.Citation != "frame/malformed-request@2025-11-25#seed=8f2c1a" {
		t.Errorf("citation = %q", f.Citation)
	}
	if !strings.HasPrefix(f.Summary, "the subject ") {
		t.Errorf("summary = %q, want the subject named as the sender", f.Summary)
	}
}

// A capped frame with no fault before it is still reported. Under the
// conformance precondition it is the stronger signal, not noise.
func TestACappedFrameWithNoFaultIsStillReported(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		ToSubject(pingReq).
		CappedFromSubject().
		Done()

	f := only(t, frameCaps(t, tr))
	if f.Citation != "" {
		t.Errorf("citation = %q, want none: no fault came first", f.Citation)
	}
}

// A frame past the cap from the subject's peer is not the subject's reaction,
// and is reported as the peer's.
func TestAPeersCappedFrameIsNotTheSubjects(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		CappedToSubject().
		Done()

	f := only(t, reactions(t, tr))
	if strings.Contains(f.Summary, "size cap") {
		t.Errorf("reaction = %q, want the peer's frame not read as the subject's", f.Summary)
	}
	if c := only(t, frameCaps(t, tr)); !strings.HasPrefix(c.Summary, "the subject's peer ") {
		t.Errorf("summary = %q", c.Summary)
	}
}

// A stream that broke on a read error after the fault leaves the run unable
// to say what the subject did: not an answer, and not a silence either.
func TestAStreamErrorAfterTheFaultIsInconclusive(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToSubject().
		ToSubject(pingReq).
		StreamError("read |0: input/output error").
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Inconclusive || f.Reason != "stream-error" {
		t.Errorf("got %s %q, want INCONCLUSIVE stream-error", f.Verdict, f.Reason)
	}
	if !strings.Contains(f.Detail, "input/output error") {
		t.Errorf("detail = %q, want the error's text", f.Detail)
	}
}
