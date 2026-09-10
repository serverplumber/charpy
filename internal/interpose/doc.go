// Package interpose is the interposer: the state machine between an
// origination source and the wire, and the only component that is ever
// hostile.
//
// It evaluates fault policy — matching frames, counting occurrences,
// scheduling injections and withdrawals — and applies faults with three
// verbs: rewrite, withhold, synthesize. It originates nothing; traffic comes
// from a relay (proxy mode), the SDK-built scenario player, or the subject
// itself, and the interposer is indifferent to which. The reference peer
// plugs into it as its transport and is never made to misbehave: charpy
// man-in-the-middles its own SDK.
//
// Its state is the ledger: double-entry, intent versus wire, per face and
// connection. The ledger back-translates the interposer's own rewrites so
// the reference peer stays sane, keys occurrence counters by (scope,
// dimension), tags downstream consequences of a lie, and emits the link
// object correlation is computed from.
//
// Nothing here may block the frame path. Transcript writes are buffered and
// asynchronous, schema validation is offline, and the oracle is offline, so
// the interposer adds microseconds where subjects reason in milliseconds.
//
// See docs/design/interposer.md and docs/design/decisions.md ADR-010.
package interpose
