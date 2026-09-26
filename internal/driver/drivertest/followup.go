// Package drivertest holds the checks every driver's tests make of the
// transcript it wrote.
//
// It reads the transcript back through transcript.Read rather than poking at
// decoded JSON, so a driver is held to what the oracle will see and not to
// what its own test happened to look for.
package drivertest

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/reaction"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Read parses a transcript a test captured, failing the test if it does not.
func Read(t *testing.T, raw string) *transcript.Transcript {
	t.Helper()
	tr, err := transcript.Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("reading the transcript back: %v", err)
	}
	return tr
}

// FollowUpAnswered asserts that, after the last fault_applied event, the
// subject was put an untampered request on the fault's connection and
// answered it there, and returns the question and the answer.
//
// It is the follow-up the drivers ask so the reaction layer has something to
// judge (ADR-013). The last fault, because a case with no ordinal faults the
// script's own requests on the way and those are the case working; the first
// request after the last fault is the follow-up, and nothing else. Untampered
// matters as much as present: a question the armed case faulted on its way
// out is one the oracle rightly ignores.
func FollowUpAnswered(t *testing.T, tr *transcript.Transcript) (question, answer *transcript.FrameLine) {
	t.Helper()
	applied := tr.Events(transcript.FaultApplied)
	if len(applied) == 0 {
		t.Fatal("no fault_applied event: the fault never acted, so nothing follows it")
	}
	anchor := applied[len(applied)-1]
	class := tr.Header.Subject.Class

	for _, fr := range tr.Frames() {
		if fr.Seq <= anchor.Seq || !sameConn(fr.ConnID, anchor.ConnID) {
			continue
		}
		switch {
		case question == nil && fr.Kind == envelope.KindRequest &&
			oracle.SubjectReceives(class, fr.Face, fr.Direction):
			if fr.Tampered() {
				t.Fatalf("the follow-up %s at seq %d was faulted on its way to the subject", fr.MethodName(), fr.Seq)
			}
			question = fr
		case question != nil && fr.Key() == question.Key() &&
			(fr.Kind == envelope.KindResponse || fr.Kind == envelope.KindError):
			if !oracle.SubjectOriginated(class, fr) {
				t.Fatalf("the answer to the follow-up at seq %d was faulted on its way back", fr.Seq)
			}
			return question, fr
		}
	}
	if question == nil {
		t.Fatalf("nothing was asked of the subject after the fault at seq %d", anchor.Seq)
	}
	t.Fatalf("the follow-up %s at seq %d was never answered", question.MethodName(), question.Seq)
	return nil, nil
}

// ReactionAnswered asserts that the reaction layer, run over the transcript,
// finds that the subject answered after every fault.
func ReactionAnswered(t *testing.T, tr *transcript.Transcript) {
	t.Helper()
	var n int
	for _, f := range reaction.Check(tr).Findings {
		if f.Check != "reaction" {
			continue
		}
		n++
		if f.Verdict != oracle.Observed || f.Summary != "the subject answered after the fault" {
			t.Errorf("reaction at seq %d: %s %s (%s)", f.Seq, f.Verdict, f.Summary, f.Reason)
		}
	}
	if n == 0 {
		t.Fatal("reaction reported nothing about the fault")
	}
}

func sameConn(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
