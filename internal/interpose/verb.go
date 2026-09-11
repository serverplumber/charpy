package interpose

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Verb is one of the three things the interposer does to traffic in flight.
// Every mechanism in the v0 catalogue reduces to these, which is what collapsed
// the "feral peer" to frame templates; see docs/design/interposer.md section 2.
type Verb string

const (
	// VerbRewrite changes the bytes of a frame in flight.
	VerbRewrite Verb = "rewrite"
	// VerbWithhold delays, reorders or never delivers a frame.
	VerbWithhold Verb = "withhold"
	// VerbSynthesize injects a frame no source emitted, or swallows one no
	// destination sees.
	VerbSynthesize Verb = "synthesize"
)

// Interposer applies faults to traffic in flight.
//
// It originates nothing and judges nothing: a scenario player or a relay hands
// it frames, it decides what the wire sees, and the ledger keeps the two
// columns straight. The verbs here are the entire vocabulary of that
// falsification.
type Interposer struct {
	ledger *Ledger
	sched  clock.Sched
}

// New returns an interposer over a ledger.
//
// The clock is [clock.Sched] because everything scheduled here -- when a
// withheld frame is released -- is charpy's own scheduling, which ADR-001 puts
// on the injected clock. Subject deadlines are real and belong to [clock.Wall],
// and the two types do not satisfy each other's interface.
func New(ledger *Ledger, sched clock.Sched) *Interposer {
	return &Interposer{ledger: ledger, sched: sched}
}

// Ledger is the interposer's state.
func (i *Interposer) Ledger() *Ledger { return i.ledger }

// Rewritten records a frame whose bytes were changed on the way to the wire.
type Rewritten struct {
	Case Case
	// Intent is the frame the origination source emitted and believes it
	// sent. Wire is what actually crossed.
	Intent envelope.Message
	Wire   envelope.Message
}

// Rewrite changes a frame's bytes in flight.
//
// When the rewrite changes the JSON-RPC id it registers the inverse, and that
// is not bookkeeping for its own sake. The subject answers the id it saw; the
// reference peer is waiting for the id it sent. Without the back-translation
// the SDK's own pending map wedges and the scenario dies of charpy's fault
// rather than the subject's -- which is the cost self-MITM incurs and the
// reason the ledger is double-entry at all.
func (i *Interposer) Rewrite(f Frame, c Case, intent, wire envelope.Message) Rewritten {
	if !intent.ID.Equal(wire.ID) {
		i.ledger.RegisterRewrite(f.Face, f.ConnID, intent.ID, wire.ID)
	}
	i.ledger.BeginConsequences(f.Face, f.ConnID, c)
	return Rewritten{Case: c, Intent: intent, Wire: wire}
}

// Withheld is a frame the interposer is holding back.
//
// Holding is an action with a lifetime, not a decision taken once: a case
// declares when the fault is withdrawn, the liveness clock starts at that
// instant, and the control plane can withdraw everything early. So a hold is a
// value a caller keeps rather than a branch it takes.
type Withheld struct {
	c   Case
	m   envelope.Message
	mu  sync.Mutex
	t   *clock.Timer
	at  clock.Mono
	out chan struct{}
	rel bool
}

// Withhold holds a frame back, releasing it after the given delay on the
// injected clock. A delay of zero or less holds it indefinitely -- the
// mechanism's withdraw_after_ms = 0 -- until something calls Release.
func (i *Interposer) Withhold(f Frame, c Case, m envelope.Message, after time.Duration) *Withheld {
	w := &Withheld{c: c, m: m, at: -1, out: make(chan struct{})}
	if after > 0 {
		w.t = i.sched.AfterFunc(after, func() { w.release(i.sched.Now()) })
	}
	i.ledger.BeginConsequences(f.Face, f.ConnID, c)
	return w
}

// Case is the case that caused the hold.
func (w *Withheld) Case() Case { return w.c }

// Message is the frame being held.
func (w *Withheld) Message() envelope.Message { return w.m }

// Held reports whether the frame is still being withheld.
func (w *Withheld) Held() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.rel
}

// Withdrawn fires when the hold ends, whether on the clock or by hand. It is
// where the liveness clock starts, so a probe waits on it rather than guessing
// at the moment.
func (w *Withheld) Withdrawn() <-chan struct{} { return w.out }

// WithdrawnAt is the instant the hold ended, or -1 while it is still held.
func (w *Withheld) WithdrawnAt() clock.Mono {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

// Release ends the hold now, reporting whether it was still held. It is the
// manual form of what a case's withdrawal does automatically, and what
// POST /control/withdraw calls.
func (w *Withheld) Release(now clock.Mono) bool { return w.release(now) }

func (w *Withheld) release(now clock.Mono) bool {
	w.mu.Lock()
	if w.rel {
		w.mu.Unlock()
		return false
	}
	w.rel, w.at = true, now
	if w.t != nil {
		w.t.Stop()
		w.t = nil
	}
	w.mu.Unlock()

	close(w.out)
	return true
}

// Synthesized records a frame charpy authored that no origination source
// emitted.
type Synthesized struct {
	Case    Case
	Message envelope.Message
}

// Synthesize authors a frame onto the wire.
//
// This is the verb that makes the corruptor an author, and it is deliberately
// bounded: the templates below build frames, not a protocol implementation.
// Anything needing more than a template is SDK configuration instead -- which
// is the observation that removed the feral peer.
func (i *Interposer) Synthesize(f Frame, c Case, m envelope.Message) Synthesized {
	i.ledger.BeginConsequences(f.Face, f.ConnID, c)
	return Synthesized{Case: c, Message: m}
}

// Swallowed records a frame that was dropped before any destination saw it.
type Swallowed struct {
	Case    Case
	Message envelope.Message
}

// Swallow drops a frame so that nothing downstream ever sees it.
//
// It is synthesis in reverse and belongs to the same verb: manifest_mutate
// with notify = "silent" is a legitimate reconfiguration whose *notification*
// is swallowed, and the suppression is the entire fault.
func (i *Interposer) Swallow(f Frame, c Case, m envelope.Message) Swallowed {
	i.ledger.BeginConsequences(f.Face, f.ConnID, c)
	return Swallowed{Case: c, Message: m}
}

// The synthesis templates. They are frame builders and nothing more: every one
// produces bytes that go on the wire exactly as built, including the ones no
// conforming implementation would emit.

// TemplateResult authors a response for an id, whether or not anything asked.
// It backs unsolicited_response and duplicate_id's double_response replay.
func TemplateResult(id envelope.ID, result json.RawMessage) (envelope.Message, error) {
	return envelope.NewResponse(id, result)
}

// TemplateError authors an error response for an id.
func TemplateError(id envelope.ID, code int64, message string) (envelope.Message, error) {
	return envelope.NewError(id, code, message, nil)
}

// TemplateNotification authors a notification, which is how a manifest change
// is announced -- or, swallowed, how it is not.
func TemplateNotification(method string, params json.RawMessage) (envelope.Message, error) {
	return envelope.NewNotification(method, params)
}

// Replay re-emits a frame that already crossed the wire, byte for byte.
//
// It is the double_response arm of duplicate_id, and it goes through the
// message rather than rebuilding one so that the second copy is identical to
// the first -- a re-serialised replay would differ in ways the subject might
// legitimately notice, which would make the fault something other than the one
// the case names.
func Replay(m envelope.Message) envelope.Message { return m }

// TranscriptFault renders a case for a transcript line's fault object, which
// is how a frame carries its attribution.
func (c Case) TranscriptFault() *transcript.Fault {
	return &transcript.Fault{
		CaseID:   c.ID,
		Citation: c.Citation,
		Kind:     c.Fault.Kind,
		Params:   c.Fault.Params,
	}
}
