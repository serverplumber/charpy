package transcript

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/revision"
)

// These mirror the patterns in schema/transcript/v1.json. They are duplicated
// rather than imported from the catalogue on purpose: the catalogue is the
// outer layer that compiles manifests into everyone else's terms, and a
// transcript that had to import it to write a line would invert that
// (ADR-009). The schema is the shared source, and a test holds the two
// together.
var (
	caseIDPattern   = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(-[a-z0-9]+)*$`)
	citationPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(-[a-z0-9]+)*(@([0-9]{4}-[0-9]{2}-[0-9]{2}|draft))?(#seed=[0-9a-f]{6,16})?$`)
	traceIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// refusal marks an error that means the line cannot be written at all, as
// opposed to a defect the writer repaired on the way out. The two are handled
// differently and the difference is load-bearing: a refused line costs one
// frame, a repaired one costs only the offending part, and both end as exit
// code 2.
type refusal struct{ error }

func refuse(format string, a ...any) error { return refusal{fmt.Errorf(format, a...)} }

// refused reports whether err means the line was not written.
func refused(err error) bool {
	var r refusal
	return errors.As(err, &r)
}

// The line structs are the transcript's wire shape, and they are exported
// because they are read as well as written. One definition serves both
// directions: a reader with its own mirror of these fields would silently drop
// whatever a later writer added, which is the drift the registries are kept
// beside their code to avoid.

// Common is on every line, header included, so a single line pasted into a
// bug report is still interpretable.
type Common struct {
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
	RunID         string `json:"run_id"`
	Seq           int64  `json:"seq"`
	TMonoNS       int64  `json:"t_mono_ns"`
	TWall         string `json:"t_wall"`
}

type HeaderLine struct {
	Common
	CharpyVersion string        `json:"charpy_version"`
	CharpyCommit  string        `json:"charpy_commit,omitempty"`
	Seed          string        `json:"seed"`
	Mode          Mode          `json:"mode"`
	Subject       SubjectLine   `json:"subject"`
	Revision      *RevisionLine `json:"revision,omitempty"`
	PolicyDigest  string        `json:"policy_digest,omitempty"`
	Cases         []string      `json:"cases,omitempty"`
	Redaction     string        `json:"redaction"`
	Clock         string        `json:"clock"`
	Fleet         FleetLine     `json:"fleet"`
}

type SubjectLine struct {
	Class      Class  `json:"class"`
	Descriptor string `json:"descriptor,omitempty"`
}

type RevisionLine struct {
	Negotiated revision.Revision   `json:"negotiated"`
	Offered    []revision.Revision `json:"offered,omitempty"`
	How        How                 `json:"how,omitempty"`
}

type FleetLine struct {
	Clients int `json:"clients"`
}

type FrameLine struct {
	Common
	Face      Face      `json:"face"`
	Direction Direction `json:"direction"`
	Transport Transport `json:"transport"`
	ClientID  string    `json:"client_id"`
	SessionID string    `json:"session_id"`
	ConnID    *string   `json:"conn_id"`
	StreamID  *string   `json:"stream_id"`

	Kind       envelope.Kind   `json:"kind"`
	ID         *string         `json:"id"`
	IDType     envelope.IDType `json:"id_type"`
	Method     *string         `json:"method"`
	ResultType *string         `json:"result_type"`
	ErrorCode  *int64          `json:"error_code"`

	Revision     revision.Revision `json:"revision,omitempty"`
	Raw          string            `json:"raw"`
	RawLen       int               `json:"raw_len"`
	RawTruncated bool              `json:"raw_truncated"`

	HTTP    *HTTPLine    `json:"http"`
	Link    LinkLine     `json:"link"`
	Fault   *FaultLine   `json:"fault"`
	Session *SessionLine `json:"session"`
}

type EventLine struct {
	Common
	EventKind EventKind      `json:"event_kind"`
	Face      *Face          `json:"face"`
	Transport *Transport     `json:"transport"`
	ClientID  *string        `json:"client_id"`
	SessionID *string        `json:"session_id"`
	ConnID    *string        `json:"conn_id"`
	StreamID  *string        `json:"stream_id"`
	Detail    map[string]any `json:"detail"`
	Fault     *FaultLine     `json:"fault"`
	Link      *LinkLine      `json:"link"`
}

type HTTPLine struct {
	Status   *int              `json:"status"`
	Headers  map[string]string `json:"headers"`
	SSEEvent *string           `json:"sse_event"`
	SSEID    *string           `json:"sse_id"`
}

type LinkLine struct {
	CharpyID     *string `json:"charpy_id"`
	Via          Via     `json:"via"`
	Confidence   float64 `json:"confidence"`
	TraceID      *string `json:"trace_id"`
	SpanID       *string `json:"span_id"`
	ParentSpanID *string `json:"parent_span_id"`
}

type FaultLine struct {
	CaseID   string         `json:"case_id"`
	Citation string         `json:"citation"`
	Kind     string         `json:"kind"`
	Params   map[string]any `json:"params,omitempty"`
}

type SessionLine struct {
	MCPSessionID *string `json:"mcp_session_id"`
	Identity     *string `json:"identity"`
}

// headerBytes renders the header line. It never locks and never fails:
// WriteHeader validates before calling it, and the fallback header Close
// writes is trivially valid.
//
// The header carries t_mono_ns 0 and the wall time the writer was created,
// not the moment it was written. It describes the run, which began then, and
// stamping it with its real capture time would put the file's first line
// after its second on the clock every consumer sorts by.
func (w *Writer) headerBytes(h Header) []byte {
	l := HeaderLine{
		Common: Common{
			SchemaVersion: SchemaVersion,
			Type:          "header",
			RunID:         w.runID,
			Seq:           0,
			TMonoNS:       0,
			TWall:         w.startWall.UTC().Format(wallFormat),
		},
		CharpyVersion: w.run.CharpyVersion,
		CharpyCommit:  w.run.CharpyCommit,
		Seed:          w.run.Seed,
		Mode:          w.run.Mode,
		Subject:       SubjectLine{Class: w.run.Subject.Class, Descriptor: w.run.Subject.Descriptor},
		PolicyDigest:  h.PolicyDigest,
		Cases:         h.Cases,
		Redaction:     redactionLabel(w.redactor.On()),
		Clock:         string(w.run.Clock),
		Fleet:         FleetLine{Clients: w.run.Clients},
	}
	if h.Revision != nil {
		l.Revision = &RevisionLine{
			Negotiated: h.Revision.Negotiated,
			Offered:    h.Revision.Offered,
			How:        h.Revision.How,
		}
	}

	b, err := json.Marshal(l)
	if err != nil {
		// Unreachable: every field is a string, an int or a slice of them.
		return []byte(`{"schema_version":1,"type":"header"}`)
	}
	return b
}

func redactionLabel(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// validateHeader checks the half of the header the run supplies. It refuses
// rather than repairing: the header is written once, from data charpy built
// itself, so a bad one is a bug the run should fail on rather than a line to
// paper over.
func validateHeader(h Header) error {
	if h.Revision == nil {
		return nil
	}
	if !revision.Known(h.Revision.Negotiated) {
		return fmt.Errorf("transcript: header negotiated revision %q is not one charpy knows",
			h.Revision.Negotiated)
	}
	for _, r := range h.Revision.Offered {
		if !revision.Known(r) {
			return fmt.Errorf("transcript: header offered revision %q is not one charpy knows", r)
		}
	}
	switch h.Revision.How {
	case HowInitialize, HowServerDiscover, HowProbe, HowForced, HowUnknown, "":
	default:
		return fmt.Errorf("transcript: unknown revision.how %q", h.Revision.How)
	}
	return nil
}

// noteBytes renders a note event at an already-assigned sequence number. It
// is the fallback for a line that would not marshal: the sequence stays
// dense, and the file says what was lost.
func (w *Writer) noteBytes(c *Common, text string) []byte {
	l := EventLine{
		Common:    Common{SchemaVersion: SchemaVersion, Type: "event", RunID: c.RunID, Seq: c.Seq, TMonoNS: c.TMonoNS, TWall: c.TWall},
		EventKind: Note,
		Detail:    map[string]any{"harness": text},
	}
	b, err := json.Marshal(l)
	if err != nil {
		return []byte(`{"schema_version":1,"type":"event","event_kind":"note"}`)
	}
	return b
}

// FrameLine derives a frame line from a Frame.
//
// Two kinds of problem are handled differently, and the difference is the
// point. A line that cannot be made valid is refused, because one missing
// frame costs less than a transcript consumers cannot trust. A line with a
// repairable defect -- an http block on a stdio frame, a malformed trace id --
// is written without the offending part, because the frame's bytes are the
// thing the transcript exists to preserve. Both record an error, and both end
// as exit code 2.
func (w *Writer) FrameLine(f Frame) (FrameLine, error) {
	var bad []error

	switch f.Face {
	case Downstream, Upstream:
	default:
		return FrameLine{}, refuse("transcript: frame has no face (got %q)", f.Face)
	}
	switch f.Direction {
	case C2S, S2C:
	default:
		return FrameLine{}, refuse("transcript: frame has no direction (got %q)", f.Direction)
	}
	switch f.Transport {
	case TransportStdio, TransportHTTP, TransportInproc:
	default:
		return FrameLine{}, refuse("transcript: frame has no transport (got %q)", f.Transport)
	}
	// Dimension totality: there is no unknown client or session. In v0 both
	// are constants, and they are still never absent -- a transcript that
	// assumes one session is the same mistake as one that assumes one face.
	if f.ClientID == "" {
		return FrameLine{}, refuse("transcript: frame has no client_id")
	}
	if f.SessionID == "" {
		return FrameLine{}, refuse("transcript: frame has no session_id")
	}

	l := FrameLine{
		Common:    Common{Type: "frame"},
		Face:      f.Face,
		Direction: f.Direction,
		Transport: f.Transport,
		ClientID:  f.ClientID,
		SessionID: f.SessionID,
		ConnID:    optional(f.ConnID),
		StreamID:  optional(f.StreamID),
		Kind:      f.Message.Kind,
		IDType:    f.Message.ID.Type(),
	}

	raw := f.Message.Raw()
	l.RawLen = len(raw)
	if l.RawLen > RawCap {
		raw, l.RawTruncated = raw[:RawCap], true
	}
	l.Raw = base64.StdEncoding.EncodeToString(raw)

	// A malformed frame has no parsed envelope and keeps every byte. The
	// envelope package already guarantees it; asserting it here as well is
	// what makes the transcript schema's rule impossible to violate from
	// either side.
	if f.Message.Kind != envelope.KindMalformed {
		switch f.Message.ID.Type() {
		case envelope.IDNull, envelope.IDAbsent:
			// The id column stays null. id_type is what tells them apart.
		default:
			l.ID = ptr(f.Message.ID.Text())
		}

		switch f.Message.Kind {
		case envelope.KindRequest, envelope.KindNotification:
			l.Method = ptr(f.Message.Method)
		default:
			// A response carries no method of its own; this is the ledger's
			// echo, and it is absent when the id could not be resolved.
			l.Method = optional(f.Method)
		}

		if rt, ok := f.Message.ResultType(); ok {
			switch rt {
			case "complete", "input_required":
				l.ResultType = ptr(rt)
			default:
				// A value outside the enum stays out of the column and in
				// the raw bytes, where the schema layer will find it.
			}
		}
		if f.Message.Error != nil {
			l.ErrorCode = ptr(f.Message.Error.Code)
		}
	}

	if f.Revision != "" {
		if revision.Known(f.Revision) {
			l.Revision = f.Revision
		} else {
			bad = append(bad, fmt.Errorf("transcript: frame revision %q is not one charpy knows; omitted", f.Revision))
		}
	}

	if f.HTTP != nil {
		if f.Transport != TransportHTTP {
			bad = append(bad, fmt.Errorf("transcript: http detail on a %s frame; omitted", f.Transport))
		} else {
			l.HTTP = w.HTTPLine(f.HTTP)
		}
	}

	link, linkErrs := linkLineFrom(f.Link)
	l.Link = link
	bad = append(bad, linkErrs...)

	if f.Fault != nil {
		fl, err := faultLineFrom(f.Fault)
		if err != nil {
			bad = append(bad, err)
		} else {
			l.Fault = fl
		}
	}

	if f.Session != nil {
		if f.Revision != "" && f.Revision.Stateless() {
			// SEP-2567 removed the protocol session in 2026-07-28. charpy's
			// own session_id is unaffected and stays in its column.
			bad = append(bad, fmt.Errorf(
				"transcript: protocol session on a %s frame, where the protocol has none; omitted", f.Revision))
		} else {
			l.Session = &SessionLine{
				MCPSessionID: optional(f.Session.MCPSessionID),
				Identity:     optional(f.Session.Identity),
			}
		}
	}

	return l, errors.Join(bad...)
}

// EventLine derives an event line from an Event. Events are charpy's own
// account of what it did, so an event that cannot be written correctly is
// refused: an oracle reading a stream_close with no reason cannot evaluate
// cancellation at all, which is worse than seeing no event.
func (w *Writer) EventLine(e Event) (EventLine, error) {
	var bad []error

	switch e.Kind {
	case ConnOpen, ConnClose, StreamOpen, StreamClose, SubjectExit,
		FaultScheduled, FaultApplied, FaultWithdrawn, Probe, ClockAdvance, Note:
	default:
		return EventLine{}, refuse("transcript: unknown event kind %q", e.Kind)
	}

	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}

	switch e.Kind {
	case StreamClose:
		switch CloseReason(str(detail["reason"])) {
		case PeerClose, CharpyClose, SubjectClose, CloseTimeout, CloseError:
		default:
			return EventLine{}, refuse(
				"transcript: stream_close detail.reason is %v; the cancellation invariant needs to know who closed", detail["reason"])
		}
	case Probe:
		if str(detail["method"]) == "" {
			return EventLine{}, refuse("transcript: probe event has no detail.method")
		}
		switch ProbeOutcome(str(detail["outcome"])) {
		case ProbeOK, ProbeError, ProbeTimeout, ProbeRefused:
		default:
			return EventLine{}, refuse(
				"transcript: probe detail.outcome is %v; liveness is computed from outcomes", detail["outcome"])
		}
	case FaultScheduled, FaultApplied, FaultWithdrawn:
		if e.Fault == nil {
			return EventLine{}, refuse("transcript: %s event does not identify its case", e.Kind)
		}
	}

	l := EventLine{
		Common:    Common{Type: "event"},
		EventKind: e.Kind,
		ClientID:  optional(e.ClientID),
		SessionID: optional(e.SessionID),
		ConnID:    optional(e.ConnID),
		StreamID:  optional(e.StreamID),
		Detail:    detail,
	}

	switch e.Face {
	case Downstream, Upstream:
		l.Face = ptr(e.Face)
	case "":
	default:
		bad = append(bad, fmt.Errorf("transcript: unknown face %q on a %s event; omitted", e.Face, e.Kind))
	}
	switch e.Transport {
	case TransportStdio, TransportHTTP, TransportInproc:
		l.Transport = ptr(e.Transport)
	case "":
	default:
		bad = append(bad, fmt.Errorf("transcript: unknown transport %q on a %s event; omitted", e.Transport, e.Kind))
	}

	if e.Fault != nil {
		fl, err := faultLineFrom(e.Fault)
		if err != nil {
			// A fault lifecycle event that cannot name its case is not
			// repairable: it is the case audit.
			switch e.Kind {
			case FaultScheduled, FaultApplied, FaultWithdrawn:
				return EventLine{}, refusal{err}
			}
			bad = append(bad, err)
		} else {
			l.Fault = fl
		}
	}

	if e.Link != nil {
		link, linkErrs := linkLineFrom(*e.Link)
		l.Link = &link
		bad = append(bad, linkErrs...)
	}

	return l, errors.Join(bad...)
}

// linkLineFrom renders the join key.
//
// Confidence is derived from the regime rather than taken from the caller,
// except for inferred joins where it is the caller's whole point. Only
// forwarded and traced joins are authoritative, and a transcript that let a
// guess claim confidence 1.0 would let a gateway verdict rest on one.
func linkLineFrom(l Link) (LinkLine, []error) {
	var bad []error

	out := LinkLine{Via: l.Via, CharpyID: optional(l.CharpyID)}

	// nojoin downgrades to the regime that claims nothing, and drops the join
	// key with it: charpy_id is the thing you join on, and carrying one under
	// via = "none" would offer a key while disclaiming the join.
	nojoin := func(format string, a ...any) {
		bad = append(bad, fmt.Errorf(format, a...))
		out.Via, out.Confidence, out.CharpyID = ViaNone, 0.0, nil
	}

	switch l.Via {
	case ViaForwarded, ViaTraced:
		out.Confidence = 1.0
	case ViaNone:
		out.Confidence = 0.0
	case ViaInferred:
		out.Confidence = l.Confidence
		if l.Confidence < 0 || l.Confidence >= 1 {
			// Downgrade rather than invent a number. A correlator that hands
			// back an impossible confidence has said nothing, and "no join"
			// is the honest recording of nothing. Substituting a plausible
			// 0.99 would put a figure charpy made up behind the one rule
			// whose entire purpose is that a guess cannot claim authority.
			nojoin("transcript: inferred join claims confidence %v; recorded as no join", l.Confidence)
		}
	default:
		nojoin("transcript: unknown join regime %q; recorded as none", l.Via)
	}

	if l.TraceID != "" {
		if traceIDPattern.MatchString(l.TraceID) {
			out.TraceID = ptr(l.TraceID)
		} else {
			bad = append(bad, fmt.Errorf("transcript: trace_id %q is not a W3C trace id; omitted", l.TraceID))
		}
	}
	for _, s := range []struct {
		val string
		dst **string
		key string
	}{
		{l.SpanID, &out.SpanID, "span_id"},
		{l.ParentSpanID, &out.ParentSpanID, "parent_span_id"},
	} {
		if s.val == "" {
			continue
		}
		if spanIDPattern.MatchString(s.val) {
			*s.dst = ptr(s.val)
		} else {
			bad = append(bad, fmt.Errorf("transcript: %s %q is not a W3C span id; omitted", s.key, s.val))
		}
	}

	return out, bad
}

func faultLineFrom(f *Fault) (*FaultLine, error) {
	if !caseIDPattern.MatchString(f.CaseID) {
		return nil, fmt.Errorf("transcript: fault case_id %q is not a case id", f.CaseID)
	}
	if !citationPattern.MatchString(f.Citation) {
		return nil, fmt.Errorf("transcript: fault citation %q is not a case citation", f.Citation)
	}
	if f.Kind == "" {
		return nil, fmt.Errorf("transcript: fault on case %q names no mechanism", f.CaseID)
	}
	return &FaultLine{CaseID: f.CaseID, Citation: f.Citation, Kind: f.Kind, Params: f.Params}, nil
}

// HTTPLine lowercases header names and puts every value through the run's
// redaction policy.
func (w *Writer) HTTPLine(h *HTTP) *HTTPLine {
	out := &HTTPLine{
		Headers:  make(map[string]string, len(h.Headers)),
		SSEEvent: optional(h.SSEEvent),
		SSEID:    optional(h.SSEID),
	}
	if h.Status != 0 {
		out.Status = ptr(h.Status)
	}
	for name, value := range h.Headers {
		lower := strings.ToLower(name)
		out.Headers[lower] = w.redactor.Value(lower, value)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// optional renders an empty string as JSON null rather than "". The
// transcript's string dimensions have a minimum length, so an empty value is
// an absent one.
func optional[T ~string](v T) *T {
	if v == "" {
		return nil
	}
	return &v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
