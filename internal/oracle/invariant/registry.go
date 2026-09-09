package invariant

import "slices"

// The registry lives here, next to the invariant implementations, rather than
// in the catalogue: a name list maintained apart from the code it describes
// drifts. The catalogue validates expect.invariants against this registry,
// and the case-manifest JSON Schema is generated from it. See
// docs/design/decisions.md ADR-009.

// Invariant describes one transcript invariant. The statements and the
// per-revision derivation are in docs/design/oracle.md section 4; the
// summaries here are what the generated schema shows an author on hover.
type Invariant struct {
	Name    string
	Summary string

	// RequiresCorrelation marks gateway invariants that need a correlated
	// two-face transcript. They are SKIPPED, never failed, when the join is
	// merely inferred — a false correlation produces a confident, specific,
	// wrong accusation about someone else's credential handling. See
	// docs/design/transcript.md section 4.
	RequiresCorrelation bool

	// SecondWave marks invariants specified now but implemented after v0
	// (I8-I13, the stateless-era set).
	SecondWave bool
}

// registry is ordered as docs/design/oracle.md section 4 presents them,
// I1 through I13.
var registry = []Invariant{
	{Name: "id-resolves-once",
		Summary: "Every request id resolves exactly once — result or error, never both, never neither."},
	{Name: "no-unsolicited-response",
		Summary: "No response or error carries an id that was never requested on that face."},
	{Name: "no-duplicate-inflight-id",
		Summary: "No two requests share an id while both are in flight on one connection."},
	{Name: "cancel-honoured",
		Summary: "No result is delivered for a request after its cancellation was acknowledged. On 2026-07-28 HTTP the cancellation signal is the stream close itself."},

	{Name: "no-credential-leak", RequiresCorrelation: true,
		Summary: "No upstream credential, Authorization header, session identifier, or internal address appears in any downstream frame."},
	{Name: "session-identity-isolation", RequiresCorrelation: true,
		Summary: "A session established under identity A never observes a frame or stream belonging to identity B. Sessioned revisions only."},
	{Name: "merged-manifest-consistency", RequiresCorrelation: true,
		Summary: "Across a list_changed fan-out there is no window in which a tool name resolves to the wrong upstream."},

	{Name: "header-body-consistency", RequiresCorrelation: true, SecondWave: true,
		Summary: "Mirrored headers agree with the body on both faces; a rewritten body gets re-derived Mcp-Method/Mcp-Name; unrecognised Mcp-Param-* headers are forwarded."},
	{Name: "cache-scope-isolation", RequiresCorrelation: true, SecondWave: true,
		Summary: "A result carrying cacheScope \"private\" is never served to a different identity, and no result is served after its ttlMs has elapsed."},
	{Name: "subscription-id-remap", RequiresCorrelation: true, SecondWave: true,
		Summary: "An upstream subscriptionId never appears downstream, and a notification is delivered only to the subscriber that opted into its type."},
	{Name: "mrtr-state-isolation", RequiresCorrelation: true, SecondWave: true,
		Summary: "requestState and inputResponses never cross identities across an MRTR retry."},
	{Name: "loglevel-gating", RequiresCorrelation: true, SecondWave: true,
		Summary: "No notifications/message is emitted for a request that did not carry a logLevel."},
	{Name: "trace-context-propagation", RequiresCorrelation: true, SecondWave: true,
		Summary: "traceparent survives the subject. Informational — always OBSERVED, never a failure."},
}

// All returns the registry in presentation order.
func All() []Invariant { return slices.Clone(registry) }

// Lookup finds an invariant by name.
func Lookup(name string) (Invariant, bool) {
	for _, i := range registry {
		if i.Name == name {
			return i, true
		}
	}
	return Invariant{}, false
}

// Known reports whether name is a registered invariant.
func Known(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// Names returns every invariant name, in registry order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, i := range registry {
		out = append(out, i.Name)
	}
	return out
}
