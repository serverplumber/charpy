package interpose

import "slices"

// The matcher-key registry lives here, next to the matching code, rather
// than in the catalogue: the interposer is what evaluates [case.match], so
// the interposer says what a match table may contain. The catalogue
// validates manifests against this registry and the case-manifest JSON
// Schema is generated from it. See docs/design/decisions.md ADR-009.
//
// Dependency direction: the catalogue imports the interposer, never the
// reverse. The interposer consumes compiled policy in its own terms and
// knows nothing about TOML.

// Scope names the population an occurrence counts within. In v0 there is one
// client and one session, so every scope collapses to run — but the ledger
// keys its counters by (scope, dimension value) from the start, because soak
// mode needs rates over populations and a matcher that has only ever counted
// globally cannot grow one. See docs/design/policy-format.md.
type Scope string

const (
	// ScopeRun counts across the whole run. The default.
	ScopeRun Scope = "run"
	// ScopeClient counts per synthetic client.
	ScopeClient Scope = "client"
	// ScopeSession counts per logical session.
	ScopeSession Scope = "session"
	// ScopeConnection counts per transport connection.
	ScopeConnection Scope = "connection"
)

var scopes = []Scope{ScopeRun, ScopeClient, ScopeSession, ScopeConnection}

// Scopes returns the scopes in declaration order.
func Scopes() []Scope { return slices.Clone(scopes) }

// KnownScope reports whether s names a scope.
func KnownScope(s string) bool { return slices.Contains(scopes, Scope(s)) }

// ScopeNames returns the scope names, in declaration order.
func ScopeNames() []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, string(s))
	}
	return out
}

// MatchKey describes one key a [case.match] table accepts. Values is nil for
// free-form keys, in which case Type says what the TOML value must decode to.
type MatchKey struct {
	Name    string
	Type    string // "string" | "integer"; enumerated keys are strings
	Summary string
	Values  []string // nil for free-form
}

// matchKeys is the registry, in the order docs/design/policy-format.md
// presents them. All present keys must match; absent keys match anything.
var matchKeys = []MatchKey{
	{Name: "method", Type: "string",
		Summary: "Exact method name, or a * glob. Responses match by the method of the request their id resolves."},
	{Name: "face", Values: []string{"downstream", "upstream"},
		Summary: "Which side of the subject, defined against the subject: downstream is its client-facing side, upstream its server-facing side."},
	{Name: "direction", Values: []string{"c2s", "s2c"},
		Summary: "The MCP role direction, independent of who charpy is impersonating."},
	{Name: "kind", Values: []string{"request", "response", "error", "notification"},
		Summary: "The JSON-RPC frame kind."},
	{Name: "client_id", Type: "string",
		Summary: "Which synthetic client, exact or a * glob. Always c0 in v0."},
	{Name: "session_id", Type: "string",
		Summary: "Which logical session, exact or a * glob."},
	{Name: "occurrence", Type: "integer",
		Summary: "1-based ordinal: the Nth matching frame within the scope. Mutually exclusive with occurrence_every."},
	{Name: "occurrence_every", Type: "integer",
		Summary: "Every Nth matching frame within the scope. Mutually exclusive with occurrence."},
	{Name: "after_mono_ms", Type: "integer",
		Summary: "Not before this point on the injected clock, in ms from run start."},
	{Name: "scope", Values: ScopeNames(),
		Summary: "The population an occurrence counts within. Defaults to run; in v0 every scope collapses to it."},
}

// MatchKeys returns the registry in presentation order.
func MatchKeys() []MatchKey { return slices.Clone(matchKeys) }

// LookupMatchKey finds a match key by name.
func LookupMatchKey(name string) (MatchKey, bool) {
	for _, k := range matchKeys {
		if k.Name == name {
			return k, true
		}
	}
	return MatchKey{}, false
}

// MatchKeyNames returns every match key name, in registry order.
func MatchKeyNames() []string {
	out := make([]string, 0, len(matchKeys))
	for _, k := range matchKeys {
		out = append(out, k.Name)
	}
	return out
}
