package stdio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

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

	Cases []interpose.Case
	// Arm, instead of Cases, defers arming until the subject's handshake
	// settles the revision, and is asked for the cases compiled against it.
	// Frames before that cross unfaulted: until the server answers, charpy
	// does not know which cases apply or which revision a citation would
	// name, and a fault it could not cite correctly is one it should not
	// inject.
	Arm func(revision.Revision) []interpose.Case

	// AnswerDestroyed has charpy answer its own peer when a fault leaves the
	// peer's request without the id it carried. The subject never received
	// that request, so it will never answer it, and a peer left waiting on it
	// sits out the script's deadline and never asks the question after the
	// fault that the reaction layer judges. Owned stimulus only: under a relay
	// the client is somebody else's, and how it copes is its own business.
	AnswerDestroyed bool

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
	// writeMu serialises every write the shim makes, and the record of it.
	// Each pipe has more than one writer: its relay, a held frame released
	// on the hold's own goroutine, and toward the peer an answer to a request
	// charpy destroyed, written from the other relay.
	writeMu sync.Mutex

	// match is swapped once, when an Arm run settles, from the goroutine
	// relaying the handshake answer while the other relay reads it.
	match atomic.Pointer[interpose.Matcher]

	cmd     *exec.Cmd
	toSubj  *wire.Stdio
	toPeer  *wire.Stdio
	subjOut io.Reader

	// faulting is held while a fault is being applied, up to and including
	// its fault_applied line, so that [Shim.Askable] cannot answer while one
	// is half done. The faulted frame is delivered before that line is
	// written, so the peer can see the fault and move on before the shim has
	// recorded it; without the lock, a follow-up asked in that gap would
	// cross before the fault it follows.
	faulting sync.Mutex
	applied  int
	broken   bool

	// inputClosing is set when charpy is about to close the shim's input
	// itself, so the read error that follows is charpy's teardown.
	inputClosing atomic.Bool
}

// New prepares a shim. Nothing is spawned until Run.
func New(o Options) (*Shim, error) {
	if len(o.Command) == 0 {
		return nil, errors.New("stdio: no subject command")
	}
	if o.Arm != nil && len(o.Cases) > 0 {
		return nil, errors.New("stdio: Cases and Arm both set; a run arms once")
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

	run := &exchange.Run{Ledger: o.Ledger, Transcript: o.Transcript, Cases: o.Cases}
	// One face here: a server subject has no second face to join to, so a
	// forwarded id would be a join to nothing.
	face := run.Face(o.Face, transcript.TransportStdio,
		func(envelope.Message) transcript.Link { return transcript.Link{Via: transcript.ViaNone} })
	s := &Shim{
		o:     o,
		x:     face.Conn(o.ClientID, o.SessionID, o.ConnID),
		inter: interpose.New(o.Ledger, o.Sched),
	}
	s.match.Store(interpose.NewMatcher(o.Ledger, o.Cases...))
	if o.Arm != nil {
		run.Settle = func(r revision.Revision) []interpose.Case {
			cases := o.Arm(r)
			s.match.Store(interpose.NewMatcher(o.Ledger, cases...))
			return cases
		}
	}
	return s, nil
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
		s.relay(s.o.In, s.toSubj, transcript.C2S, func() bool {
			return s.inputClosing.Load() || ctx.Err() != nil
		})
		// The client stopped talking; let the subject see the end of its
		// input rather than hanging on a pipe nobody will write to.
		_ = s.toSubj.Close()
	}()

	go func() {
		defer wg.Done()
		// Once the run's context is done, exec has killed the subject, and
		// the end of its output is charpy's doing.
		s.relay(s.subjOut, s.toPeer, transcript.S2C, func() bool { return ctx.Err() != nil })
		// The subject stopped talking -- it exited, or closed its output. Let
		// the peer see the end of its input too: a request it is waiting on
		// will never be answered, and a peer left waiting sits out its whole
		// deadline to learn what the pipe already says.
		s.writeMu.Lock()
		_ = s.toPeer.Close()
		s.writeMu.Unlock()
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
// to the matcher on the way. ours reports whether charpy is the one ending the
// read, which ended uses to record how the relay ended.
func (s *Shim) relay(from io.Reader, to *wire.Stdio, dir transcript.Direction, ours func() bool) {
	sc := wire.NewLineScanner(from)
	defer s.ended(sc, from, dir, to, ours)

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		m, _ := envelope.Parse(raw)

		prior, _ := s.x.PriorResolved(dir)
		s.x.Observe(m, dir)

		if s.x.FollowUp(m, dir) {
			s.deliver(m, dir, to, nil)
			continue
		}
		if cases := s.match.Load().Select(s.x.FrameOf(m, dir)); len(cases) > 0 {
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

// ClosingInput tells the shim that charpy is about to close the shim's input
// itself, as the scripted driver does with its peer's pipe at the end of a
// run. The read error that follows is charpy's teardown, not the peer's.
func (s *Shim) ClosingInput() { s.inputClosing.Store(true) }

// ended records how a relay's read ended, so that no relay ends silently:
//
//   - A frame past wire.MaxFrame: frame_capped, and the stream closed with
//     reason frame_cap (below).
//   - charpy ending the run: nothing more. subject_exit, or the script, says
//     how the run ended, and a read that ended with it is no event of its own.
//   - A read error: the stream closed with reason error and the error's text.
//     Whose doing it was is what the error cannot say.
//   - The sender's end of stream: the stream closed by the sender, unless
//     charpy already closed it -- a fault's then = close -- and recorded that.
//
// After a cap or an error the stream is broken: the caller closes it, and
// there is no session left on it to ask.
//
// On a cap, charpy stops relaying but not reading: the rest of from is drained and
// dropped. Closing the pipe would SIGPIPE a sender still writing, and the exit
// would be charpy's doing read as the subject's. Leaving it unread would block
// a sender whose oversized line does end, so it could never reach the end of
// its input and exit on its own. Drained, a subject that stops exits as
// itself, and one that never stops ends at the run's deadline, killed by
// charpy and recorded so.
func (s *Shim) ended(sc *wire.LineScanner, from io.Reader, dir transcript.Direction, to *wire.Stdio, ours func() bool) {
	err := sc.Err()
	var tl *wire.FrameTooLarge
	switch {
	case errors.As(err, &tl):
		s.markBroken()
		s.x.Event(transcript.FrameCapped, transcript.CappedDetail(dir, tl.Read, wire.MaxFrame, tl.Prefix))
		s.x.StreamClose(transcript.FrameCap, int(to.Written()))
		go func() { _, _ = io.Copy(io.Discard, from) }()
	case ours():
	case err != nil:
		s.markBroken()
		s.x.Event(transcript.StreamClose, transcript.CloseErrorDetail(int(to.Written()), err))
	case !to.Closed():
		// The subject is the server: what it sends is s2c.
		reason := transcript.PeerClose
		if dir == transcript.S2C {
			reason = transcript.SubjectClose
		}
		s.x.StreamClose(reason, int(to.Written()))
	}
}

func (s *Shim) markBroken() {
	s.faulting.Lock()
	s.broken = true
	s.faulting.Unlock()
}

// Askable reports whether a follow-up question can be put to the subject on
// this connection: a fault has acted, and charpy did not end or stall the
// stream in doing it. A closed or stalled pipe has no session left to ask on,
// and the fresh-session recovery probe is what covers that case.
func (s *Shim) Askable() bool {
	s.faulting.Lock()
	defer s.faulting.Unlock()
	return s.applied > 0 && !s.broken
}

// Ask marks the next request crossing toward the subject as charpy's
// follow-up, keeping it and its answer out of the armed case's reach. The
// returned func withdraws the mark if the question never crossed.
func (s *Shim) Ask() (withdraw func()) { return s.x.Ask(transcript.C2S) }

// applyFault carries out a plan.
func (s *Shim) applyFault(c interpose.Case, m envelope.Message, dir transcript.Direction, to *wire.Stdio, prior envelope.ID) {
	s.faulting.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			s.faulting.Unlock()
		}
	}
	defer unlock()

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
		s.deliverCut(*plan.Deliver, &m, dir, to, c.Rewrote(m, *plan.Deliver), plan.Cut)
	}

	for _, extra := range plan.After {
		s.inter.Synthesize(f, c, extra)
		s.deliver(extra, dir, to, att)
	}

	s.x.FaultEvent(transcript.FaultApplied, c, transcript.AppliedDetail(string(plan.Verb()), dir))
	s.applied++
	if plan.Then == fault.StreamClose || plan.Then == fault.StreamStall {
		s.broken = true
	}
	// A stall blocks here for as long as it lasts, and Askable must not wait
	// on it: the fault is recorded, which is all the lock protects.
	unlock()

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
		s.faulting.Lock()
		s.broken = true
		s.faulting.Unlock()
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
	s.deliverCut(m, nil, dir, to, att, nil)
}

func (s *Shim) deliverCut(m envelope.Message, orig *envelope.Message, dir transcript.Direction, to *wire.Stdio, att *transcript.Fault, cut *fault.Cut) {
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
	// orig is the frame the case matched, as the subject or the peer wrote
	// it; a synthesized frame has none, and stays charpy's whatever crossed.
	if orig != nil {
		att = interpose.Crossed(att, *orig, m, n == enc.Len())
	}

	// Both directions cross the subject's one face here: charpy sends to it
	// and it answers back, and the connection to the other peer carries the
	// same frames rather than different ones. A two-faced subject -- a
	// gateway -- is what would make this a question, and that is the proxy
	// driver's problem rather than the shim's.
	//
	// It is recorded before it is written, not after. Once the bytes are out
	// the other peer can answer them, and its answer, relayed the other way,
	// would otherwise be free to take the earlier seq -- an answer before its
	// question, which the reaction layer would read as a request never
	// answered. The bytes recorded are the same either way: n is settled
	// before the write, and a short write is noted rather than re-recorded.
	s.writeMu.Lock()
	s.x.Frame(dir, enc.Bytes[:n], att, nil)
	written, err := wire.EmitCut(to, enc, n)
	s.writeMu.Unlock()
	if err != nil {
		s.x.Note(fmt.Sprintf("write failed after %d bytes: %v", written, err))
	}

	if orig != nil && to == s.toSubj {
		s.answerDestroyed(*orig, dir, att)
	}
}

// answerDestroyed answers the peer's request itself when the fault left the
// request without its id, so the peer can go on to ask the next question.
//
// The answer is charpy's and is attributed to the case, so no layer reads it
// as the subject's: the subject never saw the request it answers. It is an
// error rather than a result because nothing answered the question, and its
// message says so for whoever reads the transcript.
func (s *Shim) answerDestroyed(req envelope.Message, dir transcript.Direction, att *transcript.Fault) {
	if !s.o.AnswerDestroyed || att == nil || req.Kind != envelope.KindRequest ||
		!att.Replaced.Present() || att.ReplacedKind != envelope.KindRequest {
		return
	}
	back := transcript.S2C
	if dir == transcript.S2C {
		back = transcript.C2S
	}
	ans, err := envelope.NewError(req.ID, destroyedCode,
		"charpy destroyed this request in transit ("+att.Citation+"); the subject never received it", nil)
	if err != nil {
		s.x.Note(fmt.Sprintf("answering a destroyed request: %v", err))
		return
	}
	s.x.Observe(ans, back)
	s.deliver(ans, back, s.toPeer, charpys(att))
}

// charpys is the case's attribution for a frame charpy wrote in its own
// right: it replaced nothing, so it names nothing it replaced.
func charpys(att *transcript.Fault) *transcript.Fault {
	out := *att
	out.Replaced, out.ReplacedKind = envelope.ID{}, ""
	return &out
}

// destroyedCode is the JSON-RPC error charpy answers its own peer with for a
// request it destroyed: implementation-defined server-error range, and not one
// MCP assigns.
const destroyedCode = -32099
