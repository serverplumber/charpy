package reaction

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Expectation is what a case declares the subject should do about its fault,
// as its [case.expect] table states it. A zero field declares nothing: no
// JSON-RPC error code is 0, and no HTTP status is.
type Expectation struct {
	ErrorCode      int64 // expect_error_code
	HTTPStatus     int64 // expect_http_status
	NegotiatedOnly bool  // expect_negotiated_only
}

// declares reports whether the case declares anything this tier judges.
func (e Expectation) declares() bool {
	return e.ErrorCode != 0 || e.HTTPStatus != 0 || e.NegotiatedOnly
}

// Expected judges the case-specific tier: for each fault that reached the
// subject, whether what the subject did next is what its case declares.
//
// The transcript names each fault's case but not its expectations, so lookup
// supplies them by case id -- from the catalogue of the charpy doing the
// replay, which is how any layer written after a run judges a transcript
// captured before it. A case that declares nothing yields nothing.
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
		if _, ok := applied.AppliedDirection(); !ok {
			continue
		}
		exp := lookup(applied.Fault.CaseID)
		if !exp.declares() {
			continue
		}

		asked := faultedIDs(t, applied, from)
		if exp.ErrorCode != 0 {
			rep.Add(expectError(t, class, applied, asked, exp.ErrorCode))
		}
		if exp.HTTPStatus != 0 {
			rep.Add(expectStatus(t, class, applied, exp.HTTPStatus))
		}
		if exp.NegotiatedOnly {
			rep.Add(expectNegotiated(t, class, applied, from))
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

// expectNegotiated judges the subject's requests after a faulted handshake
// against what that handshake advertised as it crossed -- charpy's rewrite,
// not the reference peer's original, because the rewrite is what the subject
// negotiated. The lifecycle's rule is that both sides "only use capabilities
// that were successfully negotiated", and a request whose method needs a
// capability the handshake left out is the subject using one that was not.
//
// It reads the connection the handshake was on. A later handshake is a later
// negotiation, and a request there is judged by that one.
func expectNegotiated(t *transcript.Transcript, class transcript.Class, applied *transcript.EventLine, after int64) oracle.Finding {
	f := finding(applied)
	hs := faultedHandshake(t, applied, after)
	if hs == nil {
		f.Summary = "the fault did not land on a handshake; the case expects the subject to keep to what one negotiated"
		return f
	}
	f.Seq = hs.Seq
	caps, err := advertised(hs)
	if err != nil {
		f.Summary = fmt.Sprintf("the faulted handshake at seq %d does not carry readable capabilities: %v", hs.Seq, err)
		return f
	}
	needs := serverCapability
	if hs.Direction == transcript.C2S {
		needs = clientCapability
	}

	var used []string
	first := int64(-1)
	walkAfter(t, applied, func(fr *transcript.FrameLine, exit *transcript.EventLine) bool {
		if exit != nil {
			return true
		}
		if !oracle.SubjectOriginated(class, fr) || fr.Kind != envelope.KindRequest {
			return false
		}
		method := fr.MethodName()
		if c := needs(method); c != "" && !caps[c] {
			if first < 0 {
				first = fr.Seq
			}
			used = append(used, fmt.Sprintf("%s (needs %s)", method, c))
		}
		return false
	})
	if first < 0 {
		f.Summary = fmt.Sprintf("the subject sent only what the handshake at seq %d negotiated, as the case expects", hs.Seq)
		return f
	}
	f.Seq = first
	f.Summary = fmt.Sprintf("the subject used capabilities the handshake at seq %d did not advertise: %s",
		hs.Seq, strings.Join(used, ", "))
	return f
}

// faultedHandshake is the initialize frame this fault rewrote, as it crossed:
// the last on the fault's connection, since the case's previous fault,
// attributed to its case. An answer is known by its id, paired with the
// initialize request on the same connection, rather than by the method the
// ledger echoes onto it, which a transcript need not carry.
func faultedHandshake(t *transcript.Transcript, applied *transcript.EventLine, after int64) *transcript.FrameLine {
	asked := map[string]bool{}
	var hs *transcript.FrameLine
	for _, f := range t.Frames() {
		if f.Seq >= applied.Seq || !sameConn(f.ConnID, applied.ConnID) {
			continue
		}
		handshake := f.MethodName() == "initialize"
		if f.Kind == envelope.KindRequest {
			if handshake {
				asked[f.Key()] = true
			}
		} else if f.Kind == envelope.KindResponse && asked[f.Key()] {
			handshake = true
		}
		if handshake && f.Seq > after && f.Fault != nil && f.Fault.CaseID == applied.Fault.CaseID {
			hs = f
		}
	}
	return hs
}

// advertised is the capability names a handshake frame carries: the client's
// in an initialize request's params, the server's in its result.
func advertised(hs *transcript.FrameLine) (map[string]bool, error) {
	if !hs.Complete() {
		return nil, fmt.Errorf("the frame was capped")
	}
	b, err := hs.Bytes()
	if err != nil {
		return nil, err
	}
	var msg struct {
		Params *struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"params"`
		Result *struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		return nil, err
	}
	var caps map[string]json.RawMessage
	switch {
	case msg.Result != nil:
		caps = msg.Result.Capabilities
	case msg.Params != nil:
		caps = msg.Params.Capabilities
	default:
		return nil, fmt.Errorf("neither params nor result")
	}
	out := map[string]bool{}
	for name := range caps {
		out[name] = true
	}
	return out, nil
}

// serverCapability is the server capability a client's request needs, or ""
// for one that needs none (ping, initialize). The schema's ServerCapabilities,
// read from the request side.
func serverCapability(method string) string {
	switch {
	case strings.HasPrefix(method, "tools/"):
		return "tools"
	case strings.HasPrefix(method, "resources/"):
		return "resources"
	case strings.HasPrefix(method, "prompts/"):
		return "prompts"
	case method == "logging/setLevel":
		return "logging"
	case method == "completion/complete":
		return "completions"
	case strings.HasPrefix(method, "tasks/"):
		return "tasks"
	}
	return ""
}

// clientCapability is the client capability a server's request needs: the
// schema's ClientCapabilities, read the same way.
func clientCapability(method string) string {
	switch {
	case method == "sampling/createMessage":
		return "sampling"
	case method == "roots/list":
		return "roots"
	case strings.HasPrefix(method, "elicitation/"):
		return "elicitation"
	case strings.HasPrefix(method, "tasks/"):
		return "tasks"
	}
	return ""
}

func finding(applied *transcript.EventLine) oracle.Finding {
	return oracle.Finding{
		Verdict:  oracle.Observed,
		Layer:    Layer,
		Check:    CheckExpectation,
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
