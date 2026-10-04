package schemacheck

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/schema"
)

// The root every JSON-RPC frame is validated against. It is an anyOf across
// request, notification, result and error, so a frame that matches none of
// them has failed all four and the causes say how. Where it lives depends on
// the revision: the schemas up to 2025-06-18 keep their types under
// "definitions", and 2025-11-25 on under "$defs". Assuming either one made
// every other revision's transcripts impossible to check.
const message = "JSONRPCMessage"

// root is the pointer to the message type in one revision's schema document.
func root(doc any) string {
	if m, ok := doc.(map[string]any); ok {
		if _, ok := m["$defs"]; ok {
			return "#/$defs/" + message
		}
	}
	return "#/definitions/" + message
}

// Layer is what findings from here are tagged with.
const Layer = "schema"

// Check validates every frame the subject originated against the vendored
// schema for the revision that was negotiated -- on the frame's own face, when
// the frame says. A gateway may settle different revisions with its clients
// and its servers, and each face is held to what was agreed on it; a frame
// that names none, such as the handshake request that settles it, is held to
// the header's.
//
// This is the only layer permitted to issue a MUST, and the reason is narrow:
// charpy is not asserting a reading of the specification. A generated
// normative artifact rejected the frame, and the report says which subschema
// did it. Everything else charpy concludes is OBSERVED.
//
// Three exclusions, all of them load-bearing:
//
// Frames charpy corrupted. A frame charpy deliberately broke cannot be held
// against the subject, and the fault column is how one is recognised.
//
// Frames charpy did not receive from the subject. What charpy sent is
// charpy's.
//
// Frames whose bytes are incomplete. Beyond the transcript's size cap the
// tail is missing because charpy stopped recording, not because the subject
// stopped sending, and a schema error from a clipped frame is charpy's own
// doing reported as the subject's.
func Check(t *transcript.Transcript) (oracle.Report, error) {
	rep := oracle.Report{RunID: t.Header.RunID}

	rev, ok := negotiated(t)
	if !ok {
		// Without a revision there is no artifact to validate against.
		// Skipping says so; validating against a guessed revision would
		// produce confident findings about the wrong specification.
		rep.Add(oracle.Finding{
			Verdict: oracle.Skipped, Layer: Layer, Check: "schema", Seq: -1,
			Summary: "no negotiated revision in the transcript header",
			Reason:  "not-applicable-to-revision",
		})
		return rep, nil
	}

	schemas := map[revision.Revision]*jsonschema.Schema{}
	schemaFor := func(r revision.Revision) (*jsonschema.Schema, error) {
		if sch, ok := schemas[r]; ok {
			return sch, nil
		}
		sch, err := compile(r)
		if err != nil {
			return nil, err
		}
		schemas[r] = sch
		return sch, nil
	}

	class := t.Header.Subject.Class
	for _, f := range t.Frames() {
		if !oracle.SubjectOriginated(class, f) {
			continue
		}
		if !f.Complete() {
			rep.Add(oracle.Finding{
				Verdict: oracle.Inconclusive, Layer: Layer, Check: "schema", Seq: f.Seq,
				Summary: "frame exceeds the transcript's byte cap and cannot be validated whole",
				Reason:  "raw-truncated",
			})
			continue
		}
		r := rev
		if revision.Known(f.Revision) {
			r = f.Revision
		}
		sch, err := schemaFor(r)
		if err != nil {
			return rep, err
		}
		for _, finding := range validate(sch, r, f) {
			rep.Add(finding)
		}
	}

	rep.Sort()
	return rep, nil
}

func negotiated(t *transcript.Transcript) (revision.Revision, bool) {
	if t.Header.Revision == nil {
		return "", false
	}
	r := t.Header.Revision.Negotiated
	return r, revision.Known(r)
}

func compile(r revision.Revision) (*jsonschema.Schema, error) {
	raw, err := schema.For(r)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schemacheck: %s: %w", r, err)
	}

	url := "https://charpy.dev/schema/" + r.String() + ".json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("schemacheck: %s: %w", r, err)
	}
	sch, err := c.Compile(url + root(doc))
	if err != nil {
		return nil, fmt.Errorf("schemacheck: %s: %w", r, err)
	}
	return sch, nil
}

func validate(sch *jsonschema.Schema, rev revision.Revision, f *transcript.FrameLine) []oracle.Finding {
	raw, err := f.Bytes()
	if err != nil {
		return []oracle.Finding{{
			Verdict: oracle.Inconclusive, Layer: Layer, Check: "schema", Seq: f.Seq,
			Summary: "frame bytes cannot be decoded", Reason: err.Error(),
		}}
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		// Bytes no parser accepts are not a schema finding. The schema judges
		// documents; that a frame is not one is what the malformed kind
		// already records, and saying it twice in different words would be
		// two findings for one fact.
		return nil
	}

	verr := sch.Validate(inst)
	if verr == nil {
		return nil
	}

	ve, ok := verr.(*jsonschema.ValidationError)
	if !ok {
		return []oracle.Finding{{
			Verdict: oracle.Must, Layer: Layer, Check: "schema:" + rev.String(), Seq: f.Seq,
			Summary: "frame rejected by the protocol schema", Detail: verr.Error(),
		}}
	}

	var out []oracle.Finding
	for _, cause := range closest(ve) {
		out = append(out, oracle.Finding{
			Verdict: oracle.Must,
			Layer:   Layer,
			// The citation the report prints: which artifact, and which part
			// of it did the rejecting. charpy adds no opinion of its own.
			Check:   citation(rev, cause),
			Seq:     f.Seq,
			Summary: fmt.Sprintf("%s rejected by the %s schema", frameLabel(f), rev),
			Detail:  strings.TrimSpace(cause.Error()),
		})
	}
	return out
}

// citation renders where the rejection came from: the vendored artifact, the
// subschema within it, and the keyword that failed. It is the whole basis on
// which charpy is allowed to say MUST, so it names all three rather than
// gesturing at the document.
func citation(rev revision.Revision, e *jsonschema.ValidationError) string {
	loc := e.SchemaURL
	if i := strings.Index(loc, "#"); i >= 0 {
		loc = loc[i:]
	}
	kw := strings.Join(e.ErrorKind.KeywordPath(), "/")
	if kw == "" {
		return fmt.Sprintf("schema:%s%s", rev, loc)
	}
	return fmt.Sprintf("schema:%s%s/%s", rev, loc, kw)
}

// closest picks the causes worth reporting.
//
// The root is an anyOf over four message shapes, so a frame that is wrong in
// one small way fails all four branches and the error tree holds every
// branch's complaints. Reporting them all is complete and useless: one wrong
// jsonrpc value becomes eight MUST findings, seven of which say the frame is
// not a shape it never claimed to be.
//
// So only the branch that came closest is reported, and closeness is measured
// by how deep into the frame the objection reaches before how few objections
// there are. A failure at the document root -- "missing property 'method'" --
// means the validator rejected the shape outright. A failure at /result means
// it accepted the shape and objected to something inside it, which is the
// finding a person can act on. Counting failures alone ties those two and
// picks whichever came first, which is how a wrong-typed result ends up
// reported as a request with no method.
func closest(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	// Descend through the single-cause wrappers a $ref introduces. The node
	// that actually branches is the anyOf, and it is not the root: the root is
	// the reference that led to it.
	for len(e.Causes) == 1 {
		e = e.Causes[0]
	}

	branches := e.Causes
	if len(branches) == 0 {
		return []*jsonschema.ValidationError{e}
	}

	best := leaves(branches[0])
	for _, b := range branches[1:] {
		if l := leaves(b); better(l, best) {
			best = l
		}
	}
	return best
}

// better reports whether one branch's failures are more worth showing than
// another's: deeper into the instance first, fewer of them second.
func better(a, b []*jsonschema.ValidationError) bool {
	if da, db := depth(a), depth(b); da != db {
		return da > db
	}
	return len(a) < len(b)
}

func depth(errs []*jsonschema.ValidationError) int {
	deepest := 0
	for _, e := range errs {
		if d := len(e.InstanceLocation); d > deepest {
			deepest = d
		}
	}
	return deepest
}

// leaves returns the most specific failures under an error: the keywords that
// actually rejected a value, rather than the references that led to them.
func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func frameLabel(f *transcript.FrameLine) string {
	if m := f.MethodName(); m != "" {
		return string(f.Kind) + " " + m
	}
	return string(f.Kind)
}
