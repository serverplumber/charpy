package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

	// Face is the subject's face. A server subject is faced downstream.
	Face transcript.Face
	// Correlate is the link regime for recorded frames. Nil defaults to a
	// fresh Forwarded id -- charpy forwards the bytes, so it stamps its own.
	// The hostile HTTP mode passes ViaNone: it forwards to its own reference
	// server, and the one face under test is the client, with nothing to join.
	Correlate func() transcript.Link

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
	x      *exchange.Core
	inter  *interpose.Interposer
	match  *interpose.Matcher
	client *http.Client

	// nextConn names the connection of a request that carries no session id
	// yet -- the initialize that establishes one. Each such handshake is its
	// own connection.
	nextConn atomic.Int64
}

// New prepares a proxy. It serves once Handler is mounted and a subject is up.
func New(o Options) (*Proxy, error) {
	if o.SubjectURL == "" {
		return nil, errors.New("proxy: no subject URL")
	}
	if o.Face == "" {
		o.Face = transcript.Downstream
	}
	if o.ConnID == "" {
		o.ConnID = "c-0"
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
		correlate = func() transcript.Link { return interpose.Forwarded(interpose.NewJoinID()) }
	}
	core := &exchange.Core{
		Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases,
		Face: o.Face, Transport: transcript.TransportHTTP,
		// charpy forwards the bytes itself, so it stamps one authoritative id
		// on both copies -- not the ViaNone the single-faced shim records.
		Link: correlate,
	}
	return &Proxy{
		o:      o,
		x:      core,
		inter:  interpose.New(o.Ledger, o.Sched),
		match:  interpose.NewMatcher(o.Ledger, o.Cases...),
		client: client,
	}, nil
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

	// The client's frame, toward the subject. v0 has no c2s cases, so it is
	// observed and recorded but forwarded unchanged; a c2s fault would branch
	// here, exactly as the shim's does.
	if req, ok := envelope.Parse(body); ok == nil {
		conn.Observe(req, transcript.C2S)
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

// conn identifies the connection a request belongs to. A reconnecting client
// is more than one connection, and the ledger must see that: an id resolved on
// one is not the same id resolved on another, so keying them together would
// invent a duplicate. Mcp-Session-Id is that identity where the client has one
// yet; the initialize that establishes a session carries none, so it gets a
// fresh id of its own -- which is correct, since each handshake is a distinct
// connection's opening.
func (p *Proxy) conn(r *http.Request) *exchange.Conn {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		sid = "c-" + strconv.FormatInt(p.nextConn.Add(1), 10)
	}
	return p.x.Conn(p.o.ClientID, sid, sid)
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
	prior, _ := conn.PriorResolved()
	conn.Observe(m, transcript.S2C)

	att, out, applied := p.planJSON(m, prior, conn)
	deliver := out.Raw()
	if !applied {
		deliver = body
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(deliver); err != nil {
		conn.Note(fmt.Sprintf("writing to client: %v", err))
	}
	crossed, _ := envelope.Parse(deliver)
	conn.Frame(transcript.S2C, crossed.Raw(), att, responseHTTP(resp))
}

// planJSON runs a matched case's plan against a JSON response, returning the
// message to deliver. Cut, hold, swallow and multi-frame verbs do not apply to
// a single JSON body; each is noted rather than silently dropped.
func (p *Proxy) planJSON(m envelope.Message, prior envelope.ID, conn *exchange.Conn) (*transcript.Fault, envelope.Message, bool) {
	cases := p.match.Select(conn.FrameOf(m, transcript.S2C))
	if len(cases) == 0 {
		return nil, m, false
	}
	c := cases[0]
	conn.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{RunSeed: p.o.RunSeed, Resolved: prior})
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
	att := c.TranscriptFault()
	p.inter.Rewrite(conn.FrameOf(m, transcript.S2C), c, m, *plan.Deliver)
	conn.FaultEvent(transcript.FaultApplied, c, map[string]any{"verb": string(plan.Verb())})
	return att, *plan.Deliver, true
}

// relaySSE scans the subject's event stream and relays it unit by unit,
// faulting whichever unit a case matches. A stream is where the truncate and
// multi-frame verbs live, so this is the path the HTTP catalogue exercises.
func (p *Proxy) relaySSE(w http.ResponseWriter, resp *http.Response, conn *exchange.Conn) {
	sse := wire.NewSSE(w)
	sc := wire.NewSSEScanner(resp.Body)

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
		prior, _ := conn.PriorResolved()
		conn.Observe(m, transcript.S2C)

		cases := p.match.Select(conn.FrameOf(m, transcript.S2C))
		if len(cases) == 0 {
			if _, err := wire.Emit(sse, unit); err != nil {
				conn.Note(fmt.Sprintf("relaying event: %v", err))
				return
			}
			conn.Frame(transcript.S2C, unit.Body(), nil, sseHTTP(resp, unit))
			continue
		}

		if p.faultSSE(sse, cases[0], m, unit, resp, prior, conn) {
			// then close: the stream ends here.
			return
		}
	}
	if err := sc.Err(); err != nil {
		conn.Note(fmt.Sprintf("scanning subject stream: %v", err))
	}
}

// faultSSE applies a matched case to one event. It reports whether the stream
// should end (a then=close). The verb dance mirrors the shim's; what differs
// is delivery -- a cut lands in the subject's own event bytes, and synthesized
// frames become events.
func (p *Proxy) faultSSE(sse *wire.SSE, c interpose.Case, m envelope.Message, unit wire.Encoded, resp *http.Response, prior envelope.ID, conn *exchange.Conn) bool {
	conn.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{RunSeed: p.o.RunSeed, Resolved: prior})
	if err != nil {
		conn.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		p.emitEvent(sse, unit, transcript.S2C, nil, resp, conn)
		return false
	}

	f := conn.FrameOf(m, transcript.S2C)
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
		// frame undelivered. The stream stays open; the withdrawal, when it
		// comes, is recorded but there is no later flush on this response --
		// the client's request has already been answered by silence.
		held := p.inter.Withhold(f, c, m, plan.Hold.For)
		go p.releaseHold(held, plan.Hold, conn)
	case plan.Cut != nil:
		p.inter.Rewrite(f, c, m, m)
		p.emitCut(sse, unit, plan.Cut, transcript.S2C, att, resp, conn)
	case plan.Deliver != nil:
		p.inter.Rewrite(f, c, m, *plan.Deliver)
		p.emitEvent(sse, wire.EncodeEvent("message", plan.Deliver.Raw(), ""), transcript.S2C, att, resp, conn)
	}

	for _, extra := range plan.After {
		p.inter.Synthesize(f, c, extra)
		p.emitEvent(sse, wire.EncodeEvent("message", extra.Raw(), ""), transcript.S2C, att, resp, conn)
	}

	conn.FaultEvent(transcript.FaultApplied, c, map[string]any{"verb": string(plan.Verb())})

	switch plan.Then {
	case fault.StreamClose:
		conn.StreamClose(transcript.CharpyClose, int(sse.Written()))
		return true
	case fault.StreamStall:
		if _, err := wire.Stall(sse, wire.StallOptions{Keepalive: plan.Keepalive, Wall: p.wall()}); err != nil {
			conn.Note(fmt.Sprintf("case %s could not stall: %v", c.ID, err))
		}
	}
	return false
}

// releaseHold records a withdrawal but, unlike the shim's release, cannot act
// on hold.Then. Once charpy's handler has returned, the client's response is
// closed, so a held frame has no open stream to be delivered or errored onto
// after the fact -- "hold, then deliver later" needs the persistent pipe the
// stdio path has and a one-shot HTTP response does not. No v0 HTTP case holds
// (every hang case is gateway, face = upstream), so this is unexercised rather
// than wrong; it is scoped in ../../../docs/open-problems.md so a hang case
// made HTTP-runnable later does not silently drop its post-withdrawal frame.
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
func (p *Proxy) emitCut(sse *wire.SSE, unit wire.Encoded, cut *fault.Cut, dir transcript.Direction, att *transcript.Fault, resp *http.Response, conn *exchange.Conn) {
	n := unit.Len()
	if at, err := unit.Cut(cut.At, cut.Opts); err != nil {
		conn.Note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
	} else {
		n = at
	}
	if _, err := wire.EmitCut(sse, unit, n); err != nil {
		conn.Note(fmt.Sprintf("writing cut event: %v", err))
	}
	crossed, _ := envelope.Parse(unit.BodySent(n))
	conn.Frame(dir, crossed.Raw(), att, sseHTTP(resp, unit))
}

func (p *Proxy) emitEvent(sse *wire.SSE, e wire.Encoded, dir transcript.Direction, att *transcript.Fault, resp *http.Response, conn *exchange.Conn) {
	if _, err := wire.Emit(sse, e); err != nil {
		conn.Note(fmt.Sprintf("writing event: %v", err))
	}
	crossed, _ := envelope.Parse(e.Body())
	conn.Frame(dir, crossed.Raw(), att, sseHTTP(resp, e))
}

func (p *Proxy) wall() clock.Wall { return clock.RealWall() }

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
