// Package exchange holds the transport-independent bookkeeping every driver
// does: observe the ledger, settle the negotiated revision, record frames and
// events, and name what was armed.
//
// It exists because the stdio shim and the HTTP proxy proved -- by both being
// written and diffed method for method -- that these helpers are identical bar
// the transport tag, the correlation regime, and (for a frame) an HTTP-detail
// argument. What is *not* here is the fault verb dance and delivery: those bind
// to how each transport puts bytes on the wire and ends a stream, and stay in
// the drivers.
//
// A run has connections. Core holds the run's state -- the ledger, the writer,
// the armed cases, the once-only header; a Conn binds it to one connection's
// identity. Single-connection drivers make one Conn and keep it; the HTTP
// drivers make a Conn per client session, so a client that reconnects is two
// connections and not one, which the ledger must see to count and correlate
// correctly. See notes/status.md.
package exchange

import (
	"encoding/json"
	"sync"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Core is a run's ledger- and transcript-facing state, shared across every
// connection in the run.
type Core struct {
	Ledger     *interpose.Ledger
	Transcript *transcript.Writer
	Cases      []interpose.Case
	// Settle, when set, is asked for the run's cases once the negotiated
	// revision is known, and its answer replaces Cases before the header
	// records what was armed. It is how a run on --revision auto arms: which
	// cases apply, and what revision their citations name, are both facts
	// about the handshake, and nothing before it can know them.
	Settle    func(revision.Revision) []interpose.Case
	Face      transcript.Face
	Transport transcript.Transport
	// Link is the correlation regime for a recorded frame. The one-faced shim
	// and hostile driver return ViaNone; the proxy forwards the bytes and
	// stamps a fresh Forwarded id. Nil is read as ViaNone.
	Link func() transcript.Link

	mu     sync.Mutex
	header bool

	// asks holds each connection's follow-up question, by connection id.
	asks map[string]*ask
}

// ask is one follow-up question on a connection: armed until the request
// crosses, then keyed by its wire id until the answer does.
type ask struct {
	dir transcript.Direction
	key string
}

// Conn binds a Core to one connection's identity. Its methods are the
// per-frame bookkeeping; the connection id is what the ledger keys on, so two
// client connections with distinct ids are two exchanges and not a collision.
type Conn struct {
	core      *Core
	clientID  string
	sessionID string
	connID    string
}

// Conn returns a handle bound to one connection. Reusing the same ids returns
// an equivalent handle; the ledger state lives in Core, keyed by the ids.
func (c *Core) Conn(clientID, sessionID, connID string) *Conn {
	return &Conn{core: c, clientID: clientID, sessionID: sessionID, connID: connID}
}

// Observe keeps the ledger current and, on the handshake response, settles the
// negotiated revision.
//
// Requests are recorded whichever way they travel, and answers resolve against
// the requests that came the other way: a server asks its client things too,
// and the client's answer is only attributable to the request it answers.
func (n *Conn) Observe(m envelope.Message, dir transcript.Direction) {
	switch m.Kind {
	case envelope.KindRequest:
		n.core.Ledger.Originate(n.core.Face, n.connID, dir,
			interpose.Exchange{IntentID: m.ID, Method: m.Method, Tool: toolOf(m)})
	case envelope.KindResponse, envelope.KindError:
		n.core.Ledger.Resolve(n.core.Face, n.connID, dir.Opposite(), m.ID)
		if m.Kind == envelope.KindResponse && dir == transcript.S2C {
			n.core.settleRevision(m)
		}
	}
}

// ToolFor is the tool named by the call an answer travelling dir answers, or
// "" when it answers something else.
func (n *Conn) ToolFor(m envelope.Message, dir transcript.Direction) string {
	tool, _ := n.core.Ledger.ToolFor(n.core.Face, n.connID, dir.Opposite(), m.ID)
	return tool
}

// toolOf is the tool a tools/call request names.
func toolOf(m envelope.Message) string {
	if m.Method != "tools/call" {
		return ""
	}
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return p.Name
}

// PriorResolved is the id most recently resolved among the requests an answer
// travelling dir would answer, read before the current frame resolves so
// already_resolved names an earlier answer and not the one in hand.
func (n *Conn) PriorResolved(dir transcript.Direction) (envelope.ID, bool) {
	return n.core.Ledger.LatestResolved(n.core.Face, n.connID, dir.Opposite())
}

// settleRevision writes the header once the handshake response says what the
// subject speaks. Two handshakes, because the era boundary replaced one with
// the other: a sessioned subject answers initialize with the single version it
// settled on; a 2026-07-28 subject answers server/discover with every version
// it supports and never sends an initialize.
func (c *Core) settleRevision(m envelope.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.header {
		return
	}

	var probe struct {
		ProtocolVersion   string   `json:"protocolVersion"`
		SupportedVersions []string `json:"supportedVersions"`
	}
	if json.Unmarshal(m.Result, &probe) != nil {
		return
	}

	var (
		negotiated revision.Revision
		offered    []revision.Revision
		how        transcript.How
	)
	switch {
	case probe.ProtocolVersion != "":
		negotiated, how = revision.Revision(probe.ProtocolVersion), transcript.HowInitialize
	case len(probe.SupportedVersions) > 0:
		for _, v := range probe.SupportedVersions {
			r := revision.Revision(v)
			if !revision.Known(r) {
				continue
			}
			offered = append(offered, r)
			if revision.Index(r) > revision.Index(negotiated) {
				negotiated = r
			}
		}
		how = transcript.HowServerDiscover
	default:
		return
	}
	if !revision.Known(negotiated) {
		return
	}

	c.header = true
	if c.Settle != nil {
		c.Cases = c.Settle(negotiated)
	}
	_ = c.Transcript.WriteHeader(transcript.Header{
		Revision: &transcript.Negotiation{Negotiated: negotiated, Offered: offered, How: how},
		Cases:    c.armed(),
	})
}

func (c *Core) armed() []string {
	out := make([]string, 0, len(c.Cases))
	for _, cs := range c.Cases {
		out = append(out, cs.Citation)
	}
	return out
}

// FrameOf is the matcher's view of a frame.
func (n *Conn) FrameOf(m envelope.Message, dir transcript.Direction) interpose.Frame {
	return interpose.Frame{
		Face: n.core.Face, Direction: dir, Kind: m.Kind, Method: m.Method, ID: m.ID,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
	}
}

// Frame records what crossed. raw is the bytes that actually went out; http is
// nil on transports with no such detail.
func (n *Conn) Frame(dir transcript.Direction, raw []byte, att *transcript.Fault, http *transcript.HTTP) {
	crossed, _ := envelope.Parse(raw)

	var method string
	switch crossed.Kind {
	case envelope.KindResponse, envelope.KindError:
		method, _ = n.core.Ledger.MethodFor(n.core.Face, n.connID, dir.Opposite(), crossed.ID)
	}

	link := transcript.Link{Via: transcript.ViaNone}
	if n.core.Link != nil {
		link = n.core.Link()
	}

	n.core.Transcript.Frame(transcript.Frame{
		Face: n.core.Face, Direction: dir, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Message: crossed, Method: method, HTTP: http, Link: link, Fault: att,
	})
}

// Event records something that is not a frame but changes what the oracle
// should conclude.
func (n *Conn) Event(kind transcript.EventKind, detail map[string]any) {
	n.core.Transcript.Event(transcript.Event{
		Kind: kind, Face: n.core.Face, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID, Detail: detail,
	})
}

// FaultEvent records a fault lifecycle event, tagged with the case that caused it.
func (n *Conn) FaultEvent(kind transcript.EventKind, cs interpose.Case, detail map[string]any) {
	n.core.Transcript.Event(transcript.Event{
		Kind: kind, Face: n.core.Face, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Detail: detail, Fault: cs.TranscriptFault(),
	})
}

// Note records a harness annotation the oracle ignores.
func (n *Conn) Note(text string) {
	n.core.Transcript.Event(transcript.Event{
		Kind: transcript.Note, Face: n.core.Face, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Detail: map[string]any{"harness": text},
	})
}

// StreamClose records a stream ending, with who closed it and how many bytes
// crossed.
func (n *Conn) StreamClose(reason transcript.CloseReason, bytesWritten int) {
	n.core.Transcript.Event(transcript.Event{
		Kind: transcript.StreamClose, Face: n.core.Face, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Detail: transcript.CloseDetail(reason, bytesWritten),
	})
}

// Face is the run's subject face, for callers that key ledger state on it.
func (n *Conn) Face() transcript.Face { return n.core.Face }

// Probe records a liveness probe: which method was sent, how it fared, how
// long it took. The oracle's liveness layer reads these to judge recovery.
func (n *Conn) Probe(method string, outcome transcript.ProbeOutcome, elapsedNS int64) {
	n.core.Transcript.Event(transcript.Event{
		Kind: transcript.Probe, Face: n.core.Face, Transport: n.core.Transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Detail: map[string]any{
			"method":          method,
			"outcome":         string(outcome),
			"elapsed_mono_ns": elapsedNS,
		},
	})
}

// ConnID is this connection's id, for the ledger calls a driver makes directly.
func (n *Conn) ConnID() string { return n.connID }

// Ask marks the next request crossing this connection in dir as charpy's
// follow-up question -- the one put to the subject after a fault so the
// reaction layer has an answer to judge -- and returns a func that withdraws
// the mark if the question never crossed.
//
// The question and its answer are kept out of the matcher's reach entirely,
// not merely left unfaulted. An armed case with no ordinal, or an every-nth,
// would otherwise fault the question and leave it no longer clean -- the
// oracle rightly ignores a tampered request -- and a frame the matcher sees
// is a frame it counts, so even an unfaulted question would spend an ordinal
// the case's own traffic was waiting for. Kept out, the case selects exactly
// the frames it would have selected with no follow-up at all.
func (n *Conn) Ask(dir transcript.Direction) (withdraw func()) {
	c := n.core
	c.mu.Lock()
	if c.asks == nil {
		c.asks = map[string]*ask{}
	}
	a := &ask{dir: dir}
	c.asks[n.connID] = a
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// Only a question that never crossed is withdrawn. One that did stays
		// exempt until its answer crosses, however late, because a late
		// answer is exactly the frame the oracle needs to see untouched.
		if c.asks[n.connID] == a && a.key == "" {
			delete(c.asks, n.connID)
		}
	}
}

// FollowUp reports whether m belongs to this connection's follow-up exchange,
// and so must bypass the matcher. It is called on every frame, before
// matching, because it is also what notices the question crossing: the id
// the question went out with is the only way to know its answer.
func (n *Conn) FollowUp(m envelope.Message, dir transcript.Direction) bool {
	c := n.core
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.asks[n.connID]
	if !ok {
		return false
	}
	switch {
	case a.key == "" && m.Kind == envelope.KindRequest && dir == a.dir:
		a.key = m.ID.Key()
		return true
	case a.key != "" && dir != a.dir && (m.Kind == envelope.KindResponse || m.Kind == envelope.KindError) &&
		m.ID.Key() == a.key:
		delete(c.asks, n.connID)
		return true
	}
	return false
}
