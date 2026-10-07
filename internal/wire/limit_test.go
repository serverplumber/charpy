package wire_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/wire"
)

// endless is a body that never ends: a subject stuck writing.
type endless struct{ b byte }

func (e endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.b
	}
	return len(p), nil
}

func TestReadFrameStopsAtTheCap(t *testing.T) {
	_, err := wire.ReadFrame(endless{'x'})
	var tl *wire.FrameTooLarge
	if !errors.As(err, &tl) {
		t.Fatalf("err = %v, want *FrameTooLarge", err)
	}
	if tl.Read <= wire.MaxFrame {
		t.Errorf("read = %d, want past the cap", tl.Read)
	}
}

// A frame exactly at the cap is a frame, and is returned whole: the cap is
// where charpy stops, not a size it trims to.
func TestReadFrameReturnsAFrameAtTheCapWhole(t *testing.T) {
	b, err := wire.ReadFrame(io.LimitReader(endless{'x'}, wire.MaxFrame))
	if err != nil || len(b) != wire.MaxFrame {
		t.Fatalf("got %d bytes, %v; want %d and no error", len(b), err, wire.MaxFrame)
	}
}

// A data line with no newline would grow bufio's ReadBytes without bound.
func TestScannerStopsALineThatNeverEnds(t *testing.T) {
	r := io.MultiReader(strings.NewReader("event: message\ndata: "), endless{'x'})
	sc := wire.NewSSEScanner(r)
	if sc.Scan() {
		t.Fatalf("scanned a unit, want none: charpy stopped, the subject did not")
	}
	var tl *wire.FrameTooLarge
	if !errors.As(sc.Err(), &tl) {
		t.Fatalf("err = %v, want *FrameTooLarge", sc.Err())
	}
	// The prefix is the unit from its first byte, field lines included.
	if !bytes.HasPrefix(tl.Prefix, []byte("event: message\ndata: xxx")) {
		t.Errorf("prefix = %q", tl.Prefix[:min(40, len(tl.Prefix))])
	}
}

// Short lines that never reach a blank one are one unit that never ends.
func TestScannerStopsAUnitThatNeverEnds(t *testing.T) {
	sc := wire.NewSSEScanner(io.MultiReader(strings.NewReader("data: {\n"), repeat("data: x\n")))
	for sc.Scan() {
		t.Fatalf("scanned a unit, want none")
	}
	var tl *wire.FrameTooLarge
	if !errors.As(sc.Err(), &tl) {
		t.Fatalf("err = %v, want *FrameTooLarge", sc.Err())
	}
}

// The cap is per unit. A stream of well-formed events longer than the cap is
// a stream doing its job.
func TestScannerDoesNotCapAStream(t *testing.T) {
	ev := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"
	n := wire.MaxFrame/len(ev) + 10
	sc := wire.NewSSEScanner(strings.NewReader(strings.Repeat(ev, n)))
	got := 0
	for sc.Scan() {
		got++
	}
	if sc.Err() != nil || got != n {
		t.Fatalf("scanned %d of %d, err %v", got, n, sc.Err())
	}
}

type repeater struct {
	s   string
	off int
}

func repeat(s string) io.Reader { return &repeater{s: s} }

func (r *repeater) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.s[r.off:])
		n += c
		r.off = (r.off + c) % len(r.s)
	}
	return n, nil
}
