// Package coverage reports which armed cases never fired.
//
// It is not a fifth oracle layer and asserts nothing about the subject. It
// compares two things the transcript already records -- the cases the run
// armed, and the faults it applied -- and reports the difference, because
// nothing else can: an oracle reads a finished file, and a case that never
// matched leaves no trace in it at all.
//
// The distinction it exists to keep is between UNTRIGGERED and a pass. A run
// where the fault never fired and a run where it fired and found nothing
// produce the same empty finding list, and calling both clean is how a suite
// acquires confidence it has not earned (docs/design/oracle.md section 2).
// Under relayed stimulus an untriggered case is ordinary -- the traffic is
// somebody else's and may never go where the matcher points. Under owned
// stimulus it usually means the scenario and the case disagree, which is a
// bug in charpy rather than a finding about anyone.
package coverage

import (
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Layer is what findings from here are tagged with.
const Layer = "coverage"

// What this layer reports in.
const (
	CheckFaultApplied oracle.Check = "fault-applied"

	ReasonNoFrameMatched oracle.Reason = "no-frame-matched"
)

// Check reports an UNTRIGGERED finding for every armed case that never
// applied a fault.
//
// A transcript whose header names no cases yields nothing: it was written
// before charpy recorded what it armed, and inventing coverage for it would
// be a claim about a run nobody can check.
func Check(t *transcript.Transcript) oracle.Report {
	rep := oracle.Report{RunID: t.Header.RunID}
	if t.Header == nil || len(t.Header.Cases) == 0 {
		return rep
	}

	fired := map[string]bool{}
	for _, e := range t.Events(transcript.FaultApplied) {
		if e.Fault != nil {
			fired[e.Fault.CaseID] = true
		}
	}

	for _, citation := range t.Header.Cases {
		if fired[caseID(citation)] {
			continue
		}
		rep.Add(oracle.Finding{
			Verdict:  oracle.Untriggered,
			Layer:    Layer,
			Check:    CheckFaultApplied,
			Citation: citation,
			Seq:      -1,
			Summary:  "the case was armed and its fault never fired",
			Reason:   ReasonNoFrameMatched,
			Detail: "No frame matched this case's [case.match], so nothing was injected. " +
				"That is not a pass: the subject was never asked the question.",
		})
	}
	return rep
}

// caseID is the permanent id inside a citation. The header records citations
// -- a bug report needs the revision and seed -- while a fault line records
// the bare id, so one of them has to be reduced to compare them, and the id
// is the half that never changes (docs/design/case-identity.md section 1).
func caseID(citation string) string {
	for i, r := range citation {
		if r == '@' || r == '#' {
			return citation[:i]
		}
	}
	return citation
}
