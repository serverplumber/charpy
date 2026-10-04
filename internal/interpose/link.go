package interpose

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"sync"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/seed"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Correlation is ledger job 5. It produces the link object a transcript frame
// carries, and it lives here rather than in the oracle because only the
// interposer knows what it originated -- an oracle reading a finished file
// could only guess. See docs/design/interposer.md section 6.

// Confidence values for an inferred join.
//
// They rank joins; they are not probabilities, and nothing computes with them.
// The credibility rule means no gateway verdict may rest on an inferred join
// at all (docs/design/transcript.md section 4), so what these numbers actually
// affect is what a person reading a report sees -- which is why there are two
// named constants rather than a formula implying a precision charpy does not
// have.
const (
	// ConfidenceUniqueContent is a join where exactly one originated frame
	// carried this content. The scenario player makes each call distinct
	// inside values the tool's schema accepts, so a unique match is strong --
	// but still not authoritative, because only propagated trace context is.
	ConfidenceUniqueContent = 0.9

	// ConfidenceOrderedContent is a join where several frames carried the
	// same content and the oldest unclaimed one was taken. Ordering is a
	// tie-breaker, not evidence.
	ConfidenceOrderedContent = 0.6
)

// DefaultJoinWindow is how many originated frames stay available to join
// against. Like the resolved-exchange window it is a window rather than a
// history: a forwarded copy arrives within milliseconds, and an unbounded
// table would let charpy match a frame against content from ten minutes ago
// and call it a correlation.
const DefaultJoinWindow = 4096

// NewJoinID returns a fresh charpy join id: charpy's own key, distinct from
// the W3C trace id beside it, which belongs to whoever started the trace.
func NewJoinID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b[:])
}

// Forwarded builds the authoritative link for a frame charpy relayed itself.
//
// In proxy and stdio-ingress modes charpy carries the bytes across, so it
// stamps one id on both copies and the join is a fact rather than a finding.
// Contrast the gateway case, where the subject forwards and charpy cannot
// carry anything across at all.
func Forwarded(joinID string) transcript.Link {
	return transcript.Link{CharpyID: joinID, Via: transcript.ViaForwarded}
}

// TraceContext is the W3C trace context a frame carries in _meta per SEP-414,
// where traceparent, tracestate and baggage are an explicit exception to the
// reverse-DNS prefix rule.
type TraceContext struct {
	TraceID    string
	SpanID     string
	Flags      string
	TraceState string
	Baggage    string
}

// Tracer issues the trace context charpy stamps on what one of its peers
// originates (ADR-005), drawn from a seeded stream (seed.Trace) so the ids
// reproduce with the citation. Safe for concurrent use; ids are issued in
// the order asked for, which for a scripted peer is the script's.
type Tracer struct {
	mu sync.Mutex
	r  *mrand.Rand
}

// NewTracer draws trace contexts from r.
func NewTracer(r *mrand.Rand) *Tracer { return &Tracer{r: r} }

// TraceFor is the traceparent source for one of charpy's peers in a run:
// who names the peer, so each draws a stream of its own and adding a peer
// leaves the others' ids where they were.
func TraceFor(runSeed, caseID, who string) func() string {
	return NewTracer(seed.For(runSeed, caseID, seed.Trace+"/"+who)).Traceparent
}

// Traceparent is the next trace context, rendered for _meta and the header.
// Every request gets a trace of its own: a gateway that forwards it carries
// the trace across, and that is the join.
func (t *Tracer) Traceparent() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var trace, span [16]byte
	for !nonzero(trace[:]) {
		binary.BigEndian.PutUint64(trace[:8], t.r.Uint64())
		binary.BigEndian.PutUint64(trace[8:], t.r.Uint64())
	}
	for !nonzero(span[:8]) {
		binary.BigEndian.PutUint64(span[:8], t.r.Uint64())
	}
	return TraceContext{TraceID: hex.EncodeToString(trace[:]), SpanID: hex.EncodeToString(span[:8])}.FormatTraceparent()
}

func nonzero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

// FormatTraceparent renders a traceparent header value.
func (t TraceContext) FormatTraceparent() string {
	flags := t.Flags
	if flags == "" {
		flags = "01"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + flags
}

// ParseTraceparent reads a W3C traceparent. It accepts only the version-00
// shape charpy emits and the field widths the spec fixes, because a
// half-recognised trace id joined against would be a correlation built on a
// misread.
func ParseTraceparent(v string) (TraceContext, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 || parts[0] != "00" {
		return TraceContext{}, false
	}
	if !isHex(parts[1], 32) || !isHex(parts[2], 16) || !isHex(parts[3], 2) {
		return TraceContext{}, false
	}
	// An all-zero id is defined as invalid, and joining on it would merge
	// every frame that carried one.
	if strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
		return TraceContext{}, false
	}
	return TraceContext{TraceID: parts[1], SpanID: parts[2], Flags: parts[3]}, true
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// TraceOf extracts the trace context a frame carries in its _meta.
func TraceOf(m envelope.Message) (TraceContext, bool) {
	var probe struct {
		Meta struct {
			Traceparent string `json:"traceparent"`
			Tracestate  string `json:"tracestate"`
			Baggage     string `json:"baggage"`
		} `json:"_meta"`
	}
	payload := payloadOf(m)
	if len(payload) == 0 || json.Unmarshal(payload, &probe) != nil {
		return TraceContext{}, false
	}
	tc, ok := ParseTraceparent(probe.Meta.Traceparent)
	if !ok {
		return TraceContext{}, false
	}
	tc.TraceState, tc.Baggage = probe.Meta.Tracestate, probe.Meta.Baggage
	return tc, true
}

// ContentDigest is the key an inferred join recalls against: a digest of what
// a frame *says*, with everything a forwarder is free to change left out.
//
// The envelope is excluded because a gateway rewrites the id, which is the
// whole reason joining on id is wrong by construction. Top-level _meta is
// excluded because a gateway stripping it is exactly the population that
// forces the inferred regime in the first place -- digesting it would
// guarantee a miss for every frame this mechanism exists to serve.
//
// Keys are sorted and whitespace dropped, because a forwarder may re-serialise
// an object without changing its meaning. Numbers keep their literal form
// rather than being normalised: re-spelling 1.0 as 1 produces a different
// digest and therefore no join, and a missed join degrades to via = "none",
// which is safe. A join that should not have been made is not.
//
// A frame with no payload to digest returns the empty string and cannot be
// content-joined at all -- and so does one whose payload is empty once _meta
// is set aside. notifications/initialized, a bare ping, an unparameterised
// listing: every session sends the same bytes, so a match says nothing about
// which message it was, and joining on one is exactly the confident wrong
// answer the credibility rule forbids. A gateway's own initialized was
// joined to charpy's that way before this rule.
func ContentDigest(m envelope.Message) string {
	payload := payloadOf(m)
	if len(payload) == 0 {
		return ""
	}
	canon, ok := canonical(payload)
	if !ok || string(canon) == "{}" || string(canon) == "null" {
		return ""
	}
	sum := sha256.Sum256([]byte(m.Method + "\x00" + string(canon)))
	return hex.EncodeToString(sum[:])
}

// payloadOf is the part of a frame a forwarder carries across unchanged:
// params on the way out, result on the way back. An error frame has no
// content join, which is deliberate -- error bodies repeat across unrelated
// exchanges and would join things that merely failed the same way.
func payloadOf(m envelope.Message) json.RawMessage {
	switch m.Kind {
	case envelope.KindRequest, envelope.KindNotification:
		return m.Params
	case envelope.KindResponse:
		return m.Result
	default:
		return nil
	}
}

// canonical re-renders JSON with object keys sorted, whitespace dropped and
// any top-level _meta removed. encoding/json sorts map keys on the way out, so
// decoding and re-encoding does most of the work; UseNumber is what keeps a
// number's literal spelling.
func canonical(payload json.RawMessage) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if obj, ok := v.(map[string]any); ok {
		delete(obj, "_meta")
	}

	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}

// joinEntry is one originated frame available to be joined against.
type joinEntry struct {
	joinID  string
	at      clock.Mono
	claimed bool
}

// joinEntries is held behind a pointer so a claim mutates the stored value in
// place. Writing it back through the cache would work, but it would also move
// the key to the newest position and quietly turn the window from
// first-in-first-out into something closer to an LRU -- the exact property the
// FIFO store exists to prevent.
type joinEntries struct{ e []joinEntry }

// Originated registers a frame charpy put on the wire and returns the link to
// stamp on it.
//
// Both recalls are indexed here: the trace id, for a subject that propagates
// context, and the content digest, for one that does not. Which of them the
// far copy arrives with is the subject's choice, not charpy's, so both are
// kept.
// The via is the run's, not the frame's: docs/design/transcript.md section 4
// maps each regime to a mode, and only the driver knows which it is running.
// It must be an authoritative one -- charpy minted this key itself, so
// recording the origin as a guess would be a lie in the other direction.
func (l *Ledger) Originated(joinID string, m envelope.Message, via transcript.Via) (transcript.Link, error) {
	switch via {
	case transcript.ViaForwarded, transcript.ViaTraced:
	default:
		return transcript.Link{}, fmt.Errorf(
			"interpose: originated frames join via forwarded or traced, not %q", via)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	link := transcript.Link{CharpyID: joinID, Via: via}

	if tc, ok := TraceOf(m); ok {
		l.byTrace.Put(tc.TraceID, joinID)
		link.TraceID, link.SpanID = tc.TraceID, tc.SpanID
	} else if via == transcript.ViaTraced {
		// A frame charpy sent without a trace -- one a fault synthesized,
		// say -- joins nothing. Recording it as traced would claim an
		// authority it does not carry; and recording it as none while still
		// offering its content for recall would give the other face's copy
		// a charpy_id that no frame on this face carries -- a key that joins
		// nothing. So it is not offered at all: what is lost is an inferred
		// join, which no verdict may rest on.
		return transcript.Link{Via: transcript.ViaNone}, nil
	}
	if d := ContentDigest(m); d != "" {
		entries, ok := l.byDigest.Get(d)
		if !ok {
			entries = &joinEntries{}
			l.byDigest.Put(d, entries)
		}
		entries.e = append(entries.e, joinEntry{joinID: joinID, at: l.sched.Now()})
	}
	return link, nil
}

// LinkFor finds the join key for a frame charpy did not originate -- the
// subject's forwarded copy.
//
// The regimes are tried in order of authority, and the order is the point.
// Propagated trace context is authoritative; a content recall never is, however
// distinctive the content. Falling through to none is the correct answer when
// neither is available, and is better than the alternative: an invariant that
// requires correlation is skipped, with the degraded join as the reason, rather
// than evaluated on a guess.
//
// A subject that drops trace context is separately a reportable finding, since
// it breaks tracing for everyone downstream of it. The correlation mechanism
// doubles as a test, which was a pleasant result rather than a designed one.
func (l *Ledger) LinkFor(m envelope.Message) transcript.Link {
	l.mu.Lock()
	defer l.mu.Unlock()

	if tc, ok := TraceOf(m); ok {
		if joinID, found := l.byTrace.Get(tc.TraceID); found {
			return transcript.Link{
				CharpyID: joinID,
				Via:      transcript.ViaTraced,
				TraceID:  tc.TraceID,
				SpanID:   tc.SpanID,
			}
		}
	}

	if d := ContentDigest(m); d != "" {
		if entries, found := l.byDigest.Get(d); found {
			if link, ok := entries.claim(); ok {
				return link
			}
		}
	}

	return transcript.Link{Via: transcript.ViaNone}
}

// claim takes the oldest unclaimed candidate. Ordering is how several frames
// with identical content are told apart, and it is a tie-breaker rather than
// evidence -- which is what the lower confidence records.
func (je *joinEntries) claim() (transcript.Link, bool) {
	for i := range je.e {
		if je.e[i].claimed {
			continue
		}
		je.e[i].claimed = true
		conf := ConfidenceUniqueContent
		if len(je.e) > 1 {
			conf = ConfidenceOrderedContent
		}
		return transcript.Link{
			CharpyID:   je.e[i].joinID,
			Via:        transcript.ViaInferred,
			Confidence: conf,
		}, true
	}
	// Every candidate is spoken for. A second forwarded copy of content
	// charpy sent once is not a join charpy can make, and inventing one is
	// exactly the confident wrong answer the credibility rule forbids.
	return transcript.Link{}, false
}
