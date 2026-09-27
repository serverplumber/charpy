// Package invariant is oracle layer 2: properties computable from the
// transcript alone, with no access to the implementation, and therefore
// language-agnostic for free.
//
// Built: I1 id-resolves-once, I2 no-unsolicited-response and I3
// no-duplicate-inflight-id. Designed, not built: I4 cancellation, the gateway
// invariants I5-I7, and I8-I13 for the stateless era. Gateway invariants
// require a correlated two-face transcript and are to be SKIPPED, never
// failed, when the join is merely inferred -- a false correlation produces a
// confident, specific, wrong accusation about someone else's credential
// handling.
//
// See docs/design/oracle.md section 4.
package invariant
