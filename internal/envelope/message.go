package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Version is the JSON-RPC version every MCP revision uses.
const Version = "2.0"

// Kind classifies a frame by the JSON-RPC envelope recovered from its bytes.
type Kind string

const (
	KindRequest      Kind = "request"
	KindResponse     Kind = "response"
	KindError        Kind = "error"
	KindNotification Kind = "notification"

	// KindMalformed means the bytes do not denote exactly one JSON-RPC
	// message: they are not JSON, not an object, an object with nothing to
	// dispatch on, or an object carrying several dispatchable members at
	// once. It is a first-class kind because charpy emits every one of those
	// deliberately, and the transcript must carry them without losing a byte.
	//
	// A malformed message has no id, method, result or error, by
	// construction rather than by convention. That is the point: a frame
	// charpy cannot read as one message does not enter the ledger or the
	// id-resolution invariants at all, rather than entering them as a guess.
	KindMalformed Kind = "malformed"
)

// Error is the error member of an error response.
type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is one JSON-RPC frame: the envelope charpy could recover, plus the
// exact bytes it was recovered from.
//
// The bytes are authoritative. Every re-emission comes from them, so a frame
// charpy relays or replays crosses the wire byte for byte, including the ones
// no conforming implementation would produce.
type Message struct {
	Kind    Kind
	ID      ID
	Method  string
	JSONRPC string
	Params  json.RawMessage
	Result  json.RawMessage
	Error   *Error

	raw []byte
}

// Raw returns the exact bytes of the frame. Callers must not modify them: the
// transcript writer holds this slice across an asynchronous write.
func (m Message) Raw() []byte { return m.raw }

// Len is the true length of the frame in bytes, before any transcript cap.
func (m Message) Len() int { return len(m.raw) }

// ResultType reports the resultType member of a result and whether it was
// present. 2026-07-28 made it required -- "complete" for an ordinary result,
// "input_required" for an MRTR interim result -- and clients must treat an
// omitted field from an earlier-protocol server as "complete" (SEP-2322).
//
// charpy records what was on the wire and does not apply that default: the
// transcript is the observation, and inferring the field would hide a server
// that omitted it. Any other value is returned as it came, for the schema
// layer to reject.
func (m Message) ResultType() (string, bool) {
	if m.Kind != KindResponse || len(m.Result) == 0 {
		return "", false
	}
	var probe struct {
		ResultType *string `json:"resultType"`
	}
	if err := json.Unmarshal(m.Result, &probe); err != nil || probe.ResultType == nil {
		return "", false
	}
	return *probe.ResultType, true
}

// Parse recognises the JSON-RPC message a frame's bytes denote, if they
// denote exactly one.
//
// It never discards bytes and never refuses: bytes that are not exactly one
// message come back as a KindMalformed message carrying every one of them,
// alongside an error saying why. Callers that only transcribe may ignore the
// error; callers that dispatch must not.
//
// Recognition, not recovery. A forgiving parser that extracts whatever
// envelope it can must break ties when a frame carries more than one
// dispatchable member -- and charpy emits exactly those frames on purpose
// (schema_violation with target = "envelope"). Breaking the tie writes a
// guess about someone else's frame into the kind column, which I1, I2 and I3
// then count. This package does not guess, for the same reason the oracle
// never issues a verdict from an inferred join.
//
// A member counts as present when the transcript can record it: method a
// non-empty string, error an object carrying an integer code, result any JSON
// value including null. That asymmetry is JSON-RPC's -- a result may be null,
// an error must be an object -- not a convenience. Exactly one present is a
// message; none or several is not.
func Parse(b []byte) (Message, error) {
	raw := bytes.Clone(b)

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return malformed(raw), errors.New("envelope: empty frame")
	}
	if trimmed[0] != '{' {
		return malformed(raw), errors.New("envelope: frame is not a JSON object")
	}

	var probe struct {
		JSONRPC json.RawMessage `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  json.RawMessage `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&probe); err != nil {
		return malformed(raw), fmt.Errorf("envelope: %w", err)
	}
	if dec.More() {
		return malformed(raw), errors.New("envelope: trailing bytes after the JSON object")
	}
	// Duplicate keys are deliberately not detected here. encoding/json takes
	// the last, other implementations take the first, and that disagreement
	// is differential-table material rather than a parse failure. The bytes
	// are preserved, so the oracle can find it offline.

	m := Message{ID: ParseID(probe.ID), raw: raw}

	var jsonrpc string
	if json.Unmarshal(probe.JSONRPC, &jsonrpc) == nil {
		m.JSONRPC = jsonrpc
	}

	var method string
	hasMethod := len(probe.Method) > 0 && json.Unmarshal(probe.Method, &method) == nil && method != ""
	parsedError, hasError := parseError(probe.Error)
	hasResult := len(probe.Result) > 0

	if n := count(hasMethod, hasError, hasResult); n != 1 {
		return malformed(raw), fmt.Errorf("envelope: %s; a JSON-RPC message carries exactly one",
			members(hasMethod, hasError, hasResult))
	}

	switch {
	case hasMethod:
		m.Method, m.Params = method, probe.Params
		// A notification is a request with no id member. An explicit
		// "id": null is a member, so it is a request with a null id -- which
		// is what the unsolicited_response mechanism's reserved_null case
		// puts on the wire.
		if m.ID.Present() {
			m.Kind = KindRequest
		} else {
			m.Kind = KindNotification
		}
	case hasError:
		m.Kind, m.Error = KindError, parsedError
	case hasResult:
		m.Kind, m.Result = KindResponse, probe.Result
	}
	return m, nil
}

func count(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// members names what the frame carried, for the error a malformed frame comes
// back with. It is the diagnostic that replaces the column charpy declines to
// fill: the kind says the bytes are not a message, this says why.
func members(hasMethod, hasError, hasResult bool) string {
	var present []string
	for _, m := range []struct {
		ok   bool
		name string
	}{{hasMethod, "method"}, {hasError, "error"}, {hasResult, "result"}} {
		if m.ok {
			present = append(present, m.name)
		}
	}
	if len(present) == 0 {
		return "frame carries no method, result or error member"
	}
	return "frame carries " + strings.Join(present, " and ")
}

// malformed builds the only shape a malformed message may have: the bytes,
// and nothing else. Keeping this in one place is what makes the transcript's
// "malformed frames carry no parsed envelope" rule structural.
func malformed(raw []byte) Message {
	return Message{Kind: KindMalformed, ID: AbsentID(), raw: raw}
}

// parseError reads the error member. A member with no integer code is not an
// error envelope: the transcript's error_code column is what the oracle reads,
// and a frame that cannot fill it should not claim to be an error.
func parseError(raw json.RawMessage) (*Error, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var probe struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	var code int64
	if err := json.Unmarshal(probe.Code, &code); err != nil {
		return nil, false
	}
	e := &Error{Code: code, Data: probe.Data}
	// A missing or non-string message is a schema-layer finding, not a
	// reason to lose the code.
	_ = json.Unmarshal(probe.Message, &e.Message)
	return e, true
}

// The wire structs fix the member order of frames charpy originates. Field
// order is not semantic in JSON, but a stable one makes transcripts and
// golden files diffable.

type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type wireNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
}

type wireError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *Error          `json:"error"`
}

// NewRequest builds a request frame. The id may be any ID, including one
// JSON-RPC does not permit -- the synthesize verb needs to emit those.
func NewRequest(id ID, method string, params json.RawMessage) (Message, error) {
	return build(wireRequest{JSONRPC: Version, ID: idRaw(id), Method: method, Params: params})
}

// NewNotification builds a notification frame: a request with no id member.
func NewNotification(method string, params json.RawMessage) (Message, error) {
	return build(wireNotification{JSONRPC: Version, Method: method, Params: params})
}

// NewResponse builds a result frame.
func NewResponse(id ID, result json.RawMessage) (Message, error) {
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return build(wireResponse{JSONRPC: Version, ID: idRaw(id), Result: result})
}

// NewError builds an error frame.
func NewError(id ID, code int64, message string, data json.RawMessage) (Message, error) {
	return build(wireError{
		JSONRPC: Version,
		ID:      idRaw(id),
		Error:   &Error{Code: code, Message: message, Data: data},
	})
}

// build marshals an originated frame and reparses it, so that a constructed
// message and a received one are the same thing: bytes, with an envelope
// recovered from them. Nothing downstream needs to know which it holds.
func build(v any) (Message, error) {
	b, err := json.Marshal(v)
	if err != nil {
		// Reachable: params and result are raw JSON supplied by a caller,
		// and encoding/json validates them on the way out.
		return malformed(nil), fmt.Errorf("envelope: %w", err)
	}
	m, err := Parse(b)
	if err != nil {
		return m, fmt.Errorf("envelope: originated frame does not parse: %w", err)
	}
	return m, nil
}

// idRaw renders an id for a wire struct. An absent id marshals as JSON null
// rather than vanishing, because a response without an id member is a
// different frame from one with a null id, and a caller asking for a response
// should get the frame they asked for.
func idRaw(id ID) json.RawMessage {
	if raw := id.Raw(); len(raw) > 0 {
		return raw
	}
	return json.RawMessage("null")
}
