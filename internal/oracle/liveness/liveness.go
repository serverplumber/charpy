// Package liveness is oracle layer 4: after a fault acted, did the subject
// resume serving within the budget?
//
// Everyone tests that a fault does not crash the subject. Almost nobody tests
// recovery, and recovery is the production question -- a gateway that stops
// reconnecting after a transient upstream failure is broken in a way no
// conformance suite notices. So this layer reports the recovery outcome
// explicitly, success as well as failure: "recovered" is the observation worth
// making, not a silence.
//
// It is offline, like every layer. The live probe is fired by the driver after
// the fault acts (docs/design/oracle.md section 6); this reads the probe events
// it left behind. The probe's own deadline was the case's budget, so the
// outcome already encodes within-budget (ProbeOK) versus not (ProbeTimeout),
// and this layer needs no clock of its own.
package liveness

import (
	"fmt"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Layer is what findings from here are tagged with.
const Layer = "liveness"

// Check reads a transcript's probe events and reports each as a recovery
// outcome. A run with no probes yields nothing: the fault either did not act or
// the case declared no budget, and inventing a liveness verdict for it would be
// a claim about a recovery never tested.
func Check(t *transcript.Transcript) oracle.Report {
	rep := oracle.Report{RunID: t.Header.RunID}

	for _, e := range t.Events(transcript.Probe) {
		method := str(e.Detail["method"])
		outcome := transcript.ProbeOutcome(str(e.Detail["outcome"]))
		elapsed := i64(e.Detail["elapsed_mono_ns"])

		f := oracle.Finding{
			Verdict: oracle.Observed,
			Layer:   Layer,
			Check:   "recovery",
			Seq:     e.Seq,
		}
		if outcome == transcript.ProbeOK {
			f.Summary = "the subject recovered after the fault"
			f.Detail = fmt.Sprintf("probe %s answered in %dms", method, elapsed/1_000_000)
		} else {
			f.Summary = "the subject did not recover within the budget"
			f.Detail = fmt.Sprintf("probe %s ended %s after %dms", method, outcome, elapsed/1_000_000)
		}
		rep.Add(f)
	}
	return rep
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func i64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}
