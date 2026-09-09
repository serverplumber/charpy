package catalogue

import (
	"fmt"
	"slices"
	"strings"
)

// Mechanism describes a fault mechanism and the parameters it accepts.
//
// A mechanism is code that produces a wire effect; a case is a named,
// parameterised application of one. Keeping the accepted parameters here means
// a typo in a case manifest fails at load with the alternatives listed, rather
// than silently producing a fault that does nothing. See
// docs/design/faults-and-cases.md.
type Mechanism struct {
	Kind   string
	Params map[string][]string // param name -> allowed values, nil for free-form
}

// mechanisms is the v0 registry. Adding one here is how a mechanism becomes
// usable from a case manifest.
var mechanisms = []Mechanism{
	{Kind: "hang", Params: map[string][]string{
		"withdraw_after_ms": nil,
		"scope":             {"response", "stream", "connection"},
		"then":              {"deliver", "close", "error"},
	}},
	{Kind: "truncate", Params: map[string][]string{
		"cut_at": {"byte", "mid_frame", "mid_line", "mid_event",
			"field_boundary", "event_boundary", "mid_comment"},
		"after_bytes": nil,
		"then":        {"close", "stall"},
	}},
	{Kind: "malformed_json", Params: map[string][]string{
		"kind": {"unbalanced", "trailing_garbage", "bad_utf8", "nul_byte",
			"deep_nest", "duplicate_key"},
		"depth": nil,
	}},
	{Kind: "schema_violation", Params: map[string][]string{
		"target": {"envelope", "result", "declared_output_schema", "tool_input_schema"},
		"how": {"wrong_type", "missing_required", "extra_required_absent",
			"enum_out_of_range"},
	}},
	{Kind: "duplicate_id", Params: map[string][]string{
		"mode":      {"concurrent_request", "double_response", "reuse_after_close"},
		"vary_type": nil,
	}},
	{Kind: "unsolicited_response", Params: map[string][]string{
		"id_source": {"never_used", "already_resolved", "reserved_null"},
	}},
	{Kind: "manifest_mutate", Params: map[string][]string{
		"list":   {"tools", "prompts", "resources"},
		"op":     {"add", "remove", "rename", "change_schema"},
		"notify": {"list_changed", "silent", "subscriptions_listen"},
	}},
	{Kind: "capability_flip", Params: map[string][]string{
		"field":               {"capabilities", "protocol_version", "server_info", "extensions"},
		"direction_of_change": {"narrow", "widen", "incompatible"},
	}},
}

// matchKeys are the keys [case.match] accepts.
var matchKeys = []string{
	"method", "face", "direction", "kind",
	"client_id", "session_id",
	"occurrence", "occurrence_every", "after_mono_ms", "scope",
}

// scopes name the population an occurrence counts within. In v0 there is one
// client and one session, so every scope collapses to "run". The field exists
// so that counters are keyed by (scope, dimension) from the start: soak mode
// needs rates over populations, and a matcher that has only ever counted
// globally cannot grow one. See docs/design/policy-format.md.
var scopes = []string{"run", "client", "session", "connection"}

// expectKeys are the keys [case.expect] accepts.
var expectKeys = []string{
	"invariants", "liveness_probe_within_ms", "expect_error_code", "expect_http_status",
}

// invariants are the oracle invariants a case may name. Unknown names are a
// load error: an expectation that references nothing is worse than none.
var invariants = []string{
	"id-resolves-once", "no-unsolicited-response", "no-duplicate-inflight-id",
	"cancel-honoured",
	"no-credential-leak", "session-identity-isolation", "merged-manifest-consistency",
	"header-body-consistency", "cache-scope-isolation", "subscription-id-remap",
	"mrtr-state-isolation", "loglevel-gating", "trace-context-propagation",
}

func lookupMechanism(kind string) (Mechanism, bool) {
	for _, m := range mechanisms {
		if m.Kind == kind {
			return m, true
		}
	}
	return Mechanism{}, false
}

// checkFault validates a [case.fault] table against its mechanism.
func checkFault(fault map[string]any) []string {
	kindV, ok := fault["kind"]
	if !ok {
		return []string{"[case.fault] needs a kind"}
	}
	kind, ok := kindV.(string)
	if !ok {
		return []string{"[case.fault] kind must be a string"}
	}

	m, ok := lookupMechanism(kind)
	if !ok {
		return []string{fmt.Sprintf("unknown fault kind %q; known mechanisms: %s",
			kind, strings.Join(mechanismKinds(), ", "))}
	}

	var errs []string
	for k, v := range fault {
		if k == "kind" {
			continue
		}
		allowed, known := m.Params[k]
		if !known {
			errs = append(errs, fmt.Sprintf("unknown key %q in [case.fault]; kind %q accepts: %s",
				k, kind, strings.Join(sortedKeys(m.Params), ", ")))
			continue
		}
		if allowed == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			errs = append(errs, fmt.Sprintf("[case.fault] %s must be a string", k))
			continue
		}
		if !slices.Contains(allowed, s) {
			errs = append(errs, fmt.Sprintf("[case.fault] %s = %q is not valid for kind %q; accepts: %s",
				k, s, kind, strings.Join(allowed, ", ")))
		}
	}
	return errs
}

// checkScope validates the scope named in [case.match].
func checkScope(match map[string]any) []string {
	v, ok := match["scope"]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok || !slices.Contains(scopes, s) {
		return []string{fmt.Sprintf("[case.match] scope = %v is not valid; accepts: %s",
			v, strings.Join(scopes, ", "))}
	}
	return nil
}

// checkKeys reports keys not present in allowed.
func checkKeys(table map[string]any, allowed []string, where string) []string {
	var errs []string
	for k := range table {
		if !slices.Contains(allowed, k) {
			errs = append(errs, fmt.Sprintf("unknown key %q in %s; accepts: %s",
				k, where, strings.Join(allowed, ", ")))
		}
	}
	return errs
}

// checkInvariants validates the names in expect.invariants.
func checkInvariants(expect map[string]any) []string {
	v, ok := expect["invariants"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return []string{"[case.expect] invariants must be an array of strings"}
	}
	var errs []string
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			errs = append(errs, "[case.expect] invariants must be an array of strings")
			continue
		}
		if !slices.Contains(invariants, name) {
			errs = append(errs, fmt.Sprintf("unknown invariant %q in [case.expect]", name))
		}
	}
	return errs
}

func mechanismKinds() []string {
	out := make([]string, 0, len(mechanisms))
	for _, m := range mechanisms {
		out = append(out, m.Kind)
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
