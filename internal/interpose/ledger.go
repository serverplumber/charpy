package interpose

import (
	"sync"

	"github.com/creachadair/mds/cache"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/transcript"
)

// DefaultResolvedWindow is how many resolved exchanges a connection remembers.
//
// The number is a window, not a history, and that is a correctness property
// before it is a memory one. A fault that wants "an id resolved earlier" means
// one resolved recently -- faults-and-cases.md describes the case as "a late
// duplicate for an id that was correctly resolved ten frames ago" -- and an
// unbounded table would let the ledger offer a match against something from
// ten minutes back, which is a correlation charpy cannot support.
const DefaultResolvedWindow = 4096

// Ledger is the interposer's state: double-entry, intent versus wire, kept per
// face and per connection.
//
// v0 implements two of the five jobs in docs/design/interposer.md section 3.
// Job 2, resolving responses to methods, because a JSON-RPC response does not
// carry one and [case.match] can select on it. Job 3, counting occurrences,
// keyed by (scope, dimension value) from the start -- never a single integer,
// because soak mode's population rates cannot grow out of a global count.
//
// Jobs 1, 4 and 5 -- rewrite inverses, consequence attribution and link
// emission -- wait for the three verbs, which is the only thing that could
// exercise them.
//
// Retention, per table:
//
//	in-flight exchanges   bounded by resolution, or by connection close
//	resolved exchanges    bounded by a FIFO window, DefaultResolvedWindow
//	occurrence counters   bounded by cases times live scope values
//
// A Ledger is safe for concurrent use: the interposer reads it from every
// connection goroutine.
type Ledger struct {
	mu     sync.Mutex
	sched  clock.Sched
	window int

	conns  map[connKey]*connState
	counts map[countKey]int64
}

type connKey struct {
	face transcript.Face
	conn string
}

type countKey struct {
	caseID string
	scope  Scope
	dim    string
}

type connState struct {
	inflight map[string]Exchange
	resolved *cache.Cache[string, Exchange]
}

// Exchange is what the ledger remembers about a request.
//
// IntentID is what the origination source believes it sent; WireID is what
// actually crossed. They differ whenever the interposer rewrote an id, and
// keeping both is what lets charpy un-rewrite a subject's answer before its
// own SDK sees it. In v0 nothing rewrites yet, so they are equal.
type Exchange struct {
	IntentID envelope.ID
	WireID   envelope.ID
	Method   string
	At       clock.Mono
}

// LedgerOption configures a Ledger.
type LedgerOption func(*Ledger)

// WithResolvedWindow sets how many resolved exchanges a connection remembers.
func WithResolvedWindow(n int) LedgerOption {
	return func(l *Ledger) {
		if n > 0 {
			l.window = n
		}
	}
}

// NewLedger returns an empty ledger reading the injected clock.
//
// The clock is [clock.Sched] and not [clock.Wall] on purpose: what the ledger
// records is charpy's own scheduling and the transcript's logical timeline,
// which ADR-001 puts on the injected clock. The two clock types do not satisfy
// each other's interface, so this cannot be got wrong by accident.
func NewLedger(sched clock.Sched, opts ...LedgerOption) *Ledger {
	l := &Ledger{
		sched:  sched,
		window: DefaultResolvedWindow,
		conns:  make(map[connKey]*connState),
		counts: make(map[countKey]int64),
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Now is the current point on the injected clock.
func (l *Ledger) Now() clock.Mono { return l.sched.Now() }

// Originate records a request in flight.
//
// The exchange is keyed by the wire id, because the subject answers what
// crossed the wire rather than what the origination source believed it sent.
// The key is [envelope.ID.Key], which is type-qualified, so the number 7 and
// the string "7" are two exchanges and not one -- a duplicate_id case with
// vary_type set puts both on the wire, and a ledger that merged them would
// wedge on charpy's own fault.
func (l *Ledger) Originate(face transcript.Face, conn string, e Exchange) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e.At = l.sched.Now()
	if !e.WireID.Present() {
		e.WireID = e.IntentID
	}
	if !e.IntentID.Present() {
		e.IntentID = e.WireID
	}
	l.connLocked(face, conn).inflight[e.WireID.Key()] = e
}

// Resolve marks a request answered and moves it into the resolved window,
// returning what the ledger knew about it.
//
// Resolving an id that is already resolved is not an error: a duplicate_id
// case answers the same id twice on purpose, and the second answer must still
// resolve to a method rather than falling off the end of the bookkeeping.
func (l *Ledger) Resolve(face transcript.Face, conn string, wire envelope.ID) (Exchange, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cs := l.connLocked(face, conn)
	key := wire.Key()

	if e, ok := cs.inflight[key]; ok {
		delete(cs.inflight, key)
		cs.resolved.Put(key, e)
		return e, true
	}
	return cs.resolved.Get(key)
}

// MethodFor reports the method a response's id resolves to, without resolving
// it. The matcher needs it to evaluate [case.match]'s method key on a
// response, and matching must not be a state transition.
//
// It consults in-flight exchanges first and the resolved window second, so a
// late duplicate for an already-answered id still names its method -- which is
// exactly the frame unsolicited_response and duplicate_id produce.
func (l *Ledger) MethodFor(face transcript.Face, conn string, wire envelope.ID) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cs := l.connLocked(face, conn)
	key := wire.Key()
	if e, ok := cs.inflight[key]; ok {
		return e.Method, true
	}
	if e, ok := cs.resolved.Get(key); ok {
		return e.Method, true
	}
	return "", false
}

// InFlight reports how many requests are outstanding on a connection.
func (l *Ledger) InFlight(face transcript.Face, conn string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.connLocked(face, conn).inflight)
}

// CloseConn drops a connection's state. Both of its tables are bounded by the
// connection's lifetime, so this is what stops them accumulating across a run
// that opens and closes many.
func (l *Ledger) CloseConn(face transcript.Face, conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, connKey{face: face, conn: conn})
}

// Tally counts one matching frame and returns its 1-based ordinal within the
// scope.
//
// Counters are keyed by (case, scope, dimension value) rather than by case
// alone. In v0 there is one client and one session, so every scope collapses
// to run and the dimension is always empty -- but a matcher that has only ever
// counted globally cannot grow population rates, and that is a data-structure
// decision which is cheap now and invasive later. See
// docs/design/policy-format.md section 2.
func (l *Ledger) Tally(caseID string, scope Scope, dim string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := countKey{caseID: caseID, scope: scope, dim: dim}
	l.counts[k]++
	return l.counts[k]
}

// Tallied reports the current ordinal without counting.
func (l *Ledger) Tallied(caseID string, scope Scope, dim string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[countKey{caseID: caseID, scope: scope, dim: dim}]
}

// connLocked returns the state for a connection, creating it on first use.
// Caller holds the lock.
func (l *Ledger) connLocked(face transcript.Face, conn string) *connState {
	k := connKey{face: face, conn: conn}
	cs, ok := l.conns[k]
	if !ok {
		cs = &connState{
			inflight: make(map[string]Exchange),
			resolved: cache.New(FIFO[string, Exchange]().WithLimit(int64(l.window))),
		}
		l.conns[k] = cs
	}
	return cs
}
