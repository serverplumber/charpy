// Package fault implements the fault mechanisms.
//
// A mechanism is code that produces a wire effect; a case is a named,
// parameterised, revision-scoped application of one. The eight v0 mechanisms
// are hang, truncate, malformed_json, schema_violation, duplicate_id,
// unsolicited_response, manifest_mutate and capability_flip.
//
// The rule for where a variant belongs: if it changes bytes, it is a mechanism
// parameter; if it changes what you would claim in a bug report, it is a case.
// See docs/design/faults-and-cases.md.
package fault
