package wire

import (
	"errors"
	"fmt"
)

// CutPoint names where a truncation lands. The values are the truncate
// mechanism's cut_at parameter; see docs/design/faults-and-cases.md section 2.
type CutPoint string

const (
	// CutByte cuts at exactly AfterBytes, wherever that lands.
	CutByte CutPoint = "byte"
	// CutMidFrame cuts at a seeded point inside the JSON body.
	CutMidFrame CutPoint = "mid_frame"
	// CutMidLine cuts before the terminating newline, so the frame never
	// delimits. stdio only.
	CutMidLine CutPoint = "mid_line"
	// CutMidEvent cuts inside an SSE data: value. HTTP only.
	CutMidEvent CutPoint = "mid_event"
	// CutFieldBoundary cuts after event: and before data:, leaving a
	// structurally incomplete SSE event. HTTP only.
	CutFieldBoundary CutPoint = "field_boundary"
	// CutEventBoundary cuts cleanly between events: everything delivered was
	// well formed, and the stream simply stops. HTTP only.
	CutEventBoundary CutPoint = "event_boundary"
	// CutMidComment cuts inside a : keep-alive comment. HTTP only.
	CutMidComment CutPoint = "mid_comment"
)

var (
	// ErrNotApplicable reports a cut point that cannot land in these bytes --
	// a mid_line on an SSE event, a mid_comment on anything but a comment.
	//
	// It is an error rather than a silent fallback to a nearby cut. A fault
	// charpy cannot deliver is a case that does not apply, which the oracle
	// has a verdict for; quietly degrading it into a weaker fault would be a
	// test that passes by not running.
	ErrNotApplicable = errors.New("wire: cut point does not apply")

	// ErrNoTruncation reports a cut that would deliver every byte. A
	// truncation that truncates nothing is a case that did not do what it
	// says, and saying so beats emitting a whole frame and calling it a cut.
	ErrNoTruncation = errors.New("wire: cut delivers the whole frame")
)

// CutOptions parameterises a cut.
type CutOptions struct {
	// AfterBytes is the offset CutByte uses.
	AfterBytes int

	// Pick chooses a seeded offset, returning a value in [0, n). It is how
	// the run seed reaches the cuts documented as "seeded", which is what
	// makes a case reproducible from its citation. A nil Pick takes the
	// midpoint, which is deterministic but carries no seed.
	Pick func(n int) int
}

func (o CutOptions) pick(n int) int {
	if n <= 0 {
		return 0
	}
	if o.Pick == nil {
		return n / 2
	}
	i := o.Pick(n)
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// Cut reports how many leading bytes of e a truncation delivers.
//
// The result is always in [0, e.Len()), never e.Len() itself: a cut that
// delivers everything is ErrNoTruncation. CutEventBoundary is the one
// exception in spirit -- it delivers a whole event -- and it is handled by
// cutting at the end of the event, which is a truncation of the *stream*
// rather than of the event.
func (e Encoded) Cut(at CutPoint, o CutOptions) (int, error) {
	switch at {
	case CutByte:
		if o.AfterBytes < 0 {
			return 0, fmt.Errorf("wire: after_bytes %d is negative", o.AfterBytes)
		}
		if o.AfterBytes >= len(e.Bytes) {
			return 0, fmt.Errorf("%w: after_bytes %d covers all %d bytes",
				ErrNoTruncation, o.AfterBytes, len(e.Bytes))
		}
		return o.AfterBytes, nil

	case CutMidFrame:
		return e.inside(at, e.body, o)

	case CutMidLine:
		if err := e.requireKind(at, kindLine); err != nil {
			return 0, err
		}
		// Everything but the newline: the frame is complete and never
		// delimits, so a line reader waits for a terminator that never comes.
		return len(e.Bytes) - 1, nil

	case CutMidEvent:
		if err := e.requireKind(at, kindEvent); err != nil {
			return 0, err
		}
		return e.inside(at, e.body, o)

	case CutFieldBoundary:
		if err := e.requireKind(at, kindEvent); err != nil {
			return 0, err
		}
		if e.dataStart == 0 {
			return 0, fmt.Errorf("%w: %s needs an event field before the data field",
				ErrNotApplicable, at)
		}
		return e.dataStart, nil

	case CutEventBoundary:
		if err := e.requireKind(at, kindEvent); err != nil {
			return 0, err
		}
		// The event is delivered whole and the stream stops after it. Nothing
		// malformed is ever seen, which is what makes this the least dramatic
		// variant and the most likely to find something.
		return len(e.Bytes), nil

	case CutMidComment:
		if err := e.requireKind(at, kindComment); err != nil {
			return 0, err
		}
		return e.inside(at, e.body, o)

	default:
		return 0, fmt.Errorf("wire: unknown cut point %q", at)
	}
}

// inside returns a seeded offset strictly within s, so that at least one byte
// of the payload is delivered and at least one is withheld. A payload too
// short to cut that way cannot carry the fault.
func (e Encoded) inside(at CutPoint, s span, o CutOptions) (int, error) {
	if s.len() < 2 {
		return 0, fmt.Errorf("%w: %s needs a payload of at least 2 bytes, got %d",
			ErrNotApplicable, at, s.len())
	}
	return s.start + 1 + o.pick(s.len()-1), nil
}
