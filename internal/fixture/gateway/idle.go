package gateway

import (
	"context"
	"errors"
	"time"
)

// DefaultUpstreamIdle is how long a call may hear nothing from its upstream
// before the gateway gives up on it. Short, because the fixture is a test
// instrument: it has to sit under the hang a case holds an upstream's answer
// for, or the gateway's failure path is never taken.
const DefaultUpstreamIdle = 2 * time.Second

// The deadline is on silence, not on the whole call. A call that is making
// progress is alive however long it runs, and one waiting on the downstream
// client -- for sampling, or for an elicitation a person has to answer -- is
// not the upstream's silence at all. A total deadline would have to be long
// enough for the slowest legitimate call, and then would never catch a hung
// one inside the time a case allows.

// errIdle is the cause a call is cancelled with when its upstream goes quiet.
var errIdle = errors.New("upstream idle")

// call is one downstream request in flight to an upstream.
type call struct {
	// ctx is derived from the downstream request, so its values still route
	// what the upstream sends back onto that request's stream.
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

// expire gives up on a call whose upstream has been quiet for the whole idle
// interval -- unless the upstream is waiting on the downstream client, which
// is not silence.
func (u *upstream) expire(c *call) {
	u.mu.Lock()
	waiting := u.asking > 0
	if waiting {
		c.timer.Reset(u.idle)
	}
	u.mu.Unlock()
	if !waiting {
		c.cancel(errIdle)
	}
}

// touch records that u was heard from, re-arming every call in flight to it.
// Calls are admitted one at a time, so that is the call the message belongs
// to -- or, with a nested call admitted, one of two that are both alive.
func (u *upstream) touch() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.calls {
		if c.timer != nil {
			c.timer.Reset(u.idle)
		}
	}
}

// ask marks u as waiting on the downstream client until the returned
// function is called. Waiting calls are woken, since one may be the
// downstream client's own, made to answer this.
func (u *upstream) ask() func() {
	u.mu.Lock()
	u.asking++
	u.free()
	u.mu.Unlock()
	u.touch()
	return func() {
		u.mu.Lock()
		u.asking--
		u.mu.Unlock()
		u.touch()
	}
}
