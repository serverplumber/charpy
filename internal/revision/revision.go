// Package revision knows the ordered list of MCP protocol revisions charpy
// supports, and resolves the applies_to ranges that scope a case to some of
// them.
//
// Ranges resolve against the declared order, not against date arithmetic, so
// draft always sorts last and a future revision inserted into the list is
// picked up by every open-ended range automatically. See
// docs/design/revisions.md.
package revision

import (
	"fmt"
	"slices"
	"strings"
)

// Revision is a protocol revision label: a spec date, or "draft".
type Revision string

const (
	V20241105 Revision = "2024-11-05"
	V20250326 Revision = "2025-03-26"
	V20250618 Revision = "2025-06-18"
	V20251125 Revision = "2025-11-25"
	V20260728 Revision = "2026-07-28"
	Draft     Revision = "draft"
)

// ordered is the authoritative sequence. Draft is last by construction: it is
// always the highest revision, so ">=X" ranges include it and charpy breaks
// loudly when the draft moves rather than silently ceasing to test it.
var ordered = []Revision{
	V20241105,
	V20250326,
	V20250618,
	V20251125,
	V20260728,
	Draft,
}

// Era distinguishes the two protocols that share the name MCP. The boundary is
// 2026-07-28, which removed the initialize handshake (SEP-2575) and protocol
// level sessions (SEP-2567).
type Era int

const (
	// Sessioned covers 2024-11-05 through 2025-11-25: initialize handshake,
	// Mcp-Session-Id, resumable streams, server-initiated requests.
	Sessioned Era = iota
	// Stateless covers 2026-07-28 and later: per-request _meta, no session,
	// MRTR instead of server-initiated requests.
	Stateless
)

// All returns the ordered revision list.
func All() []Revision { return slices.Clone(ordered) }

// Known reports whether r is a revision charpy knows about.
func Known(r Revision) bool { return slices.Contains(ordered, r) }

// Index returns r's position in the ordered list, or -1 if unknown.
func Index(r Revision) int { return slices.Index(ordered, r) }

// Era reports which protocol era r belongs to. Unknown revisions report
// Sessioned, but callers should check Known first.
func (r Revision) Era() Era {
	if i := Index(r); i >= Index(V20260728) {
		return Stateless
	}
	return Sessioned
}

// Stateless reports whether r uses the stateless protocol core.
func (r Revision) Stateless() bool { return r.Era() == Stateless }

func (r Revision) String() string { return string(r) }

// Range is a set of revisions expressed as one or more comparison clauses,
// as written in a case manifest's applies_to field.
//
//	"*"                        every revision charpy knows
//	">=2025-03-26"             that revision and every later one, including draft
//	"=2026-07-28"              exactly one
//	">=2025-03-26,<2026-07-28" the sessioned era
type Range struct {
	text    string
	clauses []clause
}

type clause struct {
	op  string
	rev Revision
}

// ParseRange parses an applies_to expression. Every revision it names must be
// known, so a typo fails at load rather than silently matching nothing.
func ParseRange(s string) (Range, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Range{}, fmt.Errorf("revision: empty range")
	}
	if trimmed == "*" {
		return Range{text: "*"}, nil
	}

	r := Range{text: trimmed}
	for _, part := range strings.Split(trimmed, ",") {
		part = strings.TrimSpace(part)
		op, rest, err := splitOp(part)
		if err != nil {
			return Range{}, err
		}
		rev := Revision(rest)
		if !Known(rev) {
			return Range{}, fmt.Errorf("revision: unknown revision %q in range %q", rest, s)
		}
		r.clauses = append(r.clauses, clause{op: op, rev: rev})
	}
	return r, nil
}

// splitOp peels the comparison operator off a clause. Longer operators are
// tested first so ">=" is not read as ">".
func splitOp(part string) (op, rest string, err error) {
	for _, o := range []string{">=", "<=", "==", "!=", ">", "<", "="} {
		if after, ok := strings.CutPrefix(part, o); ok {
			rest = strings.TrimSpace(after)
			if rest == "" {
				return "", "", fmt.Errorf("revision: operator %q with no revision in %q", o, part)
			}
			if o == "==" {
				o = "="
			}
			return o, rest, nil
		}
	}
	return "", "", fmt.Errorf("revision: clause %q has no comparison operator", part)
}

// Includes reports whether r falls in the range. An unknown revision is never
// included, on the principle that charpy should not run a case against a
// revision it cannot reason about.
func (rg Range) Includes(r Revision) bool {
	if !Known(r) {
		return false
	}
	if rg.text == "*" || len(rg.clauses) == 0 {
		return rg.text == "*"
	}
	i := Index(r)
	for _, c := range rg.clauses {
		j := Index(c.rev)
		var ok bool
		switch c.op {
		case ">=":
			ok = i >= j
		case "<=":
			ok = i <= j
		case ">":
			ok = i > j
		case "<":
			ok = i < j
		case "=":
			ok = i == j
		case "!=":
			ok = i != j
		}
		if !ok {
			return false
		}
	}
	return true
}

// Revisions returns every known revision the range includes, in order.
func (rg Range) Revisions() []Revision {
	var out []Revision
	for _, r := range ordered {
		if rg.Includes(r) {
			out = append(out, r)
		}
	}
	return out
}

// Empty reports whether the range matches no known revision. A case whose
// range is empty is almost certainly a mistake and the loader warns on it.
func (rg Range) Empty() bool { return len(rg.Revisions()) == 0 }

func (rg Range) String() string { return rg.text }
