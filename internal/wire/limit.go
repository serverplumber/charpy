package wire

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// MaxFrame is the most charpy reads of one frame before it stops reading.
//
// It is charpy's number, not the protocol's: MCP sets no frame size, so a
// frame past it is reported as what it is -- a frame past charpy's cap --
// and never as a violation. It is generous because a correct subject can
// answer resources/read with a large blob, and a cap that cut one would be
// charpy misreporting a correct server. What it exists to stop is a body that
// never ends taking charpy down with it. It is also not bufio's 64 KiB
// default, which is smaller than a tools/list result with real schemas in it:
// a scanner that stopped mid-catalogue would look exactly like a subject that
// did.
//
// Every transport reads to this one number.
const MaxFrame = 4 << 20

// FrameTooLarge is a frame charpy stopped reading at MaxFrame. The peer that
// sent it is still sending, or was; Read is how much charpy took before it
// stopped, a lower bound on the frame's length and never its true length.
type FrameTooLarge struct {
	Read   int
	Prefix []byte // the first bytes of the frame, as received
}

func (e *FrameTooLarge) Error() string {
	return fmt.Sprintf("wire: frame exceeds %d bytes (stopped after %d)", MaxFrame, e.Read)
}

// ReadFrame reads a whole frame -- an HTTP body -- from r, or a
// *FrameTooLarge once it has read more than MaxFrame. It never returns a frame
// cut short at the cap: a body silently truncated there would look exactly
// like one a subject cut off.
func ReadFrame(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFrame+1))
	if len(b) > MaxFrame {
		return nil, &FrameTooLarge{Read: len(b), Prefix: b}
	}
	return b, err
}

// readLine reads through the next newline, or to the end of the stream, and
// fails with a *FrameTooLarge once the line passes limit. bufio's ReadBytes
// would grow without bound on a line that never ends, and a bufio.Scanner
// stops at its limit but throws the line away, leaving no prefix to record.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, &FrameTooLarge{Read: len(line), Prefix: line}
		}
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// LineScanner reads newline-delimited frames, the stdio transport's, with the
// interface of a bufio.Scanner splitting lines -- and stops a line past
// MaxFrame with a *FrameTooLarge rather than dropping it.
type LineScanner struct {
	r    *bufio.Reader
	line []byte
	err  error
}

// NewLineScanner reads frames from r.
func NewLineScanner(r io.Reader) *LineScanner {
	return &LineScanner{r: bufio.NewReaderSize(r, 64<<10)}
}

// Scan reads the next line, without its newline or a carriage return before
// it. A last line with no newline is yielded, as bufio's line splitter does.
func (s *LineScanner) Scan() bool {
	if s.err != nil {
		return false
	}
	line, err := readLine(s.r, MaxFrame)
	if err != nil {
		s.err = err
		if _, capped := err.(*FrameTooLarge); capped || len(line) == 0 {
			return false
		}
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	s.line = bytes.TrimSuffix(line, []byte("\r"))
	return true
}

// Bytes is the last line Scan read. Unlike a bufio.Scanner's, it is not
// overwritten by the next Scan.
func (s *LineScanner) Bytes() []byte { return s.line }

// Err reports why scanning stopped: nil at the end of the stream, a
// *FrameTooLarge for a line past MaxFrame, or the read error.
func (s *LineScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}
