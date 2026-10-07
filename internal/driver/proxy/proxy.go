package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// Options configures a proxy.
type Options struct {
	// SubjectURL is the real server charpy forwards to.
	SubjectURL string
	// HTTPClient forwards to the subject. Nil takes a client with no timeout,
	// because an SSE response is long-lived and a deadline would cut streams
	// that were not the fault.
	HTTPClient *http.Client

	Cases      []interpose.Case
	Transcript *transcript.Writer
	Sched      clock.Sched
	Ledger     *interpose.Ledger

	// Wall is real time, for keep-alive comments on a stalled stream: they
	// hold off the subject's timer, which is real (ADR-001). Required.
	Wall clock.Wall

	// Face is the subject's face. A server subject is faced downstream.
	Face transcript.Face
	// Correlate is the link regime for recorded frames. Nil defaults to a
	// fresh Forwarded id -- charpy forwards the bytes, so it stamps its own.
	// The hostile HTTP mode passes ViaNone: it forwards to its own reference
	// server, and the one face under test is the client, with nothing to join.
	// It is given the frame being recorded.
	Correlate func(envelope.Message) transcript.Link

	// Applied is called once a fault has acted on a response, with the
	// session the response belongs to. The hostile HTTP mode uses it to have
	// its reference server ask the client under test a question afterwards;
	// nil asks nothing, which is what owned stimulus wants -- its own peer
	// asks, through [Proxy.Ask], once its script is done.
	Applied func(sessionID string)

	// OutputSchemas is the outputSchema each tool declares, by name, where
	// charpy knows the declaration because it is serving: the hostile HTTP
	// mode's own reference server. Nil in front of a real server, where a
	// schema_violation against the declared output does not apply until
	// charpy reads the declaration itself.
	OutputSchemas map[string]json.RawMessage

	// AnswerDestroyed has charpy answer its own peer when a fault leaves the
	// peer's request without the id it carried. The subject's answer to such
	// a request is still recorded -- it is the reaction being judged -- but
	// it is not relayed: the subject was answering bytes the peer never sent,
	// and an HTTP error on a call makes the SDK client close its whole
	// session, which leaves nothing to ask the question after the fault on.
	// Owned stimulus only, as over stdio.
	AnswerDestroyed bool

	// Run joins the proxy to a run other proxies share: a subject with more
	// than one face to it -- a gateway -- has one run, one ledger and one
	// header across all of them. Nil gives the proxy a run of its own over
	// Ledger, Transcript and Cases. When set, the run's ledger and writer are
	// the proxy's, and Ledger and Transcript must be nil or the same ones.
	Run *exchange.Run
	// Join correlates the proxy's frames with the other face of a gateway,
	// through the shared Run's ledger, in place of Correlate.
	Join bool
	// ConnPrefix is prepended to the connection ids the proxy names. Ids are
	// unique within one proxy; a run with two proxies on one face -- a
	// gateway's two upstreams -- needs them unique across both, or the
	// ledger takes one upstream's request 1 for the other's.
	ConnPrefix string

	RunSeed   string
	ClientID  string
	SessionID string
	ConnID    string
}

// Proxy is an http.Handler that forwards to a subject and faults the traffic.
//
// One instance serves a whole session, so its ledger counts occurrences across
// requests: "the second tools/call" spans two POSTs. The scenario player makes
// its calls one at a time, so requests arrive serially; the shared state is
// not built for concurrent in-flight requests, which v0 does not produce.
type Proxy struct {
	o      Options
	x      *exchange.Face
	inter  *interpose.Interposer
	match  *interpose.Matcher
	client *http.Client

	// nextConn names the connection of a request that carries no session id
	// yet -- the initialize that establishes one. Each such handshake is its
	// own connection.
	nextConn atomic.Int64

	// sessions maps a session id to the connection its handshake opened on.
	// The initialize that establishes a session carries no id, so it is named
	// before the session exists; without this, the handshake and everything
	// after it would be two connections, and a fault on the handshake's
	// answer could never be followed by a question on the same one.
	sessions sync.Map // session id -> connection id

	// faulting is held while a fault is applied, up to its fault_applied
	// line, and applied counts those lines. A faulted event reaches the
	// client before its fault_applied is written, so without the lock a
	// follow-up asked the moment the client's call returned could cross
	// ahead of the fault it follows (see [Proxy.Askable]).
	faulting sync.Mutex
	applied  int
}

// New prepares a proxy. It serves once Handler is mounted and a subject is up.
func New(o Options) (*Proxy, error) {
	if o.SubjectURL == "" {
		return nil, errors.New("proxy: no subject URL")
	}
	if o.Wall == nil {
		return nil, errors.New("proxy: Options.Wall is required; a run declares its clock")
	}
	if o.Face == "" {
		o.Face = transcript.Downstream
	}
	if o.ConnID == "" {
		o.ConnID = o.ConnPrefix + "c-0"
	}
	if o.ClientID == "" {
		o.ClientID = "c0"
	}
	if o.SessionID == "" {
		o.SessionID = "s-0"
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	correlate := o.Correlate
	if correlate == nil {
		// charpy forwards the bytes itself, so it stamps one authoritative id
		// on both copies -- not the ViaNone the single-faced shim records.
		correlate = func(envelope.Message) transcript.Link { return interpose.Forwarded(interpose.NewJoinID()) }
	}
	run := o.Run
	if run == nil {
		run = &exchange.Run{Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases}
	} else {
		switch {
		case o.Ledger == nil:
			o.Ledger = run.Ledger
		case o.Ledger != run.Ledger:
			return nil, errors.New("proxy: Ledger differs from the shared run's")
		}
		switch {
		case o.Transcript == nil:
			o.Transcript = run.Transcript
		case o.Transcript != run.Transcript:
			return nil, errors.New("proxy: Transcript differs from the shared run's")
		}
	}
	return &Proxy{
		o:      o,
		x:      face(run, o, correlate),
		inter:  interpose.New(o.Ledger, o.Sched),
		match:  interpose.NewMatcher(o.Ledger, o.Cases...),
		client: client,
	}, nil
}

func face(run *exchange.Run, o Options, correlate func(envelope.Message) transcript.Link) *exchange.Face {
	if o.Join {
		return run.JoinedFace(o.Face, transcript.TransportHTTP)
	}
	return run.Face(o.Face, transcript.TransportHTTP, correlate)
}

// RecordProbe records a liveness probe's outcome. The probe itself is an
// ordinary request the client makes through the proxy; this is the summary the
// oracle's liveness layer reads.
func (p *Proxy) RecordProbe(method string, outcome transcript.ProbeOutcome, elapsedNS int64) {
	p.x.Conn(p.o.ClientID, p.o.SessionID, p.o.ConnID).Probe(method, outcome, elapsedNS)
}

// Note records a harness annotation on sessionID's connection, for a caller
// acting on the proxy's traffic from outside a handler.
func (p *Proxy) Note(sessionID, text string) {
	p.connFor(sessionID).Note(text)
}

// QuestionFailed notes on sessionID's connection that a follow-up question
// failed in charpy's own client (exchange.Conn.QuestionFailed).
func (p *Proxy) QuestionFailed(sessionID, question string, err error) {
	p.connFor(sessionID).QuestionFailed(question, err)
}

// Askable reports whether a fault has acted in this run, so that a follow-up
// question has something to follow. It waits out a fault still being applied.
//
// A then = "close" does not make the session unaskable here, as it does over
// stdio. On HTTP it ends one response stream; the session, its id and the
// client's other streams survive it, so a question on the same session is
// still a question the subject can answer.
func (p *Proxy) Askable() bool {
	p.faulting.Lock()
	defer p.faulting.Unlock()
	return p.applied > 0
}

// Ask marks the next request crossing sessionID's connection in dir as
// charpy's follow-up question, keeping it and its answer out of the armed
// case's reach (exchange.Conn.Ask). The returned func withdraws the mark if
// the question never crossed.
func (p *Proxy) Ask(sessionID string, dir transcript.Direction) (withdraw func()) {
	return p.connFor(sessionID).Ask(dir)
}

// faultContext is what a mechanism is told about the frame it acts on.
func (p *Proxy) faultContext(conn *exchange.Conn, m envelope.Message, dir transcript.Direction, prior envelope.ID) fault.Context {
	ctx := fault.Context{RunSeed: p.o.RunSeed, Resolved: prior}
	if tool := conn.ToolFor(m, dir); tool != "" {
		ctx.OutputSchema = p.o.OutputSchemas[tool]
	}
	return ctx
}

// faultApplied records that a fault acted on a frame travelling dir, and hands
// the session to the Applied hook. Callers hold p.faulting.
func (p *Proxy) faultApplied(conn *exchange.Conn, c interpose.Case, verb interpose.Verb, m envelope.Message, dir transcript.Direction, resp *http.Response) {
	conn.Applied(c, string(verb), m, dir)
	p.applied++
	if p.o.Applied == nil || resp == nil {
		return
	}
	// The initialize that establishes a session carries no id on its request,
	// so the session it belongs to is named by the response.
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		sid = resp.Request.Header.Get("Mcp-Session-Id")
	}
	if sid != "" {
		p.o.Applied(sid)
	}
}

// ServeHTTP forwards one request to the subject and relays the response,
// faulting whichever direction a case matched.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "charpy: reading request", http.StatusBadGateway)
		return
	}
	_ = r.Body.Close()

	conn := p.conn(r)

	// The client's frame, toward the subject. A case may fault it here, as the
	// shim's may: a server subject receives c2s, so this is where its
	// questions are put (ADR-013). A follow-up question crossing here is kept
	// out of the matcher's reach, and is how its answer is known on the way
	// back.
	if req, ok := envelope.Parse(body); ok == nil {
		conn.Observe(req, transcript.C2S)
		if !conn.FollowUp(req, transcript.C2S) {
			if cases := p.match.Select(conn.FrameOf(req, transcript.C2S)); len(cases) > 0 {
				p.serveFaulted(w, r, cases[0], req, body, conn)
				return
			}
		}
		conn.Frame(transcript.C2S, req.Raw(), nil, requestHTTP(r))
	}

	resp, err := p.forward(r, body)
	if err != nil {
		conn.Note(fmt.Sprintf("forwarding to subject: %v", err))
		http.Error(w, "charpy: subject unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	p.relay(w, resp, conn)
}

// serveFaulted forwards a request a case matched, with the case's fault on it,
// and relays the subject's answer.
//
// The verbs map onto a request as they do onto a stdio line, with two
// differences the transport forces. A frame charpy synthesizes beside the
// request -- a duplicate, an answer to something never asked -- goes to the
// subject as a POST of its own, since HTTP carries one message per request;
// what the subject answers to it is recorded, and not relayed, because the
// client never sent it. And withholding the request is not deliverable: the
// client's POST is owed an HTTP answer, and a request the subject never sees
// leaves charpy nothing to answer it with. That is noted rather than faked.
//
// fault_applied is written once everything the fault puts on the wire has
// crossed -- the request, and any frames sent beside it -- and before the
// subject's answer is relayed, so the reaction layer's anchor precedes the
// reaction.
func (p *Proxy) serveFaulted(w http.ResponseWriter, r *http.Request, c interpose.Case, m envelope.Message, body []byte, conn *exchange.Conn) {
	p.faulting.Lock()
	held := true
	release := func() {
		if held {
			held = false
			p.faulting.Unlock()
		}
	}
	defer release()

	conn.FaultEvent(transcript.FaultScheduled, c, nil)
	prior, _ := conn.PriorResolved(transcript.C2S)
	plan, err := fault.Apply(c, m, p.faultContext(conn, m, transcript.C2S, prior))
	switch {
	case err != nil:
		conn.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		plan = fault.Plan{}
	case plan.Hold != nil || plan.Swallow:
		conn.Note(fmt.Sprintf("case %s: %s is not deliverable on an HTTP request; "+
			"the client's POST is owed an answer, and a request the subject never sees leaves none",
			c.ID, plan.Verb()))
		plan = fault.Plan{}
	}
	if plan.Deliver == nil && len(plan.Before) == 0 && len(plan.After) == 0 {
		conn.Frame(transcript.C2S, m.Raw(), nil, requestHTTP(r))
		release()
		p.forwardAndRelay(w, r, body, conn)
		return
	}

	f := conn.FrameOf(m, transcript.C2S)
	att := c.TranscriptFault()
	for _, extra := range plan.Before {
		p.inter.Synthesize(f, c, extra)
		p.aside(r, extra.Raw(), att, conn)
	}

	send := body
	var sentAtt *transcript.Fault
	if plan.Deliver != nil {
		p.inter.Rewrite(f, c, m, *plan.Deliver)
		raw, whole := plan.Deliver.Raw(), true
		send = raw
		if plan.Cut != nil {
			// A request body has no delimiter to withhold, so a cut is the
			// body ending early -- sent with a length that says so, which is
			// what a client that died mid-write leaves a server holding.
			if at, err := wire.EncodeLine(raw).Cut(plan.Cut.At, plan.Cut.Opts); err != nil {
				conn.Note(fmt.Sprintf("cut %s did not apply: %v", plan.Cut.At, err))
			} else if at < len(raw) {
				send, whole = raw[:at], false
			}
		}
		sentAtt = interpose.Crossed(c.Rewrote(m, *plan.Deliver), m, *plan.Deliver, whole)
	}
	crossed, _ := envelope.Parse(send)
	conn.Frame(transcript.C2S, crossed.Raw(), sentAtt, requestHTTP(r))

	resp, err := p.forward(r, send)
	if err != nil {
		p.faultApplied(conn, c, plan.Verb(), m, transcript.C2S, nil)
		release()
		conn.Note(fmt.Sprintf("forwarding to subject: %v", err))
		http.Error(w, "charpy: subject unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// After the request, and while its answer is still in flight: a duplicate
	// sent here is a second request with the same id outstanding at once.
	for _, extra := range plan.After {
		p.inter.Synthesize(f, c, extra)
		p.aside(r, extra.Raw(), att, conn)
	}
	p.faultApplied(conn, c, plan.Verb(), m, transcript.C2S, resp)
	release()

	if p.o.AnswerDestroyed && m.Kind == envelope.KindRequest && sentAtt != nil &&
		sentAtt.Replaced.Present() && sentAtt.ReplacedKind == envelope.KindRequest {
		p.relay(&discard{h: http.Header{}}, resp, conn)
		p.answerDestroyed(w, r, m, sentAtt, conn)
		return
	}
	p.relay(w, resp, conn)
}

// answerDestroyed answers the peer's request itself, as the stdio shim does:
// charpy's frame, attributed to the case, so no layer reads it as the
// subject's -- the subject never received the request it answers.
func (p *Proxy) answerDestroyed(w http.ResponseWriter, r *http.Request, req envelope.Message, att *transcript.Fault, conn *exchange.Conn) {
	ans, err := envelope.NewError(req.ID, destroyedCode,
		"charpy destroyed this request in transit ("+att.Citation+"); the subject never received it", nil)
	if err != nil {
		conn.Note(fmt.Sprintf("answering a destroyed request: %v", err))
		http.Error(w, "charpy: answering a destroyed request", http.StatusBadGateway)
		return
	}
	conn.Observe(ans, transcript.S2C)
	status := http.StatusOK
	// charpy wrote this frame in its own right, so it names nothing it replaced.
	own := *att
	own.Replaced, own.ReplacedKind = envelope.ID{}, ""
	conn.Frame(transcript.S2C, ans.Raw(), &own, &transcript.HTTP{Status: status})
	w.Header().Set("Content-Type", "application/json")
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	w.WriteHeader(status)
	if _, err := w.Write(ans.Raw()); err != nil {
		conn.Note(fmt.Sprintf("writing to client: %v", err))
	}
}

// destroyedCode is the JSON-RPC error charpy answers its own peer with for a
// request it destroyed: implementation-defined server-error range, and not one
// MCP assigns. The stdio shim uses the same.
const destroyedCode = -32099

// forwardAndRelay forwards a request unchanged and relays the answer.
func (p *Proxy) forwardAndRelay(w http.ResponseWriter, r *http.Request, body []byte, conn *exchange.Conn) {
	resp, err := p.forward(r, body)
	if err != nil {
		conn.Note(fmt.Sprintf("forwarding to subject: %v", err))
		http.Error(w, "charpy: subject unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.relay(w, resp, conn)
}

// aside sends a frame charpy synthesized to the subject as a request of its
// own, carrying the client's headers so it lands in the client's session. The
// subject's answer is recorded as ever and relayed to nobody: the client never
// sent this frame, and an answer it never asked for is not charpy's to give it.
//
// It runs under p.faulting, and relaying the answer offers it to the matcher.
// That cannot fault it, and so cannot take the lock again: the armed case is
// c2s -- that is why this runs at all -- and the loader requires every case to
// state its direction, so it never matches an s2c answer.
func (p *Proxy) aside(r *http.Request, raw []byte, att *transcript.Fault, conn *exchange.Conn) {
	m, _ := envelope.Parse(raw)
	conn.Observe(m, transcript.C2S)
	conn.Frame(transcript.C2S, m.Raw(), att, requestHTTP(r))
	resp, err := p.forward(r, raw)
	if err != nil {
		conn.Note(fmt.Sprintf("sending a synthesized frame to the subject: %v", err))
		return
	}
	defer resp.Body.Close()
	p.relay(&discard{h: http.Header{}}, resp, conn)
}

// discard is a response writer that keeps nothing, for an answer the client
// must not see. It flushes, because the event-stream path flushes each unit.
type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}

// conn identifies the connection a request belongs to. A reconnecting client
// is more than one connection, and the ledger must see that: an id resolved on
// one is not the same id resolved on another, so keying them together would
// invent a duplicate. The initialize that establishes a session carries no
// session id, so it gets a fresh connection id of its own -- each handshake is
// a distinct connection's opening -- and every later request carrying the
// session id that handshake established is on that same connection.
func (p *Proxy) conn(r *http.Request) *exchange.Conn {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		id := p.o.ConnPrefix + "c-" + strconv.FormatInt(p.nextConn.Add(1), 10)
		return p.x.Conn(p.o.ClientID, id, id)
	}
	return p.connFor(sid)
}

// connFor is the connection a session lives on: the one its handshake opened,
// or, for a session charpy never saw established, one named after it.
func (p *Proxy) connFor(sid string) *exchange.Conn {
	id := sid
	if v, ok := p.sessions.Load(sid); ok {
		id = v.(string)
	}
	return p.x.Conn(p.o.ClientID, sid, id)
}

// forward sends the client's request on to the subject, headers and all, so
// session identity and the protocol-version header pass through untouched --
// charpy is not rewriting ids on this path, so a transparent copy is correct.
func (p *Proxy) forward(r *http.Request, body []byte) (*http.Response, error) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.o.SubjectURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if dropOnForward(k) {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	return p.client.Do(out)
}

// relay copies the subject's response back to the client, faulting it on the
// way. The two content types are different shapes: an event stream charpy
// scans unit by unit, or a single JSON body.
func (p *Proxy) relay(w http.ResponseWriter, resp *http.Response, conn *exchange.Conn) {
	copyHeaders(w.Header(), resp.Header)

	// A handshake's answer names the session it established: bind it to the
	// connection the handshake was on, before anything else of the session's
	// can cross.
	if resp.Request.Header.Get("Mcp-Session-Id") == "" {
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			p.sessions.LoadOrStore(sid, conn.ConnID())
		}
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.relaySSE(w, resp, conn)
		return
	}
	p.relayJSON(w, resp, conn)
}

// relayJSON handles a single-message response. Only a whole-frame fault can
// apply here: there is no event to cut, and no second frame to hold a Before
// or After, so those are recorded as not applied rather than forced into a
// shape the transport does not have.
func (p *Proxy) relayJSON(w http.ResponseWriter, resp *http.Response, conn *exchange.Conn) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		conn.Note(fmt.Sprintf("reading subject response: %v", err))
	}
	m, _ := envelope.Parse(body)
	prior, _ := conn.PriorResolved(transcript.S2C)
	conn.Observe(m, transcript.S2C)

	att, out, applied := p.planJSON(m, prior, conn, resp, len(bytes.TrimSpace(body)) == 0)
	deliver := out.Raw()
	if !applied {
		deliver = body
	}

	// Every frame toward the client is recorded before it is written, as the
	// shim's are: the client may answer as soon as the bytes land, on another
	// request, and its answer must not take the earlier seq.
	crossed, _ := envelope.Parse(deliver)
	conn.Frame(transcript.S2C, crossed.Raw(), att, responseHTTP(resp))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(deliver); err != nil {
		conn.Note(fmt.Sprintf("writing to client: %v", err))
	}
}

// planJSON runs a matched case's plan against a JSON response, returning the
// message to deliver. Cut, hold, swallow and multi-frame verbs do not apply to
// a single JSON body; each is noted rather than silently dropped.
func (p *Proxy) planJSON(m envelope.Message, prior envelope.ID, conn *exchange.Conn, resp *http.Response, empty bool) (*transcript.Fault, envelope.Message, bool) {
	// An accepted notification or response comes back 202 with no body, and
	// no message crossed for a case to attach to. Offering the matcher an
	// empty frame would let a case with no method fault the acknowledgement
	// of a client's answer, as though the server had said something.
	if empty || conn.FollowUp(m, transcript.S2C) {
		return nil, m, false
	}
	cases := p.match.Select(conn.FrameOf(m, transcript.S2C))
	if len(cases) == 0 {
		return nil, m, false
	}
	c := cases[0]
	p.faulting.Lock()
	defer p.faulting.Unlock()
	conn.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, p.faultContext(conn, m, transcript.S2C, prior))
	if err != nil {
		conn.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		return nil, m, false
	}
	if plan.Cut != nil || plan.Hold != nil || plan.Swallow || len(plan.Before) > 0 || len(plan.After) > 0 {
		conn.Note(fmt.Sprintf("case %s: %s is not deliverable on a single JSON response; "+
			"the subject answered application/json, not an event stream", c.ID, plan.Verb()))
	}
	if plan.Deliver == nil {
		return nil, m, false
	}
	att := interpose.Crossed(c.Rewrote(m, *plan.Deliver), m, *plan.Deliver, true)
	p.inter.Rewrite(conn.FrameOf(m, transcript.S2C), c, m, *plan.Deliver)
	p.faultApplied(conn, c, plan.Verb(), m, transcript.S2C, resp)
	return att, *plan.Deliver, true
}

// relaySSE scans the subject's event stream and relays it unit by unit,
// faulting whichever unit a case matches. A stream is where the truncate and
// multi-frame verbs live, so this is the path the HTTP catalogue exercises.
func (p *Proxy) relaySSE(w http.ResponseWriter, resp *http.Response, conn *exchange.Conn) {
	sse := wire.NewSSE(w)
	sc := wire.NewSSEScanner(resp.Body)

	// A held event leaves the stream open past the subject's own end of it:
	// the client is waiting on an answer that has not come, and closing the
	// stream would tell it the answer is not coming -- an error, not a hang.
	var held *heldSSE
	defer func() {
		if held == nil {
			return
		}
		select {
		case <-held.w.Withdrawn():
		case <-resp.Request.Context().Done():
		}
	}()

	for sc.Scan() {
		unit := sc.Unit()

		if unit.IsComment() {
			// A keep-alive carries no frame to match. Relay it and record it,
			// because a stall that kept sending comments is a distinct
			// failure mode and the transcript has to show it happened.
			if _, err := wire.Emit(sse, unit); err != nil {
				conn.Note(fmt.Sprintf("relaying comment: %v", err))
				return
			}
			continue
		}

		m, _ := envelope.Parse(unit.Body())
		prior, _ := conn.PriorResolved(transcript.S2C)
		conn.Observe(m, transcript.S2C)

		var cases []interpose.Case
		if !conn.FollowUp(m, transcript.S2C) {
			cases = p.match.Select(conn.FrameOf(m, transcript.S2C))
		}
		if len(cases) == 0 {
			conn.Frame(transcript.S2C, unit.Body(), nil, sseHTTP(resp, unit))
			if _, err := wire.Emit(sse, unit); err != nil {
				conn.Note(fmt.Sprintf("relaying event: %v", err))
				return
			}
			continue
		}

		end, h := p.faultSSE(sse, cases[0], m, unit, resp, prior, conn)
		if end {
			// then close, or a stall the client has walked away from:
			// nothing more crosses on this stream.
			return
		}
		if h != nil {
			held = h
			// Held at a wider scope than the one response, nothing more
			// crosses on this stream.
			if h.scope != "" && h.scope != "response" {
				return
			}
		}
	}
	if err := sc.Err(); err != nil {
		conn.Note(fmt.Sprintf("scanning subject stream: %v", err))
	}
}

// heldSSE is an event a hold withheld, and how much of its stream the hold
// covers.
type heldSSE struct {
	w     *interpose.Withheld
	scope string
}

// faultSSE applies a matched case to one event. It reports whether the stream
// should end (a then=close, or a then=stall once the client has left), and
// the hold if it withheld the event. The verb
// dance mirrors the shim's; what differs is delivery -- a cut lands in the
// subject's own event bytes, and synthesized frames become events.
func (p *Proxy) faultSSE(sse *wire.SSE, c interpose.Case, m envelope.Message, unit wire.Encoded, resp *http.Response, prior envelope.ID, conn *exchange.Conn) (bool, *heldSSE) {
	plan, w, ok := p.applySSE(sse, c, m, unit, resp, prior, conn)
	if !ok {
		return false, nil
	}
	var held *heldSSE
	if w != nil {
		held = &heldSSE{w: w, scope: plan.Hold.Scope}
	}

	switch plan.Then {
	case fault.StreamClose:
		conn.StreamClose(transcript.CharpyClose, int(sse.Written()))
		return true, held
	case fault.StreamStall:
		st, err := wire.Stall(sse, wire.StallOptions{Keepalive: plan.Keepalive, Wall: p.o.Wall})
		if err != nil {
			conn.Note(fmt.Sprintf("case %s could not stall: %v", c.ID, err))
			break
		}
		// As over stdio, nothing further crosses a stalled stream, and it
		// stays open: returning would end the response, and the client would
		// see a cut stream close rather than go quiet. Truncation has no
		// withdrawal, so the stall lasts until the client leaves.
		<-resp.Request.Context().Done()
		_ = st.End()
		return true, held
	}
	return false, held
}

// applySSE is faultSSE up to the fault_applied line, under p.faulting. The
// stream action that follows is left outside it, because a stall lasts as long
// as it lasts and nothing waiting on the lock should wait on that.
func (p *Proxy) applySSE(sse *wire.SSE, c interpose.Case, m envelope.Message, unit wire.Encoded, resp *http.Response, prior envelope.ID, conn *exchange.Conn) (fault.Plan, *interpose.Withheld, bool) {
	p.faulting.Lock()
	defer p.faulting.Unlock()
	conn.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, p.faultContext(conn, m, transcript.S2C, prior))
	if err != nil {
		conn.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		p.emitEvent(sse, unit, transcript.S2C, nil, resp, conn)
		return plan, nil, false
	}

	f := conn.FrameOf(m, transcript.S2C)
	var held *interpose.Withheld
	att := c.TranscriptFault()

	for _, extra := range plan.Before {
		p.inter.Synthesize(f, c, extra)
		p.emitEvent(sse, wire.EncodeEvent("message", extra.Raw(), ""), transcript.S2C, att, resp, conn)
	}

	switch {
	case plan.Swallow:
		p.inter.Swallow(f, c, m) // the event simply does not cross
	case plan.Hold != nil:
		// A held event over HTTP is the response stream going quiet with the
		// frame undelivered. The stream stays open until the withdrawal
		// (relaySSE); the withdrawal is recorded, but there is no later flush
		// on this response -- the client's request has been answered by
		// silence.
		held = p.inter.Withhold(f, c, m, plan.Hold.For)
		go p.releaseHold(held, plan.Hold, conn)
	case plan.Cut != nil:
		p.inter.Rewrite(f, c, m, m)
		p.emitCut(sse, unit, m, plan.Cut, transcript.S2C, att, resp, conn)
	case plan.Deliver != nil:
		p.inter.Rewrite(f, c, m, *plan.Deliver)
		p.emitEvent(sse, wire.EncodeEvent("message", plan.Deliver.Raw(), ""), transcript.S2C,
			interpose.Crossed(c.Rewrote(m, *plan.Deliver), m, *plan.Deliver, true), resp, conn)
	}

	for _, extra := range plan.After {
		p.inter.Synthesize(f, c, extra)
		p.emitEvent(sse, wire.EncodeEvent("message", extra.Raw(), ""), transcript.S2C, att, resp, conn)
	}

	p.faultApplied(conn, c, plan.Verb(), m, transcript.S2C, resp)
	return plan, held, true
}

// releaseHold records a withdrawal but, unlike the shim's release, does not
// yet act on hold.Then. relaySSE keeps the held stream open until this
// moment, so the stream to deliver or error onto exists; writing to it from
// here is the half not built, scoped in ../../../docs/open-problems.md so a
// hang case that withdraws does not silently drop its post-withdrawal frame.
func (p *Proxy) releaseHold(held *interpose.Withheld, hold *fault.Hold, conn *exchange.Conn) {
	<-held.Withdrawn()
	c := held.Case()
	conn.FaultEvent(transcript.FaultWithdrawn, c, map[string]any{
		"at_mono_ns": int64(held.WithdrawnAt()),
		"then":       hold.Then,
	})
	p.o.Ledger.EndConsequences(conn.Face(), conn.ConnID())
	if hold.Then != "" && hold.Then != "close" {
		conn.Note(fmt.Sprintf("case %s: withdrawal action %q is not deliverable on a closed "+
			"HTTP response", c.ID, hold.Then))
	}
}

// emitCut delivers the matched event, truncated. The cut lands in the
// subject's own bytes because unit is what the scanner measured, not a
// re-rendering -- so "mid_event" is the middle of the event the subject sent.
func (p *Proxy) emitCut(sse *wire.SSE, unit wire.Encoded, from envelope.Message, cut *fault.Cut, dir transcript.Direction, att *transcript.Fault, resp *http.Response, conn *exchange.Conn) {
	n := unit.Len()
	if at, err := unit.Cut(cut.At, cut.Opts); err != nil {
		conn.Note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
	} else {
		n = at
	}
	sent := unit.BodySent(n)
	crossed, _ := envelope.Parse(sent)
	conn.Frame(dir, crossed.Raw(), interpose.Crossed(att, from, from, n == unit.Len()), sseHTTP(resp, unit))
	if _, err := wire.EmitCut(sse, unit, n); err != nil {
		conn.Note(fmt.Sprintf("writing cut event: %v", err))
	}
}

func (p *Proxy) emitEvent(sse *wire.SSE, e wire.Encoded, dir transcript.Direction, att *transcript.Fault, resp *http.Response, conn *exchange.Conn) {
	crossed, _ := envelope.Parse(e.Body())
	conn.Frame(dir, crossed.Raw(), att, sseHTTP(resp, e))
	if _, err := wire.Emit(sse, e); err != nil {
		conn.Note(fmt.Sprintf("writing event: %v", err))
	}
}

// requestHTTP records the transport detail of the client's request: its
// headers, no status. Header names are lowercased and pass through the run's
// redaction policy in the writer.
func requestHTTP(r *http.Request) *transcript.HTTP {
	return &transcript.HTTP{Headers: flatten(r.Header)}
}

func responseHTTP(resp *http.Response) *transcript.HTTP {
	return &transcript.HTTP{Status: resp.StatusCode, Headers: flatten(resp.Header)}
}

func sseHTTP(resp *http.Response, e wire.Encoded) *transcript.HTTP {
	h := responseHTTP(resp)
	h.SSEEvent = "message"
	return h
}

func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		out[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if dropOnResponse(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// dropOnForward strips headers that belong to charpy's connection to the
// client, not the subject's. Content-Type and Accept are kept -- the subject
// rejects a POST with no media type -- and Content-Length is dropped because
// the forwarding request sets its own from the body.
func dropOnForward(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length",
		"Te", "Trailer", "Upgrade", "Proxy-Connection", "Host":
		return true
	}
	return false
}

// dropOnResponse additionally strips the framing headers charpy sets itself
// when it re-emits the response: it owns Content-Type and the stream framing,
// having re-authored the event stream.
func dropOnResponse(k string) bool {
	if dropOnForward(k) {
		return true
	}
	return http.CanonicalHeaderKey(k) == "Content-Type"
}
