package stdio

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// maxFrame is how large a single frame may be. bufio's default of 64 KiB is
// smaller than a tools/list result with real schemas in it, and a scanner that
// stops mid-catalogue would look exactly like a subject that did.
const maxFrame = 4 << 20

// Options configures a shim.
type Options struct {
	// Command is the subject and its arguments.
	Command []string
	Env     []string

	// In and Out are the other peer: the client's frames arrive on In and its
	// answers leave on Out. In the shim this is charpy's own stdin and
	// stdout, because the client launched charpy believing it was the server.
	In  io.Reader
	Out io.WriteCloser
	// Errs receives the subject's stderr, which is usually where a server
	// says what went wrong.
	Errs io.Writer

	Cases      []interpose.Case
	Transcript *transcript.Writer
	// Sched is charpy's own scheduling clock -- when a withheld frame is
	// released. A relayed run passes a real one: there is no run loop to
	// advance an injected clock, so a fault scheduled on one would never
	// withdraw.
	Sched  clock.Sched
	Ledger *interpose.Ledger

	// Face is the subject's face, which decides which direction carries
	// faults. A server subject is faced downstream and is attacked by its
	// client; a client subject is faced upstream and is attacked by its
	// server.
	Face transcript.Face

	RunSeed   string
	ClientID  string
	SessionID string
	ConnID    string
}

// Shim relays between a real client and a real subject, corrupting what
// crosses toward the subject.
//
// It is the cheapest way to get charpy in front of something real, because
// nobody has to drive the client: its user does. The brief ranked clients
// priority 3 for being hard to drive, and under relay that difficulty simply
// is not charpy's.
//
// One face, not two. The subject has a single face here -- a server's
// client-facing side -- so a frame crossing it produces one transcript line.
// The connection to the other peer is not subject-facing and is not
// transcribed; charpy is the middleman there, not an observer of it.
type Shim struct {
	o     Options
	x     *exchange.Conn
	inter *interpose.Interposer
	match *interpose.Matcher

	cmd     *exec.Cmd
	toSubj  *wire.Stdio
	toPeer  *wire.Stdio
	subjOut io.Reader
}

// New prepares a shim. Nothing is spawned until Run.
func New(o Options) (*Shim, error) {
	if len(o.Command) == 0 {
		return nil, errors.New("stdio: no subject command")
	}
	if o.Face == "" {
		o.Face = transcript.Downstream
	}
	if o.ConnID == "" {
		o.ConnID = "c-0"
	}
	if o.ClientID == "" {
		o.ClientID = "c0"
	}
	if o.SessionID == "" {
		o.SessionID = "s-0"
	}

	core := &exchange.Core{
		Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases,
		Face: o.Face, Transport: transcript.TransportStdio,
		// One face here: a server subject has no second face to join to, so a
		// forwarded id would be a join to nothing.
		Link: func() transcript.Link { return transcript.Link{Via: transcript.ViaNone} },
	}
	return &Shim{
		o:     o,
		x:     core.Conn(o.ClientID, o.SessionID, o.ConnID),
		inter: interpose.New(o.Ledger, o.Sched),
		match: interpose.NewMatcher(o.Ledger, o.Cases...),
	}, nil
}

// Run spawns the subject and relays until the context is cancelled or either
// side goes away.
//
// A relayed run has no last scripted frame to stop after, so it ends on a
// signal. Cancelling the context kills the subject charpy spawned -- charpy
// owns that process -- and returns, leaving the transcript to be flushed by
// its own Close.
func (s *Shim) Run(ctx context.Context) error {
	if err := s.start(ctx); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Both directions are offered to the matcher, and the case decides.
	//
	// An earlier version faulted only toward the subject, reasoning that
	// corrupting the other way would be testing the client instead. That
	// reasoning put a rule in the driver that the manifest already carries in
	// two separate fields: [case.match].direction says where a fault lands,
	// and subject says who is under test. Hardcoding one here collapsed them
	// and made every s2c case in the catalogue unreachable -- which is all of
	// them, because the mechanisms that rewrite a result need a response to
	// rewrite.
	go func() {
		defer wg.Done()
		s.relay(s.o.In, s.toSubj, transcript.C2S)
		// The client stopped talking; let the subject see the end of its
		// input rather than hanging on a pipe nobody will write to.
		_ = s.toSubj.Close()
	}()

	go func() {
		defer wg.Done()
		s.relay(s.subjOut, s.toPeer, transcript.S2C)
	}()

	wg.Wait()
	return s.stop(ctx)
}

func (s *Shim) start(ctx context.Context) error {
	// The subject is killed when the context is cancelled; exec does that for
	// us, and doing it any other way leaves a server running after charpy has
	// gone.
	cmd := exec.CommandContext(ctx, s.o.Command[0], s.o.Command[1:]...)
	cmd.Env = s.o.Env
	cmd.Stderr = s.o.Errs

	in, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdio: subject stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdio: subject stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("stdio: %s: %w", s.o.Command[0], err)
	}

	s.cmd, s.subjOut = cmd, out
	s.toSubj = wire.NewStdio(in)
	s.toPeer = wire.NewStdio(s.o.Out)

	s.x.Event(transcript.ConnOpen, map[string]any{"command": s.o.Command[0]})
	return nil
}

// stop waits for the subject and records how it went.
//
// A subject charpy killed at shutdown and one that died on its own are not the
// same event, and recording them alike makes the subject_exit line useless for
// the thing it exists for. Under fault injection a crash is the finding, so
// the distinction is the point rather than a detail: killed is charpy's doing,
// anything else is the subject's. The distinction belongs in the transcript
// and not in this function's error, because the oracle judges runs and this
// does not.
func (s *Shim) stop(ctx context.Context) error {
	err := s.cmd.Wait()
	st := s.cmd.ProcessState

	detail := map[string]any{"exit_code": st.ExitCode()}
	killed := ctx.Err() != nil
	detail["killed_by_charpy"] = killed
	if sig, ok := signalOf(st); ok {
		detail["signal"] = sig
	}
	s.x.Event(transcript.SubjectExit, detail)

	// A non-zero exit is not a harness error, however it happened. A subject
	// that died because charpy corrupted its input is the finding, and the
	// transcript now carries what it needs to say so -- the code, the signal,
	// and whether charpy was the one who killed it. Returning an error here
	// would make the driver a second judge with a worse view than the oracle,
	// and would fail runs whose whole point was to provoke this.
	//
	// What is left to report is charpy's own failure to wait on a process it
	// spawned, which is a harness bug and not an outcome.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// signalOf reports the signal a subject died on, where the platform says so.
func signalOf(st *os.ProcessState) (string, bool) {
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", false
	}
	return ws.Signal().String(), true
}

// relay reads frames from one side and puts them on the other, offering each
// to the matcher on the way.
func (s *Shim) relay(from io.Reader, to *wire.Stdio, dir transcript.Direction) {
	sc := bufio.NewScanner(from)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		m, _ := envelope.Parse(raw)

		prior, _ := s.x.PriorResolved()
		s.x.Observe(m, dir)

		if cases := s.match.Select(s.x.FrameOf(m, dir)); len(cases) > 0 {
			// A frame may match several cases. Arbitrating between two
			// faults that want the same frame is not the matcher's job
			// and is not obviously charpy's either, so the first wins and
			// the rest are recorded as scheduled but not applied.
			s.applyFault(cases[0], m, dir, to, prior)
			continue
		}

		s.deliver(m, dir, to, nil)
	}
}

// applyFault carries out a plan.
func (s *Shim) applyFault(c interpose.Case, m envelope.Message, dir transcript.Direction, to *wire.Stdio, prior envelope.ID) {
	s.x.FaultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{RunSeed: s.o.RunSeed, Resolved: prior})
	if err != nil {
		// Not applicable is an outcome, not a failure: the case does not
		// apply to this frame, and the frame goes on untouched rather than
		// being quietly given a weaker fault.
		s.x.Note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		s.deliver(m, dir, to, nil)
		return
	}

	f := s.x.FrameOf(m, dir)
	att := c.TranscriptFault()

	for _, extra := range plan.Before {
		s.inter.Synthesize(f, c, extra)
		s.deliver(extra, dir, to, att)
	}

	switch {
	case plan.Swallow:
		s.inter.Swallow(f, c, m)
	case plan.Hold != nil:
		held := s.inter.Withhold(f, c, m, plan.Hold.For)
		go s.release(held, plan.Hold, dir, to, att)
	case plan.Deliver != nil:
		s.inter.Rewrite(f, c, m, *plan.Deliver)
		s.deliverCut(*plan.Deliver, dir, to, att, plan.Cut)
	}

	for _, extra := range plan.After {
		s.inter.Synthesize(f, c, extra)
		s.deliver(extra, dir, to, att)
	}

	s.x.FaultEvent(transcript.FaultApplied, c, map[string]any{"verb": string(plan.Verb())})

	switch plan.Then {
	case fault.StreamClose:
		s.x.StreamClose(transcript.CharpyClose, int(to.Written()))
		_ = to.Close()
	case fault.StreamStall:
		// Stalling is the absence of further frames, not the absence of a
		// call: the stream stays open and nothing else arrives on it.
		if _, err := wire.Stall(to, wire.StallOptions{Keepalive: plan.Keepalive}); err != nil {
			s.x.Note(fmt.Sprintf("case %s could not stall: %v", c.ID, err))
		}
	}
}

// release finishes a hold when its withdrawal comes due.
//
// The withdrawal instant is where the liveness clock starts, so it is recorded
// before anything else happens, and attribution ends there too: from here the
// subject is answering for itself again.
func (s *Shim) release(held *interpose.Withheld, hold *fault.Hold, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault) {
	<-held.Withdrawn()

	c := held.Case()
	s.x.FaultEvent(transcript.FaultWithdrawn, c, map[string]any{
		"at_mono_ns": int64(held.WithdrawnAt()),
		"then":       hold.Then,
	})
	s.o.Ledger.EndConsequences(s.o.Face, s.o.ConnID)

	switch hold.Then {
	case "deliver":
		s.deliver(held.Message(), dir, to, att)
	case "close":
		s.x.StreamClose(transcript.CharpyClose, int(to.Written()))
		_ = to.Close()
	case "error":
		m, err := interpose.TemplateError(held.Message().ID, -32603, "charpy: request withdrawn")
		if err != nil {
			s.x.Note(fmt.Sprintf("case %s could not build its withdrawal error: %v", c.ID, err))
			return
		}
		s.deliver(m, dir, to, att)
	default:
		s.x.Note(fmt.Sprintf("case %s: unknown withdrawal %q; the frame stays withheld", c.ID, hold.Then))
	}
}

func (s *Shim) deliver(m envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault) {
	s.deliverCut(m, dir, to, att, nil)
}

func (s *Shim) deliverCut(m envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault, cut *fault.Cut) {
	enc := wire.EncodeLine(m.Raw())

	n := enc.Len()
	if cut != nil {
		at, err := enc.Cut(cut.At, cut.Opts)
		if err != nil {
			// A cut point that cannot land is a case that does not apply, so
			// the frame goes out whole and the transcript says why.
			s.x.Note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
		} else {
			n = at
		}
	}

	written, err := wire.EmitCut(to, enc, n)
	if err != nil {
		s.x.Note(fmt.Sprintf("write failed after %d bytes: %v", written, err))
	}

	// Both directions cross the subject's one face here: charpy sends to it
	// and it answers back, and the connection to the other peer carries the
	// same frames rather than different ones. A two-faced subject -- a
	// gateway -- is what would make this a question, and that is the proxy
	// driver's problem rather than the shim's.
	s.x.Frame(dir, enc.Bytes[:n], att, nil)
}
