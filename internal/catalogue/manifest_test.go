package catalogue

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/serverplumber/charpy/internal/revision"
)

// The shipped catalogue must load clean, with unique well-formed ids, ranges
// that resolve, and no MUST that lacks a schema source.
func TestShippedCatalogueLoads(t *testing.T) {
	cat, warnings, err := Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatalf("shipped catalogue does not load:\n%v", err)
	}
	for _, w := range warnings {
		t.Logf("warning: %s", w)
	}
	if len(cat.Cases) == 0 {
		t.Fatal("catalogue is empty")
	}

	for _, cs := range cat.Cases {
		t.Run(cs.ID, func(t *testing.T) {
			if cs.ParsedID().String() != cs.ID {
				t.Errorf("id does not round trip: %q vs %q", cs.ParsedID(), cs.ID)
			}
			if cs.AppliesToRange().Empty() {
				t.Errorf("applies_to %q matches no known revision", cs.AppliesTo)
			}
			if cs.Verdict == VerdictMust && !strings.HasPrefix(cs.DerivesFrom, "schema:") {
				t.Errorf("MUST verdict without a schema: source")
			}
		})
	}
}

// v0 is sessioned-first, so the catalogue has to actually cover the revisions
// v0 targets.
func TestShippedCatalogueCoversV0Targets(t *testing.T) {
	cat, _, err := Load(os.DirFS("../.."), "cases")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []revision.Revision{revision.V20250618, revision.V20251125} {
		t.Run(r.String(), func(t *testing.T) {
			if n := len(cat.Applicable(r)); n == 0 {
				t.Errorf("no cases apply to %s, which v0 targets", r)
			}
		})
	}
}

func TestLoadRejects(t *testing.T) {
	const preamble = "schema_version = 1\n\n"

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "behavioural MUST",
			want: "requires a schema: source",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "MUST"
derives_from = "spec:2025-11-25/basic"
summary = "s"
[case.fault]
kind = "hang"`,
		},
		{
			name: "unknown family",
			want: "unknown family",
			body: `[[case]]
id = "bogus/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.fault]
kind = "hang"`,
		},
		{
			name: "unknown revision in range",
			want: "unknown revision",
			body: `[[case]]
id = "id/thing"
applies_to = ">=2027-01-01"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.fault]
kind = "hang"`,
		},
		{
			name: "typo in a key is not silently ignored",
			want: "case.fault",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.fault]
kind = "truncate"
cut_after = 91`,
		},
		{
			name: "missing fault",
			want: "[case.fault] is required",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"`,
		},
		{
			name: "malformed derives_from",
			want: "derives_from",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "https://example.com"
summary = "s"
[case.fault]
kind = "hang"`,
		},
		{
			name: "withdrawn without a reason",
			want: "withdrawn_reason",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
status = "withdrawn"
[case.fault]
kind = "hang"`,
		},
		{
			name: "unknown match scope",
			want: "scope",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.match]
method = "tools/call"
scope = "galaxy"
[case.fault]
kind = "hang"`,
		},
		{
			name: "occurrence and occurrence_every together",
			want: "mutually exclusive",
			body: `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.match]
occurrence = 2
occurrence_every = 3
[case.fault]
kind = "hang"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"cases/x.toml": &fstest.MapFile{Data: []byte(preamble + tc.body + "\n")},
			}
			_, _, err := Load(fsys, "cases")
			if err == nil {
				t.Fatal("loaded successfully; want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestDuplicateIDsAreRejected(t *testing.T) {
	const c = `[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.match]
method = "tools/call"
[case.fault]
kind = "hang"
`
	fsys := fstest.MapFS{
		"cases/a.toml": &fstest.MapFile{Data: []byte("schema_version = 1\n" + c)},
		"cases/b.toml": &fstest.MapFile{Data: []byte("schema_version = 1\n" + c)},
	}
	_, _, err := Load(fsys, "cases")
	if err == nil || !strings.Contains(err.Error(), "duplicate case id") {
		t.Fatalf("want duplicate id error, got %v", err)
	}
}

func TestEmptyMatchWarns(t *testing.T) {
	fsys := fstest.MapFS{
		"cases/x.toml": &fstest.MapFile{Data: []byte(`schema_version = 1
[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
[case.fault]
kind = "hang"
`)},
	}
	_, warnings, err := Load(fsys, "cases")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Error("an empty [case.match] should warn; it matches the first frame on any face")
	}
}

func TestWithdrawnCasesStillParseButDoNotRun(t *testing.T) {
	fsys := fstest.MapFS{
		"cases/x.toml": &fstest.MapFile{Data: []byte(`schema_version = 1
[[case]]
id = "id/retired"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "none"
summary = "s"
status = "withdrawn"
withdrawn_reason = "superseded by id/thing"
[case.match]
method = "tools/call"
[case.fault]
kind = "hang"
`)},
	}
	cat, _, err := Load(fsys, "cases")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Lookup("id/retired"); !ok {
		t.Error("withdrawn cases must still resolve, so old reports keep working")
	}
	if n := len(cat.Applicable(revision.V20251125)); n != 0 {
		t.Errorf("withdrawn case should not be applicable, got %d", n)
	}
}
