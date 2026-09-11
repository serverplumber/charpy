package interpose_test

import (
	"testing"

	"github.com/serverplumber/charpy/internal/interpose"
)

func TestGlob(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
		why     string
	}{
		// The case path.Match gets wrong: its * will not cross a /, so this
		// would be a silent no-match and a test that silently passes.
		{"*", "tools/call", true, "a bare star matches anything, separators included"},
		{"*", "", true, ""},
		{"tools/*", "tools/call", true, ""},
		{"tools/*", "tools/", true, "a trailing star may match nothing"},
		{"tools/*", "prompts/get", false, ""},
		{"*/call", "tools/call", true, ""},
		{"*/call", "tools/list", false, ""},
		{"tools/*a*", "tools/call", true, "interior wildcards match in order"},
		{"*a*z*", "abcz", true, ""},
		{"*z*a*", "abcz", false, "interior parts must occur in order"},

		// Exact matches, the common case in the shipped catalogue.
		{"tools/call", "tools/call", true, ""},
		{"tools/call", "tools/calls", false, ""},
		{"tools/call", "atools/call", false, ""},

		// Anchors must not overlap.
		{"ab*ba", "aba", false, "prefix and suffix may not share characters"},
		{"ab*ba", "abba", true, ""},
		{"ab*ba", "abXba", true, ""},

		// Empty pattern is the zero Glob: an absent key constrains nothing.
		{"", "anything", true, "an absent [case.match] key matches everything"},
		{"", "", true, ""},
	}

	for _, tc := range tests {
		name := tc.pattern + " vs " + tc.input
		t.Run(name, func(t *testing.T) {
			if got := interpose.ParseGlob(tc.pattern).Match(tc.input); got != tc.want {
				t.Errorf("ParseGlob(%q).Match(%q) = %v, want %v. %s",
					tc.pattern, tc.input, got, tc.want, tc.why)
			}
		})
	}
}

func TestGlobAny(t *testing.T) {
	if !interpose.ParseGlob("").Any() {
		t.Error("the zero glob must report that it constrains nothing")
	}
	for _, p := range []string{"*", "tools/*", "tools/call"} {
		if interpose.ParseGlob(p).Any() {
			t.Errorf("ParseGlob(%q).Any() = true; only an absent key is unconstrained", p)
		}
	}
}

func TestGlobStringRoundTrips(t *testing.T) {
	// The compiled policy is what the report cites, so a glob has to be able
	// to say what the manifest said.
	for _, p := range []string{"", "*", "tools/*", "tools/call"} {
		if got := interpose.ParseGlob(p).String(); got != p {
			t.Errorf("ParseGlob(%q).String() = %q", p, got)
		}
	}
}
