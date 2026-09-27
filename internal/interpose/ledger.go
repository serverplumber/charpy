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
// Four of the five jobs in docs/design/interposer.md section 3 are here.
// Job 1, keeping the reference peer sane, by registering the inverse of every
// id the interposer rewrites. Job 2, resolving responses to methods, because a
// JSON-RPC response does not carry one and [case.match] can select on it.
// Job 3, counting occurrences, keyed by (scope, dimension value) from the
// start -- never a single integer, because soak mode's population rates cannot
// grow out of a global count. Job 4, attributing what follows a lie to the
// fault that caused it rather than to the subject.
//
// Job 5, emitting the link object, is in link.go -- and its tables are the
// ledger's only run-scoped state, because a join crosses faces by definition.
//
// Retention, per table:
//
//	in-flight exchanges   bounded by resolution, or by connection close
//	resolved exchanges    bounded by a FIFO window, DefaultResolvedWindow
//	occurrence counters   bounded by cases times live scope values
//	consequence markers   bounded by the lifetime of the lie
//	join tables           bounded by a FIFO window, DefaultJoinWindow
//
// A Ledger is safe for concurrent use: the interposer reads it from every
// connection goroutine.
type Ledger struct {
	mu         sync.Mutex
	sched      clock.Sched
	window     int
	joinWindow int

	conns  map[connKey]*connState
	counts map[countKey]int64

	// The correlation tables are run-scoped rather than per connection: a
	// join crosses faces, and the two faces of one exchange are by definition
	// different connections. Both are windows, for the same reason the
	// resolved-exchange table is.
	byTrace  *cache.Cache[string, string]
	byDigest *cache.Cache[string, *joinEntries]
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
	// spaces holds one id space per direction a request can travel. JSON-RPC
	// ids are the sender's: a client's request 1 and a server's request 1 are
	// two exchanges, and once a server asks its client something -- sampling,
	// a ping -- a ledger keyed on the id alone resolves the client's answer
	// against the client's own request, names it with the wrong method, and
	// the matcher misses the frame a case was written for.
	spaces map[transcript.Direction]*idSpace
	// lie is the case whose fault the next frames on this connection are
	// downstream of, if any. See BeginConsequences.
	lie   Case
	lying bool
}

// idSpace is the bookkeeping for the requests one side of a connection sent.
type idSpace struct {
	inflight map[string]Exchange
	resolved *cache.Cache[string, Exchange]
	// last is the id most recently resolved in this space, for
	// unsolicited_response's already_resolved source. A window would do, but
	// the mechanism wants "an id resolved earlier" and the most recent one is
	// the least stale answer to that.
	last     envelope.ID
	haveLast bool
}

// Exchange is what the ledger remembers about a request.
//
// IntentID is what the origination source believes it sent; WireID is what
// actually crossed. They differ whenever the interposer rewrote an id, and
// keeping both is what lets charpy un-rewrite a subject's answer before its
// own SDK sees it. They are equal on any frame the interposer left alone.
type Exchange struct {
	IntentID envelope.ID
	WireID   envelope.ID
	Method   string
	// Tool is the tool a tools/call named. An answer carries neither method
	// nor tool, and a fault against a tool's declared output has to know
	// whose declaration it is breaking.
	Tool string
	At   clock.Mono
}

// LedgerOption configures a Ledger.
type LedgerOption func(*Ledger)

// WithJoinWindow sets how many originated frames stay available to join
// against.
func WithJoinWindow(n int) LedgerOption {
	return func(l *Ledger) {
		if n > 0 {
			l.joinWindow = n
		}
	}
}

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
		sched:      sched,
		window:     DefaultResolvedWindow,
		joinWindow: DefaultJoinWindow,
		conns:      make(map[connKey]*connState),
		counts:     make(map[countKey]int64),
	}
	for _, o := range opts {
		o(l)
	}
	l.byTrace = cache.New(FIFO[string, string]().WithLimit(int64(l.joinWindow)))
	l.byDigest = cache.New(FIFO[string, *joinEntries]().WithLimit(int64(l.joinWindow)))
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
func (l *Ledger) Originate(face transcript.Face, conn string, origin transcript.Direction, e Exchange) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e.At = l.sched.Now()
	if !e.WireID.Present() {
		e.WireID = e.IntentID
	}
	if !e.IntentID.Present() {
		e.IntentID = e.WireID
	}
	l.spaceLocked(face, conn, origin).inflight[e.WireID.Key()] = e
}

// Resolve marks a request answered and moves it into the resolved window,
// returning what the ledger knew about it.
//
// Resolving an id that is already resolved is not an error: a duplicate_id
// case answers the same id twice on purpose, and the second answer must still
// resolve to a method rather than falling off the end of the bookkeeping.
func (l *Ledger) Resolve(face transcript.Face, conn string, origin transcript.Direction, wire envelope.ID) (Exchange, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sp := l.spaceLocked(face, conn, origin)
	key := wire.Key()

	if e, ok := sp.inflight[key]; ok {
		delete(sp.inflight, key)
		sp.resolved.Put(key, e)
		sp.last, sp.haveLast = wire, true
		return e, true
	}
	return sp.resolved.Get(key)
}

// MethodFor reports the method a response's id resolves to, without resolving
// it. The matcher needs it to evaluate [case.match]'s method key on a
// response, and matching must not be a state transition.
//
// It consults in-flight exchanges first and the resolved window second, so a
// late duplicate for an already-answered id still names its method -- which is
// exactly the frame unsolicited_response and duplicate_id produce.
func (l *Ledger) MethodFor(face transcript.Face, conn string, origin transcript.Direction, wire envelope.ID) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sp := l.spaceLocked(face, conn, origin)
	key := wire.Key()
	if e, ok := sp.inflight[key]; ok {
		return e.Method, true
	}
	if e, ok := sp.resolved.Get(key); ok {
		return e.Method, true
	}
	return "", false
}

// ToolFor reports the tool a tools/call answer's id resolves to, the way
// MethodFor reports its method.
func (l *Ledger) ToolFor(face transcript.Face, conn string, origin transcript.Direction, wire envelope.ID) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sp := l.spaceLocked(face, conn, origin)
	key := wire.Key()
	if e, ok := sp.inflight[key]; ok {
		return e.Tool, e.Tool != ""
	}
	if e, ok := sp.resolved.Get(key); ok {
		return e.Tool, e.Tool != ""
	}
	return "", false
}

// LatestResolved returns the id most recently resolved on a connection, for a
// fault that needs "an id resolved earlier" (unsolicited_response's
// already_resolved). Absent when nothing has resolved yet, which makes that
// case not apply rather than wrong. Callers read it before resolving the
// current frame, so it names a prior id and not the one in hand.
func (l *Ledger) LatestResolved(face transcript.Face, conn string, origin transcript.Direction) (envelope.ID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sp := l.spaceLocked(face, conn, origin)
	return sp.last, sp.haveLast
}

// InFlight reports how many requests are outstanding on a connection.
func (l *Ledger) InFlight(face transcript.Face, conn string, origin transcript.Direction) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.spaceLocked(face, conn, origin).inflight)
}

// CloseConn drops a connection's state. Both of its tables are bounded by the
// connection's lifetime, so this is what stops them accumulating across a run
// that opens and closes many.
func (l *Ledger) CloseConn(face transcript.Face, conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, connKey{face: face, conn: conn})
}

// RegisterRewrite records that a frame the origination source emitted as
// intent went onto the wire as something else. This is ledger job 1.
//
// The exchange is re-keyed to the wire id, because that is the id the subject
// will answer, and the intent id is kept beside it so the answer can be
// translated back before the reference peer sees it. Charpy corrupts the id
// space it is itself tracking, which is exactly why a single-entry view is
// wedged by the first duplicate_id case.
//
// A rewrite of a frame the ledger never saw originated still registers: a
// relayed frame has no Originate call behind it, and its inverse matters just
// as much.
func (l *Ledger) RegisterRewrite(face transcript.Face, conn string, origin transcript.Direction, intent, wire envelope.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sp := l.spaceLocked(face, conn, origin)
	e, ok := sp.inflight[intent.Key()]
	if !ok {
		e = Exchange{At: l.sched.Now()}
	}
	delete(sp.inflight, intent.Key())
	e.IntentID, e.WireID = intent, wire
	sp.inflight[wire.Key()] = e
}

// Untranslate maps an id the subject answered back to the id the origination
// source is waiting for.
//
// Without it a rewritten id wedges the reference peer's own pending map and
// the scenario dies of charpy's fault rather than the subject's. An id that
// was never rewritten translates to itself, so a caller can apply this
// unconditionally.
func (l *Ledger) Untranslate(face transcript.Face, conn string, origin transcript.Direction, wire envelope.ID) (envelope.ID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sp := l.spaceLocked(face, conn, origin)
	e, ok := sp.inflight[wire.Key()]
	if !ok {
		e, ok = sp.resolved.Get(wire.Key())
	}
	if !ok || !e.IntentID.Present() {
		return wire, false
	}
	// The bool reports whether a translation actually happened, not whether
	// the ledger recognised the id. An untouched exchange carries the same id
	// in both columns, and saying "rewritten" about it would make the flag
	// useless for the transcript, which wants to record the ones that were.
	return e.IntentID, !e.IntentID.Equal(wire)
}

// BeginConsequences marks a connection as carrying a lie, so that what
// follows can be attributed to charpy rather than to the subject. This is
// ledger job 4.
//
// After a fault the reference peer keeps answering in its real configuration,
// and if the subject believed the lie its next frames are consequences of
// charpy's fault rather than behaviour worth reporting. Attribution has to be
// recorded at the moment of the lie; reconstructing it afterwards from a
// transcript means guessing which frames were downstream of what.
func (l *Ledger) BeginConsequences(face transcript.Face, conn string, c Case) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cs := l.connLocked(face, conn)
	cs.lie, cs.lying = c, true
}

// ConsequenceOf reports the case a frame on this connection is downstream of,
// if any. A transcript writer asks it so a frame carries its attribution.
func (l *Ledger) ConsequenceOf(face transcript.Face, conn string) (Case, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cs := l.connLocked(face, conn)
	return cs.lie, cs.lying
}

// EndConsequences stops attributing frames on a connection to a fault, which
// is what withdrawing one does: from here the subject is answering for itself
// again.
func (l *Ledger) EndConsequences(face transcript.Face, conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cs := l.connLocked(face, conn)
	cs.lie, cs.lying = Case{}, false
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
		cs = &connState{spaces: map[transcript.Direction]*idSpace{}}
		l.conns[k] = cs
	}
	return cs
}

// spaceLocked is the id space of the requests that travelled origin on a
// connection. An answer is looked up in the space opposite to its own
// direction: it answers a request that came the other way.
func (l *Ledger) spaceLocked(face transcript.Face, conn string, origin transcript.Direction) *idSpace {
	cs := l.connLocked(face, conn)
	sp, ok := cs.spaces[origin]
	if !ok {
		sp = &idSpace{
			inflight: make(map[string]Exchange),
			resolved: cache.New(FIFO[string, Exchange]().WithLimit(int64(l.window))),
		}
		cs.spaces[origin] = sp
	}
	return sp
}
