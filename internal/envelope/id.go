package envelope

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// IDType records what a JSON-RPC id actually was, because the canonical text
// alone cannot say. See docs/design/transcript.md section 5.
type IDType string

const (
	// IDNumber is a JSON number id.
	IDNumber IDType = "number"
	// IDString is a JSON string id.
	IDString IDType = "string"
	// IDNull is an explicit null id. JSON-RPC permits it on an error
	// response; it is not the same as no id member at all.
	IDNull IDType = "null"
	// IDAbsent is no id member: a notification, or a frame with no envelope.
	IDAbsent IDType = "absent"
	// IDInvalid is an id JSON-RPC does not permit -- an object, an array, a
	// boolean. charpy emits these deliberately, so the model must carry them.
	IDInvalid IDType = "invalid"
)

// ID is a JSON-RPC id held as canonical text plus a type tag rather than as a
// Go any, so that the transcript's id column has exactly one type.
//
// Two ids that differ only in type -- the number 7 and the string "7" -- have
// the same Text and therefore collide in that column. That collision is
// deliberate and documented (transcript.md section 5): it is a real protocol
// ambiguity a subject may itself get wrong. Where the collision would be a bug
// rather than a finding -- charpy's own ledger, which must not confuse the two
// halves of a duplicate_id case with vary_type set -- use Key, which does not
// collide.
type ID struct {
	typ  IDType
	text string
	raw  json.RawMessage
}

// NumberID returns an integer id.
func NumberID(n int64) ID {
	s := strconv.FormatInt(n, 10)
	return ID{typ: IDNumber, text: s, raw: json.RawMessage(s)}
}

// StringID returns a string id.
func StringID(s string) ID {
	raw, err := json.Marshal(s)
	if err != nil { // unreachable: a Go string always marshals
		raw = json.RawMessage(`""`)
	}
	return ID{typ: IDString, text: s, raw: raw}
}

// NullID returns an explicit null id.
func NullID() ID { return ID{typ: IDNull, raw: json.RawMessage("null")} }

// AbsentID returns the absence of an id member.
func AbsentID() ID { return ID{typ: IDAbsent} }

// InvalidID returns an id JSON-RPC does not permit, carrying the exact bytes
// charpy means to put on the wire. It is not validated: emitting something no
// conforming implementation would emit is the point.
func InvalidID(raw json.RawMessage) ID {
	return ID{typ: IDInvalid, text: compact(raw), raw: bytes.Clone(raw)}
}

// ParseID classifies the raw bytes of an id member. A nil or empty raw is
// absent. Nothing here fails: an id charpy cannot classify is IDInvalid, which
// is a value, not an error.
func ParseID(raw json.RawMessage) ID {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return AbsentID()
	}
	id := ID{raw: bytes.Clone(trimmed)}

	switch {
	case bytes.Equal(trimmed, []byte("null")):
		id.typ = IDNull
		return id

	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			id.typ, id.text = IDInvalid, compact(trimmed)
			return id
		}
		id.typ, id.text = IDString, s
		return id

	case trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9'):
		var n json.Number
		if err := json.Unmarshal(trimmed, &n); err != nil {
			id.typ, id.text = IDInvalid, compact(trimmed)
			return id
		}
		// The literal is already canonical for every number an encoder
		// emits. 7 stays 7, so it collides with the string "7" in the
		// transcript column as intended; 7.0 stays 7.0 and does not, which
		// is honest -- they are different ids to some implementations.
		id.typ, id.text = IDNumber, n.String()
		return id

	default:
		id.typ, id.text = IDInvalid, compact(trimmed)
		return id
	}
}

// Type reports what the id was.
func (id ID) Type() IDType { return id.typ }

// Text is the canonical text of the id: the digits of a number, the value of
// a string, the compact JSON of anything JSON-RPC does not permit. Null and
// absent ids have no text.
func (id ID) Text() string { return id.text }

// Raw returns the exact bytes of the id member, which callers must not
// modify. It is empty for an absent id.
func (id ID) Raw() json.RawMessage { return id.raw }

// Present reports whether there was an id member at all. A null id is
// present; a notification's is not.
func (id ID) Present() bool { return id.typ != IDAbsent }

// Key is the ledger key: type-qualified, so the number 7 and the string "7"
// never collide. Charpy's own bookkeeping must distinguish them even where
// the transcript deliberately does not -- a duplicate_id case with
// vary_type = true sends both, and a ledger that merged them would wedge the
// reference peer on charpy's own fault.
func (id ID) Key() string { return string(id.typ) + ":" + id.text }

// Equal reports whether two ids are the same id, type included.
func (id ID) Equal(o ID) bool { return id.typ == o.typ && id.text == o.text }

// AsString returns the same id carried as a string: the number 7 becomes "7".
// This is the duplicate_id mechanism's vary_type parameter, which exploits the
// number/string ambiguity -- an implementation keying its pending map on a
// stringified id treats the two as one request, one keying on the typed value
// does not, and both are defensible.
func (id ID) AsString() ID {
	if id.typ == IDString {
		return id
	}
	return StringID(id.text)
}

func (id ID) String() string {
	switch id.typ {
	case IDAbsent:
		return "(absent)"
	case IDNull:
		return "null"
	case IDString:
		return strconv.Quote(id.text)
	default:
		return id.text
	}
}

// compact renders raw JSON without insignificant whitespace, so that an
// invalid id has one stable text. Bytes that are not JSON at all are returned
// as they came: an unparseable id still has to be representable.
func compact(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return buf.String()
}
