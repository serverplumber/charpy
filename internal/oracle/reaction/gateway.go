package reaction

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/transcript"
)

// A gateway is judged by what it tells its client after a fault, on either
// face (ADR-015). The fault may land upstream, but the questions are put
// downstream and answered there, by id on one face -- a fact, so judging the
// reaction needs no join across the gateway.
//
// Each question's role is read off what it is, not recorded beside it:
//
//   - ping asks whether the gateway itself is alive;
//   - a call to a tool under the prefix of the upstream the fault reached
//     asks whether that path still works;
//   - a call to any other upstream's tool asks whether the damage stayed
//     where it was put.
//
// Before the questions there is the call the fault was carrying: the request
// of charpy's that the faulted upstream exchange was serving, asked before
// the fault and so none of the three. It is the one place a gateway's
// handling of the fault itself shows -- a deadline that turned a hung
// upstream into an error, or none -- and it is found by the join the
// fault_applied event records. Only a traced join is a fact; an inferred one
// is said, and judged no further.
//
// A question is only one asked on a downstream connection open by the time
// of the fault. A connection opened after it is charpy's liveness probe,
// whose ping is the recovery check's, not the reaction's; counting it here
// once let a probe stand in for a ping the questions never sent. And a role
// with no question asked is said, not left out: a report missing a call
// reads the same as a gateway nobody needed to ask.
//
// The upstream a fault reached is the prefix of the connection it applied on
// (u0-, u1-), and a tool's upstream is its own prefix (u0_, u1_). Both lean
// on the per-upstream prefixes charpy gives its reference servers, which is
// an open problem: replacing those has to replace this too.

// question is one of charpy's requests to the gateway after the fault.
type question struct {
	frame    *transcript.FrameLine
	upstream string // the tool's upstream, "" for a ping
}

// reactGateway reports, for one fault that reached a gateway, how it
// answered each question charpy asked downstream afterwards: first ping,
// first call per upstream.
func reactGateway(rep *oracle.Report, t *transcript.Transcript, applied *transcript.EventLine, base oracle.Finding) {
	faulted := ""
	if applied.Face != nil && *applied.Face == transcript.Upstream && applied.ConnID != nil {
		if pre, _, ok := strings.Cut(*applied.ConnID, "-"); ok {
			faulted = pre
		}
	}

	var (
		asked   []*question
		seen    = map[string]bool{}
		byKey   = map[string]*question{}
		answers = map[*question]*transcript.FrameLine{}
		exited  *transcript.EventLine
		notes   = map[string]string{}
		last    = applied.TMonoNS
		open    = map[string]bool{}
		ups     = map[string]bool{}
	)
	for i := range t.Entries {
		en := &t.Entries[i]
		face, id := entryConn(en)
		if face == transcript.Upstream {
			if pre, _, ok := strings.Cut(id, "-"); ok {
				ups[pre] = true
			}
		}
		if en.Seq <= applied.Seq {
			if face == transcript.Downstream && id != "" {
				open[id] = true
			}
			continue
		}
		if e := en.Event; e != nil && e.EventKind == transcript.Note && face == transcript.Downstream {
			// A question that failed in charpy's client (transcript.QuestionDetail).
			name, _ := e.Detail["question"].(string)
			why, _ := e.Detail["error"].(string)
			if name != "" && why != "" {
				notes[name] = why
			}
		}
		last = en.TMonoNS
		if e := en.Event; e != nil && e.EventKind == transcript.SubjectExit {
			if killed, _ := e.Detail["killed_by_charpy"].(bool); !killed && exited == nil {
				exited = e
			}
			continue
		}
		fr := en.Frame
		if fr == nil || fr.Face != transcript.Downstream || fr.Tampered() || !open[conn(fr)] {
			continue
		}
		// Ids are scoped to a connection.
		if fr.Key() == "" {
			continue
		}
		key := conn(fr) + "/" + fr.Key()
		switch {
		case fr.Direction == transcript.C2S && fr.Kind == envelope.KindRequest:
			q := &question{frame: fr}
			role := "ping"
			switch fr.MethodName() {
			case "ping":
			case "tools/call":
				q.upstream = toolUpstream(fr)
				role = "call:" + q.upstream
			default:
				continue
			}
			if seen[role] {
				continue
			}
			seen[role] = true
			asked = append(asked, q)
			byKey[key] = q
		case fr.Direction == transcript.S2C && (fr.Kind == envelope.KindResponse || fr.Kind == envelope.KindError):
			if q, ok := byKey[key]; ok && answers[q] == nil {
				answers[q] = fr
			}
		}
	}

	if exited != nil {
		f := base
		f.Verdict, f.Seq = oracle.Observed, exited.Seq
		f.Summary = "the gateway exited after the fault"
		f.Detail = fmt.Sprintf("%s, %s after the fault", exitText(exited.Detail), ms(exited.TMonoNS-applied.TMonoNS))
		rep.Add(f)
	}
	carried(rep, t, applied, base, faulted)

	for _, q := range asked {
		f := base
		f.Verdict = oracle.Observed
		what := describe(q, faulted)
		a := answers[q]
		switch {
		case a == nil:
			f.Seq = q.frame.Seq
			f.Summary = "the gateway did not answer " + what + " after the fault"
			f.Detail = fmt.Sprintf("asked at seq %d; still unanswered when the run ended %s later",
				q.frame.Seq, ms(last-q.frame.TMonoNS))
		case a.Kind == envelope.KindError:
			f.Seq = a.Seq
			f.Summary = "the gateway answered " + what + " with an error after the fault"
			f.Detail = fmt.Sprintf("seq %d answered at seq %d, %s after the fault: %s",
				q.frame.Seq, a.Seq, ms(a.TMonoNS-applied.TMonoNS), errorText(a))
		default:
			f.Seq = a.Seq
			f.Summary = "the gateway answered " + what + " after the fault"
			f.Detail = fmt.Sprintf("seq %d answered at seq %d, %s after the fault",
				q.frame.Seq, a.Seq, ms(a.TMonoNS-applied.TMonoNS))
		}
		rep.Add(f)
	}

	for _, m := range unasked(asked, faulted, ups) {
		f := base
		f.Verdict = oracle.Inconclusive
		f.Summary = "charpy did not ask the gateway " + m.what + " after the fault"
		f.Detail = "no such request crossed downstream on a connection open at the fault"
		if why, ok := notes[m.name]; ok {
			f.Detail += "; charpy's client: " + why
		}
		f.Reason = "question-not-asked"
		rep.Add(f)
	}
}

// missing is a question not asked: what to call it in a finding, and the
// name the driver notes it under, "" when it is not one upstream in
// particular.
type missing struct{ what, name string }

// unasked names each question the driver puts to a gateway (ADR-015) that
// was not asked: a ping; after an upstream fault, a call through the
// upstream it reached and one through another, if there is another; after a
// downstream fault, calls through two upstreams, or the one there is. ups is
// every upstream the transcript shows, by connection prefix; it is not
// changed.
func unasked(asked []*question, faulted string, ups map[string]bool) []missing {
	ups = maps.Clone(ups)
	pinged, through := false, map[string]bool{}
	for _, q := range asked {
		switch {
		case q.upstream != "":
			through[q.upstream] = true
			ups[q.upstream] = true
		case q.frame.MethodName() == "ping":
			pinged = true
		}
	}
	if faulted != "" {
		ups[faulted] = true
	}
	names := slices.Sorted(maps.Keys(ups))

	var out []missing
	if !pinged {
		out = append(out, missing{"a ping", "ping"})
	}
	if faulted != "" {
		if !through[faulted] {
			out = append(out, missing{"a call through " + faulted + " (the upstream the fault reached)", "call through " + faulted})
		}
		others := slices.DeleteFunc(names, func(u string) bool { return u == faulted })
		if len(others) > 0 && !slices.ContainsFunc(others, func(u string) bool { return through[u] }) {
			m := missing{what: "a call through an upstream the fault did not reach"}
			if len(others) == 1 {
				m = missing{"a call through " + others[0] + " (an upstream the fault did not reach)", "call through " + others[0]}
			}
			out = append(out, m)
		}
		return out
	}
	need := min(2, len(names)) - len(through)
	for _, u := range names {
		if need <= 0 {
			break
		}
		if !through[u] {
			out = append(out, missing{"a call through " + u, "call through " + u})
			need--
		}
	}
	return out
}

// entryConn is the face and connection an entry is on, frame or event.
func entryConn(en *transcript.Entry) (transcript.Face, string) {
	switch {
	case en.Frame != nil:
		return en.Frame.Face, conn(en.Frame)
	case en.Event != nil && en.Event.Face != nil && en.Event.ConnID != nil:
		return *en.Event.Face, *en.Event.ConnID
	}
	return "", ""
}

// carried reports what the gateway told charpy's client about the call an
// upstream fault was carrying, if there was one.
func carried(rep *oracle.Report, t *transcript.Transcript, applied *transcript.EventLine, base oracle.Finding, faulted string) {
	if applied.Face == nil || *applied.Face != transcript.Upstream ||
		applied.Link == nil || applied.Link.CharpyID == nil {
		// Downstream, the faulted frame is charpy's call itself; upstream with
		// no join, the exchange served none of charpy's calls.
		return
	}
	what := "the call the fault was carrying"
	if faulted != "" {
		what += " (through " + faulted + ")"
	}
	f := base
	if applied.Link.Via != transcript.ViaTraced {
		f.Verdict = oracle.Inconclusive
		f.Summary = what + " is joined to it only by content"
		f.Detail = "the gateway did not forward charpy's trace, and no verdict rests on an inferred join"
		f.Reason = "carried-call-join-inferred"
		rep.Add(f)
		return
	}
	id := *applied.Link.CharpyID

	var (
		call, answer, cancel *transcript.FrameLine
		last                 = applied.TMonoNS
	)
	for _, fr := range t.Frames() {
		last = fr.TMonoNS
		if fr.Face != transcript.Downstream {
			continue
		}
		switch {
		case call == nil:
			if fr.Direction == transcript.C2S && fr.Kind == envelope.KindRequest &&
				fr.Link.Via == transcript.ViaTraced && fr.Link.CharpyID != nil && *fr.Link.CharpyID == id {
				call = fr
			}
		case conn(fr) != conn(call):
		case fr.Direction == transcript.S2C && fr.Key() == call.Key() && answer == nil &&
			(fr.Kind == envelope.KindResponse || fr.Kind == envelope.KindError):
			answer = fr
		case fr.Direction == transcript.C2S && cancel == nil && cancels(fr, call):
			cancel = fr
		}
	}
	if call == nil {
		return
	}

	f.Verdict = oracle.Observed
	switch {
	case answer == nil:
		f.Seq = call.Seq
		f.Summary = "the gateway did not answer " + what
		if cancel != nil {
			f.Detail = fmt.Sprintf("asked at seq %d; charpy's client gave up and cancelled it at seq %d, %s after the fault",
				call.Seq, cancel.Seq, ms(cancel.TMonoNS-applied.TMonoNS))
		} else {
			f.Detail = fmt.Sprintf("asked at seq %d; still unanswered when the run ended %s after the fault",
				call.Seq, ms(last-applied.TMonoNS))
		}
	case answer.Kind == envelope.KindError:
		f.Seq = answer.Seq
		f.Summary = "the gateway answered " + what + " with an error"
		f.Detail = fmt.Sprintf("seq %d answered at seq %d, %s after the fault: %s",
			call.Seq, answer.Seq, ms(answer.TMonoNS-applied.TMonoNS), errorText(answer))
	default:
		f.Seq = answer.Seq
		f.Summary = "the gateway answered " + what
		f.Detail = fmt.Sprintf("seq %d answered at seq %d, %s after the fault",
			call.Seq, answer.Seq, ms(answer.TMonoNS-applied.TMonoNS))
	}
	rep.Add(f)
}

// cancels reports whether fr is a notifications/cancelled naming call.
func cancels(fr, call *transcript.FrameLine) bool {
	if fr.Kind != envelope.KindNotification || fr.MethodName() != "notifications/cancelled" {
		return false
	}
	raw, err := fr.Bytes()
	if err != nil {
		return false
	}
	var m struct {
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Params.RequestID == nil {
		return false
	}
	want, ok := call.IDText()
	if !ok {
		return false
	}
	// The id column is unquoted; its type is what tells 7 from "7".
	var text string
	isString := json.Unmarshal(m.Params.RequestID, &text) == nil
	if !isString {
		text = string(m.Params.RequestID)
	}
	return text == want && isString == (call.IDType == envelope.IDString)
}

func conn(fr *transcript.FrameLine) string {
	if fr.ConnID == nil {
		return ""
	}
	return *fr.ConnID
}

// describe names a question for a finding's summary.
func describe(q *question, faulted string) string {
	switch {
	case q.upstream == "" && q.frame.MethodName() == "ping":
		return "a ping"
	case q.upstream == "":
		return "a call through no known upstream"
	case faulted == "":
		return "a call through " + q.upstream
	case q.upstream == faulted:
		return "a call through " + q.upstream + " (the upstream the fault reached)"
	default:
		return "a call through " + q.upstream + " (an upstream the fault did not reach)"
	}
}

// toolUpstream is the upstream a tools/call names by its tool's prefix (u0_,
// u1_), or "" for a tool with none.
func toolUpstream(fr *transcript.FrameLine) string {
	raw, err := fr.Bytes()
	if err != nil {
		return ""
	}
	var m struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	pre, _, ok := strings.Cut(m.Params.Name, "_")
	if !ok || len(pre) < 2 || pre[0] != 'u' || strings.Trim(pre[1:], "0123456789") != "" {
		return ""
	}
	return pre
}

// errorText is an error answer's code and message.
func errorText(fr *transcript.FrameLine) string {
	raw, err := fr.Bytes()
	if err != nil {
		return "an error"
	}
	var m struct {
		Error struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "an error"
	}
	return fmt.Sprintf("%d %s", m.Error.Code, m.Error.Message)
}
