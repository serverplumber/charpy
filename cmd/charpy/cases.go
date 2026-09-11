package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/revision"
)

// cmdCases prints the shipped catalogue.
//
// The default output is TOML, and deliberately so: it is rendered through the
// same struct the loader reads, so what comes out is a valid manifest rather
// than a report that resembles one. "Show me what would run against
// 2025-11-25" pastes back in as "run exactly this".
func cmdCases(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("cases", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit the full catalogue as JSON, for external tooling")
	rev := fs.String("revision", "", "only cases applicable to this revision")
	all := fs.Bool("all", false, "include withdrawn cases")
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}

	cat, warnings, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		// The shipped catalogue failing to load is charpy being broken, not a
		// user's policy being wrong, so it is a harness error rather than a 4.
		fmt.Fprintf(os.Stderr, "charpy cases: the shipped catalogue does not load:\n%v\n", err)
		return exitHarness
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	selected, err := selectCases(cat, *rev, *all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy cases: %v\n", err)
		return exitHarness
	}

	if *asJSON {
		return emitJSON(selected, out)
	}
	return emitTOML(selected, out)
}

func selectCases(cat *catalogue.Catalogue, rev string, all bool) ([]catalogue.Case, error) {
	if rev == "" {
		if all {
			return cat.Cases, nil
		}
		var out []catalogue.Case
		for _, c := range cat.Cases {
			if !c.Withdrawn() {
				out = append(out, c)
			}
		}
		return out, nil
	}

	r := revision.Revision(rev)
	if !revision.Known(r) {
		return nil, fmt.Errorf("unknown revision %q; charpy knows %v", rev, revision.All())
	}
	return cat.Applicable(r), nil
}

func emitTOML(selected []catalogue.Case, out io.Writer) int {
	b, err := toml.Marshal(catalogue.Manifest{SchemaVersion: 1, Cases: selected})
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy cases: %v\n", err)
		return exitHarness
	}
	fmt.Fprintf(out, "#:schema ./case.schema.json\n\n%s", b)
	return exitClean
}

// caseJSON is what --json emits: identity, applicability and provenance, for
// tooling that wants the catalogue without parsing TOML.
type caseJSON struct {
	ID          string   `json:"id"`
	Family      string   `json:"family"`
	AppliesTo   string   `json:"applies_to"`
	Revisions   []string `json:"revisions"`
	Subject     []string `json:"subject"`
	Transport   []string `json:"transport,omitempty"`
	Verdict     string   `json:"verdict"`
	DerivesFrom string   `json:"derives_from"`
	Summary     string   `json:"summary"`
	Status      string   `json:"status"`
	Fault       string   `json:"fault"`
}

func emitJSON(selected []catalogue.Case, out io.Writer) int {
	rows := make([]caseJSON, 0, len(selected))
	for _, c := range selected {
		revs := c.AppliesToRange().Revisions()
		names := make([]string, 0, len(revs))
		for _, r := range revs {
			names = append(names, r.String())
		}
		subjects := make([]string, 0, len(c.Subject))
		for _, s := range c.Subject {
			subjects = append(subjects, string(s))
		}
		status := c.Status
		if status == "" {
			status = "active"
		}
		kind, _ := c.Fault["kind"].(string)

		rows = append(rows, caseJSON{
			ID:          c.ID,
			Family:      string(c.ParsedID().Family),
			AppliesTo:   c.AppliesTo,
			Revisions:   names,
			Subject:     subjects,
			Transport:   c.Transport,
			Verdict:     string(c.Verdict),
			DerivesFrom: c.DerivesFrom,
			Summary:     c.Summary,
			Status:      status,
			Fault:       kind,
		})
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		fmt.Fprintf(os.Stderr, "charpy cases: %v\n", err)
		return exitHarness
	}
	return exitClean
}

// cmdPolicy handles `charpy policy <subcommand>`.
func cmdPolicy(args []string, out io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprint(os.Stderr, "usage: charpy policy validate <dir|file>...\n")
		return exitHarness
	}
	return cmdPolicyValidate(args[1:], out)
}

// cmdPolicyValidate loads manifests and reports what is wrong with them.
//
// It exits 4 rather than 0, because the exit code is what a caller acts on and
// a caller that gets 0 for a broken policy proceeds with a broken policy. It
// exits 4 rather than 2, because an invalid manifest is not charpy failing.
// The code is the machine half; the diagnostics below are the half that
// actually fixes anything.
func cmdPolicyValidate(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: charpy policy validate <dir|file>...\n")
		return exitHarness
	}

	var errs, warnings int
	for _, target := range args {
		e, w, fatal := validateTarget(target, out)
		if fatal {
			return exitHarness
		}
		errs, warnings = errs+e, warnings+w
	}

	fmt.Fprintf(out, "%s, %s\n", plural(errs, "error"), plural(warnings, "warning"))
	if errs > 0 {
		return exitBadPolicy
	}
	return exitClean
}

func validateTarget(target string, out io.Writer) (errs, warnings int, fatal bool) {
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy policy validate: %v\n", err)
		return 0, 0, true
	}

	from := target
	if !info.IsDir() {
		from = filepath.Dir(target)
	}
	fsys, dir, show, err := rooted(from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy policy validate: %v\n", err)
		return 0, 0, true
	}
	if !info.IsDir() {
		// A single file is loaded as a directory of one, so the loader's
		// accumulate-every-broken-file behaviour does not have to know the
		// difference.
		fsys = onlyFile{FS: fsys, path: path.Join(dir, filepath.Base(target))}
	}

	_, warns, err := catalogue.Load(fsys, dir)
	for _, w := range warns {
		fmt.Fprintf(out, "warning: %s\n", show(w))
	}
	if err == nil {
		return 0, len(warns), false
	}

	// One diagnostic per line. The library's joined summary reads as a wall,
	// and is not what the format's own documentation promises.
	var le *catalogue.LoadError
	if errors.As(err, &le) {
		for _, e := range le.Errs {
			fmt.Fprintln(out, show(e))
		}
		return len(le.Errs), len(warns), false
	}
	fmt.Fprintln(out, show(err.Error()))
	return 1, len(warns), false
}

// rooted splits a target into an fs.FS, a path within it, and a function that
// puts a diagnostic back into the user's terms.
//
// The FS is always rooted at "/" and the path always absolute, because an
// fs.FS path may contain neither a leading separator nor "..", and a target
// like "../../cases" is an ordinary thing to type.
//
// The paths shown are a separate question from the paths read. Validating
// "cases/" should report "cases/stream.toml" -- not a bare filename that
// leaves the reader guessing which directory it came from, and not the
// resolved absolute path, which answers a question nobody asked. So
// diagnostics get the resolved prefix swapped back for whatever the user
// typed.
func rooted(target string) (fs.FS, string, func(string) string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, "", nil, err
	}

	within := strings.TrimPrefix(filepath.ToSlash(abs), "/")
	typed := filepath.ToSlash(filepath.Clean(target))

	show := func(s string) string {
		if rest, ok := strings.CutPrefix(s, within); ok {
			return typed + rest
		}
		return s
	}
	return os.DirFS("/"), within, show, nil
}

// onlyFile narrows a directory to a single named file, so that validating one
// manifest and validating a directory take the same path through the loader.
type onlyFile struct {
	fs.FS
	path string
}

func (o onlyFile) ReadDir(string) ([]fs.DirEntry, error) {
	st, err := fs.Stat(o.FS, o.path)
	if err != nil {
		return nil, err
	}
	return []fs.DirEntry{fs.FileInfoToDirEntry(st)}, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
