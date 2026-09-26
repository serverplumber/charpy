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

// A fault on a frame the subject sent lands on charpy's own peer. The subject
// was never asked, and that is said rather than rendered as a pass.
func TestAFaultThatReachedCharpyIsSkippedWithAReason(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).
		FaultToCharpy().
		ToSubject(pingReq).
		FromSubject(pingResp).
		Done()

	f := only(t, reactions(t, tr))
	if f.Verdict != oracle.Skipped {
		t.Errorf("verdict = %s, want SKIPPED", f.Verdict)
	}
	if f.Reason != "fault-reached-charpy" {
		t.Errorf("reason = %q", f.Reason)
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
