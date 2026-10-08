package wire

import (
	"bufio"
	"bytes"
	"io"
)

// SSEScanner reads an SSE stream and yields each unit -- an event or a
// keep-alive comment -- as an Encoded whose spans are measured over the bytes
// the reader actually received.
//
// It is the dual of EncodeEvent and EncodeComment. Those record spans as
// charpy builds bytes it authored; this recovers them from bytes a subject
// wrote, so a proxy can relay a stream verbatim and cut the one unit a fault
// matched at exactly the offset the cut point names -- in the subject's own
// bytes, not a re-rendering of them. That is the same discipline the stdio
// path keeps by carrying m.Raw() rather than a re-marshalled frame.
//
// Field order is not assumed. SSE permits event, id, retry and data in any
// order, and the SDK happens to put data last; the scanner records whichever
// line is the data line wherever it falls.
type SSEScanner struct {
	r    *bufio.Reader
	unit Encoded
	err  error
}

// NewSSEScanner reads SSE units from r, which is a subject's response body.
func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{r: bufio.NewReaderSize(r, 64<<10)}
}

// Scan reads the next unit. It returns false at end of stream or on the first
// read error, which Err then reports.
//
// A stream that ends mid-unit -- no terminating blank line, which is what a
// subject charpy cut off leaves behind -- yields whatever bytes arrived as a
// final unit before Scan returns false, because a truncated event is a datum
// and dropping it would hide the very thing under test.
func (s *SSEScanner) Scan() bool {
	if s.err != nil {
		return false
	}

	var (
		buf       bytes.Buffer
		body      span
		dataStart int
		haveData  bool
		isComment bool
		off       int
	)

	for {
		line, err := readLine(s.r, MaxFrame-buf.Len())
		if tl, ok := err.(*FrameTooLarge); ok {
			// Not yielded, unlike a stream that ends mid-unit: the subject did
			// not stop here, charpy did, and relaying the part read would put
			// a cut in the subject's mouth.
			tl.Read += buf.Len()
			tl.Prefix = append(buf.Bytes(), tl.Prefix...)
			s.err = tl
			return false
		}
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")

			switch {
			case len(trimmed) == 0:
				// Blank line: the event terminates. Include it, so relaying
				// the unit's bytes reproduces the stream exactly.
				buf.Write(line)
				return s.emit(buf.Bytes(), body, dataStart, haveData, isComment)

			case trimmed[0] == ':' && buf.Len() == 0:
				// A comment stands alone: emit it as its own unit so a
				// mid_comment cut has something to land in and the transcript
				// records the keep-alive that made a stall look healthy.
				b := append([]byte(nil), line...)
				cs, ce := commentValue(trimmed)
				return s.emit(b, span{cs, ce}, 0, false, true)

			default:
				if isDataLine(trimmed) && !haveData {
					vs, ve := fieldValue(trimmed)
					body = span{off + vs, off + ve}
					dataStart = off
					haveData = true
				}
				buf.Write(line)
				off += len(line)
			}
		}

		if err != nil {
			s.err = err
			if buf.Len() > 0 {
				// A unit with no terminating blank line: the stream ended
				// inside it. Yield what arrived.
				return s.emit(buf.Bytes(), body, dataStart, haveData, isComment)
			}
			return false
		}
	}
}

// emit stores the unit and reports that one was read. It never fails; the
// spans were measured against the bytes it is handed.
func (s *SSEScanner) emit(raw []byte, body span, dataStart int, haveData, isComment bool) bool {
	k := kindEvent
	if isComment {
		k = kindComment
	}
	s.unit = Encoded{Bytes: append([]byte(nil), raw...), kind: k, body: body, dataStart: dataStart}
	if k == kindEvent && !haveData {
		// An event with no data field carries no frame -- an id-only cursor
		// bump, say. body stays zero, and Body reports nothing.
		s.unit.body = span{}
		s.unit.dataStart = dataStart
	}
	return true
}

// Unit is the last unit Scan read.
func (s *SSEScanner) Unit() Encoded { return s.unit }

// Err reports why scanning stopped. io.EOF is the ordinary end and is folded
// away; a *FrameTooLarge is a unit past MaxFrame, which charpy stopped reading
// and did not yield; anything else is a real read error.
//
// The cap is per unit, not per stream. A stream of well-formed events that
// never ends is a stream doing its job, and the run's deadline bounds it.
func (s *SSEScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

func isDataLine(trimmed []byte) bool {
	return bytes.HasPrefix(trimmed, []byte("data:"))
}

// fieldValue returns the byte range of a field's value within its line:
// everything after the first colon, past one optional space, per the SSE
// grammar.
func fieldValue(trimmed []byte) (start, end int) {
	i := bytes.IndexByte(trimmed, ':')
	start = i + 1
	if start < len(trimmed) && trimmed[start] == ' ' {
		start++
	}
	return start, len(trimmed)
}

func commentValue(trimmed []byte) (start, end int) { return fieldValue(trimmed) }
