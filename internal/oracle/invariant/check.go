package invariant

import (
	"fmt"
	"slices"
	"strings"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Layer is what findings from here are tagged with.
const Layer = "invariant"

// What this layer reports in. Each check is a registered invariant's name.
const (
	CheckIDResolvesOnce        oracle.Check = "id-resolves-once"
	CheckNoUnsolicitedResponse oracle.Check = "no-unsolicited-response"
	CheckNoDuplicateInflightID oracle.Check = "no-duplicate-inflight-id"

	ReasonCharpyReplacedTheAnswer oracle.Reason = "charpy-replaced-the-answer"
)

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
	// id is the request id as findings print it; the map an exchange lives in
	// also keys it by the direction its request travelled.
	id        string
	requested []*transcript.FrameLine
	answered  []*transcript.FrameLine

	// replaced counts the answers charpy rewrote into something carrying a
	// different id, or none. They are not answers to this id on the wire and
	// they are not the subject's failure to send one either, which is the
	// distinction the fault's `replaced` attribution exists to record.
	replaced []*transcript.FrameLine
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
	// Requests and answers are paired per connection, per id, and per the
	// direction the request travelled. Pairing across connections would be
	// wrong for the same reason joining gateway faces on id is: an id is only
	// unique within the exchange that issued it. And JSON-RPC ids are the
	// sender's: a client's request 1 and its server's request 1 are two
	// exchanges, so an answer is paired with the requests that came the other
	// way to it.
	exchanges := map[string]*exchange{}
	var order []string
	file := func(origin transcript.Direction, id string) *exchange {
		k := string(origin) + "|" + id
		e, ok := exchanges[k]
		if !ok {
			e = &exchange{id: id}
			exchanges[k] = e
			order = append(order, k)
		}
		return e
	}

	for _, f := range t.Frames() {
		if f.Face != key.face || deref(f.ConnID) != key.conn {
			continue
		}
		// A frame charpy rewrote away still answers the id it used to carry,
		// which the fault attribution names. Filed under that id rather than
		// under whatever the replacement parses to -- usually nothing.
		//
		// And only there. What its bytes parse to is not an answer that
		// arrived: a line cut before its newline is whole JSON still carrying
		// the id, and the recipient never read it.
		// A replaced request is filed nowhere: the subject never received it.
		if f.Fault != nil && f.Fault.Replaced != nil {
			if r := replacedKey(f); r != "" {
				e := file(f.Direction.Opposite(), r)
				e.replaced = append(e.replaced, f)
			}
			continue
		}

		id := f.Key()
		if id == "" {
			// A notification or a frame with no envelope resolves nothing and
			// is owed nothing.
			continue
		}

		switch f.Kind {
		case "request":
			e := file(f.Direction, id)
			e.requested = append(e.requested, f)
		case "response", "error":
			e := file(f.Direction.Opposite(), id)
			e.answered = append(e.answered, f)
		}
	}

	exit := subjectExit(t)

	for _, k := range order {
		e := exchanges[k]
		idResolvesOnce(rep, class, e.id, e, exit)
		noUnsolicitedResponse(rep, class, e.id, e)
		noDuplicateInflightID(rep, class, e.id, e)
	}
}

// I1: every request id resolves exactly once -- result or error, never both,
// never neither.
func idResolvesOnce(rep *oracle.Report, class transcript.Class, id string, e *exchange, exit *transcript.EventLine) {
	if len(e.requested) == 0 {
		return // not a request; I2 is what covers an answer to nothing
	}

	// "More than once" is a claim about the subject, so it counts only the
	// answers the subject wrote. A duplicate charpy injected is charpy's --
	// reporting it would be quoting our own fault back as evidence, which I2
	// already refuses to do, and it is the verdict that would be pasted into
	// somebody else's issue tracker.
	//
	// The "never" arm below counts every answer instead, including charpy's:
	// there the question is whether an answer crossed at all, and a frame
	// charpy rewrote is still the subject's answer arriving.
	own := ownAnswers(class, e)

	// Twice is too many only if the subject was asked once. A subject handed
	// the same id twice -- a duplicate charpy sent beside the original -- and
	// answering each is doing what it was asked; that is the duplicate case's
	// question, and it is judged by what the subject did, not by this count.
	asked := received(class, e)

	switch {
	case len(own) > max(1, asked):
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: CheckIDResolvesOnce,
			Seq:     own[1].Seq,
			Summary: fmt.Sprintf("id %s was answered %d times", id, len(own)),
			Detail:  fmt.Sprintf("requested at seq %d, answered at %s", e.requested[0].Seq, seqs(own)),
		})

	case len(e.answered) == 0:
		// The "never neither" arm. An answer charpy rewrote into something
		// carrying another id, or none, is an answer the subject did send:
		// reporting it as unanswered would be charpy grading its own
		// interference. The run still owes a reader the fact, so it is
		// inconclusive rather than silent.
		if len(e.replaced) > 0 {
			rep.Add(oracle.Finding{
				Verdict: oracle.Inconclusive, Layer: Layer, Check: CheckIDResolvesOnce,
				Seq:     e.replaced[0].Seq,
				Summary: fmt.Sprintf("id %s was answered, and charpy replaced the answer", id),
				Detail: fmt.Sprintf("requested at seq %d, replaced at %s",
					e.requested[0].Seq, seqs(e.replaced)),
				Reason: ReasonCharpyReplacedTheAnswer,
			})
			return
		}

		// An id still outstanding when the subject exited is a fact, whoever
		// ended it: the subject leaving with a request it received, or charpy
		// ending the run while one was open. Neither is graded here. The time
		// from the request to the exit is what tells a hang that ran out the
		// run from a teardown that never waited, and the reader decides.
		if req := e.requested[0]; exit != nil && req.Seq < exit.Seq {
			who := "the subject exited"
			if killed, _ := exit.Detail["killed_by_charpy"].(bool); killed {
				who = "charpy ended the run"
			}
			rep.Add(oracle.Finding{
				Verdict: oracle.Observed, Layer: Layer, Check: CheckIDResolvesOnce,
				Seq: req.Seq,
				Summary: fmt.Sprintf("id %s was still outstanding when %s, %dms after it was asked",
					id, who, (exit.TMonoNS-req.TMonoNS)/1_000_000),
				Detail: fmt.Sprintf("requested at seq %d, %s; exit at seq %d, %s",
					req.Seq, method(req), exit.Seq, exitText(exit.Detail)),
			})
			return
		}
		rep.Add(oracle.Finding{
			Verdict: oracle.Observed, Layer: Layer, Check: CheckIDResolvesOnce,
			Seq:     e.requested[0].Seq,
			Summary: fmt.Sprintf("id %s was never answered", id),
			Detail:  fmt.Sprintf("requested at seq %d, %s", e.requested[0].Seq, method(e.requested[0])),
		})
	}
}

// replacedKey is the exchange an answer charpy replaced used to belong to, or
// "" when there was no such answer: charpy kept the id, did not touch the
// frame, or what it replaced was a request.
//
// A replaced request is not filed anywhere. The subject never received a
// request with that id, so it owes that id nothing, and filing it as an answer
// would report "charpy replaced the answer" for a question that was never put.
// An attribution naming no kind predates the field and replaced an answer,
// which is all those runs could replace.
func replacedKey(f *transcript.FrameLine) string {
	if f.Fault == nil || f.Fault.Replaced == nil {
		return ""
	}
	switch f.Fault.Replaced.Kind {
	case "", envelope.KindResponse, envelope.KindError:
	default:
		return ""
	}
	return string(f.Fault.Replaced.IDType) + ":" + f.Fault.Replaced.ID
}

// received counts the requests under this id that crossed toward the subject.
func received(class transcript.Class, e *exchange) int {
	n := 0
	for _, r := range e.requested {
		if transcript.SubjectReceives(class, r.Face, r.Direction) {
			n++
		}
	}
	return n
}

// ownAnswers is the answers the subject wrote, dropping the ones charpy
// authored or rewrote.
func ownAnswers(class transcript.Class, e *exchange) []*transcript.FrameLine {
	var out []*transcript.FrameLine
	for _, a := range e.answered {
		if oracle.SubjectOriginated(class, a) {
			out = append(out, a)
		}
	}
	return out
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
			Verdict: oracle.Observed, Layer: Layer, Check: CheckNoUnsolicitedResponse,
			Seq:     a.Seq,
			Summary: fmt.Sprintf("id %s was answered but never requested", id),
			Detail:  fmt.Sprintf("answered at seq %d", a.Seq),
		})
	}
}

// I3: no two requests share an id while both are in flight on one connection.
// This is an id-allocation bug, which fails differently again -- and it is only
// the subject's when the subject allocated the ids. Requests the subject
// received were numbered by whoever sent them: charpy's peer, a client charpy
// is relaying, or charpy itself, which is what a duplicate-request case does.
func noDuplicateInflightID(rep *oracle.Report, class transcript.Class, id string, e *exchange) {
	var requested []*transcript.FrameLine
	for _, r := range e.requested {
		if oracle.SubjectOriginated(class, r) {
			requested = append(requested, r)
		}
	}
	if len(requested) < 2 {
		return
	}
	e = &exchange{requested: requested, answered: e.answered}

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
			Verdict: oracle.Observed, Layer: Layer, Check: CheckNoDuplicateInflightID,
			Seq:     r.Seq,
			Summary: fmt.Sprintf("id %s was requested again while still in flight", id),
			Detail:  fmt.Sprintf("first at seq %d, again at seq %d", e.requested[0].Seq, r.Seq),
		})
	}
}

// subjectExit is the transcript's first subject_exit, or nil.
func subjectExit(t *transcript.Transcript) *transcript.EventLine {
	if events := t.Events(transcript.SubjectExit); len(events) > 0 {
		return events[0]
	}
	return nil
}

// exitText is how a subject_exit ended: its code, and its signal if it had one.
func exitText(d map[string]any) string {
	code, _ := d["exit_code"].(float64)
	s := fmt.Sprintf("exit code %d", int64(code))
	if sig, _ := d["signal"].(string); sig != "" {
		s += ", signal " + sig
	}
	return s
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
