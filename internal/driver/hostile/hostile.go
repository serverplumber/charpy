// Package hostile is the hostile-server driver: charpy serves an endpoint a
// client under test connects to, answering as a correct reference-peer server
// and corrupting its responses on the way out.
//
// stdio in v0: the client's configuration names charpy as its server command,
// so the client spawns charpy and speaks MCP over its pipes. charpy runs the
// reference peer as an in-process server, relays the client's requests to it,
// and faults the server's responses toward the client. The client under test
// is the subject, faced upstream; charpy's own server is correct and is not
// charpy's to attack, so only the server-to-client direction is faulted.
//
// It is relay-shaped, like the stdio shim: the client drives, so the run ends
// on a signal and occurrence-reproducibility is lost (interposer.md section
// 5.1). What it can do that the shim cannot is lie about its era -- the server
// peer is built at one era and claiming another is capability_flip on the
// initialize result (revisions.md section 4).
package hostile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// Options configures a hostile-server run.
type Options struct {
	// In and Out are the client under test: its requests arrive on In and its
	// responses leave on Out. In the stdio driver these are charpy's own
	// stdin and stdout, because the client spawned charpy as its server.
	In  io.Reader
	Out io.WriteCloser

	// Era is the revision charpy's server claims. Empty takes the SDK latest.
	Era revision.Revision

	Cases      []interpose.Case
	Transcript *transcript.Writer
	Sched      clock.Sched
	Ledger     *interpose.Ledger

	RunSeed   string
	ClientID  string
	SessionID string
	ConnID    string
}

// Hostile serves a client under test and faults the responses.
type Hostile struct {
	o      Options
	x      *exchange.Conn
	inter  *interpose.Interposer
	match  *interpose.Matcher
	server *peer.Server

	toClient *wire.Stdio
	toServer *wire.Stdio
	// writeMu serialises every write, and the record of it: a held frame is
	// released on the hold's own goroutine while the relay writes the same
	// pipe.
	writeMu sync.Mutex

	// asks tracks the follow-up pings in flight, and asking keeps it to one at
	// a time: a case with no ordinal faults every response, and a ping per
	// fault would pile questions onto a client still answering the first.
	asks   sync.WaitGroup
	asking atomic.Bool

	// fromClient is shut when a signal ends the run, so that nothing the
	// client sends afterwards reaches a transcript the caller is closing.
	fromClient gate
}

// gate lets a relay that cannot be waited for be stopped instead. Shutting it
// waits out the frame in hand, and every frame after that is dropped
// unrecorded: it arrived after the run ended.
type gate struct {
	mu   sync.Mutex
	shut bool
}

// pass runs f unless the gate is shut, and reports whether it did.
func (g *gate) pass(f func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return false
	}
	f()
	return true
}

func (g *gate) close() {
	g.mu.Lock()
	g.shut = true
	g.mu.Unlock()
}

// New prepares a run. The reference-peer server is built but not started.
func New(o Options) (*Hostile, error) {
	if o.In == nil || o.Out == nil {
		return nil, errors.New("hostile: the client side needs both In and Out")
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

	// A relay arms every applicable case, so the stream is the run's, not
	// one case's.
	srv, err := peer.NewServer(peer.Options{Era: o.Era, Trace: interpose.TraceFor(o.RunSeed, "", "server")})
	if err != nil {
		return nil, err
	}

	run := &exchange.Run{Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases}
	// The client is the subject, faced upstream and attacked by its server
	// (revisions.md section 4). One face: no second face to join to, so no
	// correlation to claim.
	face := run.Face(transcript.Upstream, transcript.TransportStdio,
		func(envelope.Message) transcript.Link { return transcript.Link{Via: transcript.ViaNone} })

	return &Hostile{
		o:      o,
		x:      face.Conn(o.ClientID, o.SessionID, o.ConnID),
		inter:  interpose.New(o.Ledger, o.Sched),
		match:  interpose.NewMatcher(o.Ledger, o.Cases...),
		server: srv,
	}, nil
}

// Run starts the reference server and relays until the client goes away or the
// context is cancelled. A relayed run ends on a signal, so cancelling the
// context is the ordinary stop; it closes the server, which ends its loop.
func (h *Hostile) Run(ctx context.Context) error {
	h.toClient = wire.NewStdio(h.o.Out)
	h.toServer = wire.NewStdio(h.server.In)

	served := make(chan error, 1)
	go func() { served <- h.server.Serve(ctx) }()

	// Follow-up pings run under their own context, ended before the server
	// closes, so none outlives the run to write into a closed transcript.
	askCtx, stopAsking := context.WithCancel(ctx)

	h.x.Event(transcript.ConnOpen, map[string]any{"era": string(h.server.Era())})

	// Client to server: the client's own requests, relayed clean. charpy's
	// server is correct, and faulting toward it would test charpy, not the
	// client. This ends when the client disconnects, which is what ends a
	// relayed run.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		h.relay(askCtx, h.o.In, h.toServer, transcript.C2S, false, &h.fromClient)
		_ = h.toServer.Close()
	}()

	// Server to client: the direction under test. A matched case faults the
	// response the subject client has to survive.
	s2cDone := make(chan struct{})
	go func() {
		defer close(s2cDone)
		h.relay(askCtx, h.server.Out, h.toClient, transcript.S2C, true, nil)
	}()

	// The run ends when the client goes away or a signal cancels the context.
	// Either way, closing the server ends its loop and unblocks the s2c relay,
	// which reads the server's output -- a pipe that closes only here, so
	// waiting on the relay before closing it would wedge.
	//
	// On a signal the c2s relay is not waited for: it reads the client, which
	// under --hostile is charpy's stdin, and nothing unblocks that read. It is
	// shut instead, which waits out the frame it is handling -- the client's
	// last answer can still be crossing when the signal lands -- so that
	// nothing it reads afterwards is recorded after Run returns.
	select {
	case <-clientGone:
	case <-ctx.Done():
		h.fromClient.close()
	}
	stopAsking()
	err := h.server.Close()
	<-s2cDone
	<-served
	h.asks.Wait()
	return err
}

// relay carries one direction. A gate, where there is one, is passed for every
// frame, and a shut gate ends the relay. So does a frame past wire.MaxFrame,
// which is recorded, and the rest of from drained unrelayed, as the stdio
// shim's is (Shim.capped says why it is neither closed nor left unread).
func (h *Hostile) relay(ctx context.Context, from io.Reader, to *wire.Stdio, dir transcript.Direction, faulted bool, g *gate) {
	sc := wire.NewLineScanner(from)
	defer func() {
		var tl *wire.FrameTooLarge
		if errors.As(sc.Err(), &tl) {
			h.x.Event(transcript.FrameCapped, transcript.CappedDetail(dir, tl.Read, wire.MaxFrame, tl.Prefix))
			h.x.StreamClose(transcript.FrameCap, int(to.Written()))
			go func() { _, _ = io.Copy(io.Discard, from) }()
		}
	}()

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		if g == nil {
			h.cross(ctx, raw, to, dir, faulted)
		} else if !g.pass(func() { h.cross(ctx, raw, to, dir, faulted) }) {
			return
		}
	}
}

// cross records one frame and delivers it, faulted if a case matches.
func (h *Hostile) cross(ctx context.Context, raw []byte, to *wire.Stdio, dir transcript.Direction, faulted bool) {
	m, _ := envelope.Parse(raw)
	// Capture a prior resolved id before this frame resolves, so
	// already_resolved names an earlier answer and not the one in hand.
	prior, _ := h.x.PriorResolved(dir)
	h.x.Observe(m, dir)

	if h.x.FollowUp(m, dir) {
		h.deliver(m, dir, to, nil)
		return
	}
	if faulted {
		if cases := h.match.Select(h.x.FrameOf(m, dir)); len(cases) > 0 {
			h.applyFault(ctx, cases[0], m, dir, to, prior)
			return
		}
	}
	h.deliver(m, dir, to, nil)
}

// declaredOutput is the reference server's declared outputSchema for tool, or
// nil when the tool declares none -- or when listing them failed, which leaves
// a declared_output_schema case not applicable rather than guessing a shape.
func declaredOutput(tool string) json.RawMessage {
	if tool == "" {
		return nil
	}
	schemas, err := peer.OutputSchemas()
	if err != nil {
		return nil
	}
	return schemas[tool]
}

// applyFault carries out a plan toward the client. The verb dance is the stdio
// shim's -- delivery here is stdio too, since the client is on pipes -- kept as
// a sibling rather than shared: rewrite, synthesize, and the stream actions.
func (h *Hostile) applyFault(ctx context.Context, c interpose.Case, m envelope.Message, dir transcript.Direction, to *wire.Stdio, prior envelope.ID) {
	h.x.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{
		RunSeed: h.o.RunSeed, Resolved: prior,
		// charpy serves here, so the declaration a schema_violation breaks is
		// its own reference server's, for the tool this answer is to.
		OutputSchema: declaredOutput(h.x.ToolFor(m, dir)),
	})
	if err != nil {
		h.x.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		h.deliver(m, dir, to, nil)
		return
	}

	f := h.x.FrameOf(m, dir)
	att := c.TranscriptFault()

	for _, extra := range plan.Before {
		h.inter.Synthesize(f, c, extra)
		h.deliver(extra, dir, to, att)
	}

	switch {
	case plan.Swallow:
		h.inter.Swallow(f, c, m)
	case plan.Hold != nil:
		held := h.inter.Withhold(f, c, m, plan.Hold.For)
		go h.release(held, plan.Hold, dir, to, att)
	case plan.Deliver != nil:
		h.inter.Rewrite(f, c, m, *plan.Deliver)
		h.deliverCut(*plan.Deliver, &m, dir, to, c.Rewrote(m, *plan.Deliver), plan.Cut)
	}

	for _, extra := range plan.After {
		h.inter.Synthesize(f, c, extra)
		h.deliver(extra, dir, to, att)
	}

	h.x.FaultEvent(transcript.FaultApplied, c, transcript.AppliedDetail(string(plan.Verb()), dir))

	// A closed pipe leaves no session to ask on, and a stall is this relay
	// blocking: a ping could not reach the client through it, nor its answer
	// come back. Both are left to the recovery probe.
	if plan.Then != fault.StreamClose && plan.Then != fault.StreamStall {
		h.ask(ctx)
	}

	switch plan.Then {
	case fault.StreamClose:
		h.x.StreamClose(transcript.CharpyClose, int(to.Written()))
		_ = to.Close()
	case fault.StreamStall:
		if _, err := wire.Stall(to, wire.StallOptions{Keepalive: plan.Keepalive}); err != nil {
			h.x.Note(fmt.Sprintf("case %s could not stall: %v", c.ID, err))
		}
	}
}

// ask has charpy's reference server put one ping to the client after a fault,
// so the reaction layer has an answer to judge (ADR-013).
//
// The server asks, through the SDK, rather than charpy writing a ping into the
// pipe: the question is then ordinary correct traffic crossing the interposer,
// and the answer returns to the session that asked. It is marked as the
// follow-up before it is sent, so the armed case cannot match it on the way
// out -- this relay is the one it crosses.
//
// It runs in its own goroutine because the ping crosses the very relay that
// called this, and waits for as long as the run does: a client that never
// answers is the finding, which the transcript already shows as an open
// request, so the error is noted rather than returned.
func (h *Hostile) ask(ctx context.Context) {
	if !h.asking.CompareAndSwap(false, true) {
		return
	}
	h.asks.Add(1)
	go func() {
		defer h.asks.Done()
		defer h.asking.Store(false)

		withdraw := h.x.Ask(transcript.S2C)
		defer withdraw()
		err := h.server.Ping(ctx)
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, peer.ErrCannotAsk):
			// 2026-07-28 and later: a server asks nothing, so nothing-asked is
			// the correct result rather than a gap.
		default:
			h.x.Note(fmt.Sprintf("follow-up ping: %v", err))
		}
	}()
}

func (h *Hostile) release(held *interpose.Withheld, hold *fault.Hold, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault) {
	<-held.Withdrawn()
	c := held.Case()
	h.x.FaultEvent(transcript.FaultWithdrawn, c, map[string]any{
		"at_mono_ns": int64(held.WithdrawnAt()),
		"then":       hold.Then,
	})
	h.o.Ledger.EndConsequences(h.x.Face(), h.o.ConnID)

	switch hold.Then {
	case "deliver":
		h.deliver(held.Message(), dir, to, att)
	case "close":
		h.x.StreamClose(transcript.CharpyClose, int(to.Written()))
		_ = to.Close()
	case "error":
		m, err := interpose.TemplateError(held.Message().ID, -32603, "charpy: request withdrawn")
		if err != nil {
			h.x.Note(fmt.Sprintf("case %s could not build its withdrawal error: %v", c.ID, err))
			return
		}
		h.deliver(m, dir, to, att)
	default:
		h.x.Note(fmt.Sprintf("case %s: unknown withdrawal %q", c.ID, hold.Then))
	}
}

func (h *Hostile) deliver(m envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault) {
	h.deliverCut(m, nil, dir, to, att, nil)
}

func (h *Hostile) deliverCut(m envelope.Message, orig *envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault, cut *fault.Cut) {
	enc := wire.EncodeLine(m.Raw())
	n := enc.Len()
	if cut != nil {
		if at, err := enc.Cut(cut.At, cut.Opts); err != nil {
			h.x.Note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
		} else {
			n = at
		}
	}
	// As in the shim: only the matched frame is settled against what crossed.
	if orig != nil {
		att = interpose.Crossed(att, *orig, m, n == enc.Len())
	}
	// Recorded before it is written, as the shim's is: the client may
	// answer as soon as the bytes land, and its answer must not take the
	// earlier seq.
	h.writeMu.Lock()
	h.x.Frame(dir, enc.Bytes[:n], att, nil)
	written, err := wire.EmitCut(to, enc, n)
	h.writeMu.Unlock()
	if err != nil {
		h.x.Note(fmt.Sprintf("write failed after %d bytes: %v", written, err))
	}
}
