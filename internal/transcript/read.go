package transcript

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// Reading a transcript back is the oracle's whole input. The guarantees in
// docs/design/transcript.md section 8 are checked here rather than assumed by
// every consumer: file order equals seq order, seq is dense from zero, and the
// first line is the header. A reader that trusted those silently would turn a
// truncated capture into wrong verdicts instead of fewer ones.

// Entry is one line of a transcript. Exactly one of Header, Frame and Event is
// non-nil, and Common is populated whichever it is.
type Entry struct {
	Common
	Header *HeaderLine
	Frame  *FrameLine
	Event  *EventLine
}

// Transcript is a whole file, read.
type Transcript struct {
	Header  *HeaderLine
	Entries []Entry
	// Partial reports that the file ended mid-line, which a run that crashed
	// leaves behind. Guarantee 3 makes that a legitimate oracle input: it
	// simply supports fewer conclusions, so it is reported rather than
	// refused.
	Partial bool
}

// Frames returns the frame lines in sequence order.
func (t *Transcript) Frames() []*FrameLine {
	var out []*FrameLine
	for i := range t.Entries {
		if f := t.Entries[i].Frame; f != nil {
			out = append(out, f)
		}
	}
	return out
}

// Events returns the event lines of one kind, in sequence order.
func (t *Transcript) Events(kind EventKind) []*EventLine {
	var out []*EventLine
	for i := range t.Entries {
		if e := t.Entries[i].Event; e != nil && e.EventKind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Read loads a transcript.
//
// A schema_version it does not know is refused rather than guessed at, which
// is what docs/design/transcript.md section 9 promises: a v1 reader that
// tolerated a v2 line would be reading fields that no longer mean what it
// thinks.
func Read(r io.Reader) (*Transcript, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	t := &Transcript{}
	held := map[string]int{} // case id -> withholds applied and not yet withdrawn
	for n := 0; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		e, err := decodeEntry(line)
		if err != nil {
			return nil, fmt.Errorf("transcript: line %d: %w", n+1, err)
		}
		if e.SchemaVersion != SchemaVersion {
			return nil, fmt.Errorf(
				"transcript: line %d: schema_version %d; this reader knows %d and will not guess",
				n+1, e.SchemaVersion, SchemaVersion)
		}
		if e.Seq != int64(n) {
			return nil, fmt.Errorf(
				"transcript: line %d has seq %d; file order must equal seq order and seq must be dense",
				n+1, e.Seq)
		}
		if (n == 0) != (e.Header != nil) {
			return nil, fmt.Errorf("transcript: line %d: exactly one header, first", n+1)
		}
		if e.Header != nil {
			t.Header = e.Header
		}
		if err := checkWithdrawal(e.Event, held); err != nil {
			return nil, fmt.Errorf("transcript: line %d: %w", n+1, err)
		}
		t.Entries = append(t.Entries, e)
	}

	if err := sc.Err(); err != nil {
		// A run that crashed mid-write leaves a valid prefix and a partial
		// last line. That is a transcript, not a corruption.
		if err == bufio.ErrTooLong {
			return nil, fmt.Errorf("transcript: a line exceeds %d bytes", maxLine)
		}
		t.Partial = true
	}
	if t.Header == nil {
		return nil, fmt.Errorf("transcript: no header; a file whose first line is not one is not a transcript")
	}
	return t, nil
}

// checkWithdrawal refuses a fault_withdrawn that no withhold precedes. Every
// withdrawal charpy writes is the release of a hold, so one with nothing held
// is a file charpy did not write -- a hand-edited fixture gone stale -- and
// reading it would turn the fixture's rot into findings about a subject.
//
// A fault_scheduled with no fault_applied after it is not refused: a driver
// writes exactly that, with a note, when a matched case cannot be applied. And
// a fault_applied that records no verb counts as a hold, because absence is
// not evidence of anything.
func checkWithdrawal(e *EventLine, held map[string]int) error {
	if e == nil || e.Fault == nil {
		return nil
	}
	id := e.Fault.CaseID
	switch e.EventKind {
	case FaultApplied:
		// "withhold" is interpose.VerbWithhold, which this package cannot import.
		if verb, ok := e.Detail["verb"].(string); !ok || verb == "withhold" {
			held[id]++
		}
	case FaultWithdrawn:
		if held[id] == 0 {
			return fmt.Errorf("fault_withdrawn for %s with no withhold applied before it; "+
				"charpy never writes that, so this file is a bad fixture, not a run", id)
		}
		held[id]--
	}
	return nil
}

// maxLine bounds a transcript line. RawCap caps the bytes a frame carries and
// base64 inflates by a third, so this leaves room for that plus the columns
// around it.
const maxLine = 2 * RawCap

func decodeEntry(line []byte) (Entry, error) {
	var probe Common
	if err := json.Unmarshal(line, &probe); err != nil {
		return Entry{}, fmt.Errorf("not a transcript line: %w", err)
	}

	e := Entry{Common: probe}
	switch probe.Type {
	case "header":
		e.Header = &HeaderLine{}
		return e, json.Unmarshal(line, e.Header)
	case "frame":
		e.Frame = &FrameLine{}
		return e, json.Unmarshal(line, e.Frame)
	case "event":
		e.Event = &EventLine{}
		return e, json.Unmarshal(line, e.Event)
	default:
		return Entry{}, fmt.Errorf("unknown line type %q", probe.Type)
	}
}

// Bytes decodes the frame's raw column.
//
// What comes back is what crossed the wire, which for a truncated frame is
// what was sent rather than what was meant -- the truncation is the datum. A
// frame beyond the size cap comes back short, and RawTruncated is how a
// consumer knows not to draw conclusions from its ending.
func (f *FrameLine) Bytes() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(f.Raw)
	if err != nil {
		return nil, fmt.Errorf("transcript: frame %d: raw is not base64: %w", f.Seq, err)
	}
	return b, nil
}

// Complete reports whether the frame's bytes are all of it. A capped frame is
// not a frame to validate or digest: the missing tail is charpy's doing, not
// the subject's.
func (f *FrameLine) Complete() bool { return !f.RawTruncated }

// IDText returns the frame's id column and whether it had one. Null and absent
// ids both report false, and IDType is what tells them apart.
func (f *FrameLine) IDText() (string, bool) {
	if f.ID == nil {
		return "", false
	}
	return *f.ID, true
}

// MethodName returns the frame's method, which on a response is the ledger's
// echo and may be absent.
func (f *FrameLine) MethodName() string {
	if f.Method == nil {
		return ""
	}
	return *f.Method
}

// Key identifies a frame's exchange within its connection: the id column
// qualified by its type, so a number and a string that print the same are two
// exchanges rather than one.
func (f *FrameLine) Key() string {
	text, ok := f.IDText()
	if !ok {
		return ""
	}
	return string(f.IDType) + ":" + text
}

// AppliedDirection returns the direction a fault_applied event records for
// the frame its fault acted on, and whether it records one. Transcripts
// written before ADR-013 do not, and a reader must not guess: the direction is
// what says who the fault was put to.
func (e *EventLine) AppliedDirection() (Direction, bool) {
	d, ok := e.Detail["direction"].(string)
	if !ok || d == "" {
		return "", false
	}
	return Direction(d), true
}

// Tampered reports whether charpy corrupted this frame. Such a frame cannot be
// held against the subject, so the layers that judge exclude it.
func (f *FrameLine) Tampered() bool { return f.Fault != nil }
