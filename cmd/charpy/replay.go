package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/coverage"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
	"github.com/serverplumber/charpy/internal/oracle/schemacheck"
	"github.com/serverplumber/charpy/internal/report"
	"github.com/serverplumber/charpy/internal/transcript"
)

// cmdReplay runs the oracle over a finished transcript.
//
// This is where charpy's determinism actually lives. Runs against a live
// subject are best-effort reproducible and nothing can make them otherwise --
// the subject is loaded differently, collects garbage at a different moment,
// or was redeployed. Verdicts are exactly reproducible forever, because they
// are computed here, offline, from a file. See decisions.md ADR-001.
func cmdReplay(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "text", "text, jsonl or junit")
	only := fs.String("oracle", "", "run one layer: schema, invariant or coverage")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: charpy replay [flags] <transcript.jsonl>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitHarness
	}

	t, code := readTranscript(fs.Arg(0))
	if code != exitClean {
		return code
	}

	rep, code := judge(t, *only)
	if code != exitClean {
		return code
	}

	if err := render(out, *format, rep); err != nil {
		fmt.Fprintf(os.Stderr, "charpy replay: %v\n", err)
		return exitHarness
	}

	// A MUST violation fails the build. OBSERVED findings are what happened
	// and are reported without a verdict on whether they are acceptable;
	// deciding that is the reader's, not charpy's.
	if must, _ := rep.Violations(); must > 0 {
		return exitMustViolate
	}
	return exitClean
}

func readTranscript(path string) (*transcript.Transcript, int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy replay: %v\n", err)
		return nil, exitHarness
	}
	defer f.Close()

	t, err := transcript.Read(bufio.NewReader(f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy replay: %v\n", err)
		return nil, exitHarness
	}
	return t, exitClean
}

// judge runs the layers and merges what they concluded.
//
// The merge sorts, and that is not cosmetic: two replays of one transcript
// must produce byte-identical output, so nothing downstream may depend on the
// order layers happened to run in or on a map's iteration.
func judge(t *transcript.Transcript, only string) (oracle.Report, int) {
	rep := oracle.Report{RunID: t.Header.RunID}

	if only == "" || only == schemacheck.Layer {
		got, err := schemacheck.Check(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "charpy replay: %v\n", err)
			return rep, exitHarness
		}
		rep.Findings = append(rep.Findings, got.Findings...)
	}
	if only == "" || only == invariant.Layer {
		rep.Findings = append(rep.Findings, invariant.Check(t).Findings...)
	}
	if only == "" || only == coverage.Layer {
		rep.Findings = append(rep.Findings, coverage.Check(t).Findings...)
	}
	if only != "" && only != schemacheck.Layer && only != invariant.Layer && only != coverage.Layer {
		fmt.Fprintf(os.Stderr, "charpy replay: unknown oracle %q; v0 has %s and %s\n",
			only, schemacheck.Layer, invariant.Layer+", "+coverage.Layer)
		return rep, exitHarness
	}

	rep.Sort()
	return rep, exitClean
}

func render(w io.Writer, format string, rep oracle.Report) error {
	switch format {
	case "jsonl":
		return report.JSONL(w, rep)
	case "junit":
		return report.JUnit(w, rep)
	case "text":
		return text(w, rep)
	default:
		return fmt.Errorf("unknown format %q; want text, jsonl or junit", format)
	}
}

// text is what a person reads. The non-verdicts are printed with their
// reasons, because a suite that lets a skip look like a pass has stopped
// being worth running.
func text(w io.Writer, rep oracle.Report) error {
	for _, f := range rep.Findings {
		where := "        "
		if f.Seq >= 0 {
			where = fmt.Sprintf("seq=%-4d", f.Seq)
		}
		if _, err := fmt.Fprintf(w, "%-12s %s  %s\n", f.Verdict, where, f.Check); err != nil {
			return err
		}
		for _, line := range details(f) {
			if _, err := fmt.Fprintf(w, "             %s\n", line); err != nil {
				return err
			}
		}
	}

	must, observed := rep.Violations()
	skipped := len(rep.Findings) - must - observed
	_, err := fmt.Fprintf(w, "\n%d MUST, %d OBSERVED, %d not judged\n", must, observed, skipped)
	return err
}

func details(f oracle.Finding) []string {
	var out []string
	if f.Summary != "" {
		out = append(out, f.Summary)
	}
	// The citation is what someone retypes into a bug report, and for a
	// finding with no sequence number to point at it is the only thing
	// identifying which case this is about.
	if f.Citation != "" {
		out = append(out, "case: "+f.Citation)
	}
	for _, line := range strings.Split(f.Detail, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if f.Reason != "" {
		out = append(out, "reason: "+f.Reason)
	}
	return out
}
