package fault

import "slices"

// The registry lives here, next to the mechanism implementations, rather than
// in the catalogue: a parameter table maintained apart from the code it
// describes drifts, and a drifted table makes valid cases unloadable — the
// strict-load property inverted. The catalogue validates against this
// registry, and the case-manifest JSON Schema is generated from it, so the
// loader, the editor tooling and the implementation cannot disagree. See
// docs/design/decisions.md ADR-009.

// Value is one permitted value of an enumerated mechanism parameter.
type Value struct {
	Name    string
	Summary string
}

// Param is one parameter a mechanism accepts. Values is nil for free-form
// parameters, in which case Type says what the TOML value must decode to.
type Param struct {
	Name    string
	Type    string // "string" | "integer" | "boolean"; enumerated params are strings
	Summary string
	Values  []Value // nil for free-form
	Default string  // rendered default, informational
}

// Mechanism describes a fault mechanism: code that produces a wire effect,
// and the parameters a case may set on it. A case is a named, parameterised,
// revision-scoped application of one; see docs/design/faults-and-cases.md.
//
// No parameter may be named "kind" — that key selects the mechanism itself in
// a [case.fault] table.
type Mechanism struct {
	Kind    string
	Summary string
	Params  []Param
}

// mechanisms is the v0 registry, in the order docs/design/faults-and-cases.md
// presents them. Adding an entry here is how a mechanism becomes usable from
// a case manifest.
var mechanisms = []Mechanism{
	{
		Kind:    "hang",
		Summary: "Hold a response open and never deliver it. The liveness half of the suite depends on this mechanism's withdrawal behaviour.",
		Params: []Param{
			{Name: "withdraw_after_ms", Type: "integer", Default: "0",
				Summary: "Injected-clock ms until the fault is withdrawn; 0 means never. fault_withdrawn is emitted at the withdrawal instant and is where the liveness clock starts."},
			{Name: "scope", Default: "response",
				Summary: "What is held open.",
				Values: []Value{
					{Name: "response", Summary: "Only the matched response is withheld."},
					{Name: "stream", Summary: "The whole stream goes silent."},
					{Name: "connection", Summary: "The whole connection goes silent."},
				}},
			{Name: "then", Default: "deliver",
				Summary: "What happens at withdrawal.",
				Values: []Value{
					{Name: "deliver", Summary: "The withheld response is finally delivered."},
					{Name: "close", Summary: "The stream or connection is closed without delivering."},
					{Name: "error", Summary: "A JSON-RPC error is delivered instead of the result."},
				}},
		},
	},
	{
		Kind:    "truncate",
		Summary: "Stop mid-frame: emit what was written, then close or stall.",
		Params: []Param{
			{Name: "cut_at", Default: "mid_frame",
				Summary: "Where the cut lands.",
				Values: []Value{
					{Name: "byte", Summary: "Cut at exactly after_bytes, wherever that lands. Both transports."},
					{Name: "mid_frame", Summary: "Cut at a seeded point inside the JSON body. Both transports."},
					{Name: "mid_line", Summary: "Cut before the terminating newline — the frame never delimits. stdio only."},
					{Name: "mid_event", Summary: "Cut inside an SSE data: value. HTTP only."},
					{Name: "field_boundary", Summary: "Cut after event: and before data: — a structurally incomplete SSE event. HTTP only."},
					{Name: "event_boundary", Summary: "Cut cleanly between events — every event delivered was well formed, the stream simply stops. HTTP only."},
					{Name: "mid_comment", Summary: "Cut inside a `:` keep-alive comment line. HTTP only; 2026-07-28 encourages these."},
				}},
			{Name: "after_bytes", Type: "integer", Default: "seeded",
				Summary: "Exact cut point for cut_at = \"byte\"; seeded otherwise."},
			{Name: "then", Default: "close",
				Summary: "What follows the cut.",
				Values: []Value{
					{Name: "close", Summary: "Close the stream."},
					{Name: "stall", Summary: "Hold the stream open silently."},
				}},
		},
	},
	{
		Kind:    "malformed_json",
		Summary: "Bytes a JSON parser rejects.",
		Params: []Param{
			{Name: "how", Default: "unbalanced",
				Summary: "Which way the bytes are wrong.",
				Values: []Value{
					{Name: "unbalanced", Summary: "Unbalanced braces."},
					{Name: "trailing_garbage", Summary: "A valid document followed by garbage."},
					{Name: "bad_utf8", Summary: "An invalid UTF-8 sequence."},
					{Name: "nul_byte", Summary: "An embedded NUL byte."},
					{Name: "deep_nest", Summary: "Nesting deep enough to test recursion limits; see depth."},
					{Name: "duplicate_key", Summary: "Technically valid JSON with undefined semantics: implementations disagree about last-wins versus first-wins, which is differential-table material rather than a bug."},
				}},
			{Name: "depth", Type: "integer", Default: "1000",
				Summary: "Nesting depth for how = \"deep_nest\"."},
		},
	},
	{
		Kind:    "schema_violation",
		Summary: "Valid JSON that violates a declared schema: it passes every parser and fails only the oracle.",
		Params: []Param{
			{Name: "target", Default: "result",
				Summary: "Which declaration is violated.",
				Values: []Value{
					{Name: "envelope", Summary: "The JSON-RPC envelope."},
					{Name: "result", Summary: "The result shape for the method."},
					{Name: "declared_output_schema", Summary: "The subject's own outputSchema from tools/list — the sharpest target, MUST-eligible because the rejecting artifact is the subject's own declaration."},
					{Name: "tool_input_schema", Summary: "The tool's declared inputSchema."},
				}},
			{Name: "how", Default: "wrong_type",
				Summary: "Which way the value violates it.",
				Values: []Value{
					{Name: "wrong_type", Summary: "A value of the wrong type."},
					{Name: "missing_required", Summary: "A required field absent."},
					{Name: "extra_required_absent", Summary: "An extra field where the schema forbids additions."},
					{Name: "enum_out_of_range", Summary: "A value outside a declared enum."},
				}},
		},
	},
	{
		Kind:    "duplicate_id",
		Summary: "Reuse a JSON-RPC id.",
		Params: []Param{
			{Name: "mode", Default: "double_response",
				Summary: "How the id is reused.",
				Values: []Value{
					{Name: "concurrent_request", Summary: "Two requests share an id while both are in flight."},
					{Name: "double_response", Summary: "The same request id is answered twice."},
					{Name: "reuse_after_close", Summary: "An id is reused after its exchange closed."},
				}},
			{Name: "vary_type", Type: "boolean", Default: "false",
				Summary: "Send 7 and then \"7\": an implementation keying its pending map on a stringified id treats these as the same request; one keying on the typed value does not. Both are defensible, which makes it differential material."},
		},
	},
	{
		Kind:    "unsolicited_response",
		Summary: "A response or error for an id never requested.",
		Params: []Param{
			{Name: "id_source", Default: "never_used",
				Summary: "Where the bogus id comes from.",
				Values: []Value{
					{Name: "never_used", Summary: "An id that never appeared on the wire."},
					{Name: "already_resolved", Summary: "A late duplicate for an id correctly resolved earlier — the one that finds pending-map leaks."},
					{Name: "reserved_null", Summary: "The null id, which JSON-RPC reserves."},
				}},
		},
	},
	{
		Kind:    "manifest_mutate",
		Summary: "Change the tool, prompt or resource list mid-session.",
		Params: []Param{
			{Name: "list", Default: "tools",
				Summary: "Which list mutates.",
				Values: []Value{
					{Name: "tools", Summary: "The tool list."},
					{Name: "prompts", Summary: "The prompt list."},
					{Name: "resources", Summary: "The resource list."},
				}},
			{Name: "op", Default: "remove",
				Summary: "The mutation.",
				Values: []Value{
					{Name: "add", Summary: "An entry appears."},
					{Name: "remove", Summary: "An entry disappears."},
					{Name: "rename", Summary: "An entry changes name."},
					{Name: "change_schema", Summary: "An entry's declared schema changes shape."},
				}},
			{Name: "notify", Default: "list_changed",
				Summary: "Whether and how the peer is told.",
				Values: []Value{
					{Name: "list_changed", Summary: "notifications/*/list_changed is sent."},
					{Name: "silent", Summary: "No notification. Not adversarial: at least one production gateway polls its upstreams rather than trusting list_changed, so this matches deployed reality."},
					{Name: "subscriptions_listen", Summary: "The 2026-07-28 path: the change arrives on the subscriptions/listen response stream."},
				}},
		},
	},
	{
		Kind:    "capability_flip",
		Summary: "Reconnect as a peer claiming different capabilities.",
		Params: []Param{
			{Name: "field", Default: "capabilities",
				Summary: "What changes across the reconnect.",
				Values: []Value{
					{Name: "capabilities", Summary: "The capability set."},
					{Name: "protocol_version", Summary: "The protocol version — the era-mismatch case."},
					{Name: "server_info", Summary: "The server identity."},
					{Name: "extensions", Summary: "Declared extensions."},
				}},
			{Name: "direction_of_change", Default: "narrow",
				Summary: "Which way it changes.",
				Values: []Value{
					{Name: "narrow", Summary: "Fewer than before — the dangerous one: the subject cached a capability and keeps using it after the peer stopped offering it."},
					{Name: "widen", Summary: "More than before."},
					{Name: "incompatible", Summary: "A set the first connection could not have negotiated."},
				}},
		},
	},
}

// Mechanisms returns the registry in presentation order.
func Mechanisms() []Mechanism { return slices.Clone(mechanisms) }

// Lookup finds a mechanism by kind.
func Lookup(kind string) (Mechanism, bool) {
	for _, m := range mechanisms {
		if m.Kind == kind {
			return m, true
		}
	}
	return Mechanism{}, false
}

// Kinds returns every mechanism kind, in registry order.
func Kinds() []string {
	out := make([]string, 0, len(mechanisms))
	for _, m := range mechanisms {
		out = append(out, m.Kind)
	}
	return out
}

// Param finds a parameter by name.
func (m Mechanism) Param(name string) (Param, bool) {
	for _, p := range m.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// ParamNames returns the parameter names, in registry order.
func (m Mechanism) ParamNames() []string {
	out := make([]string, 0, len(m.Params))
	for _, p := range m.Params {
		out = append(out, p.Name)
	}
	return out
}

// Allows reports whether v is a permitted value. Free-form parameters allow
// anything of their Type; the caller checks that separately.
func (p Param) Allows(v string) bool {
	for _, val := range p.Values {
		if val.Name == v {
			return true
		}
	}
	return false
}

// ValueNames returns the permitted values, in registry order. Nil for
// free-form parameters.
func (p Param) ValueNames() []string {
	if p.Values == nil {
		return nil
	}
	out := make([]string, 0, len(p.Values))
	for _, v := range p.Values {
		out = append(out, v.Name)
	}
	return out
}
