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
// State comes in three widths. A Run is the run: the ledger, the writer, the
// armed cases, the once-only header. A Face is one side of the subject: which
// face it is, the transport it speaks, how its frames are correlated, and the
// follow-up questions put on it. A server or a client under test has one face;
// a gateway has two, sharing one Run, so its run writes one header and keeps
// one ledger across both. A Conn binds a Face to one connection's identity.
// Single-connection drivers make one Conn and keep it; the HTTP drivers make a
// Conn per client session, so a client that reconnects is two connections and
// not one, which the ledger must see to count and correlate correctly.
package exchange

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Run is a run's ledger- and transcript-facing state, shared by every face
// and every connection in the run.
type Run struct {
	Ledger     *interpose.Ledger
	Transcript *transcript.Writer
	Cases      []interpose.Case
	// Settle, when set, is asked for the run's cases once the negotiated
	// revision is known, and its answer replaces Cases before the header
	// records what was armed. It is how a run on --revision auto arms: which
	// cases apply, and what revision their citations name, are both facts
	// about the handshake, and nothing before it can know them.
	Settle func(revision.Revision) []interpose.Case
	// Headed is the face whose handshake writes the header, the revision
	// the run is cited against. Empty means whichever face settles first,
	// which for a one-faced run is its only face. A gateway run names its
	// downstream face: what the gateway offers its clients is the revision
	// its cases are about, and its upstream handshake can finish first.
	Headed transcript.Face

	mu     sync.Mutex
	header bool
	// settled is the revision each face's own handshake negotiated. Every
	// frame carries its face's, so each face is judged against what was
	// agreed on it, whatever the header says.
	settled map[transcript.Face]revision.Revision
}

// Face is one side of the subject within a run.
type Face struct {
	run       *Run
	face      transcript.Face
	transport transcript.Transport
	link      func(envelope.Message) transcript.Link
	// join is set on a face of a gateway, whose frames are correlated with
	// the other face's (see Conn.join).
	join bool

	mu sync.Mutex
	// asks holds each connection's follow-up question, by connection id.
	// Ids are unique within a face, not across faces -- two faces may each
	// have a "c-0" -- so the questions are the face's, not the run's.
	asks map[string]*ask
}

// Face returns the run's view from one side of the subject. link is the
// correlation regime for a frame recorded on it, given the frame: the
// one-faced shim and hostile driver return ViaNone; the proxy forwards the
// bytes and stamps a fresh Forwarded id. Nil is read as ViaNone.
func (r *Run) Face(face transcript.Face, transport transcript.Transport, link func(envelope.Message) transcript.Link) *Face {
	return &Face{run: r, face: face, transport: transport, link: link}
}

// JoinedFace is one side of a gateway: its frames are joined with what
// crossed the gateway's other side, through the run's ledger
// (transcript.md section 4, interposer.md section 6).
func (r *Run) JoinedFace(face transcript.Face, transport transcript.Transport) *Face {
	return &Face{run: r, face: face, transport: transport, join: true}
}

// ask is one follow-up question on a connection: armed until the request
// crosses, then keyed by its wire id until the answer does.
type ask struct {
	dir transcript.Direction
	key string
}

// Conn binds a Face to one connection's identity. Its methods are the
// per-frame bookkeeping; the connection id is what the ledger keys on, so two
// client connections with distinct ids are two exchanges and not a collision.
type Conn struct {
	face      *Face
	clientID  string
	sessionID string
	connID    string
}

// Conn returns a handle bound to one connection. Reusing the same ids returns
// an equivalent handle; the ledger state lives in the Run, keyed by face and
// ids.
func (f *Face) Conn(clientID, sessionID, connID string) *Conn {
	return &Conn{face: f, clientID: clientID, sessionID: sessionID, connID: connID}
}

func (n *Conn) run() *Run { return n.face.run }

// Observe keeps the ledger current and, on the handshake response, settles the
// negotiated revision.
//
// Requests are recorded whichever way they travel, and answers resolve against
// the requests that came the other way: a server asks its client things too,
// and the client's answer is only attributable to the request it answers.
func (n *Conn) Observe(m envelope.Message, dir transcript.Direction) {
	switch m.Kind {
	case envelope.KindRequest:
		n.run().Ledger.Originate(n.face.face, n.connID, dir,
			interpose.Exchange{IntentID: m.ID, Method: m.Method, Tool: toolOf(m)})
	case envelope.KindResponse, envelope.KindError:
		n.run().Ledger.Resolve(n.face.face, n.connID, dir.Opposite(), m.ID)
		if m.Kind == envelope.KindResponse && dir == transcript.S2C {
			if neg, ok := negotiation(m); ok {
				n.settle(neg)
			}
		}
	}
}

// ToolFor is the tool named by the call an answer travelling dir answers, or
// "" when it answers something else.
func (n *Conn) ToolFor(m envelope.Message, dir transcript.Direction) string {
	tool, _ := n.run().Ledger.ToolFor(n.face.face, n.connID, dir.Opposite(), m.ID)
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
	return n.run().Ledger.LatestResolved(n.face.face, n.connID, dir.Opposite())
}

// negotiation reads what a handshake response settled. Two handshakes,
// because the era boundary replaced one with the other: a sessioned subject
// answers initialize with the single version it settled on; a 2026-07-28
// subject answers server/discover with every version it supports and never
// sends an initialize.
func negotiation(m envelope.Message) (transcript.Negotiation, bool) {
	var probe struct {
		ProtocolVersion   string   `json:"protocolVersion"`
		SupportedVersions []string `json:"supportedVersions"`
	}
	if json.Unmarshal(m.Result, &probe) != nil {
		return transcript.Negotiation{}, false
	}

	var neg transcript.Negotiation
	switch {
	case probe.ProtocolVersion != "":
		neg.Negotiated, neg.How = revision.Revision(probe.ProtocolVersion), transcript.HowInitialize
	case len(probe.SupportedVersions) > 0:
		for _, v := range probe.SupportedVersions {
			rv := revision.Revision(v)
			if !revision.Known(rv) {
				continue
			}
			neg.Offered = append(neg.Offered, rv)
			if revision.Index(rv) > revision.Index(neg.Negotiated) {
				neg.Negotiated = rv
			}
		}
		neg.How = transcript.HowServerDiscover
	default:
		return transcript.Negotiation{}, false
	}
	return neg, revision.Known(neg.Negotiated)
}

// settle records what this connection's face negotiated, the first time it
// does, and writes the header if this is the face that heads the run.
//
// A face that settles on a different revision from one already settled is
// noted, not judged. A gateway is free to speak different revisions to its
// clients and its servers; whether a given pair is a finding is a question
// for the oracle, once a real gateway shows which pairs occur.
func (n *Conn) settle(neg transcript.Negotiation) {
	r, face := n.run(), n.face.face
	r.mu.Lock()
	if _, done := r.settled[face]; done {
		r.mu.Unlock()
		return
	}
	if r.settled == nil {
		r.settled = map[transcript.Face]revision.Revision{}
	}
	r.settled[face] = neg.Negotiated
	var differs []string
	for other, rv := range r.settled {
		if other != face && rv != neg.Negotiated {
			differs = append(differs, fmt.Sprintf("%s %s", other, rv))
		}
	}
	if !r.header && (r.Headed == "" || r.Headed == face) {
		r.header = true
		if r.Settle != nil {
			r.Cases = r.Settle(neg.Negotiated)
		}
		_ = r.Transcript.WriteHeader(transcript.Header{Revision: &neg, Cases: r.armed()})
	}
	r.mu.Unlock()

	if len(differs) > 0 {
		slices.Sort(differs)
		n.Note(fmt.Sprintf("faces negotiated different revisions: %s %s, %s",
			face, neg.Negotiated, strings.Join(differs, ", ")))
	}
}

// revisionOf is what face negotiated, or "" before its handshake settles.
func (r *Run) revisionOf(face transcript.Face) revision.Revision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled[face]
}

func (r *Run) armed() []string {
	out := make([]string, 0, len(r.Cases))
	for _, cs := range r.Cases {
		out = append(out, cs.Citation)
	}
	return out
}

// FrameOf is the matcher's view of a frame.
func (n *Conn) FrameOf(m envelope.Message, dir transcript.Direction) interpose.Frame {
	return interpose.Frame{
		Face: n.face.face, Direction: dir, Kind: m.Kind, Method: m.Method, ID: m.ID,
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
		method, _ = n.run().Ledger.MethodFor(n.face.face, n.connID, dir.Opposite(), crossed.ID)
	}

	link := transcript.Link{Via: transcript.ViaNone}
	switch {
	case n.face.join:
		link = n.join(crossed, dir)
	case n.face.link != nil:
		link = n.face.link(crossed)
	}

	n.run().Transcript.Frame(transcript.Frame{
		Face: n.face.face, Direction: dir, Transport: n.face.transport,
		ClientID: n.clientID, SessionID: n.sessionID, ConnID: n.connID,
		Message: crossed, Method: method, HTTP: http, Link: link, Fault: att,
		Revision: n.run().revisionOf(n.face.face),
	})
}

// join correlates a frame on a gateway's face with the other face.
//
// Which frames are charpy's follows from the face: charpy is the client on
// the downstream face and the servers on the upstream one. What charpy sent
// is registered, by the trace it stamped and by content; what the gateway
// sent is recalled against that, trace first, content second, and is no join
// at all when neither matches. An answer is joined by the request it
// answers, found by id on its own face -- a fact, not a match -- so only
// requests and notifications are ever matched across.
func (n *Conn) join(m envelope.Message, dir transcript.Direction) transcript.Link {
	ledger, face := n.run().Ledger, n.face.face
	charpys := transcript.C2S
	if face == transcript.Upstream {
		charpys = transcript.S2C
	}
	switch m.Kind {
	case envelope.KindRequest, envelope.KindNotification:
		var link transcript.Link
		if dir == charpys {
			link, _ = ledger.Originated(interpose.NewJoinID(), m, transcript.ViaTraced)
		} else {
			link = ledger.LinkFor(m)
		}
		if m.Kind == envelope.KindRequest {
			ledger.SetLink(face, n.connID, dir, m.ID, link)
		}
		return link
	case envelope.KindResponse, envelope.KindError:
		if link, ok := ledger.LinkOf(face, n.connID, dir.Opposite(), m.ID); ok {
			return link
		}
	}
	return transcript.Link{Via: transcript.ViaNone}
}

// event records an event on this connection.
func (n *Conn) event(e transcript.Event) {
	e.Face, e.Transport = n.face.face, n.face.transport
	e.ClientID, e.SessionID, e.ConnID = n.clientID, n.sessionID, n.connID
	n.run().Transcript.Event(e)
}

// Event records something that is not a frame but changes what the oracle
// should conclude.
func (n *Conn) Event(kind transcript.EventKind, detail map[string]any) {
	n.event(transcript.Event{Kind: kind, Detail: detail})
}

// FaultEvent records a fault lifecycle event, tagged with the case that caused it.
func (n *Conn) FaultEvent(kind transcript.EventKind, cs interpose.Case, detail map[string]any) {
	n.event(transcript.Event{Kind: kind, Detail: detail, Fault: cs.TranscriptFault()})
}

// Applied records a fault taking effect on m, travelling dir. On a joined
// face it carries the join m's exchange already has, so the reaction layer
// can find the call a fault was carrying on the other face (ADR-015). It is
// read, never made: a frame the fault kept from crossing joins nothing new.
func (n *Conn) Applied(cs interpose.Case, verb string, m envelope.Message, dir transcript.Direction) {
	e := transcript.Event{
		Kind: transcript.FaultApplied, Detail: transcript.AppliedDetail(verb, dir),
		Fault: cs.TranscriptFault(),
	}
	if n.face.join {
		origin := dir
		if m.Kind == envelope.KindResponse || m.Kind == envelope.KindError {
			origin = dir.Opposite()
		}
		if link, ok := n.run().Ledger.LinkOf(n.face.face, n.connID, origin, m.ID); ok && link.Via != transcript.ViaNone {
			e.Link = &link
		}
	}
	n.event(e)
}

// Note records a harness annotation, which no verdict reads.
func (n *Conn) Note(text string) {
	n.event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"harness": text}})
}

// QuestionFailed notes that a follow-up question failed in charpy's own
// client, with transcript.QuestionDetail.
func (n *Conn) QuestionFailed(question string, err error) {
	n.event(transcript.Event{Kind: transcript.Note, Detail: transcript.QuestionDetail(question, err)})
}

// StreamClose records a stream ending, with who closed it and how many bytes
// crossed.
func (n *Conn) StreamClose(reason transcript.CloseReason, bytesWritten int) {
	n.event(transcript.Event{Kind: transcript.StreamClose, Detail: transcript.CloseDetail(reason, bytesWritten)})
}

// Face is the face this connection is on, for callers that key ledger state
// on it.
func (n *Conn) Face() transcript.Face { return n.face.face }

// Probe records a liveness probe: which method was sent, how it fared, how
// long it took. The oracle's liveness layer reads these to judge recovery.
func (n *Conn) Probe(method string, outcome transcript.ProbeOutcome, elapsedNS int64) {
	n.event(transcript.Event{Kind: transcript.Probe, Detail: map[string]any{
		"method":          method,
		"outcome":         string(outcome),
		"elapsed_mono_ns": elapsedNS,
	}})
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
	f := n.face
	f.mu.Lock()
	if f.asks == nil {
		f.asks = map[string]*ask{}
	}
	a := &ask{dir: dir}
	f.asks[n.connID] = a
	f.mu.Unlock()

	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		// Only a question that never crossed is withdrawn. One that did stays
		// exempt until its answer crosses, however late, because a late
		// answer is exactly the frame the oracle needs to see untouched.
		if f.asks[n.connID] == a && a.key == "" {
			delete(f.asks, n.connID)
		}
	}
}

// FollowUp reports whether m belongs to this connection's follow-up exchange,
// and so must bypass the matcher. It is called on every frame, before
// matching, because it is also what notices the question crossing: the id
// the question went out with is the only way to know its answer.
func (n *Conn) FollowUp(m envelope.Message, dir transcript.Direction) bool {
	f := n.face
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.asks[n.connID]
	if !ok {
		return false
	}
	switch {
	case a.key == "" && m.Kind == envelope.KindRequest && dir == a.dir:
		a.key = m.ID.Key()
		return true
	case a.key != "" && dir != a.dir && (m.Kind == envelope.KindResponse || m.Kind == envelope.KindError) &&
		m.ID.Key() == a.key:
		delete(f.asks, n.connID)
		return true
	}
	return false
}
