package interpose

import (
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
)

// Matcher decides which cases' faults attach to a frame.
//
// It evaluates compiled policy and nothing else: it does not apply a fault,
// write a transcript line, or judge anything. Selection is the whole job.
type Matcher struct {
	cases  []Case
	ledger *Ledger
}

// NewMatcher returns a matcher over cases, sharing a ledger with the rest of
// the interposer. Cases are evaluated in the order given, which is catalogue
// order, so a report lists findings the way the manifest reads.
func NewMatcher(ledger *Ledger, cases ...Case) *Matcher {
	return &Matcher{cases: cases, ledger: ledger}
}

// Cases returns the compiled cases the matcher holds.
func (m *Matcher) Cases() []Case { return m.cases }

// Select returns every case whose fault attaches to f.
//
// The order of evaluation is semantic rather than incidental. `occurrence`
// means "the Nth *matching* frame within the scope", so every dimension
// predicate is evaluated first, the counter is incremented only if all of them
// pass, and the ordinal is compared last. after_mono_ms is one of those
// predicates, not a post-filter: a frame arriving before the case's clock gate
// is not a matching frame, and must not consume an ordinal that a later frame
// is waiting for.
//
// A frame may match several cases. Deciding what to do when two faults want
// the same frame belongs to whatever applies them, not here -- the matcher
// reports what the policy says and does not arbitrate.
func (m *Matcher) Select(f Frame) []Case {
	f.Method = m.methodFor(f)
	now := m.ledger.Now()

	var out []Case
	for _, c := range m.cases {
		if !c.Match.predicates(f, now) {
			continue
		}
		n := m.ledger.Tally(c.ID, c.Match.Scope, f.dimension(c.Match.Scope))
		if !c.Match.ordinal(n) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// methodFor fills in the method of a frame that does not carry one.
//
// A JSON-RPC response has no method member, so a case selecting on
// method = "tools/call" can only reach one through the ledger, keyed by the
// id that crossed the wire. An id the ledger has never seen resolves to
// nothing, and a case naming a method then does not match -- which is correct:
// charpy cannot claim a frame is a tools/call response when it has no record
// of a tools/call request.
func (m *Matcher) methodFor(f Frame) string {
	if f.Method != "" {
		return f.Method
	}
	switch f.Kind {
	case envelope.KindResponse, envelope.KindError:
		if method, ok := m.ledger.MethodFor(f.Face, f.ConnID, f.ID); ok {
			return method
		}
	}
	return ""
}

// predicates reports whether every dimension key present in the match table is
// satisfied. Absent keys are zero-valued and match anything.
func (m Match) predicates(f Frame, now clock.Mono) bool {
	if m.Face != "" && m.Face != f.Face {
		return false
	}
	if m.Direction != "" && m.Direction != f.Direction {
		return false
	}
	if m.Kind != "" && m.Kind != f.Kind {
		return false
	}
	if !m.Method.Match(f.Method) {
		return false
	}
	if !m.ClientID.Match(f.ClientID) {
		return false
	}
	if !m.SessionID.Match(f.SessionID) {
		return false
	}
	// Not before this point on the injected clock. Evaluated here, with the
	// other predicates, so that an early frame does not spend an ordinal.
	if m.AfterMono > 0 && now < m.AfterMono {
		return false
	}
	return true
}

// ordinal reports whether the nth matching frame is the one this case wants.
//
// A match table with neither key selects the first matching frame, not every
// one. One fault is what a case means: a transcript carrying the same fault on
// every frame that fits is a puzzle rather than a finding (ADR-012), and under
// a relay, where charpy does not choose the traffic, "every" is a storm. A case
// that does want each frame says so, with occurrence_every = 1.
func (m Match) ordinal(n int64) bool {
	switch {
	case m.Occurrence > 0:
		return n == m.Occurrence
	case m.Every > 0:
		return n%m.Every == 0
	default:
		return n == 1
	}
}
