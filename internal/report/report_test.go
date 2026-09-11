package report_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
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
				Seq: 3, Summary: "outstanding when the subject exited", Reason: "subject-exited-first"},
			{Verdict: oracle.Untriggered, Layer: "invariant", Check: "stream/truncate-mid-event",
				Seq: -1, Summary: "no frame matched", Citation: "stream/truncate-mid-event@2025-11-25#seed=8f2c1a"},
		},
	}
}

// The property oracle.md insists on hardest: a suite that reports skips as
// passes acquires false confidence, which is how test suites become worthless.
// All three non-verdicts must be visibly not-passes, and must stay
// distinguishable from each other -- they mean different things and a reader
// acts differently on each.
func TestNonVerdictsAreNeverPasses(t *testing.T) {
	var buf bytes.Buffer
	if err := report.JUnit(&buf, sample()); err != nil {
		t.Fatal(err)
	}

	var suite struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Skipped  int `xml:"skipped,attr"`
		Cases    []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Type string `xml:"type,attr"`
			} `xml:"failure"`
			Skipped *struct {
				Message string `xml:"message,attr"`
			} `xml:"skipped"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &suite); err != nil {
		t.Fatalf("not valid JUnit XML: %v\n%s", err, buf.String())
	}

	if suite.Tests != 5 || suite.Failures != 2 || suite.Skipped != 3 {
		t.Errorf("tests=%d failures=%d skipped=%d, want 5/2/3", suite.Tests, suite.Failures, suite.Skipped)
	}

	// Not one of the five may come out as a bare pass.
	for _, c := range suite.Cases {
		if c.Failure == nil && c.Skipped == nil {
			t.Errorf("%q rendered as a pass", c.Name)
		}
	}

	// The three skips must still say which they were: SKIPPED means the check
	// did not apply, INCONCLUSIVE that it could not be settled, UNTRIGGERED
	// that the fault never fired. CI shows all three the same way, so the
	// message is the only place the difference survives.
	want := map[string]bool{"SKIPPED": false, "INCONCLUSIVE": false, "UNTRIGGERED": false}
	for _, c := range suite.Cases {
		if c.Skipped == nil {
			continue
		}
		for verdict := range want {
			if strings.HasPrefix(c.Skipped.Message, verdict+":") {
				want[verdict] = true
			}
		}
	}
	for verdict, found := range want {
		if !found {
			t.Errorf("%s is indistinguishable from the other non-verdicts in JUnit", verdict)
		}
	}
}

// A non-verdict without a reason reads as a pass to whoever gets the report.
func TestNonVerdictsCarryTheirReason(t *testing.T) {
	var buf bytes.Buffer
	if err := report.JUnit(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"not-applicable-to-revision", "subject-exited-first"} {
		if !strings.Contains(buf.String(), reason) {
			t.Errorf("the report does not carry the reason %q", reason)
		}
	}
}

// CI keys history off classname and name, which is why a citation's seed never
// appears in either: a per-run seed there makes every run look like a brand
// new test and destroys flake history.
func TestJUnitNamesCarryNoSeed(t *testing.T) {
	var buf bytes.Buffer
	if err := report.JUnit(&buf, sample()); err != nil {
		t.Fatal(err)
	}

	var suite struct {
		Cases []struct {
			ClassName string `xml:"classname,attr"`
			Name      string `xml:"name,attr"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &suite); err != nil {
		t.Fatal(err)
	}
	for _, c := range suite.Cases {
		if strings.Contains(c.Name, "seed=") || strings.Contains(c.ClassName, "seed=") {
			t.Errorf("a seed reached CI's history key: %s / %s", c.ClassName, c.Name)
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
