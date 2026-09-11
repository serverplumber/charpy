package wire

import (
	"bufio"
	"net"
	"net/http"
	"time"
)

// SSE is an event stream over an HTTP response.
//
// It goes through the ordinary http.ResponseWriter and
// http.NewResponseController rather than a hand-rolled HTTP stack: the
// controller is the extension point net/http already provides, and Flush on it
// is all seven cut points need. See the package comment for what that does and
// does not reach.
type SSE struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	counter
}

// NewSSE takes over a response as an event stream, writing the headers and
// sending them.
//
// X-Accel-Buffering: no is not decoration. A reverse proxy that buffers the
// response would hold a truncated event until the connection closed and then
// deliver it whole, which turns every cut into a no-op and every stream case
// into a test that silently passes.
func NewSSE(w http.ResponseWriter) *SSE {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s := &SSE{w: w, rc: http.NewResponseController(w)}
	_ = s.Flush() // send the headers before any event
	return s
}

func (s *SSE) Write(b []byte) (int, error) {
	n, err := s.w.Write(b)
	s.add(n)
	return n, err
}

// Flush pushes what has been written onto the wire. Every partial write goes
// through it, because an unflushed cut is not a cut.
func (s *SSE) Flush() error { return s.rc.Flush() }

// Close ends the stream by returning from the handler, which the driver does;
// there is nothing to close here. It exists to satisfy Stream so that a caller
// can treat both transports alike.
func (s *SSE) Close() error { return nil }

// WriteComment sends an SSE keep-alive comment.
//
// A comment is not decoration: it is what makes a stalled stream look healthy,
// and it is what mid_comment cuts into. 2026-07-28 encourages them.
func (s *SSE) WriteComment(text string) (int, error) { return Emit(s, EncodeComment(text)) }

// Hold clears this response's write deadline so a hang can outlive the
// server's own limits.
//
// Per-response rather than a server-wide WriteTimeout of zero: exactly the
// response being held gives up its deadline, and every other request keeps
// one. A harness that disarmed its own timeouts globally would eventually hang
// on something that was not the fault, and report it as the subject's.
//
// It returns http.ErrNotSupported where the transport cannot do it, which the
// caller should treat as a case that does not apply rather than as a hang it
// can deliver.
func (s *SSE) Hold() error { return s.rc.SetWriteDeadline(time.Time{}) }

// Raw returns the connection beneath the response, for the deferred faults
// that need framing control net/http will not give up -- a reset mid-body, a
// Content-Length that lies.
//
// It returns http.ErrNotSupported over HTTP/2, which net/http has no plan to
// support hijacking on, and the MCP specification mandates no HTTP version. So
// the capability is probed here rather than assumed anywhere: a fault that
// needs the raw connection is scoped to the transport that can carry it.
func (s *SSE) Raw() (net.Conn, *bufio.ReadWriter, error) { return s.rc.Hijack() }
