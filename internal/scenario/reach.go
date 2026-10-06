package scenario

import (
	"fmt"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Reaches reports whether a scripted run against this subject class ever puts
// on the wire the frame m selects, and says why not when it does not.
//
// It is applicability, decided at selection like transport and subject class
// (ADR-013): a case whose frame nothing here sends would arm, see nothing, and
// report UNTRIGGERED -- a subject that behaved, as far as the report could
// tell. It is derived from the matcher rather than declared on the case, for
// the reason the script is: a second statement of what traffic a case needs
// could drift from the one the matcher already makes.
//
// Who sends what follows from the class:
//
//   - server: charpy's client sends downstream c2s, and what it sends is
//     [Basic]; the server answers it, downstream s2c.
//   - gateway: the same client and the gateway's answers on its downstream
//     face. Upstream, the gateway forwards what the script sends, c2s, and
//     charpy's reference servers answer it, s2c -- the gateway's handshake
//     with each, and the script's calls. They send no notification and no
//     request of their own.
//   - client: the client under test drives, so what it provokes is its own
//     to say, and nothing is ruled out.
//
// An unset face or direction matches either, so the frame is reachable when
// any sender it could be reaches it.
func Reaches(m interpose.Match, class transcript.Class) error {
	var senders []sender
	switch class {
	case transcript.ClassClient:
		return nil
	case transcript.ClassServer:
		senders = []sender{
			{transcript.Downstream, transcript.C2S, script(sampled)},
			{transcript.Downstream, transcript.S2C, script(sampled)},
		}
	case transcript.ClassGateway:
		senders = []sender{
			{transcript.Downstream, transcript.C2S, script(unsampled)},
			{transcript.Downstream, transcript.S2C, script(unsampled)},
			{transcript.Upstream, transcript.C2S, upstreamMethod},
			{transcript.Upstream, transcript.S2C, upstreams},
		}
	}

	var why error
	for _, s := range senders {
		if (m.Face != "" && m.Face != s.face) || (m.Direction != "" && m.Direction != s.dir) {
			continue
		}
		reason := s.reaches(m)
		if reason == "" {
			return nil
		}
		if why == nil {
			why = fmt.Errorf("%w: %s %s %s: %s", ErrUnreachable, frame(m), s.face, s.dir, reason)
		}
	}
	if why == nil {
		why = fmt.Errorf("%w: %s %s %s: nothing charpy stands up sends there", ErrUnreachable, frame(m), m.Face, m.Direction)
	}
	return why
}

// sender is one party in the run, the face and direction it sends on, and
// what of the matcher's frame it brings about there: an empty reason when it
// does, why not when it does not.
type sender struct {
	face    transcript.Face
	dir     transcript.Direction
	reaches func(interpose.Match) string
}

// sampling is whether the script's call can reach a tool that samples. A
// server's can, when the run names one with --tool: the server asks its
// client inside the call, and the case's matcher lands on the request or the
// answer. Behind a gateway the request would have to start at an upstream,
// and charpy's never sample.
type sampling bool

const (
	sampled   sampling = true
	unsampled sampling = false
)

// script is the traffic [Basic] brings about on the downstream face, in
// either direction: its calls and their answers.
func script(s sampling) func(interpose.Match) string {
	return func(m interpose.Match) string {
		switch method := m.Method.String(); method {
		case "", "*", "tools/call":
			return ""
		case "sampling/createMessage":
			if s == sampled {
				return ""
			}
			return "charpy's upstreams never sample"
		default:
			return "charpy's script sends only tools/call"
		}
	}
}

// upstreams is charpy's reference servers behind a gateway, which only answer.
func upstreams(m interpose.Match) string {
	if m.Kind == envelope.KindRequest || m.Kind == envelope.KindNotification {
		return "charpy's upstreams send no request or notification of their own"
	}
	return upstreamMethod(m)
}

// upstreamMethod is the methods crossing the upstream face, either way: the
// gateway's handshake with each upstream, and what it forwards of the script.
func upstreamMethod(m interpose.Match) string {
	switch m.Method.String() {
	case "", "*", "tools/call", "tools/list":
		return ""
	case "initialize":
		// Each upstream handshakes once, concurrently and in an order the
		// gateway chooses, and nothing here asks one to reconnect. A later
		// handshake is either never sent or another upstream's first, which
		// is not the frame an ordinal past one means, nor a reproducible one.
		if m.Occurrence <= 1 && m.Every <= 1 {
			return ""
		}
		return "each upstream handshakes once, and none is asked to reconnect"
	default:
		return "the upstream face carries only the handshake and what the gateway forwards"
	}
}

// frame names what m selects, for a refusal.
func frame(m interpose.Match) string {
	name := m.Method.String()
	if name == "" || name == "*" {
		name = "any method"
	}
	if m.Kind != "" {
		name += " " + string(m.Kind)
	}
	if m.Occurrence > 1 {
		name += fmt.Sprintf(" #%d", m.Occurrence)
	}
	return name
}
