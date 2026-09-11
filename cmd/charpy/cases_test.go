package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/catalogue"
)

func listCases(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := cmdCases(args, &out)
	return out.String(), code
}

func policy(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := cmdPolicy(args, &out)
	return out.String(), code
}

// The reason `charpy cases` prints TOML rather than a table: the output is a
// valid manifest, so "show me what would run" pastes back in as "run exactly
// this". A rendered report would not have that property, and nothing but a
// round trip proves it.
func TestCasesOutputIsAValidManifest(t *testing.T) {
	printed, code := listCases(t)
	if code != exitClean {
		t.Fatalf("charpy cases exited %d", code)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.toml"), []byte(printed), 0o600); err != nil {
		t.Fatal(err)
	}

	cat, warnings, err := catalogue.Load(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("the printed catalogue does not load:\n%v\n\n%s", err, printed)
	}
	for _, w := range warnings {
		t.Errorf("round trip produced a warning: %s", w)
	}

	original, _, err := catalogue.Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Cases) != len(original.Cases) {
		t.Errorf("round trip carried %d cases, want %d", len(cat.Cases), len(original.Cases))
	}
	for _, want := range original.Cases {
		got, ok := cat.Lookup(want.ID)
		if !ok {
			t.Errorf("case %q did not survive the round trip", want.ID)
			continue
		}
		if got.AppliesTo != want.AppliesTo || got.Verdict != want.Verdict || got.Summary != want.Summary {
			t.Errorf("case %q changed: %+v", want.ID, got)
		}
		if len(got.Match) != len(want.Match) || len(got.Fault) != len(want.Fault) {
			t.Errorf("case %q lost table keys: match %v fault %v", want.ID, got.Match, got.Fault)
		}
	}
}

// The schema directive has to survive, or the editor tooling the format leans
// on stops working on the output.
func TestCasesOutputCarriesTheSchemaDirective(t *testing.T) {
	printed, _ := listCases(t)
	if !strings.HasPrefix(printed, "#:schema ") {
		t.Errorf("output does not open with a #:schema directive:\n%s", firstLine(printed))
	}
}

func TestCasesFiltersByRevision(t *testing.T) {
	all, _ := listCases(t)
	old, code := listCases(t, "--revision", "2024-11-05")
	if code != exitClean {
		t.Fatalf("exited %d", code)
	}
	if strings.Count(old, "[[case]]") >= strings.Count(all, "[[case]]") {
		t.Error("--revision did not narrow the catalogue")
	}

	if _, code := listCases(t, "--revision", "1999-01-01"); code == exitClean {
		t.Error("an unknown revision was accepted")
	}
}

func TestCasesJSON(t *testing.T) {
	printed, code := listCases(t, "--json")
	if code != exitClean {
		t.Fatalf("exited %d", code)
	}

	var rows []caseJSON
	if err := json.Unmarshal([]byte(printed), &rows); err != nil {
		t.Fatalf("--json is not valid JSON: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no cases")
	}
	for _, r := range rows {
		if r.ID == "" || r.Family == "" || r.Verdict == "" || r.Fault == "" {
			t.Errorf("row is missing identity or provenance: %+v", r)
		}
		if len(r.Revisions) == 0 {
			t.Errorf("case %q resolves to no revisions", r.ID)
		}
	}
}

func TestPolicyValidateAcceptsTheShippedCatalogue(t *testing.T) {
	out, code := policy(t, "validate", "../../cases")
	if code != exitClean {
		t.Errorf("the shipped catalogue does not validate: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "0 errors") {
		t.Errorf("missing the summary line:\n%s", out)
	}
}

// Exit 4, not 0: the code is what a caller acts on, and a caller that gets 0
// for a broken policy proceeds with a broken policy.
func TestPolicyValidateReportsEveryLayer(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     []string
	}{
		{
			name: "decode errors carry file, line, column and key",
			manifest: `schema_version = 1
[[case]]
id = "stream/typo"
sumary = "a typo in a key"
`,
			want: []string{"broken.toml:4:1", `unknown key "case.sumary"`},
		},
		{
			name: "registry errors name the accepted alternatives",
			manifest: `schema_version = 1
[[case]]
id = "stream/bad-value"
applies_to = ">=2025-03-26"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "x"
  [case.fault]
  kind = "truncate"
  cut_at = "sideways"
`,
			want: []string{`case "stream/bad-value"`, "cut_at", "accepts:", "event_boundary"},
		},
		{
			name: "semantic errors explain the rule",
			manifest: `schema_version = 1
[[case]]
id = "stream/bad-must"
applies_to = ">=2025-03-26"
subject = ["server"]
verdict = "MUST"
derives_from = "spec:2025-11-25/basic/transports"
summary = "x"
  [case.fault]
  kind = "truncate"
`,
			want: []string{"MUST", "schema:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "broken.toml"), []byte(tc.manifest), 0o600); err != nil {
				t.Fatal(err)
			}

			out, code := policy(t, "validate", dir)
			if code != exitBadPolicy {
				t.Errorf("exit %d, want %d (invalid policy)\n%s", code, exitBadPolicy, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("diagnostics do not mention %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, "error") {
				t.Errorf("missing the summary line:\n%s", out)
			}
		})
	}
}

// A validate run reports every broken file, not the first one.
func TestPolicyValidateReportsEveryFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.toml", "b.toml"} {
		body := "schema_version = 1\n[[case]]\nid = \"stream/x\"\nsumary = \"typo\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, code := policy(t, "validate", dir)
	if code != exitBadPolicy {
		t.Fatalf("exit %d, want %d", code, exitBadPolicy)
	}
	for _, name := range []string{"a.toml", "b.toml"} {
		if !strings.Contains(out, name) {
			t.Errorf("%s is missing from the diagnostics:\n%s", name, out)
		}
	}
}

// Diagnostics have to name the path the user typed, or they send the reader
// looking in the wrong directory.
func TestPolicyValidatePathsReadAsTyped(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(file, []byte("schema_version = 1\n[[case]]\nsumary = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// An absolute single file: the leading separator must survive, or the
	// path reads as a relative one and is not.
	out, _ := policy(t, "validate", file)
	if !strings.Contains(out, file) {
		t.Errorf("diagnostics do not carry the absolute path %q:\n%s", file, out)
	}
}

func TestPolicyValidateNeedsATarget(t *testing.T) {
	if _, code := policy(t, "validate"); code != exitHarness {
		t.Errorf("exit %d with no target, want %d", code, exitHarness)
	}
	if _, code := policy(t, "lint", "cases"); code != exitHarness {
		t.Errorf("exit %d for an unknown subcommand, want %d", code, exitHarness)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
