package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
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
	inter *interpose.Interposer
	match *interpose.Matcher

	cmd     *exec.Cmd
	toSubj  *wire.Stdio
	toPeer  *wire.Stdio
	subjOut io.Reader

	mu     sync.Mutex
	header bool
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

	return &Shim{
		o:     o,
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

	// Toward the subject: this is the direction faults are injected on,
	// because this is what the subject has to survive.
	go func() {
		defer wg.Done()
		s.relay(s.o.In, s.toSubj, transcript.C2S, true)
		// The client stopped talking; let the subject see the end of its
		// input rather than hanging on a pipe nobody will write to.
		_ = s.toSubj.Close()
	}()

	// Away from the subject: relayed untouched. Corrupting this direction
	// would be testing the client, which is a different run with a different
	// subject class.
	go func() {
		defer wg.Done()
		s.relay(s.subjOut, s.toPeer, transcript.S2C, false)
	}()

	wg.Wait()
	return s.stop()
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

	s.event(transcript.ConnOpen, map[string]any{"command": s.o.Command[0]})
	return nil
}

func (s *Shim) stop() error {
	err := s.cmd.Wait()
	code := s.cmd.ProcessState.ExitCode()
	s.event(transcript.SubjectExit, map[string]any{"exit_code": code})

	// A subject killed by charpy's own shutdown is not a subject that failed.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// relay reads frames from one side and puts them on the other.
func (s *Shim) relay(from io.Reader, to *wire.Stdio, dir transcript.Direction, faulted bool) {
	sc := bufio.NewScanner(from)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)

	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		m, _ := envelope.Parse(raw)

		s.observe(m, dir)

		if faulted {
			if cases := s.match.Select(s.frameOf(m, dir)); len(cases) > 0 {
				// A frame may match several cases. Arbitrating between two
				// faults that want the same frame is not the matcher's job
				// and is not obviously charpy's either, so the first wins and
				// the rest are recorded as scheduled but not applied.
				s.applyFault(cases[0], m, dir, to)
				continue
			}
		}

		s.deliver(m, dir, to, nil)
	}
}

// observe keeps the ledger current and settles the negotiated revision.
func (s *Shim) observe(m envelope.Message, dir transcript.Direction) {
	switch {
	case m.Kind == envelope.KindRequest && dir == transcript.C2S:
		s.o.Ledger.Originate(s.o.Face, s.o.ConnID, interpose.Exchange{
			IntentID: m.ID, Method: m.Method,
		})
	case m.Kind == envelope.KindResponse && dir == transcript.S2C:
		s.o.Ledger.Resolve(s.o.Face, s.o.ConnID, m.ID)
		s.settleRevision(m)
	}
}

// settleRevision writes the transcript header once the initialize exchange
// says what the subject speaks.
//
// Under the conformance precondition charpy is usually told the revision
// rather than discovering it, and this is the narrow case where it is not:
// relayed traffic charpy did not originate. The transcript writer holds
// everything until the header lands, which is why frames can be captured
// before this happens.
func (s *Shim) settleRevision(m envelope.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header {
		return
	}

	var probe struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(m.Result, &probe) != nil || probe.ProtocolVersion == "" {
		return
	}
	r := revision.Revision(probe.ProtocolVersion)
	if !revision.Known(r) {
		return
	}

	s.header = true
	_ = s.o.Transcript.WriteHeader(transcript.Header{
		Revision: &transcript.Negotiation{Negotiated: r, How: transcript.HowInitialize},
	})
}

// applyFault carries out a plan.
func (s *Shim) applyFault(c interpose.Case, m envelope.Message, dir transcript.Direction, to *wire.Stdio) {
	s.faultEvent(transcript.FaultScheduled, c, nil)

	plan, err := fault.Apply(c, m, fault.Context{RunSeed: s.o.RunSeed})
	if err != nil {
		// Not applicable is an outcome, not a failure: the case does not
		// apply to this frame, and the frame goes on untouched rather than
		// being quietly given a weaker fault.
		s.note(fmt.Sprintf("case %s not applied: %v", c.ID, err))
		s.deliver(m, dir, to, nil)
		return
	}

	f := s.frameOf(m, dir)
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

	s.faultEvent(transcript.FaultApplied, c, map[string]any{"verb": string(plan.Verb())})

	switch plan.Then {
	case fault.StreamClose:
		s.streamClose(transcript.CharpyClose, to)
		_ = to.Close()
	case fault.StreamStall:
		// Stalling is the absence of further frames, not the absence of a
		// call: the stream stays open and nothing else arrives on it.
		if _, err := wire.Stall(to, wire.StallOptions{Keepalive: plan.Keepalive}); err != nil {
			s.note(fmt.Sprintf("case %s could not stall: %v", c.ID, err))
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
	s.faultEvent(transcript.FaultWithdrawn, c, map[string]any{
		"at_mono_ns": int64(held.WithdrawnAt()),
		"then":       hold.Then,
	})
	s.o.Ledger.EndConsequences(s.o.Face, s.o.ConnID)

	switch hold.Then {
	case "deliver":
		s.deliver(held.Message(), dir, to, att)
	case "close":
		s.streamClose(transcript.CharpyClose, to)
		_ = to.Close()
	case "error":
		m, err := interpose.TemplateError(held.Message().ID, -32603, "charpy: request withdrawn")
		if err != nil {
			s.note(fmt.Sprintf("case %s could not build its withdrawal error: %v", c.ID, err))
			return
		}
		s.deliver(m, dir, to, att)
	default:
		s.note(fmt.Sprintf("case %s: unknown withdrawal %q; the frame stays withheld", c.ID, hold.Then))
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
			s.note(fmt.Sprintf("cut %s did not apply: %v", cut.At, err))
		} else {
			n = at
		}
	}

	written, err := wire.EmitCut(to, enc, n)
	if err != nil {
		s.note(fmt.Sprintf("write failed after %d bytes: %v", written, err))
	}

	// Both directions cross the subject's one face here: charpy sends to it
	// and it answers back, and the connection to the other peer carries the
	// same frames rather than different ones. A two-faced subject -- a
	// gateway -- is what would make this a question, and that is the proxy
	// driver's problem rather than the shim's.
	s.frame(dir, enc.Bytes[:n], att)
}

func (s *Shim) frameOf(m envelope.Message, dir transcript.Direction) interpose.Frame {
	return interpose.Frame{
		Face:      s.o.Face,
		Direction: dir,
		Kind:      m.Kind,
		Method:    m.Method,
		ID:        m.ID,
		ClientID:  s.o.ClientID,
		SessionID: s.o.SessionID,
		ConnID:    s.o.ConnID,
	}
}

func (s *Shim) frame(dir transcript.Direction, raw []byte, att *transcript.Fault) {
	// What is recorded is what crossed, parsed from the bytes that actually
	// went out. After a cut that is the truncated frame, which will not have
	// an envelope -- and a truncated frame with no envelope is precisely what
	// the transcript's malformed kind exists to carry.
	crossed, _ := envelope.Parse(raw)

	// A response carries no method, so the only way one reaches the column is
	// the ledger resolving its id. That is job 2, and this is what it is for.
	var method string
	switch crossed.Kind {
	case envelope.KindResponse, envelope.KindError:
		method, _ = s.o.Ledger.MethodFor(s.o.Face, s.o.ConnID, crossed.ID)
	}

	s.o.Transcript.Frame(transcript.Frame{
		Face: s.o.Face, Direction: dir, Transport: transcript.TransportStdio,
		ClientID: s.o.ClientID, SessionID: s.o.SessionID, ConnID: s.o.ConnID,
		Message: crossed,
		Method:  method,
		// A single-faced subject has no second face to join to, so there is
		// no correlation to claim. forwarded stamps one id across the two
		// faces of a gateway; here it would be a join to nothing.
		Link:  transcript.Link{Via: transcript.ViaNone},
		Fault: att,
	})
}

func (s *Shim) event(kind transcript.EventKind, detail map[string]any) {
	s.o.Transcript.Event(transcript.Event{
		Kind: kind, Face: s.o.Face, Transport: transcript.TransportStdio,
		ClientID: s.o.ClientID, SessionID: s.o.SessionID, ConnID: s.o.ConnID,
		Detail: detail,
	})
}

func (s *Shim) faultEvent(kind transcript.EventKind, c interpose.Case, detail map[string]any) {
	s.o.Transcript.Event(transcript.Event{
		Kind: kind, Face: s.o.Face, Transport: transcript.TransportStdio,
		ClientID: s.o.ClientID, SessionID: s.o.SessionID, ConnID: s.o.ConnID,
		Detail: detail, Fault: c.TranscriptFault(),
	})
}

func (s *Shim) streamClose(reason transcript.CloseReason, to *wire.Stdio) {
	s.o.Transcript.Event(transcript.Event{
		Kind: transcript.StreamClose, Face: s.o.Face, Transport: transcript.TransportStdio,
		ClientID: s.o.ClientID, SessionID: s.o.SessionID, ConnID: s.o.ConnID,
		Detail: transcript.CloseDetail(reason, int(to.Written())),
	})
}

func (s *Shim) note(text string) {
	s.o.Transcript.Event(transcript.Event{
		Kind: transcript.Note, Face: s.o.Face, Transport: transcript.TransportStdio,
		ClientID: s.o.ClientID, SessionID: s.o.SessionID, ConnID: s.o.ConnID,
		Detail: map[string]any{"harness": text},
	})
}
