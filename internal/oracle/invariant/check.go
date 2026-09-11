package invariant

import (
	"fmt"
	"slices"
	"strings"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Layer is what findings from here are tagged with.
const Layer = "invariant"

// Check runs the universal invariants over a transcript.
//
// v0 ships I1 to I3, the set that holds for every revision and every subject
// class. Each is a small function over the transcript and nothing else, which
// is what makes them language-agnostic for free: they never touch the
// implementation, only what crossed the wire.
//
// They are computed together because they share one pass over the frames and
// one view of what was outstanding. They are reported separately because they
// fail differently and need different reproducers -- I1 is a bookkeeping or
// liveness failure, I2 is a pending-map leak, I3 is an id-allocation bug --
// which is why they are three invariants rather than one.
func Check(t *transcript.Transcript) oracle.Report {
	rep := oracle.Report{RunID: t.Header.RunID}
	class := t.Header.Subject.Class

	for _, conn := range connections(t) {
		checkConn(&rep, class, conn, t)
	}

	rep.Sort()
	return rep
}

// exchange is what the oracle remembers about one request id on one
// connection.
type exchange struct {
	requested []*transcript.FrameLine
	answered  []*transcript.FrameLine
}

type connKey struct {
	face transcript.Face
	conn string
}

// connections lists the connections a transcript covers, in first-seen order
// so that a report's ordering does not depend on map iteration.
func connections(t *transcript.Transcript) []connKey {
	var out []connKey
	seen := map[connKey]bool{}
	for _, f := range t.Frames() {
		k := connKey{face: f.Face, conn: deref(f.ConnID)}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func checkConn(rep *oracle.Report, class transcript.Class, key connKey, t *transcript.Transcript) {
	// Requests and answers are paired per connection and per id. Pairing
	// across connections would be wrong for the same reason joining gateway
	// faces on id is: an id is only unique within the exchange that issued it.
	exchanges := map[string]*exchange{}
	var order []string

	for _, f := range t.Frames() {
		if f.Face != key.face || deref(f.ConnID) != key.conn {
			continue
		}
		id := f.Key()
		if id == "" {
			// A notification or a frame with no envelope resolves nothing and
			// is owed nothing.
			continue
		}
		e, ok := exchanges[id]
		if !ok {
			e = &exchange{}
			exchanges[id] = e
			order = append(order, id)
		}

		switch f.Kind {
		case "request":
			e.requested = append(e.requested, f)
		case "response", "error":
			e.answered = append(e.answered, f)
		}
	}

	died, diedAt := subjectDied(t)

	for _, id := range order {
		e := exchanges[id]
		idResolvesOnce(rep, id, e, died, diedAt)
		noUnsolicitedResponse(rep, class, id, e)
		noDuplicateInflightID(rep, id, e)
	}
}

// I1: every request id resolves exactly once -- result or error, never both,
// never neither.
func idResolvesOnce(rep *oracle.Report, id string, e *exchange, died bool, diedAt int64) {
	if len(e.requested) == 0 {
		return // not a request; I2 is what covers an answer to nothing
	}

	switch {
	case len(e.answered) > 1:
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: "id-resolves-once",
			Seq:     e.answered[1].Seq,
			Summary: fmt.Sprintf("id %s was answered %d times", id, len(e.answered)),
			Detail:  fmt.Sprintf("requested at seq %d, answered at %s", e.requested[0].Seq, seqs(e.answered)),
		})

	case len(e.answered) == 0:
		// The "never neither" arm. An id still outstanding when the subject
		// died is not a subject that failed to answer -- it is a transcript
		// that stopped before the answer could arrive, which supports no
		// conclusion either way.
		if died && e.requested[0].Seq < diedAt {
			rep.Add(oracle.Finding{
				Verdict: oracle.Inconclusive, Layer: Layer, Check: "id-resolves-once",
				Seq:     e.requested[0].Seq,
				Summary: fmt.Sprintf("id %s was outstanding when the subject exited", id),
				Reason:  "subject-exited-first",
			})
			return
		}
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: "id-resolves-once",
			Seq:     e.requested[0].Seq,
			Summary: fmt.Sprintf("id %s was never answered", id),
			Detail:  fmt.Sprintf("requested at seq %d, %s", e.requested[0].Seq, method(e.requested[0])),
		})
	}
}

// I2: no response or error carries an id that was never requested on that
// face. This is the pending-map leak, which is why it is split from I1.
func noUnsolicitedResponse(rep *oracle.Report, class transcript.Class, id string, e *exchange) {
	if len(e.requested) > 0 || len(e.answered) == 0 {
		return
	}
	for _, a := range e.answered {
		// An answer charpy fabricated is charpy's, and holding it against the
		// subject would be reporting charpy's own fault as a finding.
		if !oracle.SubjectOriginated(class, a) {
			continue
		}
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: "no-unsolicited-response",
			Seq:     a.Seq,
			Summary: fmt.Sprintf("id %s was answered but never requested", id),
			Detail:  fmt.Sprintf("answered at seq %d", a.Seq),
		})
	}
}

// I3: no two requests share an id while both are in flight on one connection.
// This is an id-allocation bug, which fails differently again.
func noDuplicateInflightID(rep *oracle.Report, id string, e *exchange) {
	if len(e.requested) < 2 {
		return
	}

	// Reuse after the first was answered is not the fault; overlap is. The
	// second request is in flight alongside the first when it arrives before
	// the first is answered.
	firstAnswer := int64(-1)
	if len(e.answered) > 0 {
		firstAnswer = e.answered[0].Seq
	}
	for _, r := range e.requested[1:] {
		if firstAnswer >= 0 && r.Seq > firstAnswer {
			continue
		}
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: "no-duplicate-inflight-id",
			Seq:     r.Seq,
			Summary: fmt.Sprintf("id %s was requested again while still in flight", id),
			Detail:  fmt.Sprintf("first at seq %d, again at seq %d", e.requested[0].Seq, r.Seq),
		})
	}
}

// subjectDied reports whether the subject exited, and where.
func subjectDied(t *transcript.Transcript) (bool, int64) {
	events := t.Events(transcript.SubjectExit)
	if len(events) == 0 {
		return false, 0
	}
	return true, events[0].Seq
}

func seqs(frames []*transcript.FrameLine) string {
	nums := make([]string, 0, len(frames))
	for _, f := range frames {
		nums = append(nums, fmt.Sprint(f.Seq))
	}
	return "seq " + strings.Join(nums, ", ")
}

func method(f *transcript.FrameLine) string {
	if m := f.MethodName(); m != "" {
		return "method " + m
	}
	return "no method recorded"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Implemented reports which registered invariants this package actually
// evaluates. The registry names thirteen; v0 evaluates the universal three,
// and a report that quietly omitted the rest would read as ten passes.
func Implemented() []string {
	return slices.Clone([]string{
		"id-resolves-once",
		"no-unsolicited-response",
		"no-duplicate-inflight-id",
	})
}
