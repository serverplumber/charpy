package wire

import "io"

// Stdio is a newline-delimited stream over a pipe: a subprocess's stdin, or
// charpy's own stdout when it is the server behind a stdio shim.
//
// There is no buffering to defeat, so Flush is a no-op and a partial write is
// already partial on the wire. That is the whole difference from SSE, where a
// cut is invisible unless it is flushed.
type Stdio struct {
	w io.WriteCloser
	counter
}

// NewStdio wraps a pipe.
func NewStdio(w io.WriteCloser) *Stdio { return &Stdio{w: w} }

func (s *Stdio) Write(b []byte) (int, error) {
	n, err := s.w.Write(b)
	s.add(n)
	return n, err
}

// Flush is a no-op: a pipe write has already left the process.
func (s *Stdio) Flush() error { return nil }

// Close closes the pipe, which is what the subject sees as EOF.
//
// The alternative is [Stall], which holds the pipe open so a subject reading a
// frame that never delimits blocks instead of seeing the stream end. Those
// find different bugs, and both are things charpy does rather than one of them
// being a call it omits.
func (s *Stdio) Close() error { return s.w.Close() }
