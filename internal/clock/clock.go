package clock

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Mono is a point on the injected clock: nanoseconds since the start of the
// run. It is the transcript's t_mono_ns and the only time value the oracle is
// permitted to read, which is why it is a distinct type from time.Time rather
// than an alias for it.
type Mono int64

// Duration returns the offset from the start of the run.
func (m Mono) Duration() time.Duration { return time.Duration(m) }

// Millis returns the offset in whole milliseconds, the unit case manifests
// are written in (withdraw_after_ms, after_mono_ms).
func (m Mono) Millis() int64 { return int64(m) / int64(time.Millisecond) }

// FromMillis converts a manifest's milliseconds to a point on the clock.
func FromMillis(ms int64) Mono { return Mono(ms * int64(time.Millisecond)) }

func (m Mono) String() string { return m.Duration().String() }

// Mode says which clock drives charpy's own scheduling for a run. It is a
// property of the run, recorded in the transcript header, not a property of
// the build. See docs/design/decisions.md ADR-001.
type Mode string

const (
	// ModeInjected drives scheduling from the injected clock, advanced by the
	// run loop. The default, and what v0 case runs use.
	ModeInjected Mode = "injected"
	// ModeReal drives scheduling from wall time. Soak runs declare it,
	// because a six-hour leak test cannot be compressed: the leak is a
	// function of the subject's real elapsed time and its real backoff.
	ModeReal Mode = "real"
)

// Sched is the injected clock: charpy's own scheduling, and the transcript's
// logical timeline. Subject deadlines are never on this clock -- charpy cannot
// freeze a subprocess's clock without forfeiting the language-agnosticism that
// makes it adoptable (ADR-001).
//
// Sched and [Wall] are separate types with incompatible Now methods, so
// reading the wrong clock is a compile error rather than a review comment.
type Sched interface {
	// Now is the current point on the injected clock.
	Now() Mono
	// AfterFunc runs f once, d after now. Under ModeInjected f runs on the
	// goroutine that advances the clock, in deadline order.
	AfterFunc(d time.Duration, f func()) *Timer
}

// Wall is real time. It exists for exactly two purposes: the transcript's
// t_wall column, which joins a run against the subject's own telemetry, and
// liveness budgets, which are real because the subject's deadlines are real.
//
// Nothing in internal/oracle may read it -- a verdict that depends on wall
// time is not reproducible, and reproducible verdicts are what charpy ships
// instead of reproducible runs.
type Wall interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) *Timer
}

// Timer is a pending callback. Stop reports whether it was cancelled before
// it ran; a timer that already ran, or was already stopped, reports false.
type Timer struct {
	stop func() bool
}

// Stop cancels the timer.
func (t *Timer) Stop() bool {
	if t == nil || t.stop == nil {
		return false
	}
	return t.stop()
}

// NewSched returns the scheduling clock for a run mode. The returned clock is
// an [*Injected] under ModeInjected, which the run loop advances; under
// ModeReal it follows wall time. Both report Mono monotonically from run
// start, so the transcript means the same thing either way and the oracle
// cannot tell which produced it.
func NewSched(m Mode) (Sched, error) {
	switch m {
	case ModeInjected:
		return NewInjected(), nil
	case ModeReal:
		return RealSched(), nil
	default:
		return nil, fmt.Errorf("clock: unknown mode %q; want %q or %q", m, ModeInjected, ModeReal)
	}
}

// RealSched returns a scheduling clock that follows wall time, counting from
// the moment it is created. Its Mono readings come from Go's monotonic clock
// reading, so a wall-clock jump mid-run does not move them.
func RealSched() Sched { return &realSched{start: time.Now()} }

type realSched struct{ start time.Time }

func (c *realSched) Now() Mono { return Mono(time.Since(c.start)) }

func (c *realSched) AfterFunc(d time.Duration, f func()) *Timer {
	t := time.AfterFunc(d, f)
	return &Timer{stop: t.Stop}
}

// RealWall returns real wall time.
func RealWall() Wall { return realWall{} }

type realWall struct{}

func (realWall) Now() time.Time { return time.Now() }

func (realWall) AfterFunc(d time.Duration, f func()) *Timer {
	t := time.AfterFunc(d, f)
	return &Timer{stop: t.Stop}
}

// Injected is a clock that only moves when it is told to. It is the default
// for case runs: a fault scheduled for 30s into the run fires when the run
// loop says 30s have passed, so a case executes identically on a fast machine
// and a loaded CI runner.
//
// Advance must be called from a single goroutine -- the run loop. Every other
// method is safe to call from any goroutine, including from inside a timer
// callback.
type Injected struct {
	mu     sync.Mutex
	now    Mono
	nextID int64
	q      []*pending
}

// NewInjected returns an injected clock reading zero: the start of the run.
func NewInjected() *Injected { return &Injected{} }

type pending struct {
	deadline Mono
	id       int64
	f        func()
	done     bool
}

// Now is the current point on the clock.
func (c *Injected) Now() Mono {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc registers f to run once, d after now. A non-positive d schedules
// f for the current instant, which means the next Advance -- including
// Advance(0). Nothing on an injected clock happens without time passing, and
// a callback that ran during registration would fire before its own
// scheduling had been recorded.
func (c *Injected) AfterFunc(d time.Duration, f func()) *Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	if d < 0 {
		d = 0
	}
	c.nextID++
	p := &pending{deadline: c.now + Mono(d), id: c.nextID, f: f}
	c.q = append(c.q, p)

	return &Timer{stop: func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if p.done {
			return false
		}
		p.done = true
		c.remove(p)
		return true
	}}
}

// Advance moves the clock forward by d, running every callback that comes due
// on the way.
//
// Callbacks run in deadline order, ties broken by registration order, with
// the clock set to each callback's own deadline rather than to the end of the
// advance -- so a callback reading Now sees the instant it was scheduled for.
// A callback that schedules another timer inside the window is picked up by
// the same advance. Both properties are what make a replayed run land in the
// same order as the original.
//
// It panics on a negative duration: a monotonic clock that can go backwards
// is not one.
func (c *Injected) Advance(d time.Duration) Mono {
	if d < 0 {
		panic("clock: Advance with a negative duration")
	}

	c.mu.Lock()
	target := c.now + Mono(d)
	c.mu.Unlock()

	return c.AdvanceTo(target)
}

// AdvanceTo moves the clock forward to an absolute point, running everything
// that comes due. A target already in the past still fires the callbacks due
// at the current instant, and does not move the clock backwards.
func (c *Injected) AdvanceTo(target Mono) Mono {
	for {
		c.mu.Lock()
		p := c.earliest()
		if p == nil || p.deadline > target {
			if target > c.now {
				c.now = target
			}
			now := c.now
			c.mu.Unlock()
			return now
		}
		if p.deadline > c.now {
			c.now = p.deadline
		}
		p.done = true
		c.remove(p)
		c.mu.Unlock()

		// Outside the lock: a callback may read the clock or schedule
		// another timer, and holding the lock across it would deadlock.
		p.f()
	}
}

// NextDeadline reports when the next timer comes due, and whether there is
// one. The run loop uses it to advance exactly as far as the next scheduled
// thing rather than guessing an interval.
func (c *Injected) NextDeadline() (Mono, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	p := c.earliest()
	if p == nil {
		return 0, false
	}
	return p.deadline, true
}

// Pending reports how many timers are outstanding.
func (c *Injected) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.q)
}

// earliest returns the next timer due, or nil. Caller holds the lock. The
// queue is small -- a handful of faults in flight -- so a scan beats a heap,
// and the sort keeps the tie-break on registration order explicit.
func (c *Injected) earliest() *pending {
	if len(c.q) == 0 {
		return nil
	}
	sort.SliceStable(c.q, func(i, j int) bool {
		if c.q[i].deadline != c.q[j].deadline {
			return c.q[i].deadline < c.q[j].deadline
		}
		return c.q[i].id < c.q[j].id
	})
	return c.q[0]
}

// remove drops p from the queue. Caller holds the lock.
func (c *Injected) remove(p *pending) {
	for i, q := range c.q {
		if q == p {
			c.q = append(c.q[:i], c.q[i+1:]...)
			return
		}
	}
}

// FixedWall is wall time that only moves when it is told to, for tests that
// need a stable t_wall column. Production runs use [RealWall].
type FixedWall struct {
	mu  sync.Mutex
	now time.Time
	sch Injected
}

// NewFixedWall returns a wall clock reading t.
func NewFixedWall(t time.Time) *FixedWall { return &FixedWall{now: t} }

// Now is the current wall time.
func (w *FixedWall) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// AfterFunc registers f to run d from now, when the wall clock is advanced.
func (w *FixedWall) AfterFunc(d time.Duration, f func()) *Timer {
	return w.sch.AfterFunc(d, f)
}

// Advance moves wall time forward, running any callbacks that come due.
func (w *FixedWall) Advance(d time.Duration) time.Time {
	w.mu.Lock()
	w.now = w.now.Add(d)
	now := w.now
	w.mu.Unlock()

	w.sch.Advance(d)
	return now
}
