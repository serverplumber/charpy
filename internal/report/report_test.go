package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/report"
)

func sample() oracle.Report {
	return oracle.Report{
		RunID: "01TEST",
		Findings: []oracle.Finding{
			{Verdict: oracle.Must, Layer: "schema", Check: "schema:2025-11-25#/$defs/Result/type",
				Seq: 6, Summary: "rejected by the schema", Detail: "got string, want object"},
			{Verdict: oracle.Observed, Layer: "invariant", Check: "id-resolves-once",
				Seq: 7, Summary: "answered twice"},
			{Verdict: oracle.Skipped, Layer: "invariant", Check: "session-identity-isolation",
				Seq: -1, Summary: "no protocol session on this revision", Reason: "not-applicable-to-revision"},
			{Verdict: oracle.Inconclusive, Layer: "invariant", Check: "id-resolves-once",
				Seq: 3, Summary: "answered, and charpy replaced the answer", Reason: "charpy-replaced-the-answer"},
			{Verdict: oracle.Untriggered, Layer: "invariant", Check: "stream/truncate-mid-event",
				Seq: -1, Summary: "no frame matched", Citation: "stream/truncate-mid-event@2025-11-25#seed=8f2c1a"},
		},
	}
}

// The property oracle.md insists on hardest: a suite that reports skips as
// passes acquires false confidence, which is how test suites become worthless.
// Every finding keeps its own verdict -- the three non-verdicts mean different
// things and a reader acts differently on each -- and a non-verdict keeps its
// reason, without which it reads as a pass to whoever gets the report.
func TestEveryVerdictSurvivesDistinctly(t *testing.T) {
	var buf bytes.Buffer
	if err := report.JSONL(&buf, sample()); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var f struct{ Verdict, Reason string }
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		got[f.Verdict] = true
		if f.Reason != "" {
			got["reason:"+f.Reason] = true
		}
	}
	for _, want := range []string{"MUST", "OBSERVED", "SKIPPED", "INCONCLUSIVE", "UNTRIGGERED",
		"reason:not-applicable-to-revision", "reason:charpy-replaced-the-answer"} {
		if !got[want] {
			t.Errorf("the report lost %s", want)
		}
	}
}

// The JSONL report is a sibling of the transcript rather than a summary of it:
// same one-object-per-line rule, queryable with the same tools.
func TestJSONLIsOneFindingPerLine(t *testing.T) {
	var buf bytes.Buffer
	if err := report.JSONL(&buf, sample()); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines for 5 findings", len(lines))
	}
	for i, line := range lines {
		var f map[string]any
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if f["schema_version"] != float64(1) || f["run_id"] != "01TEST" {
			t.Errorf("line %d is not self-describing: %v", i+1, f)
		}
	}
}
