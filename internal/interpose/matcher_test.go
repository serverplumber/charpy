package interpose_test

import (
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

// frame is a frame of the ordinary exchange: a client's request travelling
// c2s, and the server's answer, or its notification, travelling s2c. The
// ledger keys an exchange by the direction its request came, so the two have
// to agree.
func frame(kind envelope.Kind, method string) interpose.Frame {
	dir := transcript.S2C
	if kind == envelope.KindRequest {
		dir = transcript.C2S
	}
	return interpose.Frame{
		Face:      transcript.Downstream,
		Direction: dir,
		Kind:      kind,
		Method:    method,
		ClientID:  "c0",
		SessionID: "s-1",
		ConnID:    conn,
	}
}

func caseWith(id string, m interpose.Match) interpose.Case {
	return interpose.Case{ID: id, Citation: id, Match: m}
}

func matched(cases []interpose.Case) bool { return len(cases) > 0 }

func TestMatcherPredicates(t *testing.T) {
	tests := []struct {
		name  string
		match interpose.Match
		frame interpose.Frame
		want  bool
	}{
		{
			name:  "an empty match table takes the first frame on any face",
			match: interpose.Match{},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  true,
		},
		{
			name:  "method exact",
			match: interpose.Match{Method: interpose.ParseGlob("tools/call")},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  true,
		},
		{
			name:  "method mismatch",
			match: interpose.Match{Method: interpose.ParseGlob("tools/call")},
			frame: frame(envelope.KindRequest, "tools/list"),
			want:  false,
		},
		{
			name:  "method glob spans the separator",
			match: interpose.Match{Method: interpose.ParseGlob("*")},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  true,
		},
		{
			name:  "face mismatch",
			match: interpose.Match{Face: transcript.Upstream},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  false,
		},
		{
			name:  "direction match",
			match: interpose.Match{Direction: transcript.C2S},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  true,
		},
		{
			name:  "kind mismatch",
			match: interpose.Match{Kind: envelope.KindNotification},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  false,
		},
		{
			name:  "session glob",
			match: interpose.Match{SessionID: interpose.ParseGlob("s-*")},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  true,
		},
		{
			name: "every present key must match",
			match: interpose.Match{
				Method: interpose.ParseGlob("tools/call"),
				Face:   transcript.Downstream,
				Kind:   envelope.KindResponse,
			},
			frame: frame(envelope.KindRequest, "tools/call"),
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := interpose.NewLedger(clock.NewInjected())
			m := interpose.NewMatcher(l, caseWith("x/y", tc.match))
			if got := matched(m.Select(tc.frame)); got != tc.want {
				t.Errorf("Select = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOccurrenceSelectsTheNthMatchingFrame(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("id/dup", interpose.Match{
		Method:     interpose.ParseGlob("tools/call"),
		Occurrence: 2,
	}))

	// A frame that fails the predicates must not advance the ordinal.
	if matched(m.Select(frame(envelope.KindRequest, "tools/list"))) {
		t.Fatal("a non-matching method matched")
	}
	if matched(m.Select(frame(envelope.KindRequest, "tools/call"))) {
		t.Error("the first matching frame matched a case wanting the second")
	}
	if !matched(m.Select(frame(envelope.KindRequest, "tools/call"))) {
		t.Error("the second matching frame did not match")
	}
	if matched(m.Select(frame(envelope.KindRequest, "tools/call"))) {
		t.Error("the third matching frame matched a case wanting only the second")
	}
}

func TestOccurrenceEvery(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("id/dup", interpose.Match{Every: 3}))

	var hits []int
	for i := 1; i <= 7; i++ {
		if matched(m.Select(frame(envelope.KindRequest, "ping"))) {
			hits = append(hits, i)
		}
	}
	if len(hits) != 2 || hits[0] != 3 || hits[1] != 6 {
		t.Errorf("matched frames %v, want every third: [3 6]", hits)
	}
}

// A case with no ordinal takes the first frame that fits and no other. One
// fault is what a case means; under a relay, where charpy does not choose the
// traffic, "every frame that fits" was a storm of the same fault.
func TestNoOrdinalSelectsTheFirstMatchingFrameOnly(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("frame/malformed", interpose.Match{
		Method: interpose.ParseGlob("tools/call"),
	}))

	// A frame that fails the predicates is not the first match.
	if matched(m.Select(frame(envelope.KindRequest, "initialize"))) {
		t.Fatal("a non-matching method matched")
	}
	var hits []int
	for i := 1; i <= 3; i++ {
		if matched(m.Select(frame(envelope.KindRequest, "tools/call"))) {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 || hits[0] != 1 {
		t.Errorf("matched frames %v, want only the first: [1]", hits)
	}
}

// Every matching frame is still available, but it has to be asked for.
func TestEveryOneSelectsEachMatchingFrame(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("frame/malformed", interpose.Match{Every: 1}))

	for i := 1; i <= 3; i++ {
		if !matched(m.Select(frame(envelope.KindRequest, "ping"))) {
			t.Errorf("frame %d did not match a case asking for every frame", i)
		}
	}
}

// after_mono_ms is a predicate, not a post-filter. A frame arriving before the
// gate is not a matching frame and must not spend the ordinal a later frame is
// waiting for. If this ever regresses, a case with both keys fires one frame
// early and nothing else in the suite notices.
func TestClockGateDoesNotConsumeAnOrdinal(t *testing.T) {
	sched := clock.NewInjected()
	l := interpose.NewLedger(sched)
	m := interpose.NewMatcher(l, caseWith("stream/trunc", interpose.Match{
		Occurrence: 2,
		AfterMono:  clock.FromMillis(100),
	}))

	// Too early: gated out, and must not count.
	if matched(m.Select(frame(envelope.KindRequest, "ping"))) {
		t.Fatal("a frame before after_mono_ms matched")
	}

	sched.Advance(200 * time.Millisecond)

	if matched(m.Select(frame(envelope.KindRequest, "ping"))) {
		t.Error("the first eligible frame matched; the gated frame spent an ordinal")
	}
	if !matched(m.Select(frame(envelope.KindRequest, "ping"))) {
		t.Error("the second eligible frame did not match")
	}
}

// A response carries no method, so selecting on one can only work through the
// ledger.
func TestMatcherResolvesAResponseMethodThroughTheLedger(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("id/dup", interpose.Match{
		Method: interpose.ParseGlob("tools/call"),
		Kind:   envelope.KindResponse,
	}))

	id := envelope.NumberID(7)
	l.Originate(transcript.Downstream, conn, transcript.C2S, interpose.Exchange{IntentID: id, Method: "tools/call"})

	resp := frame(envelope.KindResponse, "")
	resp.ID = id
	if !matched(m.Select(resp)) {
		t.Error("a response did not match on the method its id resolves")
	}

	// An id the ledger never saw resolves to nothing, and a case naming a
	// method must not match it: charpy cannot claim a frame answers a
	// tools/call it has no record of.
	unknown := frame(envelope.KindResponse, "")
	unknown.ID = envelope.NumberID(999)
	if matched(m.Select(unknown)) {
		t.Error("a response to an unknown id matched a case selecting on a method")
	}
}

// In v0 every scope collapses to run, but the counters are keyed by dimension
// from the start, so this is what stops a session-scoped ordinal becoming a
// global one the day a fleet exists.
func TestScopeCountsWithinItsPopulation(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l, caseWith("id/dup", interpose.Match{
		Occurrence: 2,
		Scope:      interpose.ScopeSession,
	}))

	a := frame(envelope.KindRequest, "ping")
	b := frame(envelope.KindRequest, "ping")
	b.SessionID = "s-2"

	if matched(m.Select(a)) || matched(m.Select(b)) {
		t.Fatal("a first frame in each session matched a case wanting the second")
	}
	if !matched(m.Select(a)) {
		t.Error("the second frame of session s-1 did not match")
	}
	if !matched(m.Select(b)) {
		t.Error("the second frame of session s-2 did not match; sessions share a counter")
	}
}

// The matcher reports what the policy says; arbitrating between two faults
// that want the same frame is somebody else's job.
func TestSelectReturnsEveryMatchingCaseInOrder(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	m := interpose.NewMatcher(l,
		caseWith("stream/a", interpose.Match{Direction: transcript.C2S}),
		caseWith("id/b", interpose.Match{Method: interpose.ParseGlob("tools/*")}),
		caseWith("frame/c", interpose.Match{Face: transcript.Upstream}),
	)

	got := m.Select(frame(envelope.KindRequest, "tools/call"))
	if len(got) != 2 || got[0].ID != "stream/a" || got[1].ID != "id/b" {
		t.Errorf("Select returned %v, want stream/a then id/b in catalogue order", got)
	}
}
