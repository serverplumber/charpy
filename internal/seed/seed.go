// Package seed derives the deterministic randomness a case draws on.
//
// A citation promises that case-id + revision + seed reproduces charpy's
// injected behaviour byte for byte (docs/design/case-identity.md section 5).
// That promise is what this package exists to keep, and it constrains the
// implementation more than performance ever could.
package seed

import (
	"crypto/sha256"
	"math/rand/v2"
	"regexp"
)

// Purposes. Each names an independent stream within a case.
const (
	// CutOffset chooses where a truncation lands.
	CutOffset = "cut_offset"
	// Payload generates content for a synthesized frame.
	Payload = "payload"
	// IDs chooses identifiers a fault needs that nothing requested.
	IDs = "ids"
	// Jitter is scheduling wobble within a case.
	Jitter = "jitter"
)

// For returns the stream a case draws on for one purpose.
//
// Three properties, in the order they constrain the choice:
//
// Per case, because a single run-level generator drawn from in sequence would
// make a case's bytes depend on which cases ran before it, and
// `charpy run --case X --seed S` would then reproduce X only when X ran first.
//
// Per purpose, because one stream per case is still order-dependent a level
// down: adding a seeded parameter to a mechanism would shift every draw after
// it, and archived citations would quietly stop reproducing. Naming the draw
// makes each stream independent of the rest, so a mechanism can grow a
// parameter without invalidating history.
//
// On a named algorithm, because math/rand/v2's generators are specified rather
// than an unspecified source -- math/rand's top-level functions never promised
// a seed would mean the same thing across releases. ChaCha8 takes a 32-byte
// seed, which is exactly SHA-256's output, so nothing is truncated or expanded
// to fit.
//
// This is a permanent semantic contract rather than an implementation detail:
// every archived transcript reproduces only while this function returns the
// same bytes. Changing it invalidates history rather than breaking a build,
// which is why it uses stdlib hashing and a specified generator, and why it is
// not somewhere a dependency could drift underneath it.
func For(runSeed, caseID, purpose string) *rand.Rand {
	h := sha256.New()
	// A separator, so that ("ab", "c") and ("a", "bc") are different streams.
	for _, part := range []string{runSeed, caseID, purpose} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return rand.New(rand.NewChaCha8([32]byte(h.Sum(nil))))
}

// Pick adapts a stream to the picker [internal/wire.CutOptions] takes, which
// is how the run seed reaches a cut point.
func Pick(r *rand.Rand) func(n int) int {
	return func(n int) int {
		if n <= 0 {
			return 0
		}
		return r.IntN(n)
	}
}

// Pattern is a run seed's format unanchored so a larger pattern can
// incorporate it.
const Pattern = `[0-9a-f]{6,16}`

var valid = regexp.MustCompile(`^` + Pattern + `$`)

// Valid reports whether a string is a valid seed.
func Valid(s string) bool {
	return valid.MatchString(s)
}
