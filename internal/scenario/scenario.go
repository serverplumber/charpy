package scenario

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// A Scenario originates the traffic one case needs. It is a function, not an
// interface: a scenario has one job, does it once, and stops.
//
// It returns nil when the script ran to its last step, and the cause from
// [Why] when it was halted -- a withdrawal is not a failure, and neither is a
// signal, so callers check with [Stopped] rather than treating any non-nil
// error as a problem.
type Scenario func(ctx context.Context, sess *mcp.ClientSession) error

// ErrCannotOriginate is returned when a case's matcher selects on traffic no
// scenario in this package knows how to produce.
//
// It names the method rather than silently running a script the fault can
// never attach to, which would report UNTRIGGERED and look like a subject
// that behaved.
var ErrCannotOriginate = errors.New("scenario: nothing here originates that method")

// ErrStimulusInterrupted wraps a call that did not complete because the wire
// broke under it -- which, under a fault, is the fault working.
//
// A truncated response makes the peer's own call return an error, and that is
// a datum, not a harness failure: the transcript already records the cut and
// the stream close, and the oracle judges the subject's recovery from there.
// A driver treats this as an ordinary end of stimulus rather than a reason to
// fail the run. It is distinct from a halt, which the run asked for.
var ErrStimulusInterrupted = errors.New("scenario: stimulus interrupted")

// Options configures [Basic].
type Options struct {
	// Tool is the tool to call. Empty takes the first the subject lists,
	// which is what a catalogue case means by "a tools/call".
	Tool string

	// Arguments are passed to every call. Empty sends none.
	//
	// They are constant across calls, and deliberately not seeded. Distinct
	// per-call arguments exist to give the ledger a content join across two
	// faces (interposer.md section 6), there is no second face until the
	// gateway drivers land, and doing it properly means generating values a
	// tool's own inputSchema accepts rather than guessing at one. Faking it
	// here would perturb subjects for a join nothing reads yet.
	Arguments map[string]any

	// Repeats overrides the count derived from the case's matcher. Zero
	// derives it, which is the normal path.
	Repeats int
}

// Basic is the scenario every v0 server case runs: list the tools, then call
// one of them enough times for the case's matcher to reach it.
//
// It is the only scenario, so it is a function rather than an entry in a
// registry. The cases that need a different shape -- the three matching on
// face = "upstream" -- cannot run until the gateway drivers exist, and a
// registry holding one scenario is ceremony around a function call. Add one
// when there are two.
//
// The script itself draws no randomness. Its two variables come from the
// compiled case: which method to originate, and how many times. Every other
// choice is fixed, which is reproducibility in its strongest form rather than
// its seeded one -- and inventing a seeded draw nothing consumes would make
// the archived-citation contract in case-identity.md section 5 harder to keep
// for no gain.
func Basic(m interpose.Match, o Options) (Scenario, error) {
	method := m.Method.String()
	switch method {
	case "", "*", "tools/call":
	default:
		return nil, fmt.Errorf("%w: %s", ErrCannotOriginate, method)
	}

	repeats := o.Repeats
	if repeats == 0 {
		repeats = Repeats(m)
	}

	return func(ctx context.Context, sess *mcp.ClientSession) error {
		tool := o.Tool
		if tool == "" {
			listed, err := sess.ListTools(ctx, nil)
			if err != nil {
				return halted(ctx, fmt.Errorf("scenario: listing tools: %w", err))
			}
			if len(listed.Tools) == 0 {
				return errors.New("scenario: the subject lists no tools to call")
			}
			tool = listed.Tools[0].Name
		}

		for i := range repeats {
			params := &mcp.CallToolParams{Name: tool, Arguments: o.Arguments}
			if _, err := sess.CallTool(ctx, params); err != nil {
				// A halt beat the call: report the named reason.
				if why := Why(ctx); why != nil {
					return why
				}
				// A tool that answers with an error still produced the frame
				// the matcher wanted, so IsError is not seen here -- the SDK
				// packs it into a successful result. What reaches here is the
				// wire breaking under the call, which under a fault is the
				// fault working: end the stimulus, do not fail the run.
				return fmt.Errorf("%w: call %d of %d: %v", ErrStimulusInterrupted, i+1, repeats, err)
			}
		}
		return nil
	}, nil
}

// ProbeResult is the outcome of a liveness probe: which method was sent, how
// it fared, and how long it took.
type ProbeResult struct {
	Method  string
	Outcome transcript.ProbeOutcome
	Elapsed time.Duration
}

// Probe asks whether the subject is serving again after a fault acted on it.
// It sends the revision-appropriate probe on the session and reports the
// outcome; the caller bounds it with the case's budget via ctx, and opens a
// fresh session so the probe measures the subject resuming service rather than
// a wedged session surviving.
//
// ping through 2025-11-25; tools/list from 2026-07-28, where ping is gone and
// server/discover is not a method the client can re-issue after connecting
// (docs/design/oracle.md section 6). tools/list is heavier, which the report
// notes so a slow recovery is not misread as a slow probe.
func Probe(ctx context.Context, sess *mcp.ClientSession, era revision.Revision) ProbeResult {
	method := ProbeMethod(era)
	start := time.Now()
	var err error
	switch method {
	case "ping":
		err = sess.Ping(ctx, nil)
	default:
		_, err = sess.ListTools(ctx, nil)
	}
	return ProbeResult{Method: method, Outcome: Classify(ctx, err), Elapsed: time.Since(start)}
}

// ProbeMethod is the liveness probe for an era: ping through 2025-11-25,
// tools/list from 2026-07-28 (ping removed, server/discover not re-issuable).
func ProbeMethod(era revision.Revision) string {
	if era >= revision.V20260728 {
		return "tools/list"
	}
	return "ping"
}

// Classify turns a probe's error into an outcome: none is recovery, a deadline
// is a subject that did not recover in the budget, anything else an error --
// including a fresh connection the subject would not even accept.
func Classify(ctx context.Context, err error) transcript.ProbeOutcome {
	switch {
	case err == nil:
		return transcript.ProbeOK
	case ctx.Err() == context.DeadlineExceeded:
		return transcript.ProbeTimeout
	default:
		return transcript.ProbeError
	}
}

// Repeats is how many matching frames a case needs before its fault can
// attach, read off the compiled matcher.
//
// An ordinal of n needs n frames to reach the nth. Every-nth needs n to reach
// the first multiple. Neither means the first frame matches, so one will do.
// Nothing here pads the count "to be safe": a scenario that sends more than
// the case needs is a scenario whose traffic is not derived from the case,
// and the extra frames are indistinguishable from a subject being chatty.
func Repeats(m interpose.Match) int {
	switch {
	case m.Occurrence > 0:
		return int(m.Occurrence)
	case m.Every > 0:
		return int(m.Every)
	default:
		return 1
	}
}

// halted reports a halt as its named cause rather than as whatever the SDK
// call failed with. A cancelled CallTool surfaces as a context error, and
// "context canceled" tells nobody whether the fault was withdrawn or the
// operator pressed ctrl-C.
func halted(ctx context.Context, err error) error {
	if why := Why(ctx); why != nil {
		return why
	}
	return err
}
