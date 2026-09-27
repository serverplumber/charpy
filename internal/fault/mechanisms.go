package fault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/seed"
	"github.com/serverplumber/charpy/internal/wire"
)

// The eight v0 mechanisms, in the order faults-and-cases.md section 2 presents
// them. Each is small on purpose: the design judgement lives in the cases, and
// a mechanism that grew logic of its own would be a case nobody could cite.

// hang holds a response open and never delivers it. The liveness half of the
// suite depends on its withdrawal behaviour, which is why the withdrawal
// instant is carried rather than implied.
func planHang(p Plan, params map[string]any) (Plan, error) {
	p.Hold = &Hold{
		For:   time.Duration(integer(params, "withdraw_after_ms", 0)) * time.Millisecond,
		Then:  str(params, "then", "deliver"),
		Scope: str(params, "scope", "response"),
	}
	p.Keepalive = keepalive(params)
	return p, nil
}

// truncate stops mid-frame. The cut itself belongs to internal/wire, which
// knows where an SSE event's fields begin and end; what happens here is
// choosing the point and carrying the seed to it.
func planTruncate(p Plan, m envelope.Message, params map[string]any, ctx Context) (Plan, error) {
	cut := &Cut{At: wire.CutPoint(str(params, "cut_at", string(wire.CutMidFrame)))}
	if n := integer(params, "after_bytes", -1); n >= 0 {
		cut.Opts.AfterBytes = int(n)
	}
	cut.Opts.Pick = seed.Pick(seed.For(ctx.RunSeed, p.Case.ID, seed.CutOffset))

	p.Deliver = &m
	p.Cut = cut
	p.Keepalive = keepalive(params)

	switch then := str(params, "then", "close"); then {
	case "close":
		p.Then = StreamClose
	case "stall":
		p.Then = StreamStall
	default:
		return Plan{}, fmt.Errorf("fault: truncate then = %q", then)
	}
	return p, nil
}

// malformedJSON emits bytes a parser rejects.
//
// Every variant works on the frame's own bytes rather than rebuilding one,
// because a frame charpy re-serialised would differ from the subject's in ways
// that have nothing to do with the fault.
func planMalformedJSON(p Plan, m envelope.Message, params map[string]any, ctx Context) (Plan, error) {
	raw := bytes.Clone(m.Raw())
	how := str(params, "how", "unbalanced")

	var out []byte
	switch how {
	case "unbalanced":
		// Drop the closing brace: the commonest shape of a stream that ended
		// early, and one every parser rejects.
		out = bytes.TrimRight(raw, " \t\r\n")
		if n := len(out); n > 0 && out[n-1] == '}' {
			out = out[:n-1]
		}
	case "trailing_garbage":
		out = append(raw, " not json"...)
	case "bad_utf8":
		out = spliceIntoBody(raw, []byte{0xff, 0xfe})
	case "nul_byte":
		out = spliceIntoBody(raw, []byte{0x00})
	case "deep_nest":
		depth := int(integer(params, "depth", 1000))
		out = []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":` +
			strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`)
	case "duplicate_key":
		// Valid JSON with undefined semantics, which is the interesting case:
		// implementations disagree about last-wins versus first-wins, and the
		// disagreement is differential-table material rather than a bug.
		out = duplicateFirstKey(raw)
	default:
		return Plan{}, fmt.Errorf("fault: malformed_json how = %q", how)
	}

	// The seed is drawn even where the variant is deterministic, so that
	// adding a seeded variant later does not shift the others.
	_ = seed.For(ctx.RunSeed, p.Case.ID, seed.Payload)

	broken, _ := envelope.Parse(out)
	p.Deliver = &broken
	return p, nil
}

// spliceIntoBody puts bytes inside the frame rather than at either end, so the
// damage is somewhere a parser must actually reach.
func spliceIntoBody(raw, insert []byte) []byte {
	at := len(raw) / 2
	out := make([]byte, 0, len(raw)+len(insert))
	out = append(out, raw[:at]...)
	out = append(out, insert...)
	return append(out, raw[at:]...)
}

// duplicateFirstKey repeats the frame's first member with a different value.
func duplicateFirstKey(raw []byte) []byte {
	i := bytes.IndexByte(raw, '"')
	if i < 0 {
		return raw
	}
	j := bytes.IndexByte(raw[i+1:], '"')
	if j < 0 {
		return raw
	}
	key := string(raw[i : i+j+2])

	out := make([]byte, 0, len(raw)+len(key)+16)
	out = append(out, raw[:i]...)
	out = append(out, key...)
	out = append(out, `:"duplicate",`...)
	return append(out, raw[i:]...)
}

// schemaViolation emits valid JSON that violates a declared schema. It passes
// every parser and fails only the oracle, which is what makes it the
// interesting one.
func planSchemaViolation(p Plan, m envelope.Message, params map[string]any, ctx Context) (Plan, error) {
	target := str(params, "target", "result")
	how := str(params, "how", "wrong_type")

	switch target {
	case "declared_output_schema":
		// This breaks a shape somebody declared, so the declaration has to be
		// in hand. Where charpy serves, it is charpy's own and the driver
		// passes it; where charpy relays a real server it would have to ask
		// with its own tools/list first, which is not built, so the case does
		// not apply there rather than breaking a shape it has not read.
		if len(ctx.OutputSchema) == 0 {
			return Plan{}, fmt.Errorf("%w: schema_violation target %q with no declared schema in hand",
				ErrNotApplicable, target)
		}
		out, err := violateDeclared(m, ctx.OutputSchema, how)
		if err != nil {
			return Plan{}, err
		}
		p.Deliver = &out
		return p, nil
	case "tool_input_schema":
		return Plan{}, fmt.Errorf("%w: schema_violation target %q", ErrNotApplicable, target)
	case "envelope":
		out, err := violateEnvelope(m, how)
		if err != nil {
			return Plan{}, err
		}
		p.Deliver = &out
		return p, nil
	case "result":
		out, err := violateResult(m, how)
		if err != nil {
			return Plan{}, err
		}
		p.Deliver = &out
		return p, nil
	default:
		return Plan{}, fmt.Errorf("fault: schema_violation target = %q", target)
	}
}

// violateEnvelope breaks the JSON-RPC envelope itself, which is the one shape
// charpy can break without having read anything the subject said.
func violateEnvelope(m envelope.Message, how string) (envelope.Message, error) {
	obj, err := object(m.Raw())
	if err != nil {
		return envelope.Message{}, err
	}

	switch how {
	case "wrong_type":
		obj["jsonrpc"] = 2.0 // a number where the schema fixes the string "2.0"
	case "missing_required":
		delete(obj, "jsonrpc")
	case "extra_required_absent":
		delete(obj, "jsonrpc")
		obj["jsonrpc_version"] = "2.0"
	case "enum_out_of_range":
		obj["jsonrpc"] = "3.0"
	default:
		return envelope.Message{}, fmt.Errorf("fault: schema_violation how = %q", how)
	}
	return remarshal(obj)
}

// violateResult breaks the result's shape without needing to know what shape
// was declared: every how here is wrong against any object-valued result.
func violateResult(m envelope.Message, how string) (envelope.Message, error) {
	if m.Kind != envelope.KindResponse {
		return envelope.Message{}, fmt.Errorf(
			"fault: schema_violation target = \"result\" needs a response, got %s", m.Kind)
	}
	obj, err := object(m.Raw())
	if err != nil {
		return envelope.Message{}, err
	}

	switch how {
	case "wrong_type":
		obj["result"] = "a string where an object belongs"
	case "missing_required":
		obj["result"] = map[string]any{}
	case "extra_required_absent":
		obj["result"] = map[string]any{"unexpected": true}
	case "enum_out_of_range":
		obj["result"] = map[string]any{"resultType": "sideways"}
	default:
		return envelope.Message{}, fmt.Errorf("fault: schema_violation how = %q", how)
	}
	return remarshal(obj)
}

// violateDeclared breaks a result's structuredContent against the outputSchema
// its tool declared: valid JSON-RPC, a valid CallToolResult, and wrong only
// against the declaration -- which a client that validates structured output
// is supposed to catch.
//
// The property it breaks is the first, by name, that the schema declares and
// the content carries, so the fault is the same on every run without drawing
// on the seed for a choice nothing reads.
func violateDeclared(m envelope.Message, schema json.RawMessage, how string) (envelope.Message, error) {
	if m.Kind != envelope.KindResponse {
		return envelope.Message{}, fmt.Errorf("%w: declared_output_schema needs a result, got %s",
			ErrNotApplicable, m.Kind)
	}
	var decl struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &decl); err != nil {
		return envelope.Message{}, fmt.Errorf("fault: declared outputSchema is not a JSON object: %w", err)
	}
	obj, err := object(m.Raw())
	if err != nil {
		return envelope.Message{}, err
	}
	result, _ := obj["result"].(map[string]any)
	content, _ := result["structuredContent"].(map[string]any)
	if content == nil {
		return envelope.Message{}, fmt.Errorf("%w: the result carries no structuredContent to break",
			ErrNotApplicable)
	}

	names := make([]string, 0, len(decl.Properties))
	for name := range decl.Properties {
		if _, ok := content[name]; ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	switch how {
	case "wrong_type":
		for _, name := range names {
			if v, ok := wrongTypeFor(decl.Properties[name].Type); ok {
				content[name] = v
				return remarshal(obj)
			}
		}
		return envelope.Message{}, fmt.Errorf("%w: no typed property to give the wrong type", ErrNotApplicable)
	case "missing_required":
		req := slices.Clone(decl.Required)
		slices.Sort(req)
		for _, name := range req {
			if _, ok := content[name]; ok {
				delete(content, name)
				return remarshal(obj)
			}
		}
		return envelope.Message{}, fmt.Errorf("%w: no required property present to remove", ErrNotApplicable)
	default:
		return envelope.Message{}, fmt.Errorf("%w: schema_violation how %q against a declared schema",
			ErrNotApplicable, how)
	}
}

// wrongTypeFor is a value of some type the declaration does not allow.
func wrongTypeFor(declared any) (any, bool) {
	t, _ := declared.(string)
	switch t {
	case "number", "integer":
		return "not a number", true
	case "string":
		return 0, true
	case "boolean":
		return "not a boolean", true
	case "object":
		return "not an object", true
	case "array":
		return "not an array", true
	}
	return nil, false
}

// duplicateID reuses a JSON-RPC id.
func planDuplicateID(p Plan, m envelope.Message, params map[string]any) (Plan, error) {
	mode := str(params, "mode", "double_response")

	id := m.ID
	if boolean(params, "vary_type", false) {
		// Send 7 then "7". An implementation keying its pending map on a
		// stringified id treats these as one request; one keying on the typed
		// value does not. Both are defensible, which makes it differential
		// material rather than a bug.
		id = id.AsString()
	}

	// double_response and concurrent_request do the same thing to the wire --
	// send the matched frame again under the same id -- and differ only in
	// which frame the case matched. Rather than two identical branches, the
	// mode is checked against the frame's kind, so a case that names one and
	// matches the other fails loudly instead of injecting the other fault.
	switch mode {
	case "double_response":
		if m.Kind != envelope.KindResponse && m.Kind != envelope.KindError {
			return Plan{}, fmt.Errorf("%w: duplicate_id mode %q matched a %s, not an answer",
				ErrNotApplicable, mode, m.Kind)
		}
	case "concurrent_request":
		if m.Kind != envelope.KindRequest {
			return Plan{}, fmt.Errorf("%w: duplicate_id mode %q matched a %s, not a request",
				ErrNotApplicable, mode, m.Kind)
		}
	case "reuse_after_close":
		// The reuse happens on the *next* connection, so what this mode needs
		// is an id carried across a close -- which a plan for one frame
		// cannot express, and the driver has to hold.
		return Plan{}, fmt.Errorf("%w: duplicate_id mode %q spans a connection boundary",
			ErrNotApplicable, mode)
	default:
		return Plan{}, fmt.Errorf("fault: duplicate_id mode = %q", mode)
	}

	second, err := retarget(m, id)
	if err != nil {
		return Plan{}, err
	}
	p.Deliver = &m
	p.After = []envelope.Message{second}
	return p, nil
}

// unsolicitedResponse answers an id nothing asked about.
//
// The matched frame still crosses, and that is the whole case: a late answer
// for an id already resolved is an *extra* frame beside the traffic, not a
// substitute for it. Dropping the matched one instead makes every subject look
// wedged -- it is waiting on an answer charpy ate -- which is a false positive
// against software charpy does not own, and credibility spends once.
func planUnsolicitedResponse(p Plan, m envelope.Message, params map[string]any, ctx Context) (Plan, error) {
	source := str(params, "id_source", "never_used")

	var id envelope.ID
	switch source {
	case "never_used":
		// High enough that no scenario reaches it, and seeded so two runs of
		// the same case pick the same one.
		r := seed.For(ctx.RunSeed, p.Case.ID, seed.IDs)
		id = envelope.NumberID(1_000_000 + int64(r.IntN(1_000_000)))
	case "already_resolved":
		// The one that finds pending-map leaks: a late duplicate for an id
		// that was correctly resolved. Without one the case does not apply.
		if !ctx.Resolved.Present() {
			return Plan{}, fmt.Errorf(
				"%w: id_source %q needs an id this connection already resolved", ErrNotApplicable, source)
		}
		id = ctx.Resolved
	case "reserved_null":
		id = envelope.NullID()
	default:
		return Plan{}, fmt.Errorf("fault: unsolicited_response id_source = %q", source)
	}

	extra, err := envelope.NewResponse(id, json.RawMessage(`{}`))
	if err != nil {
		return Plan{}, err
	}
	p.Before = []envelope.Message{extra}
	p.Deliver = &m
	return p, nil
}

// manifestMutate changes a list mid-session.
//
// Where charpy serves, the change is legitimate reconfiguration of its own
// peer and only the notification is a fault. Where charpy relays a real
// server, the change is a rewrite of the forwarded result -- so which frame
// matched decides which half of the mechanism runs.
func planManifestMutate(p Plan, m envelope.Message, params map[string]any) (Plan, error) {
	list := str(params, "list", "tools")
	notify := str(params, "notify", "list_changed")

	if m.Kind == envelope.KindNotification {
		if notify != "silent" {
			return Plan{}, fmt.Errorf(
				"fault: manifest_mutate matched a notification but notify = %q", notify)
		}
		// Notification reliability is already distrusted in the field: at
		// least one production gateway polls its upstreams rather than
		// trusting list_changed. The suppression is the whole fault.
		p.Swallow = true
		return p, nil
	}

	out, err := mutateList(m, list, str(params, "op", "remove"))
	if err != nil {
		return Plan{}, err
	}
	p.Deliver = &out
	return p, nil
}

func mutateList(m envelope.Message, list, op string) (envelope.Message, error) {
	obj, err := object(m.Raw())
	if err != nil {
		return envelope.Message{}, err
	}
	result, ok := obj["result"].(map[string]any)
	if !ok {
		return envelope.Message{}, fmt.Errorf(
			"fault: manifest_mutate needs a %s list result, got %s", list, m.Kind)
	}
	items, ok := result[list].([]any)
	if !ok || len(items) == 0 {
		return envelope.Message{}, fmt.Errorf("fault: manifest_mutate found no %q to change", list)
	}

	switch op {
	case "remove":
		result[list] = items[1:]
	case "add":
		result[list] = append(items, map[string]any{
			"name":        "charpy.added",
			"description": "an entry the subject never declared",
			"inputSchema": map[string]any{"type": "object"},
		})
	case "rename":
		if first, ok := items[0].(map[string]any); ok {
			first["name"] = "charpy.renamed"
		}
	case "change_schema":
		if first, ok := items[0].(map[string]any); ok {
			first["inputSchema"] = map[string]any{
				"type":                 "object",
				"required":             []any{"charpyRequired"},
				"additionalProperties": false,
			}
		}
	default:
		return envelope.Message{}, fmt.Errorf("fault: manifest_mutate op = %q", op)
	}
	return remarshal(obj)
}

// capabilityFlip claims something different from what was claimed before.
//
// narrow is the dangerous one: the subject cached a capability from the first
// connection and keeps using it after the peer stopped offering it.
func planCapabilityFlip(p Plan, m envelope.Message, params map[string]any) (Plan, error) {
	field := str(params, "field", "capabilities")
	change := str(params, "direction_of_change", "narrow")

	obj, err := object(m.Raw())
	if err != nil {
		return Plan{}, err
	}
	result, ok := obj["result"].(map[string]any)
	if !ok {
		return Plan{}, fmt.Errorf("fault: capability_flip needs an initialize result, got %s", m.Kind)
	}

	switch field {
	case "capabilities":
		switch change {
		case "narrow":
			result["capabilities"] = map[string]any{}
		case "widen":
			result["capabilities"] = map[string]any{
				"tools": map[string]any{"listChanged": true}, "prompts": map[string]any{},
				"resources": map[string]any{"subscribe": true}, "logging": map[string]any{},
			}
		case "incompatible":
			result["capabilities"] = "not an object"
		default:
			return Plan{}, fmt.Errorf("fault: capability_flip direction_of_change = %q", change)
		}
	case "protocol_version":
		result["protocolVersion"] = flippedVersion(change)
	case "server_info":
		result["serverInfo"] = map[string]any{"name": "charpy.flipped", "version": "0.0.0"}
	case "extensions":
		result["extensions"] = map[string]any{"dev.charpy/unknown": true}
	default:
		return Plan{}, fmt.Errorf("fault: capability_flip field = %q", field)
	}

	out, err := remarshal(obj)
	if err != nil {
		return Plan{}, err
	}
	p.Deliver = &out
	return p, nil
}

func flippedVersion(change string) string {
	switch change {
	case "widen":
		return "2026-07-28"
	case "incompatible":
		return "1999-01-01"
	default:
		return "2024-11-05"
	}
}

// object decodes a frame for structural editing, keeping numbers in their
// literal spelling so that re-marshalling changes only what the fault meant to
// change.
func object(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("fault: frame is not a JSON object: %w", err)
	}
	return obj, nil
}

func remarshal(obj map[string]any) (envelope.Message, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return envelope.Message{}, fmt.Errorf("fault: %w", err)
	}
	m, _ := envelope.Parse(b)
	return m, nil
}

// retarget rebuilds a frame under a different id, for the mechanisms that send
// the same thing twice.
func retarget(m envelope.Message, id envelope.ID) (envelope.Message, error) {
	if id.Equal(m.ID) {
		return m, nil
	}
	obj, err := object(m.Raw())
	if err != nil {
		return envelope.Message{}, err
	}
	obj["id"] = json.RawMessage(id.Raw())
	return remarshal(obj)
}
