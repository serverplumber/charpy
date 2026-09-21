package stdio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
)

// ScriptOptions configures a scripted run. It is Options without the two
// fields the peer supplies: In and Out are the peer's pipes, not the caller's.
type ScriptOptions struct {
	Options

	// Case is the one case armed for this run. One, not a set: a transcript
	// carrying every fault at once is a puzzle rather than a finding, and
	// nothing else armed is also what makes case identity reproduce exactly
	// (ADR-012).
	Case interpose.Case

	// Era is the revision to ask the subject for. Empty takes the SDK's
	// latest, which is the stateless one.
	Era revision.Revision

	// Scenario overrides the script. Nil derives it from the case, which is
	// the normal path and the one that cannot drift from the matcher.
	Scenario scenario.Scenario

	// Stimulus shapes the derived script: which tool it calls and with what
	// arguments. Zero takes the first tool the subject lists, which is what a
	// catalogue case means by "a tools/call" -- fine against a fixture, and
	// not against a subject whose first tool does something.
	Stimulus scenario.Options

	// Timeout bounds the script. Zero takes DefaultTimeout.
	//
	// It is not belt and braces. A fault that kills the subject's parser
	// leaves it unable to answer anything, so a script waiting on a reply
	// waits forever -- which is the fault working, and a run that hangs
	// reports nothing about it. The bound turns a wedged subject into a
	// finished transcript the oracle can read.
	Timeout time.Duration
}

// DefaultTimeout bounds a scripted run that the subject has stopped answering.
//
// Thirty seconds because that is what the catalogue's own
// liveness_probe_within_ms budgets use for a subject expected to recover, so a
// script still waiting past it is waiting on something that is not coming.
const DefaultTimeout = 30 * time.Second

// Script is owned stimulus over stdio: charpy spawns the subject, drives it
// with the reference peer, and corrupts what crosses in between.
//
// It is the relay [Shim] with charpy's own peer on the near side instead of
// somebody else's client, which is not a coincidence -- the interposer
// originates nothing and does not care where a frame came from
// (docs/design/interposer.md section 1), so the two modes differ in their
// stimulus and in nothing else. Shim already reads frames from an io.Reader
// and answers to an io.WriteCloser; peer.Client is exactly a pair of those.
//
// What does differ is how a run ends. A relayed run has no last frame and
// stops on a signal; a scripted one stops when the script does, which is why
// this is the mode that can exit 0 with a verdict rather than with a summary.
type Script struct {
	o    ScriptOptions
	peer *peer.Client
	shim *Shim
	run  scenario.Scenario
}

// NewScript prepares a scripted run. Nothing is spawned and nothing connects
// until Run.
func NewScript(o ScriptOptions) (*Script, error) {
	if o.Case.ID == "" {
		return nil, errors.New("stdio: a scripted run needs a case to arm")
	}
	// A compiled case always names a mechanism; one that does not is a
	// hand-built case, and arming it produces a fault attribution the
	// transcript will refuse to write. Refusing here says which case.
	if o.Case.Fault.Kind == "" {
		return nil, fmt.Errorf("stdio: case %s names no fault mechanism", o.Case.ID)
	}

	p, err := peer.NewClient(peer.Options{Era: o.Era})
	if err != nil {
		return nil, err
	}

	run := o.Scenario
	if run == nil {
		run, err = scenario.Basic(o.Case.Match, o.Stimulus)
		if err != nil {
			return nil, err
		}
	}

	// The peer's frames arrive on its Out and charpy answers into its In,
	// which is what the shim's In and Out mean from the other side.
	inner := o.Options
	inner.In, inner.Out = p.Out, p.In
	inner.Cases = []interpose.Case{o.Case}

	shim, err := New(inner)
	if err != nil {
		return nil, err
	}

	return &Script{o: o, peer: p, shim: shim, run: run}, nil
}

// Run spawns the subject, connects the peer and plays the script.
//
// The ordering is forced rather than chosen. The shim must be relaying before
// the peer connects, because Connect blocks on a handshake that crosses the
// shim; the peer must be closed before the run can be waited on, because the
// relays end when the pipes do. Getting either backwards deadlocks, so both
// are here rather than left to a caller to rediscover.
//
// The script's context is not the subject's. The shim keeps the caller's,
// because that one owns the process: cancelling it means shut down, and
// exec.CommandContext kills the subject on it. The script gets the
// interruptible one, so finishing a script -- or withdrawing a fault -- ends
// the talking without killing what charpy was talking to. Sharing them makes
// every clean ending look like charpy shot the subject, which is exactly the
// distinction subject_exit exists to draw.
func (s *Script) Run(ctx context.Context) error {
	relayed := make(chan error, 1)
	go func() { relayed <- s.shim.Run(ctx) }()

	sctx, halt := scenario.Interruptible(ctx)

	timeout := s.o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	expiry := time.AfterFunc(timeout, halt.Expired)
	defer expiry.Stop()

	sess, err := s.peer.Connect(sctx)
	if err != nil {
		_ = s.peer.Close()
		<-relayed
		// A fault that lands on the handshake breaks the peer's own Connect,
		// and that is the fault working rather than the run failing: a
		// malformed initialize result leaves the peer waiting until the script
		// deadline names why. The transcript already carries the corruption
		// and the oracle judges it, so a named halt ends the run the same way
		// a broken call does once the session is up.
		if why := scenario.Why(sctx); cleanEnd(why) && why != nil {
			return nil
		}
		return fmt.Errorf("stdio: %w", err)
	}

	scriptErr := s.run(sctx, sess)
	halt.Done()

	// Closing the session first lets the peer say goodbye over a wire that is
	// still up; closing the pipes is what ends the relays.
	_ = sess.Close()
	_ = s.peer.Close()

	relayErr := <-relayed

	// A halt is not a failure. A withdrawn fault and a finished script are
	// both ordinary endings, and only a signal or a real error is not.
	if !cleanEnd(scriptErr) {
		return scriptErr
	}
	return relayErr
}

// cleanEnd reports whether a scenario's error is an ordinary ending rather
// than a failure. A withdrawn fault, a finished script, a subject that stopped
// answering, and a call the wire broke under are all endings the transcript
// carries and the oracle judges; returning an error for any of them would make
// the driver a second judge with a worse view.
func cleanEnd(err error) bool {
	return err == nil ||
		errors.Is(err, scenario.ErrScriptDone) ||
		errors.Is(err, scenario.ErrWithdrawn) ||
		errors.Is(err, scenario.ErrDeadline) ||
		errors.Is(err, scenario.ErrStimulusInterrupted)
}
