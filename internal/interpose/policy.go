package interpose

import (
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Compiled policy: the interposer's own terms, which the catalogue compiles
// manifests into. Nothing here knows what TOML is, and nothing here imports
// the catalogue -- the dependency runs one way, which is what makes the cycle
// structurally impossible rather than merely avoided. See
// docs/design/decisions.md ADR-009.

// Case is a compiled catalogue entry: an identity, the frame its fault
// attaches to, and the mechanism to apply.
//
// [case.expect] is deliberately absent. Invariant names and liveness budgets
// are oracle input, and the interposer evaluates none of it; carrying them
// here would blur the boundary on the first commit that touched it.
type Case struct {
	// ID is the permanent case identifier, "family/name".
	ID string
	// Citation is the ID with the revision and seed applied, which is what
	// the transcript and the report quote.
	Citation string
	Match    Match
	Fault    Fault
}

// Fault names a mechanism from the registry in internal/fault, with the
// parameters a case set on it.
//
// The parameters stay untyped. The catalogue has already validated them
// against the mechanism registry, and no mechanism exists yet to consume a
// typed form, so compiling one now would be inventing a shape for no reader.
type Fault struct {
	Kind   string
	Params map[string]any
}

// Match selects which frame a fault attaches to. Every zero-valued field
// matches anything, which is what an absent key in a [case.match] table
// means -- all present keys must match, absent keys match everything.
type Match struct {
	Method    Glob
	Face      transcript.Face
	Direction transcript.Direction
	Kind      envelope.Kind
	ClientID  Glob
	SessionID Glob

	// Occurrence is a 1-based ordinal over matching frames; Every selects
	// each Nth. They are mutually exclusive and the loader enforces it. Zero
	// means unset for both.
	Occurrence int64
	Every      int64

	// AfterMono gates on the injected clock. It is a predicate, not a
	// post-filter: see [Matcher.Select].
	AfterMono clock.Mono

	// Scope names the population an ordinal counts within. The zero value is
	// ScopeRun.
	Scope Scope
}

// Frame is what the matcher sees: the dimensions a [case.match] table can
// select on, and nothing else.
//
// It is deliberately not a [transcript.Frame], which is a write-side struct
// carrying raw bytes, HTTP detail, the join key and fault attribution. The
// matcher decides whether a fault attaches; it has no business holding the
// things the transcript writes.
type Frame struct {
	Face      transcript.Face
	Direction transcript.Direction
	Kind      envelope.Kind
	// Method is empty on a response or an error, which carry none. The
	// matcher fills it from the ledger before evaluating.
	Method string
	ID     envelope.ID

	ClientID  string
	SessionID string
	ConnID    string
}

// dimension returns the value of the dimension a scope counts within. Run
// scope has no dimension, so every frame shares one counter.
func (f Frame) dimension(s Scope) string {
	switch s {
	case ScopeClient:
		return f.ClientID
	case ScopeSession:
		return f.SessionID
	case ScopeConnection:
		return f.ConnID
	default:
		return ""
	}
}
