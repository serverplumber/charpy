package interpose

import "strings"

// Glob is a matcher key's value: an exact string, or a pattern in which *
// stands for any run of characters. The zero Glob constrains nothing, which is
// what an absent key in a [case.match] table means.
//
// Note what this is not: [path.Match] refuses to let * cross a /, so under it
// method = "*" would fail to match "tools/call". A matcher that silently
// matches nothing is a test that silently passes, which is the failure the
// whole strict-loading design exists to prevent, so the wildcard here spans
// separators.
type Glob struct {
	// parts are the literal runs between wildcards. A nil parts matches
	// anything; a single part is an exact match; otherwise the first part is
	// a required prefix, the last a required suffix, and the rest must occur
	// in order in between.
	parts []string
	text  string
}

// ParseGlob builds a Glob. An empty pattern is the zero Glob, matching
// anything.
func ParseGlob(pattern string) Glob {
	if pattern == "" {
		return Glob{}
	}
	return Glob{parts: strings.Split(pattern, "*"), text: pattern}
}

// Any reports whether the glob constrains anything.
func (g Glob) Any() bool { return len(g.parts) == 0 }

// String returns the pattern as written.
func (g Glob) String() string { return g.text }

// Match reports whether s satisfies the glob.
func (g Glob) Match(s string) bool {
	switch len(g.parts) {
	case 0:
		return true
	case 1:
		return g.parts[0] == s
	}

	first, last := g.parts[0], g.parts[len(g.parts)-1]
	if !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	// The anchors must not overlap: "ab*ba" does not match "aba", even though
	// that string both begins with "ab" and ends with "ba".
	if len(first)+len(last) > len(s) {
		return false
	}

	rest := s[len(first) : len(s)-len(last)]
	for _, p := range g.parts[1 : len(g.parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return true
}
