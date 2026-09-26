package reaction

import (
	"fmt"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Expectation is what a case declares the subject should do about its fault,
// as its [case.expect] table states it. A zero field declares nothing: no
// JSON-RPC error code is 0, and no HTTP status is.
type Expectation struct {
	ErrorCode  int64 // expect_error_code
	HTTPStatus int64 // expect_http_status
}

// Expected judges the case-specific tier: for each fault that reached the
// subject, whether what the subject did next is what its case declares.
//
// The transcript names each fault's case but not its expectations, so lookup
// supplies them by case id -- from the catalogue of the charpy doing the
// replay, which is how any layer written after a run judges a transcript
// captured before it. A case that declares nothing yields nothing, and a fault
// that reached charpy's own peer yields nothing here either: the generic
// check has already said so.
//
// Every verdict is OBSERVED, and worded as the fact it is -- "answered with
// -32601; the case expects -32700" -- because an expectation is the case's
// reading of what a subject should do, not a generated artifact rejecting a
// frame.
func Expected(t *transcript.Transcript, lookup func(caseID string) Expectation) oracle.Report {
	rep := oracle.Report{RunID: t.Header.RunID}
	class := t.Header.Subject.Class

	var prev int64 = -1
	for _, applied := range t.Events(transcript.FaultApplied) {
		from := prev
		prev = applied.Seq
		if applied.Fault == nil {
			continue
		}
		dir, ok := applied.AppliedDirection()
		if !ok {
			continue
		}
		var face transcript.Face
		if applied.Face != nil {
			face = *applied.Face
		}
		if !oracle.SubjectReceives(class, face, dir) {
			continue
		}
		exp := lookup(applied.Fault.CaseID)
		if exp.ErrorCode == 0 && exp.HTTPStatus == 0 {
			continue
		}

		asked := faultedIDs(t, applied, from)
		if exp.ErrorCode != 0 {
			rep.Add(expectError(t, class, applied, asked, exp.ErrorCode))
		}
		if exp.HTTPStatus != 0 {
			rep.Add(expectStatus(t, class, applied, exp.HTTPStatus))
		}
	}
	return rep
}

// faultedIDs is the ids of the frames this fault acted on: what they carried,
// or, where charpy's rewrite or cut hid it, what they used to carry. They are
// the frames between the case's previous fault_applied and this one, on its
// connection, attributed to its case.
func faultedIDs(t *transcript.Transcript, applied *transcript.EventLine, after int64) map[string]bool {
	out := map[string]bool{}
	for _, f := range t.Frames() {
		if f.Seq <= after || f.Seq >= applied.Seq || f.Fault == nil ||
			f.Fault.CaseID != applied.Fault.CaseID || !sameConn(f.ConnID, applied.ConnID) {
			continue
		}
		if r := f.Fault.Replaced; r != nil {
			out[string(r.IDType)+":"+r.ID] = true
		} else if k := f.Key(); k != "" {
			out[k] = true
		}
	}
	return out
}

// expectError looks for the subject's answer to the faulted request: an error
// or a result under its id, or an error with a null id, which is how JSON-RPC
// answers a request too broken to have one.
func expectError(t *transcript.Transcript, class transcript.Class, applied *transcript.EventLine, asked map[string]bool, want int64) oracle.Finding {
	f := finding(applied)
	walkAfter(t, applied, func(fr *transcript.FrameLine, exit *transcript.EventLine) bool {
		if exit != nil {
			f.Seq = exit.Seq
			f.Summary = fmt.Sprintf("the subject exited without answering; the case expects error %d", want)
			return true
		}
		if !oracle.SubjectOriginated(class, fr) {
			return false
		}
		key := fr.Key()
		switch {
		case fr.Kind == envelope.KindError && (asked[key] || fr.IDType == envelope.IDNull):
			f.Seq = fr.Seq
			got := int64(0)
			if fr.ErrorCode != nil {
				got = *fr.ErrorCode
			}
			if got == want {
				f.Summary = fmt.Sprintf("the subject answered with error %d, as the case expects", want)
			} else {
				f.Summary = fmt.Sprintf("the subject answered with error %d; the case expects %d", got, want)
			}
			return true
		case fr.Kind == envelope.KindResponse && asked[key]:
			f.Seq = fr.Seq
			f.Summary = fmt.Sprintf("the subject answered with a result; the case expects error %d", want)
			return true
		}
		return false
	})
	if f.Summary == "" {
		f.Summary = fmt.Sprintf("the subject did not answer with an error; the case expects %d", want)
	}
	return f
}

// expectStatus reads the HTTP status of the subject's first answer after the
// fault, which on the request path is its answer to the faulted request.
func expectStatus(t *transcript.Transcript, class transcript.Class, applied *transcript.EventLine, want int64) oracle.Finding {
	f := finding(applied)
	walkAfter(t, applied, func(fr *transcript.FrameLine, exit *transcript.EventLine) bool {
		if exit != nil {
			f.Seq = exit.Seq
			f.Summary = fmt.Sprintf("the subject exited without answering; the case expects HTTP %d", want)
			return true
		}
		if !oracle.SubjectOriginated(class, fr) || fr.HTTP == nil || fr.HTTP.Status == nil {
			return false
		}
		f.Seq = fr.Seq
		if got := int64(*fr.HTTP.Status); got == want {
			f.Summary = fmt.Sprintf("the subject answered HTTP %d, as the case expects", want)
		} else {
			f.Summary = fmt.Sprintf("the subject answered HTTP %d; the case expects %d", got, want)
		}
		return true
	})
	if f.Summary == "" {
		f.Summary = fmt.Sprintf("no HTTP answer from the subject was recorded; the case expects %d", want)
	}
	return f
}

func finding(applied *transcript.EventLine) oracle.Finding {
	return oracle.Finding{
		Verdict:  oracle.Observed,
		Layer:    Layer,
		Check:    "expectation",
		Citation: applied.Fault.Citation,
		Seq:      applied.Seq,
	}
}

// walkAfter visits the frames on the fault's connection after it, and a
// subject exit that was not charpy's doing, until visit says it has its
// answer. charpy ending the run ends the walk: it is not the subject leaving.
func walkAfter(t *transcript.Transcript, applied *transcript.EventLine, visit func(*transcript.FrameLine, *transcript.EventLine) bool) {
	for i := range t.Entries {
		en := &t.Entries[i]
		if en.Seq <= applied.Seq {
			continue
		}
		if e := en.Event; e != nil && e.EventKind == transcript.SubjectExit {
			if killed, _ := e.Detail["killed_by_charpy"].(bool); killed {
				return
			}
			if visit(nil, e) {
				return
			}
			continue
		}
		if fr := en.Frame; fr != nil && sameConn(fr.ConnID, applied.ConnID) && visit(fr, nil) {
			return
		}
	}
}
