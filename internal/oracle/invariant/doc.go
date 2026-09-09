// Package invariant is oracle layer 2: properties computable from the
// transcript alone, with no access to the implementation, and therefore
// language-agnostic for free.
//
// v0 ships I1-I7; I8-I13 cover the stateless era and land in the second wave.
// Gateway invariants require a correlated two-face transcript and are SKIPPED,
// never failed, when the join is merely inferred -- a false correlation
// produces a confident, specific, wrong accusation about someone else's
// credential handling.
//
// See docs/design/oracle.md section 4.
package invariant
