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
		{
			name: "an observer that does not exist",
			want: "unknown observer",
			body: coherence("subject = [\"server\"]\nobserved_by = [\"telepathy\"]", `direction = "c2s"`, `kind = "malformed_json"`),
		},
		// ADR-013: a fault is put to whoever receives the frame it damages.
		{
			name: "no direction, so half the frames it damages reach charpy",
			want: "needs a direction",
			body: coherence(`subject = ["server"]`, `method = "tools/call"`, `kind = "malformed_json"`),
		},
		{
			name: "an s2c fault on a server subject reaches charpy's peer",
			want: "does not reach a server subject",
			body: coherence(`subject = ["server"]`, `direction = "s2c"`, `kind = "malformed_json"`),
		},
		{
			name: "a c2s fault on a client subject reaches charpy's server",
			want: "does not reach a client subject",
			body: coherence(`subject = ["client"]`, `direction = "c2s"`, `kind = "malformed_json"`),
		},
		{
			name: "a gateway fault must say which face",
			want: "does not reach a gateway subject",
			body: coherence(`subject = ["gateway"]`, `direction = "s2c"`, `kind = "malformed_json"`),
		},
		{
			name: "a gateway's own output is not put to the gateway",
			want: "does not reach a gateway subject",
			body: coherence(`subject = ["gateway"]`, "direction = \"s2c\"\nface = \"downstream\"", `kind = "malformed_json"`),
		},
		{
			name: "one direction cannot reach both a server and a client",
			want: "does not reach a client subject",
			body: coherence(`subject = ["server", "client"]`, `direction = "c2s"`, `kind = "malformed_json"`),
		},
		{
			name: "a kind-restricted mechanism must be told the kind",
			want: "must name one with kind",
			body: coherence(`subject = ["client"]`, `direction = "s2c"`, `kind = "capability_flip"`),
		},
		{
			name: "and the kind it is told must be one it acts on",
			want: "acts only on response frames",
			body: coherence(`subject = ["client"]`, "direction = \"s2c\"\nkind = \"request\"", `kind = "capability_flip"`),
		},
		{
			name: "a mode decides what duplicate_id acts on",
			want: "acts only on request frames",
			body: coherence(`subject = ["server"]`, "direction = \"c2s\"\nkind = \"response\"",
				"kind = \"duplicate_id\"\nmode = \"concurrent_request\""),
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

// A validate run should report every broken file at once, so decode errors
// accumulate per file rather than aborting on the first.
func TestLoadAccumulatesAcrossFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"cases/a.toml": &fstest.MapFile{Data: []byte("schema_version = 1\nbogus_key = true\n")},
		"cases/b.toml": &fstest.MapFile{Data: []byte(`schema_version = 1
[[case]]
id = "id/thing"
applies_to = "*"
subject = ["server"]
verdict = "OBSERVED"
derives_from = "bogus"
summary = "s"
[case.fault]
kind = "hang"
`)},
	}
	_, _, err := Load(fsys, "cases")
	if err == nil {
		t.Fatal("loaded successfully; want errors from both files")
	}
	for _, want := range []string{"cases/a.toml", "cases/b.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
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
direction = "c2s"
[case.fault]
kind = "malformed_json"
`
	fsys := fstest.MapFS{
		"cases/a.toml": &fstest.MapFile{Data: []byte("schema_version = 1\n" + c)},
		"cases/b.toml": &fstest.MapFile{Data: []byte("schema_version = 1\n" + c)},
	}
	_, _, err := Load(fsys, "cases")
	if err == nil || !strings.Contains(err.Error(), "duplicate case id") {
		t.Fatalf("want duplicate id error, got %v", err)
	}
	// "(also in %s)" must name the file the first definition lives in.
	if !strings.Contains(err.Error(), "also in cases/a.toml") {
		t.Errorf("duplicate id error does not name the first file:\n%v", err)
	}
}

// An empty [case.match] used to earn a warning: it matches the first frame on
// any face, which is almost never what an author means. It is refused now,
// because a case must state the direction that puts its fault to the subject.
func TestEmptyMatchIsRefused(t *testing.T) {
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
kind = "malformed_json"
`)},
	}
	_, _, err := Load(fsys, "cases")
	if err == nil || !strings.Contains(err.Error(), "needs a direction") {
		t.Errorf("an empty [case.match] loaded: %v", err)
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
direction = "c2s"
[case.fault]
kind = "malformed_json"
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

// coherence builds a case differing only in who it names, what it matches and
// what fault it carries: the three things the recipient rule relates.
func coherence(subject, match, fault string) string {
	return "[[case]]\nid = \"frame/thing\"\napplies_to = \"*\"\n" + subject +
		"\nverdict = \"OBSERVED\"\nderives_from = \"none\"\nsummary = \"s\"\n" +
		"[case.match]\n" + match + "\n[case.fault]\n" + fault
}

// The rules accept what they are for: a fault each named subject receives, on a
// frame kind the mechanism acts on.
func TestCoherentCasesLoad(t *testing.T) {
	for name, body := range map[string]string{
		"c2s to a server":            coherence(`subject = ["server"]`, `direction = "c2s"`, `kind = "malformed_json"`),
		"s2c to a client":            coherence(`subject = ["client"]`, "direction = \"s2c\"\nface = \"upstream\"", `kind = "malformed_json"`),
		"s2c upstream to a gateway":  coherence(`subject = ["client", "gateway"]`, "direction = \"s2c\"\nface = \"upstream\"", `kind = "malformed_json"`),
		"c2s downstream to both":     coherence(`subject = ["server", "gateway"]`, "direction = \"c2s\"\nface = \"downstream\"", `kind = "malformed_json"`),
		"a kind the mechanism takes": coherence(`subject = ["client"]`, "direction = \"s2c\"\nkind = \"response\"", `kind = "capability_flip"`),
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{"cases/x.toml": &fstest.MapFile{Data: []byte("schema_version = 1\n\n" + body + "\n")}}
			if _, _, err := Load(fsys, "cases"); err != nil {
				t.Errorf("a coherent case was refused:\n%v", err)
			}
		})
	}
}

// A case names what can see its answer, and the wire is what it means when it
// names nothing. Only what a run has can judge it.
func TestObservableBy(t *testing.T) {
	plain := Case{}
	if !plain.ObservableBy("wire") {
		t.Error("a case naming no observer should be observable on the wire")
	}
	differential := Case{ObservedBy: []string{"differential"}}
	if differential.ObservableBy("wire") {
		t.Error("a case whose answer only the differential sees was observable on the wire")
	}
	if !differential.ObservableBy("wire", "differential") {
		t.Error("a case was not observable by the observer it names")
	}
}
