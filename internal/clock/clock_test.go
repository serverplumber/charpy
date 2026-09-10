package clock_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
)

var (
	_ clock.Sched = (*clock.Injected)(nil)
	_ clock.Wall  = (*clock.FixedWall)(nil)
)

// The two clocks are separate types so that reading the wrong one cannot
// compile. If Wall ever satisfies Sched, that guarantee is gone and the
// distinction is back to being a review comment.
func TestClocksAreNotInterchangeable(t *testing.T) {
	sched := reflect.TypeOf((*clock.Sched)(nil)).Elem()
	wall := reflect.TypeOf((*clock.Wall)(nil)).Elem()

	if reflect.TypeOf(clock.RealWall()).Implements(sched) {
		t.Error("a Wall satisfies Sched; injected and real time are confusable again")
	}
	if reflect.TypeOf(clock.NewInjected()).Implements(wall) {
		t.Error("a Sched satisfies Wall; injected and real time are confusable again")
	}
}

func TestInjectedStartsAtRunStart(t *testing.T) {
	c := clock.NewInjected()
	if got := c.Now(); got != 0 {
		t.Errorf("Now() = %v at construction, want 0: t_mono_ns counts from run start", got)
	}
	if got := c.Advance(1500 * time.Millisecond); got != clock.FromMillis(1500) {
		t.Errorf("Advance returned %v, want 1500ms", got)
	}
	if got := c.Now().Millis(); got != 1500 {
		t.Errorf("Now().Millis() = %d, want 1500", got)
	}
}

// A callback sees the instant it was scheduled for, not the end of the
// advance that ran it. Replay determinism depends on it: the transcript line
// a callback writes must carry its own deadline.
func TestCallbackSeesItsOwnDeadline(t *testing.T) {
	c := clock.NewInjected()

	var at clock.Mono
	c.AfterFunc(30*time.Millisecond, func() { at = c.Now() })
	c.Advance(time.Second)

	if at != clock.FromMillis(30) {
		t.Errorf("callback read Now() = %v, want 30ms", at)
	}
	if c.Now() != clock.FromMillis(1000) {
		t.Errorf("clock ended at %v, want 1000ms", c.Now())
	}
}

func TestTimersFireInDeadlineThenRegistrationOrder(t *testing.T) {
	c := clock.NewInjected()

	var order []string
	c.AfterFunc(20*time.Millisecond, func() { order = append(order, "late") })
	c.AfterFunc(10*time.Millisecond, func() { order = append(order, "early-first") })
	c.AfterFunc(10*time.Millisecond, func() { order = append(order, "early-second") })

	c.Advance(time.Second)

	want := []string{"early-first", "early-second", "late"}
	if len(order) != len(want) {
		t.Fatalf("fired %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("fired %v, want %v", order, want)
		}
	}
}

// A fault that schedules its own withdrawal is the shape this exists for.
func TestTimerScheduledFromACallbackFiresInTheSameAdvance(t *testing.T) {
	c := clock.NewInjected()

	fired := false
	c.AfterFunc(10*time.Millisecond, func() {
		c.AfterFunc(10*time.Millisecond, func() { fired = true })
	})
	c.Advance(time.Second)

	if !fired {
		t.Error("a timer scheduled inside the advance window never fired")
	}
	if c.Pending() != 0 {
		t.Errorf("%d timers left pending", c.Pending())
	}
}

func TestAdvanceZeroFiresWhatIsAlreadyDue(t *testing.T) {
	c := clock.NewInjected()

	fired := false
	c.AfterFunc(0, func() { fired = true })
	if fired {
		t.Fatal("a zero-duration timer fired during registration; nothing happens on an injected clock without an advance")
	}

	c.Advance(0)
	if !fired {
		t.Error("Advance(0) did not fire a timer already due")
	}
}

func TestStop(t *testing.T) {
	c := clock.NewInjected()

	fired := false
	timer := c.AfterFunc(10*time.Millisecond, func() { fired = true })

	if !timer.Stop() {
		t.Error("Stop() on a pending timer returned false")
	}
	if timer.Stop() {
		t.Error("Stop() twice returned true the second time")
	}

	c.Advance(time.Second)
	if fired {
		t.Error("a stopped timer fired")
	}

	ran := c.AfterFunc(time.Millisecond, func() {})
	c.Advance(time.Second)
	if ran.Stop() {
		t.Error("Stop() on an already-fired timer returned true")
	}
}

func TestNextDeadline(t *testing.T) {
	c := clock.NewInjected()

	if _, ok := c.NextDeadline(); ok {
		t.Error("NextDeadline reported a deadline with nothing scheduled")
	}

	c.AfterFunc(50*time.Millisecond, func() {})
	c.AfterFunc(20*time.Millisecond, func() {})

	got, ok := c.NextDeadline()
	if !ok || got != clock.FromMillis(20) {
		t.Errorf("NextDeadline() = %v,%v want 20ms,true", got, ok)
	}

	c.AdvanceTo(got)
	next, ok := c.NextDeadline()
	if !ok || next != clock.FromMillis(50) {
		t.Errorf("after advancing to the first deadline, NextDeadline() = %v,%v want 50ms,true", next, ok)
	}
}

func TestAdvanceToDoesNotGoBackwards(t *testing.T) {
	c := clock.NewInjected()
	c.Advance(time.Second)

	if got := c.AdvanceTo(clock.FromMillis(10)); got != clock.FromMillis(1000) {
		t.Errorf("AdvanceTo(10ms) from 1s = %v, want 1000ms", got)
	}
}

func TestAdvanceBackwardsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Advance with a negative duration did not panic")
		}
	}()
	clock.NewInjected().Advance(-time.Second)
}

// The interposer reads the clock from every frame-handling goroutine while
// the run loop advances it. Meaningful under -race, which is why just check
// runs the tests that way.
func TestConcurrentReadsAndRegistrations(t *testing.T) {
	c := clock.NewInjected()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = c.Now()
				c.AfterFunc(time.Millisecond, func() {}).Stop()
			}
		}()
	}
	for range 50 {
		c.Advance(time.Millisecond)
	}
	wg.Wait()
}

func TestRealSchedCountsFromCreation(t *testing.T) {
	c := clock.RealSched()
	if got := c.Now(); got < 0 || got > clock.FromMillis(1000) {
		t.Errorf("Now() = %v immediately after creation, want a small positive offset", got)
	}

	done := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("a real timer never fired")
	}
}

func TestNewSched(t *testing.T) {
	injected, err := clock.NewSched(clock.ModeInjected)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := injected.(*clock.Injected); !ok {
		t.Errorf("ModeInjected returned %T, want *clock.Injected", injected)
	}
	if _, err := clock.NewSched(clock.ModeReal); err != nil {
		t.Fatal(err)
	}
	if _, err := clock.NewSched("frozen"); err == nil {
		t.Error("NewSched accepted an unknown mode")
	}
}

func TestFixedWall(t *testing.T) {
	start := time.Date(2026, 9, 8, 14, 3, 10, 0, time.UTC)
	w := clock.NewFixedWall(start)

	if !w.Now().Equal(start) {
		t.Errorf("Now() = %v, want %v", w.Now(), start)
	}

	fired := false
	w.AfterFunc(time.Second, func() { fired = true })
	w.Advance(2 * time.Second)

	if !w.Now().Equal(start.Add(2 * time.Second)) {
		t.Errorf("Now() = %v after advancing 2s", w.Now())
	}
	if !fired {
		t.Error("a wall timer did not fire on advance")
	}
}
