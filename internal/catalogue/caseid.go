// Package catalogue loads the case catalogue and parses case identifiers.
//
// A case has a permanent ID that never changes and never contains a date, and
// is quoted as a citation that adds the revision and seed. See
// docs/design/case-identity.md for why revision lives at citation time rather
// than in the ID.
package catalogue

import (
	"fmt"
	"strings"

	"github.com/serverplumber/charpy/internal/revision"
)

// Family is the first segment of a case ID. The list is fixed: family names
// appear in JUnit classname and become CI history keys, so adding one is a
// design change rather than a case-authoring change.
type Family string

const (
	FamilyFrame     Family = "frame"     // framing and encoding
	FamilyStream    Family = "stream"    // SSE and stdio stream behaviour
	FamilyID        Family = "id"        // JSON-RPC correlation
	FamilySchema    Family = "schema"    // valid JSON violating a declared schema
	FamilyLifecycle Family = "lifecycle" // negotiation, capabilities, reconnect
	FamilyManifest  Family = "manifest"  // list mutation and list_changed fan-out
	FamilyCancel    Family = "cancel"    // cancellation across transports and eras
	FamilyGateway   Family = "gateway"   // seam behaviour needing both faces
	FamilyLiveness  Family = "liveness"  // recovery after a withdrawn fault
)

var families = []Family{
	FamilyFrame, FamilyStream, FamilyID, FamilySchema, FamilyLifecycle,
	FamilyManifest, FamilyCancel, FamilyGateway, FamilyLiveness,
}

// KnownFamily reports whether f is one of the fixed families.
func KnownFamily(f Family) bool {
	for _, k := range families {
		if k == f {
			return true
		}
	}
	return false
}

// ID is a permanent case identifier, "family/name". It never contains a
// revision and never changes once released.
type ID struct {
	Family Family
	Name   string
}

// ParseID parses a permanent case ID.
func ParseID(s string) (ID, error) {
	fam, name, ok := strings.Cut(s, "/")
	if !ok {
		return ID{}, fmt.Errorf("case id %q: want family/name", s)
	}
	if !isKebab(fam) || strings.Contains(fam, "-") {
		return ID{}, fmt.Errorf("case id %q: family %q must be a single lowercase segment", s, fam)
	}
	if !KnownFamily(Family(fam)) {
		return ID{}, fmt.Errorf("case id %q: unknown family %q (see docs/design/case-identity.md)", s, fam)
	}
	if !isKebab(name) {
		return ID{}, fmt.Errorf("case id %q: name %q must be lowercase kebab-case", s, name)
	}
	if strings.Contains(name, "/") {
		return ID{}, fmt.Errorf("case id %q: exactly one %q separator allowed", s, "/")
	}
	return ID{Family: Family(fam), Name: name}, nil
}

func (id ID) String() string { return string(id.Family) + "/" + id.Name }

// JUnitClass renders the classname a JUnit testcase uses for this case.
func (id ID) JUnitClass() string { return "charpy." + string(id.Family) }

// Citation is a case ID qualified by the revision it was run against and the
// seed that drove it. This is the form quoted in reports and bug reports;
// the bare ID is never quoted as a verdict.
//
//	stream/truncate-mid-event@2025-11-25#seed=8f2c1a
type Citation struct {
	ID       ID
	Revision revision.Revision // zero value means unqualified
	Seed     string            // lowercase hex, 6-16 digits; empty means unseeded
}

// ParseCitation parses a full or partial citation. Revision and seed are both
// optional, so ParseCitation also accepts a bare ID.
func ParseCitation(s string) (Citation, error) {
	rest, seed, hasSeed := strings.Cut(s, "#")
	c := Citation{}

	if hasSeed {
		v, ok := strings.CutPrefix(seed, "seed=")
		if !ok {
			return Citation{}, fmt.Errorf("citation %q: fragment must be seed=<hex>", s)
		}
		if !isSeed(v) {
			return Citation{}, fmt.Errorf("citation %q: seed %q must be 6-16 lowercase hex digits", s, v)
		}
		c.Seed = v
	}

	idPart, rev, hasRev := strings.Cut(rest, "@")
	if hasRev {
		r := revision.Revision(rev)
		if !revision.Known(r) {
			return Citation{}, fmt.Errorf("citation %q: unknown revision %q", s, rev)
		}
		c.Revision = r
	}

	id, err := ParseID(idPart)
	if err != nil {
		return Citation{}, err
	}
	c.ID = id
	return c, nil
}

// String renders the citation. It round-trips with ParseCitation.
func (c Citation) String() string {
	var b strings.Builder
	b.WriteString(c.ID.String())
	if c.Revision != "" {
		b.WriteString("@")
		b.WriteString(string(c.Revision))
	}
	if c.Seed != "" {
		b.WriteString("#seed=")
		b.WriteString(c.Seed)
	}
	return b.String()
}

// JUnitName renders the name a JUnit testcase uses. The seed is deliberately
// omitted: CI systems key history off classname+name, so a per-run seed there
// would make every run look like a new test and destroy flake history.
func (c Citation) JUnitName() string {
	if c.Revision == "" {
		return c.ID.Name
	}
	return c.ID.Name + "@" + string(c.Revision)
}

func isKebab(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			prevDash = false
		case r == '-':
			if prevDash {
				return false // no doubled separators
			}
			prevDash = true
		default:
			return false
		}
	}
	return true
}

func isSeed(s string) bool {
	if len(s) < 6 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
