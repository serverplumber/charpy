package fault

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/wire"
)

// A mechanism turns a case's parameters into what the wire should see. The
// verbs it expresses itself in live in internal/interpose and the byte effects
// in internal/wire; this package is the seam where a case's TOML becomes both.
//
// fault imports interpose, which ADR-009 permits: a mechanism is expressed as
// applications of the three verbs, so it has to name them, and interpose does
// not import back.

// ErrNotApplicable reports a fault that cannot be planned against this frame,
// in this run, yet. Three things produce it: a declaration charpy has not read
// (the driver answers by asking the subject for it), a run that has not
// produced what the fault needs, and a fault that spans a connection boundary
// a single-frame plan cannot carry.
//
// All three mean the same thing to the oracle -- the case does not apply,
// which is a non-verdict it already has -- so they are one sentinel with
// specific messages rather than three a caller would have to enumerate. The
// policy matches internal/wire's: a fault charpy cannot deliver is a case that
// does not apply, never a weaker fault delivered quietly.
var ErrNotApplicable = errors.New("fault: not applicable")

// StreamAction is what becomes of the stream after a fault's frame.
type StreamAction string

const (
	// StreamContinue leaves the stream alone.
	StreamContinue StreamAction = ""
	// StreamClose closes it.
	StreamClose StreamAction = "close"
	// StreamStall holds it open with nothing further on it.
	StreamStall StreamAction = "stall"
)

// Cut truncates a frame's encoding.
type Cut struct {
	At   wire.CutPoint
	Opts wire.CutOptions
}

// Hold withholds a frame.
type Hold struct {
	// For is how long, on the injected clock. Zero means until something
	// withdraws it by hand -- withdraw_after_ms = 0.
	For time.Duration
	// Then is what happens at withdrawal: deliver, close or error.
	Then string
	// Scope is what is held: response, stream or connection.
	Scope string
}

// Context is what a mechanism needs from the run that its own parameters
// cannot carry.
//
// It is passed in rather than reached for, so that a mechanism stays a pure
// function of its inputs and the ledger's shape is not something every
// mechanism has to know.
type Context struct {
	// RunSeed is what every seeded choice derives from.
	RunSeed string

	// Resolved is an id already answered on this face and connection, for
	// unsolicited_response's already_resolved source. Absent when the ledger
	// has none, which makes that case not apply rather than wrong.
	Resolved envelope.ID

	// OutputSchema is the outputSchema declared for the tool whose call the
	// matched frame answers, for schema_violation's declared_output_schema
	// target. A driver supplies it where it knows the declaration -- where
	// charpy serves, the schema is its own -- and leaves it empty otherwise,
	// which makes that target not apply rather than guess at a shape.
	OutputSchema json.RawMessage
}

// Plan is what a mechanism does to one matched frame.
//
// It describes wire actions rather than performing them, so a mechanism can be
// tested without a subject, a socket or a clock -- which is most of why the
// eight are cheap and the oracle is not.
type Plan struct {
	Case interpose.Case

	// Before and After are frames charpy authors around the matched one.
	Before []envelope.Message
	After  []envelope.Message

	// Deliver is what crosses the wire in place of the matched frame. Nil
	// means it does not cross at all, and Hold or Swallow says why.
	Deliver *envelope.Message

	// Cut truncates Deliver's encoding.
	Cut *Cut
	// Hold withholds Deliver.
	Hold *Hold
	// Swallow drops the frame, so no destination ever sees it.
	Swallow bool

	Then      StreamAction
	Keepalive wire.Keepalive
}

// Verb reports which of the three verbs a plan applies, for the transcript.
func (p Plan) Verb() interpose.Verb {
	switch {
	case p.Hold != nil:
		return interpose.VerbWithhold
	case p.Swallow || len(p.Before) > 0 || len(p.After) > 0:
		return interpose.VerbSynthesize
	default:
		return interpose.VerbRewrite
	}
}

// Apply plans a case's fault against the frame that matched it.
//
// An unknown mechanism is an error rather than a no-op: the loader validates
// kinds against the registry, so reaching here with one it does not know means
// charpy is inconsistent with itself, and quietly delivering the frame
// unchanged would be a case that passes by not running.
func Apply(c interpose.Case, m envelope.Message, ctx Context) (Plan, error) {
	p := Plan{Case: c}
	params := c.Fault.Params

	switch c.Fault.Kind {
	case "hang":
		return planned(planHang(p, params))
	case "truncate":
		return planned(planTruncate(p, m, params, ctx))
	case "malformed_json":
		return planned(planMalformedJSON(p, m, params, ctx))
	case "schema_violation":
		return planned(planSchemaViolation(p, m, params, ctx))
	case "duplicate_id":
		return planned(planDuplicateID(p, m, params))
	case "unsolicited_response":
		return planned(planUnsolicitedResponse(p, m, params, ctx))
	case "manifest_mutate":
		return planned(planManifestMutate(p, m, params))
	case "capability_flip":
		return planned(planCapabilityFlip(p, m, params))
	default:
		return Plan{}, fmt.Errorf("fault: no mechanism implements kind %q", c.Fault.Kind)
	}
}

// planned refuses a plan that never says what becomes of the matched frame.
//
// Deliver, Hold and Swallow are the three answers, and a plan carrying none of
// them drops the frame in every driver: the switch that carries a plan out has
// an arm for each, so a plan matching no arm means the frame quietly does not
// cross, with nothing in the transcript saying charpy is why. That is the
// worst kind of wrong for this tool, because the subject then looks like it
// stopped answering.
//
// Like the unknown-kind branch above, this is charpy being inconsistent with
// itself rather than a case that does not apply, so it is an error and not
// ErrNotApplicable.
func planned(p Plan, err error) (Plan, error) {
	if err != nil {
		return Plan{}, err
	}
	if p.Deliver == nil && p.Hold == nil && !p.Swallow {
		return Plan{}, fmt.Errorf(
			"fault: mechanism %q planned neither a delivery, a hold nor a swallow for the matched frame",
			p.Case.Fault.Kind)
	}
	return p, nil
}

// Parameter readers. The loader has already validated these against the
// registry, so a missing key means the default and a wrong type means charpy
// built a case the loader would have rejected.

func str(params map[string]any, key, def string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return def
}

func integer(params map[string]any, key string, def int64) int64 {
	if v, ok := params[key].(int64); ok {
		return v
	}
	return def
}

func boolean(params map[string]any, key string, def bool) bool {
	if v, ok := params[key].(bool); ok {
		return v
	}
	return def
}

func keepalive(params map[string]any) wire.Keepalive {
	return wire.Keepalive(str(params, "keepalive", string(wire.KeepaliveNone)))
}
