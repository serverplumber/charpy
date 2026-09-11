package wire

import (
	"fmt"
	"strconv"
)

// kind is what an Encoded holds, which decides which cut points apply to it.
type kind int

const (
	kindLine    kind = iota // a newline-delimited stdio frame
	kindEvent               // an SSE event
	kindComment             // an SSE keep-alive comment
)

func (k kind) String() string {
	switch k {
	case kindLine:
		return "stdio frame"
	case kindEvent:
		return "SSE event"
	default:
		return "SSE comment"
	}
}

// span is a half-open byte range within an Encoded's bytes.
type span struct{ start, end int }

func (s span) len() int { return s.end - s.start }

// Encoded is a unit of wire bytes together with the structure a cut point
// needs to locate itself.
//
// The offsets are recorded by the encoder as it builds the bytes rather than
// recovered by parsing them afterwards. Charpy emits frames that no parser
// accepts, so a cut that had to re-parse its own output would be unable to
// truncate exactly the frames truncation exists for.
type Encoded struct {
	Bytes []byte

	kind kind
	// body is the JSON payload: the whole frame on stdio, the data: value on
	// an SSE event, the comment text in a comment.
	body span
	// dataStart is the offset of the "data:" line, which is where a
	// field_boundary cut lands. Meaningful for kindEvent only.
	dataStart int
}

// Len is the number of bytes a complete write would put on the wire.
func (e Encoded) Len() int { return len(e.Bytes) }

// EncodeLine frames a JSON-RPC frame for stdio: the bytes, then a newline.
//
// The frame is not validated or re-serialised. It goes out exactly as given,
// which is the only way a deliberately malformed frame reaches the subject
// intact.
func EncodeLine(frame []byte) Encoded {
	b := make([]byte, 0, len(frame)+1)
	b = append(b, frame...)
	b = append(b, '\n')
	return Encoded{Bytes: b, kind: kindLine, body: span{0, len(frame)}}
}

// EncodeEvent frames a JSON-RPC frame as an SSE event.
//
// Field order is event, data, id. SSE itself permits any order, but
// faults-and-cases.md defines field_boundary as the point "after event: and
// before data:", so the order the cut points describe is the order charpy
// writes. An empty name omits the event field, and an empty id omits the id
// field -- which is what 2026-07-28 wants, having removed resumability.
func EncodeEvent(name string, frame []byte, id string) Encoded {
	var b []byte
	if name != "" {
		b = append(b, "event: "...)
		b = append(b, name...)
		b = append(b, '\n')
	}

	dataStart := len(b)
	b = append(b, "data: "...)
	body := span{start: len(b)}
	b = append(b, frame...)
	body.end = len(b)
	b = append(b, '\n')

	if id != "" {
		b = append(b, "id: "...)
		b = append(b, id...)
		b = append(b, '\n')
	}
	b = append(b, '\n') // the blank line that ends the event

	return Encoded{Bytes: b, kind: kindEvent, body: body, dataStart: dataStart}
}

// EncodeEventID is EncodeEvent with a numeric event id, which is how a
// sessioned-era stream numbers its cursor.
func EncodeEventID(name string, frame []byte, id int64) Encoded {
	return EncodeEvent(name, frame, strconv.FormatInt(id, 10))
}

// EncodeComment frames an SSE keep-alive comment.
//
// 2026-07-28 encourages these, and they are what a stalled stream emits under
// keepalive = "comments" -- a stream that looks perfectly healthy while no
// data ever arrives on it. They are also what mid_comment cuts into.
func EncodeComment(text string) Encoded {
	b := append([]byte(": "), text...)
	body := span{start: 2, end: len(b)}
	b = append(b, '\n')
	return Encoded{Bytes: b, kind: kindComment, body: body}
}

func (e Encoded) requireKind(at CutPoint, want kind) error {
	if e.kind != want {
		return fmt.Errorf("%w: %s does not apply to a %s", ErrNotApplicable, at, e.kind)
	}
	return nil
}
