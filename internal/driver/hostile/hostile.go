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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

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

const maxFrame = 4 << 20

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

	srv, err := peer.NewServer(peer.Options{Era: o.Era})
	if err != nil {
		return nil, err
	}

	core := &exchange.Core{
		Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases,
		// The client is the subject, faced upstream and attacked by its
		// server (revisions.md section 4).
		Face: transcript.Upstream, Transport: transcript.TransportStdio,
		// One face: no second face to join to, so no correlation to claim.
		Link: func() transcript.Link { return transcript.Link{Via: transcript.ViaNone} },
	}

	return &Hostile{
		o:      o,
		x:      core.Conn(o.ClientID, o.SessionID, o.ConnID),
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

	h.x.Event(transcript.ConnOpen, map[string]any{"era": string(h.server.Era())})

	// Client to server: the client's own requests, relayed clean. charpy's
	// server is correct, and faulting toward it would test charpy, not the
	// client. This ends when the client disconnects, which is what ends a
	// relayed run.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		h.relay(h.o.In, h.toServer, transcript.C2S, false)
		_ = h.toServer.Close()
	}()

	// Server to client: the direction under test. A matched case faults the
	// response the subject client has to survive.
	s2cDone := make(chan struct{})
	go func() {
		defer close(s2cDone)
		h.relay(h.server.Out, h.toClient, transcript.S2C, true)
	}()

	// The run ends when the client goes away or a signal cancels the context.
	// Either way, closing the server ends its loop and unblocks the s2c relay,
	// which reads the server's output -- a pipe that closes only here, so
	// waiting on the relay before closing it would wedge.
	select {
	case <-clientGone:
	case <-ctx.Done():
	}
	err := h.server.Close()
	<-s2cDone
	<-served
	return err
}

func (h *Hostile) relay(from io.Reader, to *wire.Stdio, dir transcript.Direction, faulted bool) {
	sc := bufio.NewScanner(from)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		m, _ := envelope.Parse(raw)
		// Capture a prior resolved id before this frame resolves, so
		// already_resolved names an earlier answer and not the one in hand.
		prior, _ := h.x.PriorResolved()
		h.x.Observe(m, dir)

		if faulted {
			if cases := h.match.Select(h.x.FrameOf(m, dir)); len(cases) > 0 {
				h.applyFault(cases[0], m, dir, to, prior)
				continue
			}
		}
		h.deliver(m, dir, to, nil)
	}
}

// applyFault carries out a plan toward the client. The verb dance is the stdio
// shim's -- delivery here is stdio too, since the client is on pipes -- kept as
// a sibling for now (see notes): rewrite, synthesize, and the stream actions.
func (h *Hostile) applyFault(c interpose.Case, m envelope.Message, dir transcript.Direction, to *wire.Stdio, prior envelope.ID) {
	h.x.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{RunSeed: h.o.RunSeed, Resolved: prior})
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
		h.deliverCut(*plan.Deliver, dir, to, c.Rewrote(m, *plan.Deliver), plan.Cut)
	}

	for _, extra := range plan.After {
		h.inter.Synthesize(f, c, extra)
		h.deliver(extra, dir, to, att)
	}

	h.x.FaultEvent(transcript.FaultApplied, c, map[string]any{"verb": string(plan.Verb())})

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
	h.deliverCut(m, dir, to, att, nil)
}

func (h *Hostile) deliverCut(m envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault, cut *fault.Cut) {
	enc := wire.EncodeLine(m.Raw())
	n := enc.Len()
	if cut != nil {
		if at, err := enc.Cut(cut.At, cut.Opts); err != nil {
			h.x.Note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
		} else {
			n = at
		}
	}
	if written, err := wire.EmitCut(to, enc, n); err != nil {
		h.x.Note(fmt.Sprintf("write failed after %d bytes: %v", written, err))
	}
	h.x.Frame(dir, enc.Bytes[:n], att, nil)
}
