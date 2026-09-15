package coverage_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/coverage"
	"github.com/serverplumber/charpy/internal/transcript"
)

func read(t *testing.T, lines ...string) *transcript.Transcript {
	t.Helper()
	got, err := transcript.Read(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return got
}

func header(cases string) string {
	return `{"schema_version":1,"type":"header","run_id":"01JBW3K9F2Q7XN4TCHARPY01","seq":0,` +
		`"t_mono_ns":0,"t_wall":"2026-09-14T00:00:00.000000000Z","charpy_version":"test",` +
		`"seed":"8f2c1a","mode":"stdio-ingress","subject":{"class":"server"},` +
		`"revision":{"negotiated":"2025-11-25"},"redaction":"on","clock":"real",` +
		`"fleet":{"clients":1},"cases":[` + cases + `]}`
}

func applied(seq int, caseID string) string {
	return `{"schema_version":1,"type":"event","run_id":"01JBW3K9F2Q7XN4TCHARPY01","seq":` +
		itoa(seq) + `,"t_mono_ns":1,"t_wall":"2026-09-14T00:00:00.000000001Z",` +
		`"event_kind":"fault_applied","face":"downstream","transport":"stdio",` +
		`"client_id":"c0","session_id":"s-0","conn_id":"c-0","stream_id":null,` +
		`"link":null,"detail":{"verb":"rewrite"},` +
		`"fault":{"case_id":"` + caseID + `","citation":"` + caseID + `@2025-11-25#seed=8f2c1a",` +
		`"kind":"truncate","params":{}}}`
}

func itoa(n int) string { return string(rune('0' + n)) }

// The distinction the whole package exists for: a case that never fired must
// not read as a clean run.
func TestAnArmedCaseThatNeverFiredIsUntriggered(t *testing.T) {
	tr := read(t, header(`"id/duplicate-response@2025-11-25#seed=8f2c1a"`))

	rep := coverage.Check(tr)
	if len(rep.Findings) != 1 {
		t.Fatalf("findings: %d, want 1", len(rep.Findings))
	}
	f := rep.Findings[0]
	if f.Verdict != oracle.Untriggered {
		t.Errorf("verdict = %s, want %s", f.Verdict, oracle.Untriggered)
	}
	if f.Verdict.Passed() {
		t.Error("an untriggered case reports as a pass")
	}
	if f.Citation != "id/duplicate-response@2025-11-25#seed=8f2c1a" {
		t.Errorf("citation = %q; a bug report needs the revision and seed", f.Citation)
	}
	if f.Reason == "" {
		t.Error("a non-verdict without a reason is indistinguishable from a pass")
	}
}

// A case whose fault landed is not reported. The header carries a citation and
// the fault line carries the bare id, so the comparison has to reduce one.
func TestACaseThatFiredIsNotReported(t *testing.T) {
	tr := read(t,
		header(`"id/duplicate-response@2025-11-25#seed=8f2c1a"`),
		applied(1, "id/duplicate-response"),
	)

	if got := coverage.Check(tr); len(got.Findings) != 0 {
		t.Errorf("findings: %v, want none", got.Findings)
	}
}

// Several armed, one fired: the other two are still untriggered, and each is
// named separately so a report says which.
func TestOnlyTheCasesThatDidNotFireAreReported(t *testing.T) {
	tr := read(t,
		header(`"a/one@2025-11-25#seed=8f2c1a","b/two@2025-11-25#seed=8f2c1a",`+
			`"c/three@2025-11-25#seed=8f2c1a"`),
		applied(1, "b/two"),
	)

	rep := coverage.Check(tr)
	if len(rep.Findings) != 2 {
		t.Fatalf("findings: %d, want 2", len(rep.Findings))
	}
	var named []string
	for _, f := range rep.Findings {
		named = append(named, f.Citation)
	}
	for _, want := range []string{"a/one@2025-11-25#seed=8f2c1a", "c/three@2025-11-25#seed=8f2c1a"} {
		found := false
		for _, n := range named {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not reported; got %v", want, named)
		}
	}
}

// A transcript from before charpy recorded what it armed yields nothing.
// Inventing coverage for it would be a claim about a run nobody can check.
func TestATranscriptWithNoArmedCasesYieldsNothing(t *testing.T) {
	tr := read(t, header(``))

	if got := coverage.Check(tr); len(got.Findings) != 0 {
		t.Errorf("findings: %v, want none", got.Findings)
	}
}
