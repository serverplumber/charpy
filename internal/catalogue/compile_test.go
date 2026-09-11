package catalogue

import (
	"os"
	"testing"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
)

const testSeed = "8f2c1a"

// Every case charpy ships must survive the trip into the interposer's terms,
// for every revision it claims to apply to.
func TestShippedCatalogueCompiles(t *testing.T) {
	cat, _, err := Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range revision.All() {
		applicable := cat.Applicable(r)
		if len(applicable) == 0 {
			continue
		}
		t.Run(r.String(), func(t *testing.T) {
			compiled, err := cat.CompileAll(r, testSeed)
			if err != nil {
				t.Fatalf("shipped catalogue does not compile: %v", err)
			}
			if len(compiled) != len(applicable) {
				t.Fatalf("compiled %d cases, want %d", len(compiled), len(applicable))
			}
			for i, cs := range applicable {
				assertMatchReproduced(t, cs, compiled[i])
			}
		})
	}
}

// The conversion is a hand-written switch, so the failure to guard against is
// a key that parses, validates, and is then silently dropped on the way into
// the matcher -- which would leave a case matching more frames than it says.
func assertMatchReproduced(t *testing.T, cs Case, got interpose.Case) {
	t.Helper()

	if got.ID != cs.ID {
		t.Errorf("compiled id = %q, want %q", got.ID, cs.ID)
	}
	if kind, _ := cs.Fault["kind"].(string); got.Fault.Kind != kind {
		t.Errorf("%s: fault kind = %q, want %q", cs.ID, got.Fault.Kind, kind)
	}
	if _, ok := got.Fault.Params["kind"]; ok {
		t.Errorf("%s: the mechanism selector leaked into the parameters", cs.ID)
	}

	for key, raw := range cs.Match {
		m := got.Match
		switch key {
		case "method":
			assertGlob(t, cs.ID, key, m.Method, raw)
		case "client_id":
			assertGlob(t, cs.ID, key, m.ClientID, raw)
		case "session_id":
			assertGlob(t, cs.ID, key, m.SessionID, raw)
		case "face":
			assertString(t, cs.ID, key, string(m.Face), raw)
		case "direction":
			assertString(t, cs.ID, key, string(m.Direction), raw)
		case "kind":
			assertString(t, cs.ID, key, string(m.Kind), raw)
		case "scope":
			assertString(t, cs.ID, key, string(m.Scope), raw)
		case "occurrence":
			assertInt(t, cs.ID, key, m.Occurrence, raw)
		case "occurrence_every":
			assertInt(t, cs.ID, key, m.Every, raw)
		case "after_mono_ms":
			assertInt(t, cs.ID, key, m.AfterMono.Millis(), raw)
		default:
			t.Errorf("%s: match key %q has no compile-time assertion; add one", cs.ID, key)
		}
	}
}

func assertGlob(t *testing.T, id, key string, g interpose.Glob, raw any) {
	t.Helper()
	if g.Any() {
		t.Errorf("%s: %s was declared but compiled to an unconstrained glob", id, key)
	}
	if want, _ := raw.(string); g.String() != want {
		t.Errorf("%s: %s compiled to %q, want %q", id, key, g.String(), want)
	}
}

func assertString(t *testing.T, id, key, got string, raw any) {
	t.Helper()
	if want, _ := raw.(string); got != want {
		t.Errorf("%s: %s compiled to %q, want %q", id, key, got, want)
	}
}

func assertInt(t *testing.T, id, key string, got int64, raw any) {
	t.Helper()
	if want, _ := raw.(int64); got != want {
		t.Errorf("%s: %s compiled to %d, want %d", id, key, got, want)
	}
}

func TestCompileBuildsTheCitation(t *testing.T) {
	cat, _, err := Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatal(err)
	}
	cs := cat.Cases[0]

	got, err := cs.Compile(revision.V20251125, testSeed)
	if err != nil {
		t.Fatal(err)
	}

	want := cs.ID + "@2025-11-25#seed=" + testSeed
	if got.Citation != want {
		t.Errorf("citation = %q, want %q", got.Citation, want)
	}
	// A citation the report quotes must parse back, or the identity chain is
	// broken at the point it is actually used.
	if _, err := ParseCitation(got.Citation); err != nil {
		t.Errorf("compiled citation does not parse: %v", err)
	}
}

func TestCompileMatchDefaultsAndConversions(t *testing.T) {
	cs := Case{
		ID: "stream/x",
		Match: map[string]any{
			"after_mono_ms": int64(1500),
			"occurrence":    int64(3),
		},
		Fault: map[string]any{"kind": "truncate", "cut_at": "mid_event"},
	}
	cs.parsedID, _ = ParseID(cs.ID)

	got, err := cs.Compile(revision.V20251125, testSeed)
	if err != nil {
		t.Fatal(err)
	}

	if got.Match.Scope != interpose.ScopeRun {
		t.Errorf("scope = %q, want run by default", got.Match.Scope)
	}
	if got.Match.AfterMono != clock.FromMillis(1500) {
		t.Errorf("after_mono_ms compiled to %v, want 1500ms on the injected clock", got.Match.AfterMono)
	}
	if got.Match.Occurrence != 3 {
		t.Errorf("occurrence = %d, want 3", got.Match.Occurrence)
	}
	if got.Fault.Params["cut_at"] != "mid_event" {
		t.Errorf("fault params = %v, want cut_at preserved", got.Fault.Params)
	}
}

// Load validates against the match-key registry, so these are unreachable
// through it. Compiling without Load must still refuse rather than silently
// ignore, because a matcher that quietly matches nothing is a test that
// quietly passes.
func TestCompileRefusesWhatLoadWouldHaveCaught(t *testing.T) {
	tests := []struct {
		name  string
		match map[string]any
	}{
		{"unknown key", map[string]any{"methodd": "tools/call"}},
		{"string where an integer belongs", map[string]any{"occurrence": "2"}},
		{"integer where a string belongs", map[string]any{"method": int64(7)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := Case{ID: "stream/x", Match: tc.match, Fault: map[string]any{"kind": "truncate"}}
			cs.parsedID, _ = ParseID(cs.ID)
			if _, err := cs.Compile(revision.V20251125, testSeed); err == nil {
				t.Error("Compile accepted a match table Load would have rejected")
			}
		})
	}
}

// A case that cannot run is not policy. Carrying it into the interposer would
// only give the matcher something to ignore on every frame.
func TestCompileAllDropsWhatCannotRun(t *testing.T) {
	cat, _, err := Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range revision.All() {
		compiled, err := cat.CompileAll(r, testSeed)
		if err != nil {
			t.Fatal(err)
		}
		ids := make(map[string]bool, len(compiled))
		for _, c := range compiled {
			ids[c.ID] = true
		}
		for _, cs := range cat.Cases {
			runnable := !cs.Withdrawn() && cs.AppliesToRange().Includes(r)
			if ids[cs.ID] != runnable {
				t.Errorf("%s on %s: compiled = %v, runnable = %v", cs.ID, r, ids[cs.ID], runnable)
			}
		}
	}
}
