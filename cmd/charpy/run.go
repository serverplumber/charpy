package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/gateway"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
	"github.com/serverplumber/charpy/internal/seed"
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
	caseGlob := fs.String("case", "", "drive the subject with charpy's own peer, running cases whose id matches this glob")
	subjectURL := fs.String("subject-url", "", "an HTTP subject already running at this URL; charpy proxies it (requires --case)")
	hostileMode := fs.Bool("hostile", false, "serve as a hostile server for a client under test that spawned charpy over stdio; --case filters which cases arm")
	hostileHTTP := fs.String("hostile-http", "", "serve hostile over HTTP at this address instead of stdio (e.g. :8080); the client under test connects here")
	gatewayMode := fs.Bool("gateway", false, "drive a gateway charpy spawns per case: {upstream0}, {upstream1}, ... in its command become charpy's upstream URLs; --subject-url is where it serves (requires --case)")
	tool := fs.String("tool", "", "tool the script calls; empty takes the first the subject lists")
	toolArgs := fs.String("args", "", "JSON object of arguments passed to every --tool call; empty sends none")
	timeout := fs.Duration("timeout", 0, "how long a scripted run waits on a subject that has stopped answering (default 30s)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: charpy run [flags] -- <subject command>\n"+
			"       charpy run --subject-url <url> --case <glob> --revision <rev> [flags]\n"+
			"       charpy run --gateway --subject-url <url> --case <glob> --revision <rev> [flags] -- <gateway command>\n\n"+
			"Over stdio, charpy launches the subject and speaks to it over its pipes.\n"+
			"With --subject-url, charpy proxies an HTTP subject the user is already running.\n"+
			"With --gateway, charpy spawns the gateway per case and stands on both sides of it.\n\n"+
			"Without --case, charpy relays: the client's own user drives it and the run\n"+
			"ends on a signal. With --case, charpy drives, one case per run, and each\n"+
			"case gets its own transcript.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}

	command := fs.Args()

	stim, code := stimulus(*tool, *toolArgs, *caseGlob, *hostileMode || *hostileHTTP != "")
	if code != exitClean {
		return code
	}
	if *timeout < 0 {
		fmt.Fprintf(os.Stderr, "charpy run: --timeout %s is negative\n", *timeout)
		return exitHarness
	}

	if *gatewayMode {
		cfg, code := runConfig(*rev, *seedFlag, string(transcript.ClassGateway), string(transcript.TransportHTTP))
		if code != exitClean {
			return code
		}
		cfg.stim, cfg.timeout = stim, *timeout
		return gatewayScripted(command, *subjectURL, cfg, *caseGlob, *outDir, *noRedact, out)
	}

	if *subjectURL != "" {
		if len(command) != 0 {
			fmt.Fprint(os.Stderr, "charpy run: --subject-url proxies a running server; do not also pass a subject command.\n")
			return exitHarness
		}
		cfg, code := runConfig(*rev, *seedFlag, *class, string(transcript.TransportHTTP))
		if code != exitClean {
			return code
		}
		cfg.stim, cfg.timeout = stim, *timeout
		return httpScripted(*subjectURL, cfg, *caseGlob, *outDir, *noRedact, out)
	}

	if *hostileMode || *hostileHTTP != "" {
		if len(command) != 0 {
			fmt.Fprint(os.Stderr, "charpy run: --hostile serves the client that connects to charpy; do not pass a subject command.\n")
			return exitHarness
		}
		transport := transcript.TransportStdio
		if *hostileHTTP != "" {
			transport = transcript.TransportHTTP
		}
		cfg, code := runConfig(*rev, *seedFlag, "client", string(transport))
		if code != exitClean {
			return code
		}
		if *hostileHTTP != "" {
			return hostileHTTPRun(*hostileHTTP, cfg, *caseGlob, *outDir, *noRedact, out)
		}
		return hostileRun(cfg, *caseGlob, *outDir, *noRedact, out)
	}

	if len(command) == 0 {
		fs.Usage()
		return exitHarness
	}

	cfg, code := runConfig(*rev, *seedFlag, *class, string(transcript.TransportStdio))
	if code != exitClean {
		return code
	}
	cfg.stim, cfg.timeout = stim, *timeout
	if cfg.class == transcript.ClassGateway {
		// A gateway has two faces, and the shim holds one connection. Saying
		// so beats producing a half-faced transcript that reads as complete.
		fmt.Fprint(os.Stderr, "charpy run: a gateway has two faces; the stdio shim drives one.\n"+
			"Use --gateway, which stands charpy on both sides of it.\n")
		return exitHarness
	}

	if *caseGlob != "" {
		return scripted(command, cfg, *caseGlob, *outDir, *noRedact, out)
	}

	tr, runID, closeT, code := openTranscript(*outDir, cfg, transcript.ModeStdioIngress, nil, *noRedact, out)
	if code != exitClean {
		return code
	}
	fmt.Fprintf(out, "charpy: seed %s, transcript %s\n",
		cfg.seed, filepath.Join(*outDir, runID+".jsonl"))

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

	// unobservable is the cases dropped at selection only because nothing this
	// charpy has can see their answer, kept so a refusal can say so rather
	// than claim the case does not apply.
	unobservable []catalogue.Case

	// unreachable is the cases dropped at selection only because nothing this
	// run stands up sends the frame they select on, each with the reason.
	unreachable []unreached

	// byRevision is what an auto run arms, keyed by the revision the
	// handshake may settle on, each case compiled against that revision so
	// its citation names it. Compiled here rather than at settlement so a
	// case that does not compile fails the run before the subject starts.
	byRevision map[revision.Revision][]interpose.Case

	// stim shapes what the script originates. Zero calls the first tool the
	// subject lists, with no arguments.
	stim scenario.Options

	// timeout bounds a script whose subject has stopped answering. Zero takes
	// the driver's own default.
	timeout time.Duration
}

// stimulus reads the two flags that say what charpy originates.
//
// Both are owned stimulus only. Under a relay the client under test chooses
// its own traffic, so a tool named there would be quietly ignored, and a flag
// that does nothing is worse than one that is refused.
func stimulus(tool, args, glob string, relay bool) (scenario.Options, int) {
	if tool == "" && args == "" {
		return scenario.Options{}, exitClean
	}
	if glob == "" || relay {
		fmt.Fprint(os.Stderr, "charpy run: --tool and --args say what charpy originates, so they need --case.\n"+
			"Under a relay the client under test chooses its own traffic.\n")
		return scenario.Options{}, exitHarness
	}

	o := scenario.Options{Tool: tool}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &o.Arguments); err != nil {
			fmt.Fprintf(os.Stderr, "charpy run: --args must be a JSON object: %v\n", err)
			return scenario.Options{}, exitHarness
		}
	}
	return o, exitClean
}

func runConfig(rev, seedFlag, class, transport string) (config, int) {
	cfg := config{seed: seedFlag}
	if cfg.seed == "" {
		cfg.seed = newSeed()
	} else {
		if !seed.Valid(cfg.seed) {
			fmt.Fprintf(os.Stderr, "charpy run: --seed %q must be 6 to 16 lowercase hex digits\n", cfg.seed)
			return config{}, exitHarness
		}
	}

	switch transcript.Class(class) {
	case transcript.ClassServer:
		cfg.class, cfg.face = transcript.ClassServer, transcript.Downstream
	case transcript.ClassClient:
		cfg.class, cfg.face = transcript.ClassClient, transcript.Upstream
	case transcript.ClassGateway:
		// The header's revision is the downstream face's (exchange.Run's
		// Headed), so that is the face a gateway run is cited from.
		cfg.class, cfg.face = transcript.ClassGateway, transcript.Downstream
	default:
		fmt.Fprintf(os.Stderr, "charpy run: unknown subject class %q\n", class)
		return config{}, exitHarness
	}

	// "auto" means charpy has not been told, so it watches the handshake
	// instead, and arms nothing until the handshake says which revision is in
	// play: both which cases apply and what revision their citations name are
	// facts about it. Arming the union beforehand would cite every case
	// against the oldest revision it allows and fire cases the negotiated one
	// rules out.
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

	if cfg.auto {
		cfg.byRevision = map[revision.Revision][]interpose.Case{}
		for _, r := range revision.All() {
			if cfg.byRevision[r], err = selectFor(cat, r, cfg.seed, transport, class); err != nil {
				fmt.Fprintf(os.Stderr, "charpy run: %v\n", err)
				return config{}, exitHarness
			}
		}
		return cfg, exitClean
	}
	cfg.cases, err = selectFor(cat, cfg.revision, cfg.seed, transport, class)
	if err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: %v\n", err)
		return config{}, exitHarness
	}
	for _, cs := range cat.Applicable(cfg.revision) {
		if !cs.SupportsTransport(transport) || !cs.SupportsSubject(class) {
			continue
		}
		if !cs.ObservableBy(observers...) {
			cfg.unobservable = append(cfg.unobservable, cs)
			continue
		}
		if why := reaches(cs, cfg.revision, cfg.seed, class); why != nil {
			cfg.unreachable = append(cfg.unreachable, unreached{cs.ID, why})
		}
	}
	return cfg, exitClean
}

// unreached is a case dropped at selection because nothing the run stands up
// sends its frame, and why.
type unreached struct {
	id  string
	why error
}

// explainDropped says which cases a glob would have selected but that this run
// dropped -- no observer here can judge them, or nothing here sends their
// frame -- so a refusal names the reason instead of claiming the case does
// not apply to the run.
func explainDropped(cfg config, glob string) {
	g := interpose.ParseGlob(glob)
	for _, cs := range cfg.unobservable {
		if glob == "" || g.Match(cs.ID) {
			fmt.Fprintf(os.Stderr, "charpy run: %s applies, but its answer is seen by %s, "+
				"and this charpy observes only %s\n",
				cs.ID, strings.Join(cs.ObservedBy, " or "), strings.Join(observers, ", "))
		}
	}
	for _, u := range cfg.unreachable {
		if glob == "" || g.Match(u.id) {
			fmt.Fprintf(os.Stderr, "charpy run: %s applies, but %v\n", u.id, u.why)
		}
	}
}

// reaches compiles a case for the revision and asks whether a run of this
// class ever sends the frame it selects on. A case that does not compile is
// left for selectFor to report.
func reaches(cs catalogue.Case, r revision.Revision, seed, class string) error {
	compiled, err := cs.Compile(r, seed)
	if err != nil {
		return nil
	}
	return scenario.Reaches(compiled.Match, transcript.Class(class))
}

// observers is what every driver can judge a case's answer by today: what
// crossed the wire. The in-process driver and the differential will add to it.
var observers = []string{"wire"}

// selectFor compiles the cases a run will arm: those active for the revision,
// runnable on the transport, applicable to the subject class, observable by
// something the run has, and selecting a frame something in the run sends.
// All five are applicability, like the revision range -- a stdio-only case
// armed on an HTTP run, a server case armed on a client run, a case whose
// answer nothing here can see, or one whose frame nothing here sends could
// only report UNTRIGGERED, test the wrong side, or fire with nobody watching
// -- so each is dropped at selection the same way an out-of-revision case is.
func selectFor(cat *catalogue.Catalogue, r revision.Revision, seed, transport, class string) ([]interpose.Case, error) {
	var out []interpose.Case
	for _, cs := range cat.Applicable(r) {
		if !cs.SupportsTransport(transport) || !cs.SupportsSubject(class) || !cs.ObservableBy(observers...) {
			continue
		}
		compiled, err := cs.Compile(r, seed)
		if err != nil {
			return nil, err
		}
		if scenario.Reaches(compiled.Match, transcript.Class(class)) != nil {
			continue
		}
		out = append(out, compiled)
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

func openTranscript(dir string, cfg config, mode transcript.Mode, pr *transcript.Peer, noRedact bool, out io.Writer) (*transcript.Writer, string, func() error, int) {
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
			Mode:          mode,
			Subject:       transcript.Subject{Class: cfg.class},
			// Named only when charpy originated the traffic. A relayed run
			// has no peer to name (ADR-011).
			Peer: pr,
			// A relayed run is driven by someone else in real time, so
			// charpy's own scheduling follows wall time too: a fault that
			// withdrew only when a run loop advanced would never withdraw,
			// because there is no run loop here.
			Clock: clock.ModeReal,
		},
		Sched:    clock.RealSched(),
		Wall:     clock.RealWall(),
		RunID:    runID,
		Redactor: redactor,
	})
	if err != nil {
		f.Close()
		fmt.Fprintf(out, "charpy run: %v\n", err)
		return nil, "", nil, exitHarness
	}

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
		Arm:        armAtSettlement(cfg),
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

// armAtSettlement is how an auto run arms: nil for a run told its revision,
// whose cases were compiled up front, and otherwise the lookup the shim asks
// once the handshake settles.
func armAtSettlement(cfg config) func(revision.Revision) []interpose.Case {
	if !cfg.auto {
		return nil
	}
	return func(r revision.Revision) []interpose.Case { return cfg.byRevision[r] }
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

// scripted drives the subject with charpy's own peer, one case at a time.
//
// One case per run is ADR-012, and the loop is what that decision looks like
// from the CLI: `--case 'stream/*'` is three runs against three subjects
// writing three transcripts, not one session with a glob armed. A transcript
// carrying every fault at once is a puzzle rather than a finding, and the
// person opening one has most likely never run a hostile fixture before.
//
// Nothing here judges. Each run writes a transcript and `charpy replay` turns
// it into verdicts, so a harness failure is the only thing that changes the
// exit code -- a subject that crashed under a fault is a finding the oracle
// reports, and failing the run would fail exactly the runs that worked.
func scripted(command []string, cfg config, glob, outDir string, noRedact bool, out io.Writer) int {
	if cfg.auto {
		// The peer is constructed at an era, so "I have not been told" is not
		// an answer charpy can act on. Under relay it is, because the subject
		// says what it speaks and charpy watches; here charpy speaks first.
		fmt.Fprint(os.Stderr, "charpy run: --case needs an explicit --revision.\n"+
			"charpy originates the traffic here, so it has to choose an era to speak;\n"+
			"auto is the fallback for watching somebody else's handshake.\n")
		return exitHarness
	}
	if cfg.class != transcript.ClassServer {
		fmt.Fprintf(os.Stderr, "charpy run: --case drives a server subject; --subject %s needs "+
			"charpy to serve instead, which is the hostile driver.\n", cfg.class)
		return exitHarness
	}

	selected := matching(cfg.cases, glob)
	if len(selected) == 0 {
		// A selection that matches nothing is a run that passes by not
		// running, which is the failure mode the loader exists to prevent.
		fmt.Fprintf(os.Stderr, "charpy run: no case matching %q applies to %s\n", glob, cfg.revision)
		explainDropped(cfg, glob)
		return exitHarness
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "charpy: seed %s, %d case(s) against %s\n", cfg.seed, len(selected), cfg.revision)

	worst := exitClean
	for _, c := range selected {
		if ctx.Err() != nil {
			fmt.Fprintf(out, "charpy: stopped before %s\n", c.ID)
			break
		}
		if code := scriptOne(ctx, command, cfg, c, outDir, noRedact, out); code != exitClean {
			worst = code
		}
	}
	return worst
}

// scriptOne is one case: one subject, one peer, one transcript.
func scriptOne(ctx context.Context, command []string, cfg config, c interpose.Case,
	outDir string, noRedact bool, out io.Writer) int {

	tr, runID, closeT, code := openTranscript(outDir, cfg, transcript.ModeStdioIngress, peer.Describe(cfg.revision), noRedact, out)
	if code != exitClean {
		return code
	}

	subjLog, closeLog, code := openSubjectLog(outDir, runID, out)
	if code != exitClean {
		_ = closeT()
		return code
	}

	sched := clock.RealSched()
	sc, err := stdio.NewScript(stdio.ScriptOptions{
		Case:     c,
		Era:      cfg.revision,
		Stimulus: cfg.stim,
		Timeout:  cfg.timeout,
		Options: stdio.Options{
			Command:    command,
			Env:        os.Environ(),
			Errs:       io.MultiWriter(out, subjLog),
			Cases:      []interpose.Case{c},
			Transcript: tr,
			Sched:      sched,
			Ledger:     interpose.NewLedger(sched),
			Face:       cfg.face,
			RunSeed:    cfg.seed,
		},
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, err)
		_ = closeLog()
		_ = closeT()
		return exitHarness
	}

	runErr := sc.Run(ctx)

	if err := closeLog(); err != nil {
		fmt.Fprintf(out, "charpy run: subject log: %v\n", err)
	}
	if err := closeT(); err != nil {
		// A transcript that lost a line is not one to draw conclusions from.
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	if runErr != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, runErr)
		return exitHarness
	}

	fmt.Fprintf(out, "  %s\n    %s\n", c.Citation, filepath.Join(outDir, runID+".jsonl"))
	return exitClean
}

// matching selects cases by permanent id. Globs match the id only, never the
// citation: a citation carries the run's own seed, so a glob over one would
// match differently on every run (docs/design/case-identity.md section 1).
func matching(all []interpose.Case, pattern string) []interpose.Case {
	g := interpose.ParseGlob(pattern)
	var out []interpose.Case
	for _, c := range all {
		if g.Match(c.ID) {
			out = append(out, c)
		}
	}
	return out
}

// httpScripted drives an HTTP subject through the proxy, one case per run.
//
// The subject here is a server the user is already running -- charpy connects
// to it rather than spawning it, so there is no process to own and no stderr
// to capture, unlike the stdio path. Everything else is the same shape as
// scripted: one case, one transcript, a citation on stdout (ADR-012).
func httpScripted(url string, cfg config, glob, outDir string, noRedact bool, out io.Writer) int {
	if cfg.auto {
		fmt.Fprint(os.Stderr, "charpy run: --subject-url needs an explicit --revision.\n"+
			"charpy originates the traffic here, so it has to choose an era to speak.\n")
		return exitHarness
	}
	if cfg.class != transcript.ClassServer {
		fmt.Fprintf(os.Stderr, "charpy run: the proxy drives a server subject; --subject %s is a different driver.\n", cfg.class)
		return exitHarness
	}
	if glob == "" {
		fmt.Fprint(os.Stderr, "charpy run: --subject-url is owned stimulus and needs --case to say what to run.\n")
		return exitHarness
	}

	selected := matching(cfg.cases, glob)
	if len(selected) == 0 {
		fmt.Fprintf(os.Stderr, "charpy run: no case matching %q applies to %s\n", glob, cfg.revision)
		explainDropped(cfg, glob)
		return exitHarness
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "charpy: seed %s, %d case(s) against %s at %s\n", cfg.seed, len(selected), cfg.revision, url)

	worst := exitClean
	for _, c := range selected {
		if ctx.Err() != nil {
			fmt.Fprintf(out, "charpy: stopped before %s\n", c.ID)
			break
		}
		if code := httpScriptOne(ctx, url, cfg, c, outDir, noRedact, out); code != exitClean {
			worst = code
		}
	}
	return worst
}

func httpScriptOne(ctx context.Context, url string, cfg config, c interpose.Case,
	outDir string, noRedact bool, out io.Writer) int {

	tr, runID, closeT, code := openTranscript(outDir, cfg, transcript.ModeProxy, peer.Describe(cfg.revision), noRedact, out)
	if code != exitClean {
		return code
	}

	sched := clock.RealSched()
	sc, err := proxy.NewScript(proxy.ScriptOptions{
		Case:     c,
		Era:      cfg.revision,
		Stimulus: cfg.stim,
		Timeout:  cfg.timeout,
		Options: proxy.Options{
			SubjectURL: url,
			Transcript: tr,
			Sched:      sched,
			Wall:       clock.RealWall(),
			Ledger:     interpose.NewLedger(sched),
			Face:       cfg.face,
			RunSeed:    cfg.seed,
		},
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, err)
		_ = closeT()
		return exitHarness
	}

	runErr := sc.Run(ctx)

	if err := closeT(); err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	if runErr != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, runErr)
		return exitHarness
	}

	fmt.Fprintf(out, "  %s\n    %s\n", c.Citation, filepath.Join(outDir, runID+".jsonl"))
	return exitClean
}

// gatewayScripted runs cases against a gateway charpy spawns, one gateway
// process and one transcript per case (internal/driver/gateway).
func gatewayScripted(command []string, url string, cfg config, glob, outDir string, noRedact bool, out io.Writer) int {
	switch {
	case cfg.auto:
		fmt.Fprint(os.Stderr, "charpy run: --gateway needs an explicit --revision.\n"+
			"charpy originates the traffic on both faces, so it has to choose an era to speak.\n")
		return exitHarness
	case url == "":
		fmt.Fprint(os.Stderr, "charpy run: --gateway needs --subject-url, where the gateway serves its clients.\n")
		return exitHarness
	case len(command) == 0:
		fmt.Fprint(os.Stderr, "charpy run: --gateway needs the command that starts the gateway, after --.\n")
		return exitHarness
	case glob == "":
		fmt.Fprint(os.Stderr, "charpy run: --gateway is owned stimulus and needs --case to say what to run.\n")
		return exitHarness
	}

	selected := matching(cfg.cases, glob)
	if len(selected) == 0 {
		fmt.Fprintf(os.Stderr, "charpy run: no case matching %q applies to a gateway at %s\n", glob, cfg.revision)
		explainDropped(cfg, glob)
		return exitHarness
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "charpy: seed %s, %d case(s) against a gateway at %s, %s\n", cfg.seed, len(selected), url, cfg.revision)

	worst := exitClean
	for _, c := range selected {
		if ctx.Err() != nil {
			fmt.Fprintf(out, "charpy: stopped before %s\n", c.ID)
			break
		}
		if code := gatewayScriptOne(ctx, command, url, cfg, c, outDir, noRedact, out); code != exitClean {
			worst = code
		}
	}
	return worst
}

// gatewayScriptOne is one case: one gateway, charpy on both sides of it, one
// transcript.
func gatewayScriptOne(ctx context.Context, command []string, url string, cfg config, c interpose.Case,
	outDir string, noRedact bool, out io.Writer) int {

	tr, runID, closeT, code := openTranscript(outDir, cfg, transcript.ModeProxy, peer.Describe(cfg.revision), noRedact, out)
	if code != exitClean {
		return code
	}
	subjLog, closeLog, code := openSubjectLog(outDir, runID, out)
	if code != exitClean {
		_ = closeT()
		return code
	}

	sched := clock.RealSched()
	d, err := gateway.New(gateway.Options{
		Command:    command,
		Env:        os.Environ(),
		SubjectURL: url,
		Errs:       io.MultiWriter(out, subjLog),
		Case:       c,
		Era:        cfg.revision,
		Stimulus:   cfg.stim,
		Timeout:    cfg.timeout,
		Transcript: tr,
		Sched:      sched,
		Wall:       clock.RealWall(),
		Ledger:     interpose.NewLedger(sched),
		RunSeed:    cfg.seed,
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, err)
		_ = closeLog()
		_ = closeT()
		return exitHarness
	}

	runErr := d.Run(ctx)

	if err := closeLog(); err != nil {
		fmt.Fprintf(out, "charpy run: subject log: %v\n", err)
	}
	if err := closeT(); err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	if runErr != nil {
		fmt.Fprintf(out, "charpy run: %s: %v\n", c.ID, runErr)
		return exitHarness
	}

	fmt.Fprintf(out, "  %s\n    %s\n", c.Citation, filepath.Join(outDir, runID+".jsonl"))
	return exitClean
}

// hostileRun serves a client under test over stdio and faults the responses.
//
// It is relay-shaped, not scripted: the client drives, so charpy arms every
// applicable case at once (filtered by --case if given), writes one transcript,
// and ends on a signal -- the same model as the relay shim, and unlike the
// per-case scripted modes (ADR-012 is about owned stimulus, which this is not).
// Cases that never match end UNTRIGGERED, which the oracle reports.
func hostileRun(cfg config, glob, outDir string, noRedact bool, out io.Writer) int {
	if cfg.auto {
		fmt.Fprint(os.Stderr, "charpy run: --hostile needs an explicit --revision.\n"+
			"charpy claims an era to the client, so it has to be told which.\n")
		return exitHarness
	}

	cases := cfg.cases
	if glob != "" {
		cases = matching(cases, glob)
	}
	if len(cases) == 0 {
		fmt.Fprintf(os.Stderr, "charpy run: no client case applies to %s\n", cfg.revision)
		explainDropped(cfg, glob)
		return exitHarness
	}

	tr, runID, closeT, code := openTranscript(outDir, cfg, transcript.ModeHostileServer, peer.Describe(cfg.revision), noRedact, out)
	if code != exitClean {
		return code
	}
	fmt.Fprintf(out, "charpy: seed %s, hostile server claiming %s, %d case(s) armed\n",
		cfg.seed, cfg.revision, len(cases))

	// SIGINT/SIGTERM ends the run; the client disconnecting ends it too.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched := clock.RealSched()
	h, err := hostile.New(hostile.Options{
		In:         os.Stdin,
		Out:        os.Stdout,
		Era:        cfg.revision,
		Cases:      cases,
		Transcript: tr,
		Sched:      sched,
		Ledger:     interpose.NewLedger(sched),
		RunSeed:    cfg.seed,
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		_ = closeT()
		return exitHarness
	}

	runErr := h.Run(ctx)
	if err := closeT(); err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	if runErr != nil {
		fmt.Fprintf(out, "charpy run: %v\n", runErr)
		return exitHarness
	}
	fmt.Fprintf(out, "charpy: run ended; transcript %s\n", filepath.Join(outDir, runID+".jsonl"))
	return exitClean
}

// hostileHTTPRun serves a hostile server over HTTP for a client under test that
// connects to charpy's ingress. Like the stdio hostile mode it is relay-shaped:
// the client drives, all applicable cases are armed, and a signal ends the run.
// HTTP is what makes a second handshake possible, so
// capability-narrowed-on-later-handshake can fire here where it cannot over
// stdio.
func hostileHTTPRun(listen string, cfg config, glob, outDir string, noRedact bool, out io.Writer) int {
	if cfg.auto {
		fmt.Fprint(os.Stderr, "charpy run: --hostile-http needs an explicit --revision.\n"+
			"charpy claims an era to the client, so it has to be told which.\n")
		return exitHarness
	}

	cases := cfg.cases
	if glob != "" {
		cases = matching(cases, glob)
	}
	if len(cases) == 0 {
		fmt.Fprintf(os.Stderr, "charpy run: no client case applies to %s\n", cfg.revision)
		explainDropped(cfg, glob)
		return exitHarness
	}

	tr, runID, closeT, code := openTranscript(outDir, cfg, transcript.ModeHostileServer, peer.Describe(cfg.revision), noRedact, out)
	if code != exitClean {
		return code
	}

	sched := clock.RealSched()
	h, err := hostile.NewHTTP(hostile.HTTPOptions{
		Listen:     listen,
		Era:        cfg.revision,
		Cases:      cases,
		Transcript: tr,
		Sched:      sched,
		Wall:       clock.RealWall(),
		Ledger:     interpose.NewLedger(sched),
		RunSeed:    cfg.seed,
	})
	if err != nil {
		fmt.Fprintf(out, "charpy run: %v\n", err)
		_ = closeT()
		return exitHarness
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The listeners bind in NewHTTP, so the endpoint is known before serving.
	fmt.Fprintf(out, "charpy: seed %s, hostile server claiming %s at %s, %d case(s) armed\n",
		cfg.seed, cfg.revision, h.Endpoint(), len(cases))

	runErr := h.Run(ctx)
	if err := closeT(); err != nil {
		fmt.Fprintf(os.Stderr, "charpy run: transcript: %v\n", err)
		return exitHarness
	}
	if runErr != nil {
		fmt.Fprintf(out, "charpy run: %v\n", runErr)
		return exitHarness
	}
	fmt.Fprintf(out, "charpy: run ended; transcript %s\n", filepath.Join(outDir, runID+".jsonl"))
	return exitClean
}
