package catalogue

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/serverplumber/charpy/internal/revision"
)

// Verdict is the bucket a case's finding lands in. Two buckets, nothing
// between: MUST is schema-mechanical only, OBSERVED is everything else.
type Verdict string

const (
	VerdictMust     Verdict = "MUST"
	VerdictObserved Verdict = "OBSERVED"
)

// Subject is the class of implementation a case applies to.
type Subject string

const (
	SubjectServer  Subject = "server"
	SubjectClient  Subject = "client"
	SubjectGateway Subject = "gateway"
)

// Case is one entry of the catalogue. Only the identity-bearing fields are
// decoded strictly here; Match, Fault and Expect are held as raw maps until
// the engine lands and can typecheck them against a real mechanism.
type Case struct {
	ID              string    `toml:"id"`
	AppliesTo       string    `toml:"applies_to"`
	Subject         []Subject `toml:"subject"`
	Transport       []string  `toml:"transport"`
	Verdict         Verdict   `toml:"verdict"`
	DerivesFrom     string    `toml:"derives_from"`
	Summary         string    `toml:"summary"`
	Status          string    `toml:"status"`
	WithdrawnReason string    `toml:"withdrawn_reason"`

	Match  map[string]any `toml:"match"`
	Fault  map[string]any `toml:"fault"`
	Expect map[string]any `toml:"expect"`

	// Resolved during loading.
	parsedID    ID             `toml:"-"`
	parsedRange revision.Range `toml:"-"`
}

// ParsedID returns the case's parsed permanent identifier.
func (c Case) ParsedID() ID { return c.parsedID }

// AppliesToRange returns the case's parsed revision range.
func (c Case) AppliesToRange() revision.Range { return c.parsedRange }

// Withdrawn reports whether the case has been retired. Withdrawn cases still
// parse, so old transcripts and old reports keep resolving.
func (c Case) Withdrawn() bool { return c.Status == "withdrawn" }

type manifest struct {
	SchemaVersion int    `toml:"schema_version"`
	Cases         []Case `toml:"case"`
}

// Catalogue is a validated set of cases, keyed by permanent ID.
type Catalogue struct {
	Cases []Case
	byID  map[string]int
}

// Lookup finds a case by permanent ID.
func (c *Catalogue) Lookup(id string) (Case, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Case{}, false
	}
	return c.Cases[i], true
}

// Applicable returns the active cases that apply to a revision.
func (c *Catalogue) Applicable(r revision.Revision) []Case {
	var out []Case
	for _, cs := range c.Cases {
		if !cs.Withdrawn() && cs.parsedRange.Includes(r) {
			out = append(out, cs)
		}
	}
	return out
}

// Load reads and validates every *.toml case manifest under dir.
//
// Validation is strict by design: unknown keys are errors, not silently
// ignored lines. A matcher that silently matches nothing is a test that
// silently passes, which is the failure mode this loader exists to prevent.
func Load(fsys fs.FS, dir string) (*Catalogue, []string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("catalogue: %w", err)
	}

	cat := &Catalogue{byID: map[string]int{}}
	var warnings []string
	var errs []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, nil, fmt.Errorf("catalogue: %w", err)
		}

		var m manifest
		dec := toml.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if m.SchemaVersion != 1 {
			return nil, nil, fmt.Errorf("%s: schema_version = %d, want 1", name, m.SchemaVersion)
		}

		for _, cs := range m.Cases {
			e, w := validate(&cs, name)
			errs = append(errs, e...)
			warnings = append(warnings, w...)
			if len(e) > 0 {
				continue
			}
			if prev, dup := cat.byID[cs.ID]; dup {
				errs = append(errs, fmt.Sprintf(
					"%s: duplicate case id %q (also in %s)", name, cs.ID, cat.Cases[prev].Summary))
				continue
			}
			cat.byID[cs.ID] = len(cat.Cases)
			cat.Cases = append(cat.Cases, cs)
		}
	}

	if len(errs) > 0 {
		return nil, warnings, fmt.Errorf("catalogue: %d error(s):\n  %s",
			len(errs), strings.Join(errs, "\n  "))
	}
	return cat, warnings, nil
}

func validate(cs *Case, file string) (errs, warnings []string) {
	fail := func(format string, a ...any) {
		errs = append(errs, file+": "+fmt.Sprintf(format, a...))
	}
	warn := func(format string, a ...any) {
		warnings = append(warnings, file+": "+fmt.Sprintf(format, a...))
	}

	id, err := ParseID(cs.ID)
	if err != nil {
		fail("%v", err)
		return errs, warnings
	}
	cs.parsedID = id

	rg, err := revision.ParseRange(cs.AppliesTo)
	if err != nil {
		fail("case %q: %v", cs.ID, err)
	} else {
		cs.parsedRange = rg
		if rg.Empty() {
			warn("case %q: applies_to %q matches no known revision", cs.ID, cs.AppliesTo)
		}
	}

	switch cs.Verdict {
	case VerdictMust:
		// Never issue a behavioural MUST. Only a generated normative artifact
		// rejecting a frame earns one, so a MUST must cite a schema source.
		// Enforced here rather than in review. See docs/design/oracle.md.
		if !strings.HasPrefix(cs.DerivesFrom, "schema:") {
			fail("case %q: verdict = \"MUST\" requires a schema: source, got %q", cs.ID, cs.DerivesFrom)
		}
	case VerdictObserved:
	default:
		fail("case %q: verdict must be MUST or OBSERVED, got %q", cs.ID, cs.Verdict)
	}

	if !validDerivesFrom(cs.DerivesFrom) {
		fail("case %q: derives_from %q must start with spec:, sep:, schema:, or be \"none\"",
			cs.ID, cs.DerivesFrom)
	}

	if len(cs.Subject) == 0 {
		fail("case %q: subject must name at least one of server, client, gateway", cs.ID)
	}
	for _, s := range cs.Subject {
		switch s {
		case SubjectServer, SubjectClient, SubjectGateway:
		default:
			fail("case %q: unknown subject %q", cs.ID, s)
		}
	}

	for _, tr := range cs.Transport {
		if tr != "stdio" && tr != "http" {
			fail("case %q: unknown transport %q", cs.ID, tr)
		}
	}

	if strings.TrimSpace(cs.Summary) == "" {
		fail("case %q: summary is required; it appears in the report and in JUnit", cs.ID)
	}

	switch cs.Status {
	case "", "active":
	case "withdrawn":
		if cs.WithdrawnReason == "" {
			fail("case %q: withdrawn cases need a withdrawn_reason", cs.ID)
		}
	default:
		fail("case %q: status must be active or withdrawn, got %q", cs.ID, cs.Status)
	}

	if len(cs.Fault) == 0 {
		fail("case %q: [case.fault] is required", cs.ID)
	} else {
		for _, e := range checkFault(cs.Fault) {
			fail("case %q: %s", cs.ID, e)
		}
	}

	for _, e := range checkMatch(cs.Match) {
		fail("case %q: %s", cs.ID, e)
	}
	for _, e := range checkExpect(cs.Expect) {
		fail("case %q: %s", cs.ID, e)
	}

	if len(cs.Match) == 0 {
		warn("case %q: empty [case.match] matches the first frame on any face; "+
			"this is almost never intended", cs.ID)
	}
	_, hasOcc := cs.Match["occurrence"]
	_, hasEvery := cs.Match["occurrence_every"]
	if hasOcc && hasEvery {
		fail("case %q: occurrence and occurrence_every are mutually exclusive", cs.ID)
	}

	return errs, warnings
}

func validDerivesFrom(s string) bool {
	if s == "none" {
		return true
	}
	for _, p := range []string{"spec:", "sep:", "schema:"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest != ""
		}
	}
	return false
}
