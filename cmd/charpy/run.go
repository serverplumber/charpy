package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// cmdRun drives a subject and writes a transcript.
//
// v0 implements one mode: the stdio shim. charpy is launched where the server
// would be, spawns the real server, and sits on both pipes while the client's
// own user drives it. That needs no scenario player and no reference SDK,
// which is why it is the mode that exists first.
func cmdRun(args []string, _ io.Writer) int {
	// Everything charpy says goes to stderr, and that is the spec's rule
	// rather than a preference: a stdio server "MUST NOT write anything to
	// its stdout that is not a valid MCP message". In a shim charpy *is* the
	// server as far as the client is concerned, so one line of chatter on
	// stdout is a frame the client cannot parse. The client spawned charpy
	// and owns those descriptors; there is no choosing a different channel
	// for the protocol.
	//
	// stderr is shared with the subject's own logging, so writes to it are
	// serialised -- see console.
	out := newConsole(os.Stderr)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rev := fs.String("revision", "auto", "revision to select cases for, or auto to observe it")
	seedFlag := fs.String("seed", "", "run seed; generated and recorded when omitted")
	outDir := fs.String("out", "./charpy-out", "where the transcript is written")
	class := fs.String("subject", "server", "what is under test: server, client or gateway")
	noRedact := fs.Bool("no-redact", false, "record header values in the clear; the transcript is then unsafe to share")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: charpy run [flags] -- <subject command>\n\n"+
			"The subject is launched by charpy and spoken to over its pipes.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}

	command := fs.Args()
	if len(command) == 0 {
		fs.Usage()
		return exitHarness
	}

	cfg, code := runConfig(*rev, *seedFlag, *class)
	if code != exitClean {
		return code
	}

	tr, runID, closeT, code := openTranscript(*outDir, cfg, *noRedact, out)
	if code != exitClean {
		return code
	}

	// The subject's stderr is evidence. A server that logs a panic before it
	// stops answering is exactly what a resilience run wants to keep, and
	// passing it through to a terminal that may not exist -- a shim launched
	// by a GUI client has none -- loses it. It goes to a file beside the
	// transcript, sharing the run id, and is teed onward so a developer
	// watching a terminal still sees it live.
	subjLog, closeLog, code := openSubjectLog(*outDir, runID, out)
	if code != exitClean {
		_ = closeT()
		return code
	}
	tr.Event(transcript.Event{
		Kind:   transcript.Note,
		Detail: map[string]any{"subject_stderr": filepath.Join(*outDir, runID+".stderr.log")},
	})

	code = shim(command, cfg, tr, out, io.MultiWriter(out, subjLog))
	if err := closeLog(); err != nil {
		fmt.Fprintf(out, "charpy run: subject log: %v\n", err)
	}
	if err := closeT(); err != nil {
		// A transcript that lost a line is not one to draw conclusions from,
		// so it fails the run rather than being mentioned in passing.
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	return code
}

type config struct {
	seed     string
	revision revision.Revision
	auto     bool
	class    transcript.Class
	face     transcript.Face
	cases    []interpose.Case
}

func runConfig(rev, seedFlag, class string) (config, int) {
	cfg := config{seed: seedFlag}
	if cfg.seed == "" {
		cfg.seed = newSeed()
	}

	switch transcript.Class(class) {
	case transcript.ClassServer:
		cfg.class, cfg.face = transcript.ClassServer, transcript.Downstream
	case transcript.ClassClient:
		cfg.class, cfg.face = transcript.ClassClient, transcript.Upstream
	case transcript.ClassGateway:
		// A gateway has two faces, and the shim holds one connection. Saying
		// so beats producing a half-faced transcript that reads as complete.
		fmt.Fprint(os.Stderr, "charpy run: a gateway has two faces; the stdio shim drives one.\n"+
			"Use --subject server or client, or wait for the proxy driver.\n")
		return config{}, exitHarness
	default:
		fmt.Fprintf(os.Stderr, "charpy run: unknown subject class %q\n", class)
		return config{}, exitHarness
	}

	// "auto" means charpy has not been told, so it watches the initialize
	// exchange instead. Cases are selected for every revision they could
	// apply to and the matcher sorts it out, because the alternative is
	// waiting for the handshake before any case can arm.
	cfg.auto = rev == "auto"
	if !cfg.auto {
		cfg.revision = revision.Revision(rev)
		if !revision.Known(cfg.revision) {
			fmt.Fprintf(os.Stderr, "charpy run: unknown revision %q\n", rev)
			return config{}, exitHarness
		}
	}

	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: the shipped catalogue does not load:\n%v\n", err)
		return config{}, exitHarness
	}

	cfg.cases, err = selectFor(cat, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: %v\n", err)
		return config{}, exitHarness
	}
	return cfg, exitClean
}

func selectFor(cat *catalogue.Catalogue, cfg config) ([]interpose.Case, error) {
	if !cfg.auto {
		return cat.CompileAll(cfg.revision, cfg.seed)
	}

	seen := map[string]bool{}
	var out []interpose.Case
	for _, r := range revision.All() {
		compiled, err := cat.CompileAll(r, cfg.seed)
		if err != nil {
			return nil, err
		}
		for _, c := range compiled {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// openSubjectLog opens the file the subject's stderr is kept in.
func openSubjectLog(dir, runID string, out io.Writer) (io.Writer, func() error, int) {
	f, err := os.Create(filepath.Join(dir, runID+".stderr.log"))
	if err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return nil, nil, exitHarness
	}
	return f, f.Close, exitClean
}

func openTranscript(dir string, cfg config, noRedact bool, out io.Writer) (*transcript.Writer, string, func() error, int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return nil, "", nil, exitHarness
	}

	runID := transcript.NewRunID()
	path := filepath.Join(dir, runID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return nil, "", nil, exitHarness
	}

	redactor := transcript.NewRedactor()
	if noRedact {
		redactor = transcript.NoRedaction()
	}

	tr, err := transcript.New(f, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: version,
			CharpyCommit:  commit,
			Seed:          cfg.seed,
			Mode:          transcript.ModeStdioIngress,
			Subject:       transcript.Subject{Class: cfg.class},
			// A relayed run is driven by someone else in real time, so
			// charpy's own scheduling follows wall time too: a fault that
			// withdrew only when a run loop advanced would never withdraw,
			// because there is no run loop here.
			Clock: clock.ModeReal,
		},
		Sched:    clock.RealSched(),
		RunID:    runID,
		Redactor: redactor,
	})
	if err != nil {
		f.Close()
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return nil, "", nil, exitHarness
	}

	fmt.Fprintf(out, "charpy: seed %s, transcript %s\n", cfg.seed, path)
	return tr, runID, func() error {
		err := tr.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}, exitClean
}

func shim(command []string, cfg config, tr *transcript.Writer, out, subjErr io.Writer) int {
	// A relayed run has no last scripted frame to stop after, so it ends on a
	// signal. The context carries that to the subject, which charpy spawned
	// and therefore owns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One clock for the run, matching what the header declares. A relay is
	// driven by someone else in real time, so charpy's scheduling is too.
	sched := clock.RealSched()

	sh, err := stdio.New(stdio.Options{
		Command:    command,
		Env:        os.Environ(),
		In:         os.Stdin,
		Out:        os.Stdout,
		Errs:       subjErr,
		Cases:      cfg.cases,
		Transcript: tr,
		Sched:      sched,
		Ledger:     interpose.NewLedger(sched),
		Face:       cfg.face,
		RunSeed:    cfg.seed,
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return exitHarness
	}

	if err := sh.Run(ctx); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			fmt.Fprintf(out, "charpy run: subject exited: %v\n", err)
			return exitSubject
		}
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return exitHarness
	}

	// Cases that never matched end UNTRIGGERED, which is not a failure: the
	// traffic was someone else's and may simply never have gone where a
	// matcher pointed. Verdicts come from the oracle over the transcript, so
	// a clean relay exits 0 and leaves judgement offline where it belongs.
	fmt.Fprintf(out, "charpy: run ended\n")
	return exitClean
}

// console serialises writes to a descriptor two things are using: charpy's own
// diagnostics and the subject's log output.
//
// Without it they are two independent writers on one file descriptor, and a
// line from either can tear through the middle of a line from the other. The
// spec permits a server to log freely on stderr and expects a client to
// capture it, so interleaving is not hypothetical -- a chatty subject writes
// there constantly.
type console struct {
	mu sync.Mutex
	w  io.Writer
}

func newConsole(w io.Writer) *console { return &console{w: w} }

func (c *console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

func newSeed() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b[:])
}
