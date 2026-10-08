package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/serverplumber/charpy/internal/oracle"
)

// Rendering a finished run. The build gate is replay's exit code, which fails
// on a MUST and nothing else; the formats here are for reading and querying.
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
			Check:         string(f.Check),
			Cites:         f.Cites,
			Citation:      f.Citation,
			Seq:           f.Seq,
			Summary:       f.Summary,
			Detail:        f.Detail,
			Reason:        string(f.Reason),
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
	Cites         string `json:"cites,omitempty"`
	Citation      string `json:"citation,omitempty"`
	Seq           int64  `json:"seq"`
	Summary       string `json:"summary"`
	Detail        string `json:"detail,omitempty"`
	Reason        string `json:"reason,omitempty"`
}
