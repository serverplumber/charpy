package interpose_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

const conn = "c-04"

func originate(l *interpose.Ledger, id envelope.ID, method string) {
	l.Originate(transcript.Downstream, conn, transcript.C2S, interpose.Exchange{IntentID: id, Method: method})
}

func TestLedgerResolvesResponsesToMethods(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	originate(l, envelope.NumberID(7), "tools/call")

	// A response carries no method; the ledger is the only way to one.
	if got, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); !ok || got != "tools/call" {
		t.Errorf("MethodFor(7) = %q,%v want tools/call,true", got, ok)
	}
	if l.InFlight(transcript.Downstream, conn, transcript.C2S) != 1 {
		t.Error("a lookup must not resolve the exchange")
	}

	e, ok := l.Resolve(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7))
	if !ok || e.Method != "tools/call" {
		t.Fatalf("Resolve(7) = %+v,%v", e, ok)
	}
	if l.InFlight(transcript.Downstream, conn, transcript.C2S) != 0 {
		t.Error("Resolve did not clear the in-flight entry")
	}
}

// The frame duplicate_id and unsolicited_response produce: a second answer for
// an id already resolved. It must still name its method rather than falling
// off the end of the bookkeeping.
func TestLedgerRemembersResolvedExchanges(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	originate(l, envelope.NumberID(7), "tools/call")
	l.Resolve(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7))

	if got, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); !ok || got != "tools/call" {
		t.Errorf("after resolution MethodFor(7) = %q,%v want tools/call,true", got, ok)
	}
	if _, ok := l.Resolve(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); !ok {
		t.Error("resolving an already-resolved id must not fail; a case answers one twice on purpose")
	}
}

// envelope.ID.Key is type-qualified precisely so this does not merge. A
// duplicate_id case with vary_type puts both on the wire, and a ledger that
// confused them would wedge on charpy's own fault.
func TestLedgerDoesNotMergeNumberAndStringIDs(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	originate(l, envelope.NumberID(7), "tools/call")
	originate(l, envelope.StringID("7"), "tools/list")

	if l.InFlight(transcript.Downstream, conn, transcript.C2S) != 2 {
		t.Fatalf("InFlight = %d, want 2: 7 and \"7\" are different ids",
			l.InFlight(transcript.Downstream, conn, transcript.C2S))
	}
	if got, _ := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); got != "tools/call" {
		t.Errorf("MethodFor(7) = %q", got)
	}
	if got, _ := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.StringID("7")); got != "tools/list" {
		t.Errorf(`MethodFor("7") = %q`, got)
	}
}

// The resolved table is a window, not a history. An id resolved long ago must
// stop being matchable, or the ledger will offer a join charpy cannot support.
func TestResolvedExchangesAreAWindow(t *testing.T) {
	const window = 4
	l := interpose.NewLedger(clock.NewInjected(), interpose.WithResolvedWindow(window))

	for i := range int64(20) {
		id := envelope.NumberID(i)
		originate(l, id, fmt.Sprintf("m%d", i))
		l.Resolve(transcript.Downstream, conn, transcript.C2S, id)
	}

	if _, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(0)); ok {
		t.Error("the oldest resolved exchange is still matchable; the window is not bounding")
	}
	if _, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(19)); !ok {
		t.Error("the newest resolved exchange was evicted")
	}
}

// Both connection tables are bounded by the connection's lifetime. Without
// this, a run that opens and closes many connections accumulates all of them.
func TestCloseConnDropsState(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	originate(l, envelope.NumberID(7), "tools/call")

	l.CloseConn(transcript.Downstream, conn)

	if l.InFlight(transcript.Downstream, conn, transcript.C2S) != 0 {
		t.Error("closing a connection left in-flight state behind")
	}
	if _, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); ok {
		t.Error("closing a connection left resolved state behind")
	}
}

func TestLedgerKeepsFacesAndConnectionsApart(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	l.Originate(transcript.Downstream, "c-1", transcript.C2S, interpose.Exchange{IntentID: envelope.NumberID(1), Method: "tools/call"})
	l.Originate(transcript.Upstream, "c-2", transcript.C2S, interpose.Exchange{IntentID: envelope.NumberID(1), Method: "tools/list"})

	down, _ := l.MethodFor(transcript.Downstream, "c-1", transcript.C2S, envelope.NumberID(1))
	up, _ := l.MethodFor(transcript.Upstream, "c-2", transcript.C2S, envelope.NumberID(1))
	if down != "tools/call" || up != "tools/list" {
		t.Errorf("faces or connections share an id space: downstream=%q upstream=%q", down, up)
	}
	if _, ok := l.MethodFor(transcript.Downstream, "c-2", transcript.C2S, envelope.NumberID(1)); ok {
		t.Error("an id leaked across connections")
	}
}

func TestOriginateStampsTheInjectedClock(t *testing.T) {
	sched := clock.NewInjected()
	l := interpose.NewLedger(sched)
	sched.Advance(250 * time.Millisecond)

	originate(l, envelope.NumberID(1), "ping")
	e, _ := l.Resolve(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(1))

	if e.At != clock.FromMillis(250) {
		t.Errorf("exchange stamped at %v, want 250ms on the injected clock", e.At)
	}
}

// Counters are keyed by (case, scope, dimension) from v0, even though every
// scope collapses to run here. A matcher that has only ever counted globally
// cannot grow population rates.
func TestTallyIsKeyedByScopeAndDimension(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())

	if n := l.Tally("id/dup", interpose.ScopeSession, "s-1"); n != 1 {
		t.Errorf("first tally = %d, want 1 (ordinals are 1-based)", n)
	}
	if n := l.Tally("id/dup", interpose.ScopeSession, "s-1"); n != 2 {
		t.Errorf("second tally = %d, want 2", n)
	}
	if n := l.Tally("id/dup", interpose.ScopeSession, "s-2"); n != 1 {
		t.Errorf("a different session counted %d, want its own 1", n)
	}
	if n := l.Tally("stream/trunc", interpose.ScopeSession, "s-1"); n != 1 {
		t.Errorf("a different case counted %d, want its own 1", n)
	}
	if got := l.Tallied("id/dup", interpose.ScopeSession, "s-1"); got != 2 {
		t.Errorf("Tallied = %d, want 2 without counting", got)
	}
}

// The interposer reads the ledger from every connection goroutine, which is
// why just check runs under -race.
func TestLedgerIsConcurrencySafe(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := fmt.Sprintf("c-%d", w)
			for i := range int64(200) {
				id := envelope.NumberID(i)
				l.Originate(transcript.Downstream, c, transcript.C2S, interpose.Exchange{IntentID: id, Method: "ping"})
				l.MethodFor(transcript.Downstream, c, transcript.C2S, id)
				l.Resolve(transcript.Downstream, c, transcript.C2S, id)
				l.Tally("id/dup", interpose.ScopeConnection, c)
			}
			l.CloseConn(transcript.Downstream, c)
		}()
	}
	wg.Wait()

	for w := range 8 {
		if got := l.Tallied("id/dup", interpose.ScopeConnection, fmt.Sprintf("c-%d", w)); got != 200 {
			t.Errorf("connection %d tallied %d, want 200", w, got)
		}
	}
}

// JSON-RPC ids are the sender's. Once a server asks its client something --
// sampling, a ping -- the same id is in flight both ways at once, and an
// answer names the method of the request that came the other way to it, not
// of whichever request happens to share its number.
func TestEachDirectionHasItsOwnIDSpace(t *testing.T) {
	l := interpose.NewLedger(clock.NewInjected())
	one := envelope.NumberID(1)
	l.Originate(transcript.Downstream, conn, transcript.C2S, interpose.Exchange{IntentID: one, Method: "initialize"})
	l.Originate(transcript.Downstream, conn, transcript.S2C, interpose.Exchange{IntentID: one, Method: "sampling/createMessage"})

	// The client's answer travels c2s, so it answers the server's request.
	if m, _ := l.MethodFor(transcript.Downstream, conn, transcript.S2C, one); m != "sampling/createMessage" {
		t.Errorf("the client's answer resolves to %q, want sampling/createMessage", m)
	}
	if m, _ := l.MethodFor(transcript.Downstream, conn, transcript.C2S, one); m != "initialize" {
		t.Errorf("the server's answer resolves to %q, want initialize", m)
	}

	// Resolving one leaves the other in flight.
	l.Resolve(transcript.Downstream, conn, transcript.S2C, one)
	if n := l.InFlight(transcript.Downstream, conn, transcript.C2S); n != 1 {
		t.Errorf("the client's request was resolved by an answer to the server's: in flight %d", n)
	}
}
