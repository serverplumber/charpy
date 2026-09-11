package wire

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
)

// Stream is a transport charpy writes bytes onto and can stop mid-frame.
//
// The method that matters is Flush. Without it a cut is invisible: the bytes
// sit in a buffer, the connection closes, and the subject sees nothing rather
// than half an event. Every partial write here is flushed before returning,
// because the truncation is the datum.
type Stream interface {
	io.Writer
	// Flush pushes buffered bytes onto the transport.
	Flush() error
	// Close ends the stream.
	Close() error
	// Written is the total number of bytes handed to the transport, which is
	// what the transcript's stream_close detail reports.
	Written() int64
}

// Emit writes a complete unit and flushes it.
func Emit(s Stream, e Encoded) (int, error) {
	return write(s, e.Bytes)
}

// EmitCut writes the first n bytes of a unit, flushes, and leaves the rest
// unsent. This is the entire wire effect of the truncate mechanism.
//
// What reaches the subject is bytes written, never bytes received: kernel
// buffers, the OS and any intermediary sit between this flush and the
// subject's read. See the package comment.
func EmitCut(s Stream, e Encoded, n int) (int, error) {
	if n < 0 || n > len(e.Bytes) {
		return 0, errors.New("wire: cut offset out of range")
	}
	return write(s, e.Bytes[:n])
}

func write(s Stream, b []byte) (int, error) {
	n, err := s.Write(b)
	if err != nil {
		return n, err
	}
	return n, s.Flush()
}

// counter tracks bytes handed to a transport, shared by the stream
// implementations.
type counter struct{ n int64 }

func (c *counter) add(n int) { c.n += int64(n) }

// Written is the total handed to the transport.
func (c *counter) Written() int64 { return c.n }

// Keepalive says what, if anything, keeps arriving on a held stream. It is the
// truncate and hang mechanisms' keepalive parameter.
type Keepalive string

const (
	// KeepaliveNone sends nothing at all. The subject has to notice silence,
	// which tests dead-peer detection and transport-level timeouts.
	KeepaliveNone Keepalive = "none"
	// KeepaliveComments keeps : comment lines arriving while no data ever
	// does, so the stream looks perfectly healthy throughout. That tests
	// whether the subject has an application-level timeout at all.
	KeepaliveComments Keepalive = "comments"
)

// DefaultKeepaliveEvery is the cadence of keep-alive comments on a held
// stream when a case does not name one.
const DefaultKeepaliveEvery = 15 * time.Second

// commenter is a stream that can carry SSE comments. stdio cannot, which is
// why keepalive is documented as HTTP only.
type commenter interface {
	WriteComment(text string) (int, error)
}

// StallOptions configures a stall.
type StallOptions struct {
	Keepalive Keepalive
	// Every is the keep-alive cadence. Zero means DefaultKeepaliveEvery.
	Every time.Duration
	// Comment is the keep-alive text.
	Comment string

	// Wall is real time, and deliberately not the injected clock. A
	// keep-alive exists to stop the *subject's* timer firing, and the
	// subject's deadlines are real (ADR-001) -- comments that only arrived
	// when a run loop advanced would not keep anything alive. Required when
	// Keepalive is KeepaliveComments.
	Wall clock.Wall
}

// Stalled is a stream being held open with no frames on it.
//
// A stall is a thing charpy does, not a thing it omits. Expressing it as "do
// not call Close" would make the fault invisible in the code that performs it,
// impossible to assert on, and would leave keepalive with nowhere to live --
// a held stream that keeps sending comments is a different fault from one that
// goes silent, and neither is the absence of an action.
type Stalled struct {
	s   Stream
	o   StallOptions
	mu  sync.Mutex
	t   *clock.Timer
	n   int
	end bool
	err error
}

// Stall holds a stream open, emitting whatever its keepalive says and nothing
// else. It does not close the stream; End and Close do that.
//
// Requesting comments on a transport that has none -- stdio -- is
// ErrNotApplicable rather than a silent downgrade to silence, for the same
// reason a cut point that cannot land is an error: a fault charpy cannot
// deliver is a case that does not apply.
func Stall(s Stream, o StallOptions) (*Stalled, error) {
	switch o.Keepalive {
	case "", KeepaliveNone:
		o.Keepalive = KeepaliveNone
	case KeepaliveComments:
		if _, ok := s.(commenter); !ok {
			return nil, fmt.Errorf("%w: keepalive = %q needs a transport with comments", ErrNotApplicable, o.Keepalive)
		}
		if o.Wall == nil {
			return nil, errors.New("wire: keep-alive comments need a wall clock; the subject's timer is real")
		}
	default:
		return nil, fmt.Errorf("wire: unknown keepalive %q", o.Keepalive)
	}
	if o.Every <= 0 {
		o.Every = DefaultKeepaliveEvery
	}

	st := &Stalled{s: s, o: o}
	if o.Keepalive == KeepaliveComments {
		st.arm()
	}
	return st, nil
}

// Comments is how many keep-alives have gone out, which is what a transcript
// reports about a stall that looked alive.
func (st *Stalled) Comments() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.n
}

// Err reports the first write error a keep-alive hit, which usually means the
// subject hung up during the stall.
func (st *Stalled) Err() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

// End stops the stall and leaves the stream open, which is what withdrawing a
// fault does: normal service resumes on the same stream.
func (st *Stalled) End() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.end = true
	if st.t != nil {
		st.t.Stop()
		st.t = nil
	}
	return st.err
}

// Close ends the stall by closing the stream.
func (st *Stalled) Close() error {
	if err := st.End(); err != nil {
		return err
	}
	return st.s.Close()
}

func (st *Stalled) arm() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.end {
		return
	}
	st.t = st.o.Wall.AfterFunc(st.o.Every, func() {
		st.tick()
		st.arm()
	})
}

func (st *Stalled) tick() {
	st.mu.Lock()
	if st.end {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()

	_, err := Emit(st.s, EncodeComment(st.o.Comment))

	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil && st.err == nil {
		st.err = err
	}
	if err == nil {
		st.n++
	}
}
