package transcript

// SchemaVersion is the transcript schema this package writes. It appears on
// every line, not only the header, so a single line pasted into a bug report
// stays interpretable. See schema/transcript/v1.json.
const SchemaVersion = 1

// Face is which side of the subject a frame crossed. It is defined against
// the subject, never against charpy: a server under test has only a
// downstream face, a client under test only an upstream one, and a gateway
// has both, which is the entire point.
type Face string

const (
	// Downstream is the subject's client-facing side. charpy plays the
	// client here.
	Downstream Face = "downstream"
	// Upstream is the subject's server-facing side. charpy plays the server.
	Upstream Face = "upstream"
)

// Direction is the MCP role direction, independent of who charpy is
// impersonating. Neither Face nor Direction alone is unambiguous; the pair
// always is.
type Direction string

const (
	// C2S is client role to server role.
	C2S Direction = "c2s"
	// S2C is server role to client role.
	S2C Direction = "s2c"
)

// Opposite is the direction an answer to a frame travelling d goes back in,
// and so the direction the request an answer travelling d answers came in.
func (d Direction) Opposite() Direction {
	switch d {
	case C2S:
		return S2C
	case S2C:
		return C2S
	}
	return d
}

// Transport is how the frame travelled.
type Transport string

const (
	TransportStdio  Transport = "stdio"
	TransportHTTP   Transport = "http"
	TransportInproc Transport = "inproc"
)

// Mode is how charpy is positioned against the subject for this run.
type Mode string

const (
	ModeProxy         Mode = "proxy"
	ModeHostileServer Mode = "hostile-server"
	ModeStdioIngress  Mode = "stdio-ingress"
	ModeInproc        Mode = "inproc"
)

// Class is what the subject is.
type Class string

const (
	ClassServer  Class = "server"
	ClassClient  Class = "client"
	ClassGateway Class = "gateway"
)

// How records which rung of the fingerprint ladder settled the revision. See
// docs/design/revisions.md section 3.
type How string

const (
	HowInitialize     How = "initialize"
	HowServerDiscover How = "server-discover"
	HowProbe          How = "probe"
	// HowForced is --revision: the subject is tested on a version it would
	// not have chosen.
	HowForced How = "forced"
	// HowUnknown is a run that never settled the question.
	HowUnknown How = "unknown"
)

// Via records which correlation regime produced a frame's join key. See
// docs/design/transcript.md section 4.
type Via string

const (
	// ViaForwarded is proxy and stdio-ingress: charpy relays the bytes, so
	// it stamps its own id on both copies. Authoritative.
	ViaForwarded Via = "forwarded"
	// ViaTraced is a join on traceparent in _meta per SEP-414. Authoritative
	// when the subject propagates it.
	ViaTraced Via = "traced"
	// ViaInferred is a ledger content join, used when the subject drops
	// trace context. Never authoritative, and never the basis of a gateway
	// verdict: a false correlation produces a confident, specific, wrong
	// accusation about someone else's credential handling.
	ViaInferred Via = "inferred"
	// ViaNone is no join attempted or possible.
	ViaNone Via = "none"
)

// EventKind names something that is not a frame but changes what the oracle
// should conclude. Events are load-bearing: under 2026-07-28 Streamable HTTP,
// closing the response stream is itself the cancellation signal, so an oracle
// reading only frames cannot evaluate cancellation at all.
type EventKind string

const (
	ConnOpen       EventKind = "conn_open"
	ConnClose      EventKind = "conn_close"
	StreamOpen     EventKind = "stream_open"
	StreamClose    EventKind = "stream_close"
	SubjectExit    EventKind = "subject_exit"
	FaultScheduled EventKind = "fault_scheduled"
	FaultApplied   EventKind = "fault_applied"
	// FaultWithdrawn is where the liveness clock starts.
	FaultWithdrawn EventKind = "fault_withdrawn"
	Probe          EventKind = "probe"
	ClockAdvance   EventKind = "clock_advance"
	// Note is a free-text harness annotation. The oracle ignores it.
	Note EventKind = "note"
)

// CloseReason says who closed a stream. The cancellation invariant needs to
// know, and inferring it later from a bare close is not possible.
type CloseReason string

const (
	PeerClose    CloseReason = "peer_close"
	CharpyClose  CloseReason = "charpy_close"
	SubjectClose CloseReason = "subject_close"
	CloseTimeout CloseReason = "timeout"
	CloseError   CloseReason = "error"
)

// ProbeOutcome is the result of a liveness probe.
type ProbeOutcome string

const (
	ProbeOK      ProbeOutcome = "ok"
	ProbeError   ProbeOutcome = "error"
	ProbeTimeout ProbeOutcome = "timeout"
	ProbeRefused ProbeOutcome = "refused"
)
