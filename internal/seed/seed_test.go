package seed_test

import (
	"testing"

	"github.com/serverplumber/charpy/internal/seed"
)

func draw(t *testing.T, run, caseID, purpose string, n int) []int {
	t.Helper()
	r := seed.For(run, caseID, purpose)
	out := make([]int, n)
	for i := range out {
		out[i] = r.IntN(1000)
	}
	return out
}

func same(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The promise: case-id plus seed reproduces byte for byte.
func TestSameInputsGiveTheSameStream(t *testing.T) {
	a := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.CutOffset, 8)
	b := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.CutOffset, 8)
	if !same(a, b) {
		t.Errorf("the same inputs drew %v then %v", a, b)
	}
}

// The reason it is keyed by case: otherwise a case's bytes depend on which
// cases ran before it, and running one alone reproduces nothing.
func TestStreamsAreIndependentPerCase(t *testing.T) {
	a := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.CutOffset, 8)
	b := draw(t, "8f2c1a", "id/duplicate-response", seed.CutOffset, 8)
	if same(a, b) {
		t.Error("two cases share a stream")
	}
}

// The reason it is keyed by purpose: otherwise adding a seeded parameter to a
// mechanism shifts every draw after it, and archived citations stop
// reproducing.
func TestStreamsAreIndependentPerPurpose(t *testing.T) {
	a := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.CutOffset, 8)
	b := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.Payload, 8)
	if same(a, b) {
		t.Error("two purposes share a stream")
	}

	// Draining one purpose must not move another: that independence is the
	// entire reason purposes exist.
	before := draw(t, "8f2c1a", "stream/x", seed.Payload, 4)
	drained := seed.For("8f2c1a", "stream/x", seed.CutOffset)
	for range 100 {
		drained.IntN(1000)
	}
	if after := draw(t, "8f2c1a", "stream/x", seed.Payload, 4); !same(before, after) {
		t.Errorf("draining one purpose moved another: %v then %v", before, after)
	}
}

func TestDifferentRunSeedsGiveDifferentStreams(t *testing.T) {
	a := draw(t, "8f2c1a", "stream/x", seed.CutOffset, 8)
	b := draw(t, "000000", "stream/x", seed.CutOffset, 8)
	if same(a, b) {
		t.Error("the run seed does not reach the stream")
	}
}

// Concatenation must not collide: ("ab","c") and ("a","bc") are different
// cases and must not share randomness.
func TestFieldsAreSeparated(t *testing.T) {
	a := draw(t, "8f2c1a", "ab", "c", 4)
	b := draw(t, "8f2c1a", "a", "bc", 4)
	if same(a, b) {
		t.Error("adjacent fields concatenate into the same stream")
	}
}

// The whole point is that this is stable forever, so the values are pinned.
// If this test fails, every archived citation has stopped reproducing -- which
// is a decision to take deliberately, not a golden file to update.
func TestStreamIsPinned(t *testing.T) {
	got := draw(t, "8f2c1a", "stream/truncate-mid-event", seed.CutOffset, 4)
	want := []int{seedPin0, seedPin1, seedPin2, seedPin3}
	if !same(got, want) {
		t.Errorf("the derivation changed: drew %v, want %v.\n"+
			"Every archived transcript's citation reproduces only while this holds.", got, want)
	}
}

// Pinned on 2026-09-11 from the derivation in seed.go. These are not a golden
// file to regenerate when they fail.
const (
	seedPin0 = 747
	seedPin1 = 114
	seedPin2 = 893
	seedPin3 = 828
)

func TestPickStaysInRange(t *testing.T) {
	p := seed.Pick(seed.For("8f2c1a", "stream/x", seed.CutOffset))
	for _, n := range []int{1, 2, 10, 1000} {
		for range 50 {
			if got := p(n); got < 0 || got >= n {
				t.Fatalf("Pick(%d) = %d, out of range", n, got)
			}
		}
	}
	if got := p(0); got != 0 {
		t.Errorf("Pick(0) = %d, want 0", got)
	}
}
