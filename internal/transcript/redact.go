package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// defaultRedacted is the set of header names whose values are replaced by a
// digest. It is deliberately a name list rather than a value heuristic:
// guessing at secrets by shape produces both false negatives and a transcript
// that is hard to reason about.
//
// Mcp-Session-Id is deliberately absent. It is a session identifier, which
// invariant I5 does treat as leakable, but the transcript already carries it
// in the clear in session.mcp_session_id, and redacting the header while
// printing the field would be incoherent rather than safe. I5 compares
// session identifiers by value and credentials by digest.
var defaultRedacted = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"x-api-key",
	"api-key",
	"x-auth-token",
	"x-access-token",
	"x-amz-security-token",
}

// Redactor decides which header values a transcript carries in the clear.
//
// Redaction is a stable digest, not erasure, which is what lets invariant I5
// prove the same secret appeared on both faces without the transcript itself
// becoming a secret. Equality of digests is equality of exact bytes: a
// credential re-encoded or embedded in a larger value produces a different
// digest, so I5 is verbatim-only and its report line says so. See
// docs/design/oracle.md section 4 and docs/open-problems.md.
type Redactor struct {
	on    bool
	names map[string]bool
}

// NewRedactor returns the default redaction policy, plus any extra header
// names given. Names are matched case-insensitively.
func NewRedactor(extra ...string) *Redactor {
	r := &Redactor{on: true, names: make(map[string]bool, len(defaultRedacted)+len(extra))}
	for _, n := range defaultRedacted {
		r.names[n] = true
	}
	for _, n := range extra {
		r.names[strings.ToLower(strings.TrimSpace(n))] = true
	}
	return r
}

// NoRedaction returns a policy that redacts nothing. It backs --no-redact,
// which stamps redaction: "off" into the header line so a report generated
// from the transcript is visibly unsafe to share.
func NoRedaction() *Redactor { return &Redactor{} }

// On reports whether redaction is in force. It is what the header line
// records.
func (r *Redactor) On() bool { return r != nil && r.on }

// Names returns the redacted header names, sorted.
func (r *Redactor) Names() []string {
	if !r.On() {
		return nil
	}
	return slices.Sorted(maps.Keys(r.names))
}

// Value returns what the transcript should carry for a header. The name is
// matched lowercased; callers get back either the original value or a stable
// digest of it.
func (r *Redactor) Value(name, value string) string {
	if !r.On() || !r.names[strings.ToLower(name)] {
		return value
	}
	return Digest(value)
}

// Digest renders the redaction marker for a value: a stable SHA-256 of the
// exact bytes, whole and untruncated. Equality across faces is therefore
// equality of exact bytes and nothing weaker.
//
// The digest is not truncated, because truncating buys nothing charpy needs
// yet. Its only cost is transcript size, and without soak runs the space is
// there; its only benefit is a shorter marker, while every bit dropped is
// collision probability added -- and a collision here is invariant I5 saying a
// credential propagated when it did not, which is the confident, specific,
// wrong accusation that spends charpy's credibility permanently. At full width
// collisions stop being a consideration rather than being made unlikely.
//
// Truncation is something a membership filter buys back, once soak scale makes
// holding the exact digest set expensive enough to want one. Paying for it in
// advance is the wrong order. See docs/open-problems.md.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("<redacted:sha256:%s>", hex.EncodeToString(sum[:]))
}
