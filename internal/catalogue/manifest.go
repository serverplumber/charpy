package catalogue

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
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
// the interposer lands and can compile them against a real mechanism.
type Case struct {
	ID              string    `toml:"id"`
	AppliesTo       string    `toml:"applies_to"`
	Subject         []Subject `toml:"subject"`
	Transport       []string  `toml:"transport,omitempty"`
	Verdict         Verdict   `toml:"verdict"`
	DerivesFrom     string    `toml:"derives_from"`
	Summary         string    `toml:"summary"`
	Status          string    `toml:"status,omitempty"`
	WithdrawnReason string    `toml:"withdrawn_reason,omitempty"`

	Match  map[string]any `toml:"match,omitempty"`
	Fault  map[string]any `toml:"fault,omitempty"`
	Expect map[string]any `toml:"expect,omitempty"`

	// Resolved during loading.
	parsedID    ID             `toml:"-"`
	parsedRange revision.Range `toml:"-"`
	srcFile     string         `toml:"-"`
}

// ParsedID returns the case's parsed permanent identifier.
func (c Case) ParsedID() ID { return c.parsedID }

// AppliesToRange returns the case's parsed revision range.
func (c Case) AppliesToRange() revision.Range { return c.parsedRange }

// Withdrawn reports whether the case has been retired. Withdrawn cases still
// parse, so old transcripts and old reports keep resolving.
func (c Case) Withdrawn() bool { return c.Status == "withdrawn" }

// SupportsTransport reports whether a case can run on a transport. An empty
// transport list means any, matching the manifest default where an omitted
// transport constrains nothing.
func (c Case) SupportsTransport(t string) bool {
	if len(c.Transport) == 0 {
		return true
	}
	return slices.Contains(c.Transport, t)
}

// SupportsSubject reports whether a case applies to a subject class. An empty
// subject list means any. Like transport, it is applicability: a server-only
// case armed on a client-subject run would match frames and test the wrong
// side, so it is dropped at selection rather than carried in.
func (c Case) SupportsSubject(class string) bool {
	if len(c.Subject) == 0 {
		return true
	}
	return slices.Contains(c.Subject, Subject(class))
}

// Manifest is the top-level shape of a case file. It is exported because it
// is also how a catalogue is rendered back out: `charpy cases` prints TOML,
// and printing through the same struct the loader reads is what makes the
// output a valid manifest rather than a report that resembles one.
type Manifest struct {
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

		var m Manifest
		dec := toml.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			// Accumulate rather than abort: `charpy policy validate` should
			// report every broken file in one run, not one per run.
			errs = append(errs, decodeErrors(name, err)...)
			continue
		}
		if m.SchemaVersion != 1 {
			errs = append(errs, fmt.Sprintf("%s: schema_version = %d, want 1", name, m.SchemaVersion))
			continue
		}

		for _, cs := range m.Cases {
			cs.srcFile = name
			e, w := validate(&cs, name)
			errs = append(errs, e...)
			warnings = append(warnings, w...)
			if len(e) > 0 {
				continue
			}
			if prev, dup := cat.byID[cs.ID]; dup {
				errs = append(errs, fmt.Sprintf(
					"%s: duplicate case id %q (also in %s)", name, cs.ID, cat.Cases[prev].srcFile))
				continue
			}
			cat.byID[cs.ID] = len(cat.Cases)
			cat.Cases = append(cat.Cases, cs)
		}
	}

	if len(errs) > 0 {
		return nil, warnings, &LoadError{Errs: errs}
	}
	return cat, warnings, nil
}

// LoadError is what Load returns when manifests do not validate. It keeps the
// diagnostics as a list rather than one joined string, so a caller can count
// them and render them without parsing prose back apart.
type LoadError struct{ Errs []string }

func (e *LoadError) Error() string {
	return fmt.Sprintf("catalogue: %d error(s):\n  %s", len(e.Errs), strings.Join(e.Errs, "\n  "))
}

// decodeErrors renders what go-toml knows about a failed decode.
//
// This is the one layer that can say where: positions exist during decode and
// are gone by the time the registry and semantic checks run over plain Go
// values (policy-format.md §4). So it is worth unwrapping properly rather than
// printing the library's summary line -- "fields in the document are missing
// in the target struct" names neither the file, the line, nor the key, which
// is three-quarters of what a person needs to fix it.
func decodeErrors(file string, err error) []string {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		out := make([]string, 0, len(strict.Errors))
		for i := range strict.Errors {
			e := &strict.Errors[i]
			row, col := e.Position()
			out = append(out, fmt.Sprintf("%s:%d:%d: unknown key %q", file, row, col, strings.Join(e.Key(), ".")))
		}
		return out
	}

	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return []string{fmt.Sprintf("%s:%d:%d: %s", file, row, col, de.Error())}
	}
	return []string{fmt.Sprintf("%s: %v", file, err)}
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
