package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/serverplumber/charpy/internal/oracle"
)

// Rendering a finished run. The formats are the ones CI already consumes,
// because a suite nobody can wire into a pipeline is a suite nobody runs.
//
// SKIPPED, INCONCLUSIVE and UNTRIGGERED render distinctly from passes
// everywhere. A suite that reports skips as passes acquires false confidence,
// which is how test suites become worthless over time.

// JSONL writes one finding per line.
//
// This is the machine-readable form, and it is a sibling of the transcript
// rather than a summary of it: same shape of file, same one-object-per-line
// rule, queryable with the same tools and no charpy code involved.
func JSONL(w io.Writer, rep oracle.Report) error {
	enc := json.NewEncoder(w)
	for _, f := range rep.Findings {
		if err := enc.Encode(finding{
			SchemaVersion: 1,
			RunID:         rep.RunID,
			Verdict:       string(f.Verdict),
			Layer:         f.Layer,
			Check:         f.Check,
			Citation:      f.Citation,
			Seq:           f.Seq,
			Summary:       f.Summary,
			Detail:        f.Detail,
			Reason:        f.Reason,
		}); err != nil {
			return fmt.Errorf("report: %w", err)
		}
	}
	return nil
}

type finding struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	Verdict       string `json:"verdict"`
	Layer         string `json:"layer"`
	Check         string `json:"check"`
	Citation      string `json:"citation,omitempty"`
	Seq           int64  `json:"seq"`
	Summary       string `json:"summary"`
	Detail        string `json:"detail,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// JUnit XML. The shapes below are the subset every CI system agrees on;
// anything richer is read by one tool and ignored by the rest.

type testsuite struct {
	XMLName  xml.Name   `xml:"testsuite"`
	Name     string     `xml:"name,attr"`
	Tests    int        `xml:"tests,attr"`
	Failures int        `xml:"failures,attr"`
	Skipped  int        `xml:"skipped,attr"`
	Cases    []testcase `xml:"testcase"`
}

type testcase struct {
	ClassName string   `xml:"classname,attr"`
	Name      string   `xml:"name,attr"`
	Failure   *failure `xml:"failure,omitempty"`
	Skipped   *skipped `xml:"skipped,omitempty"`
}

type failure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type skipped struct {
	Message string `xml:"message,attr"`
}

// JUnit writes the report as JUnit XML, one testcase per finding.
//
// The three non-verdicts map to <skipped/> and never to a pass, each carrying
// a message that says which it was. CI shows them as skips either way, so the
// message is the only place the difference survives -- and the difference
// matters: SKIPPED means the check did not apply, INCONCLUSIVE means it
// applied and the transcript could not settle it, UNTRIGGERED means the fault
// never fired at all.
func JUnit(w io.Writer, rep oracle.Report) error {
	suite := testsuite{Name: "charpy"}

	for _, f := range rep.Findings {
		tc := testcase{ClassName: className(f), Name: caseName(f)}

		switch f.Verdict {
		case oracle.Must, oracle.Observed:
			suite.Failures++
			tc.Failure = &failure{
				Message: f.Summary,
				Type:    string(f.Verdict),
				Body:    body(f),
			}
		default:
			suite.Skipped++
			tc.Skipped = &skipped{Message: nonVerdict(f)}
		}
		suite.Cases = append(suite.Cases, tc)
	}
	suite.Tests = len(suite.Cases)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(suite); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// className groups findings the way CI groups tests. The citation's family is
// used where there is one, because CI keys history off classname and name --
// which is also why the seed never appears in either.
func className(f oracle.Finding) string {
	if f.Citation != "" {
		for i, r := range f.Citation {
			if r == '/' {
				return "charpy." + f.Citation[:i]
			}
		}
	}
	return "charpy." + f.Layer
}

func caseName(f oracle.Finding) string {
	if f.Seq >= 0 {
		return fmt.Sprintf("%s (seq %d)", f.Check, f.Seq)
	}
	return f.Check
}

func body(f oracle.Finding) string {
	out := f.Summary
	if f.Detail != "" {
		out += "\n" + f.Detail
	}
	if f.Citation != "" {
		out += "\ncase: " + f.Citation
	}
	return out
}

func nonVerdict(f oracle.Finding) string {
	out := string(f.Verdict) + ": " + f.Summary
	if f.Reason != "" {
		out += " (" + f.Reason + ")"
	}
	return out
}
