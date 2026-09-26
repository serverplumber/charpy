package reaction

import (
	"fmt"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Layer is what findings from here are tagged with.
const Layer = "reaction"

// Check reads a transcript's applied faults and probes and reports what the
// subject did about each.
func Check(t *transcript.Transcript) oracle.Report {
	rep := oracle.Report{RunID: t.Header.RunID}
	class := t.Header.Subject.Class

	for _, e := range t.Events(transcript.FaultApplied) {
		react(&rep, t, class, e)
	}
	recovery(&rep, t)
	return rep
}

// react reports the subject's reaction to one applied fault.
//
// A fault_applied event that records no direction predates ADR-013 and yields
// nothing: who the fault was put to is exactly what it cannot say, and
// guessing would be a claim about a run nobody can check.
func react(rep *oracle.Report, t *transcript.Transcript, class transcript.Class, applied *transcript.EventLine) {
	dir, ok := applied.AppliedDirection()
	if !ok {
		return
	}

	f := oracle.Finding{Layer: Layer, Check: "reaction", Seq: applied.Seq}
	if applied.Fault != nil {
		f.Citation = applied.Fault.Citation
	}

	var face transcript.Face
	if applied.Face != nil {
		face = *applied.Face
	}
	if !oracle.SubjectReceives(class, face, dir) {
		f.Verdict = oracle.Skipped
		f.Summary = "the fault reached charpy's own peer, not the subject"
		f.Detail = fmt.Sprintf("it acted on a %s frame, which a %s subject sends rather than receives", dir, class)
		f.Reason = "fault-reached-charpy"
		rep.Add(f)
		return
	}

	// Walk what came after the fault. A question is a request the subject
	// received untouched; an answer is the subject's own response to one. A
	// request asked before the fault does not count even if answered after:
	// the question is whether the subject still serves, and that exchange was
	// already under way.
	asked := map[string]*transcript.FrameLine{}
	var first *transcript.FrameLine
	var last int64 = applied.TMonoNS
	for i := range t.Entries {
		en := &t.Entries[i]
		if en.Seq <= applied.Seq {
			continue
		}
		last = en.TMonoNS

		if e := en.Event; e != nil && e.EventKind == transcript.SubjectExit {
			// charpy ending the run is not the subject leaving.
			if killed, _ := e.Detail["killed_by_charpy"].(bool); killed {
				break
			}
			f.Verdict = oracle.Observed
			f.Seq = e.Seq
			f.Summary = "the subject exited after the fault"
			f.Detail = fmt.Sprintf("%s, %s after the fault", exitText(e.Detail), ms(e.TMonoNS-applied.TMonoNS))
			rep.Add(f)
			return
		}

		fr := en.Frame
		if fr == nil || !sameConn(fr.ConnID, applied.ConnID) {
			continue
		}
		key := fr.Key()
		if key == "" {
			continue
		}
		switch {
		case fr.Kind == envelope.KindRequest && !fr.Tampered() &&
			oracle.SubjectReceives(class, fr.Face, fr.Direction):
			if _, seen := asked[key]; !seen {
				asked[key] = fr
				if first == nil {
					first = fr
				}
			}
		case (fr.Kind == envelope.KindResponse || fr.Kind == envelope.KindError) &&
			oracle.SubjectOriginated(class, fr):
			q, ok := asked[key]
			if !ok {
				continue
			}
			f.Verdict = oracle.Observed
			f.Seq = fr.Seq
			f.Summary = "the subject answered after the fault"
			f.Detail = fmt.Sprintf("request at seq %d (%s) answered at seq %d, %s after the fault",
				q.Seq, q.MethodName(), fr.Seq, ms(fr.TMonoNS-applied.TMonoNS))
			rep.Add(f)
			return
		}
	}

	if first == nil {
		// The fault reached the subject and then nothing asked it anything.
		// That is a run which did not put the question, not a subject that
		// failed it.
		f.Verdict = oracle.Inconclusive
		f.Summary = "nothing asked the subject anything after the fault"
		f.Reason = "nothing-asked-after-fault"
		rep.Add(f)
		return
	}
	f.Verdict = oracle.Observed
	f.Seq = first.Seq
	f.Summary = "the subject did not answer after the fault"
	f.Detail = fmt.Sprintf("%d request(s) after the fault went unanswered; the first, at seq %d (%s), "+
		"was still open when the run ended %s later", len(asked), first.Seq, first.MethodName(), ms(last-first.TMonoNS))
	rep.Add(f)
}

// recovery reads the probe events a driver fired once the fault ended, and
// reports each as a recovery outcome. A run with no probes yields nothing:
// the fault either did not act or the case declared no budget, and inventing
// a verdict for it would be a claim about a recovery never tested.
//
// The probe's own deadline was the case's budget, so the outcome already
// encodes within-budget (ProbeOK) versus not (ProbeTimeout), and this needs no
// clock of its own.
func recovery(rep *oracle.Report, t *transcript.Transcript) {
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
}

func sameConn(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func exitText(d map[string]any) string {
	s := fmt.Sprintf("exit code %d", i64(d["exit_code"]))
	if sig := str(d["signal"]); sig != "" {
		s += ", signal " + sig
	}
	return s
}

func ms(ns int64) string { return fmt.Sprintf("%dms", ns/1_000_000) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func i64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
