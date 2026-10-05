package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/seed"
)

// RawCap is how many bytes of a frame the transcript carries. Beyond it the
// raw column holds the first RawCap bytes and raw_truncated is set; raw_len
// is always the true length.
const RawCap = 64 << 10

// wallFormat is RFC 3339 with a fixed nine fractional digits, so the t_wall
// column has one width and sorts as a string.
const wallFormat = "2006-01-02T15:04:05.000000000Z07:00"

// HeldCap bounds how many lines the writer holds while waiting for a header.
// The header is written as soon as fingerprinting settles -- a few frames in
// -- so reaching this cap means the run never wrote one, and the writer
// supplies its own rather than growing until it dies.
const HeldCap = 1 << 16

// Run is the half of the header known before the first frame: what charpy is,
// what it is pointed at, and how it will keep time.
type Run struct {
	CharpyVersion string
	CharpyCommit  string
	// Seed is the run seed, 6 to 16 lowercase hex digits. It is what makes a
	// case citation reproducible, so it is required even when charpy
	// generated it.
	Seed    string
	Mode    Mode
	Subject Subject
	Clock   clock.Mode
	// Peer identifies the reference peer that originated the stimulus. Nil
	// under relay, where the traffic is somebody else's and there is no peer
	// to name. ADR-004 promises a divergence table published today reproduces
	// a year from now, and it cannot without this: charpy_version says what
	// judged the run, not what spoke in it.
	Peer *Peer
	// Clients is the synthetic client population. Always 1 in v0; soak mode
	// drives many. Zero is read as 1.
	Clients int
}

// Subject is what is under test.
type Subject struct {
	Class      Class
	Descriptor string
}

// Peer is the reference peer charpy originated stimulus with: which SDK, at
// which version, configured for which era. Era is the revision charpy asked
// the peer to speak, which is not always the one the run negotiated -- a
// subject may refuse it, and a fault may rewrite it in flight. The negotiated
// revision is recorded separately, in Header.Revision.
type Peer struct {
	Module  string
	Version string
	Era     revision.Revision
}

// Header is the half of the header known only once the run has started: what
// revision the subject turned out to speak, and what charpy decided to run
// against it.
type Header struct {
	// Revision is nil when fingerprinting never settled. The frames each
	// carry their own revision either way.
	Revision     *Negotiation
	PolicyDigest string
	Cases        []string
}

// Negotiation is the outcome of the fingerprint ladder.
type Negotiation struct {
	Negotiated revision.Revision
	Offered    []revision.Revision
	How        How
}

// Link is the cross-face join key. Confidence is not a free parameter: three
// of the four regimes fix it, and the writer sets it from Via so that a
// transcript cannot claim authority a regime does not have. Only ViaInferred
// reads the caller's value.
type Link struct {
	CharpyID     string
	Via          Via
	Confidence   float64
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// HTTP is the transport detail of a frame that crossed HTTP. Header names are
// lowercased and values are passed through the run's redaction policy.
type HTTP struct {
	Status   int
	Headers  map[string]string
	SSEEvent string
	SSEID    string
}

// Fault records what charpy did to a frame. Nil on frames it did not tamper
// with, which is most of them.
type Fault struct {
	CaseID   string
	Citation string
	Kind     string
	Params   map[string]any

	// Replaced is the id the frame carried before charpy rewrote it, when the
	// replacement no longer carries the same one. It is how a reader tells
	// "the subject never answered" from "charpy replaced the answer": a
	// malformed_json rewrite leaves bytes that parse to no id at all, and
	// without this the oracle sees an outstanding request and blames the
	// subject for charpy's own doing. Absent when the rewrite kept the id.
	Replaced envelope.ID
	// ReplacedKind is what the replaced frame was. A request charpy destroyed
	// and an answer charpy destroyed owe the oracle different things: the
	// first means the subject was never asked, the second that it answered
	// and charpy hid the answer.
	ReplacedKind envelope.Kind
}

// Session is the protocol-level session, which exists only through
// 2025-11-25. Identity is charpy's label for the credential or tenant it
// presented, and is what the session-isolation invariant partitions on.
type Session struct {
	MCPSessionID string
	Identity     string
}

// Frame is one protocol frame crossing one face.
//
// The envelope columns -- kind, id, id_type, method, result_type, error_code,
// raw -- are not fields here. They are derived from Message, so a caller
// cannot describe a frame as something other than what its bytes say, and the
// transcript's rule that a malformed frame carries no parsed envelope holds
// by construction rather than by discipline.
type Frame struct {
	Face      Face
	Direction Direction
	Transport Transport
	ClientID  string
	SessionID string
	ConnID    string
	StreamID  string

	Message envelope.Message
	// Method is the ledger's echo onto a response or an error, which carries
	// no method of its own. Ignored on requests and notifications, which
	// carry theirs in Message.
	Method   string
	Revision revision.Revision

	HTTP    *HTTP
	Link    Link
	Fault   *Fault
	Session *Session
}

// Event is something that is not a frame but changes what the oracle should
// conclude.
type Event struct {
	Kind      EventKind
	Face      Face
	Transport Transport
	ClientID  string
	SessionID string
	ConnID    string
	StreamID  string

	Detail map[string]any
	Fault  *Fault
	Link   *Link
}

// CloseDetail builds the detail a stream_close event must carry.
func CloseDetail(reason CloseReason, bytesWritten int) map[string]any {
	return map[string]any{"reason": string(reason), "bytes_written": bytesWritten}
}

// AppliedDetail builds the detail a fault_applied event must carry. The
// direction is the way the frame the fault acted on was travelling, which
// names the fault's recipient: a damaged frame is a question put to whoever
// receives it, and the reaction layer judges that party
// (docs/design/decisions.md ADR-013).
func AppliedDetail(verb string, dir Direction) map[string]any {
	return map[string]any{"verb": verb, "direction": string(dir)}
}

// QuestionDetail builds the detail of a note recording a follow-up question
// that failed in charpy's own client: "harness" for a reader, as on any note,
// and the question and the error as keys of their own. A question that fails
// without crossing leaves no frame, so this note is the only record of why it
// was not asked. The reaction layer cites it in a finding's detail and never
// in a verdict.
func QuestionDetail(question string, err error) map[string]any {
	return map[string]any{
		"harness":  fmt.Sprintf("question %s: %v", question, err),
		"question": question,
		"error":    err.Error(),
	}
}

// ProbeDetail builds the detail a probe event must carry. Liveness is
// computed from these outcomes.
func ProbeDetail(method string, outcome ProbeOutcome, elapsed clock.Mono) map[string]any {
	return map[string]any{
		"method":          method,
		"outcome":         string(outcome),
		"elapsed_mono_ns": int64(elapsed),
	}
}

// Options configures a Writer.
type Options struct {
	Run Run
	// Sched is the injected clock, and is required: which clock is in force
	// is a property of the run, not a default (ADR-001).
	Sched clock.Sched
	// Wall defaults to real wall time, because wall time is the only kind
	// there is. Tests substitute a fixed one.
	Wall clock.Wall
	// RunID is generated when empty.
	RunID string
	// Redactor defaults to the standard policy. Pass NoRedaction to disable
	// it; the header then records redaction: "off".
	Redactor *Redactor
	// Buffer is the depth of the queue between the frame path and the file.
	Buffer int
}

// Writer writes a JSONL transcript.
//
// Writes are buffered and asynchronous: the frame path marshals a line, hands
// it to a queue and returns, so nothing waiting on the subject is also
// waiting on a disk. Sequence numbers are assigned under one lock at capture
// time and lines enter the queue in that order, so file order equals seq
// order, which the oracle relies on.
//
// A full queue applies backpressure rather than dropping lines. A transcript
// that quietly omits frames under load would break the guarantee every
// consumer reads it under, and slowing charpy down is the cheaper failure.
type Writer struct {
	mu     sync.Mutex
	seq    int64
	closed bool
	err    error
	lost   int

	// header is claimed by whoever writes the header line: WriteHeader,
	// Close, or the drain goroutine at the hold cap. It is atomic rather
	// than under mu because the drain must be able to claim it without
	// touching mu, and exactly one of the three may win.
	header atomic.Bool

	// The drain goroutine keeps its errors behind its own lock and never
	// touches mu. It must not: a producer holding mu can be blocked on a
	// full queue, and only the drain can empty it, so a drain waiting for mu
	// would wedge the run.
	dmu  sync.Mutex
	derr error
	dwr  int

	lines chan line
	done  chan struct{}

	runID     string
	run       Run
	sched     clock.Sched
	wall      clock.Wall
	redactor  *Redactor
	startWall time.Time
}

type line struct {
	seq int64
	b   []byte
}

// New starts a transcript writer over w. The caller retains ownership of w
// and should close it after Close returns.
func New(w io.Writer, opts Options) (*Writer, error) {
	if w == nil {
		return nil, errors.New("transcript: nil writer")
	}
	if opts.Sched == nil {
		return nil, errors.New("transcript: Options.Sched is required; a run declares its clock")
	}
	if err := validateRun(&opts.Run); err != nil {
		return nil, err
	}

	if opts.Wall == nil {
		opts.Wall = clock.RealWall()
	}
	if opts.Redactor == nil {
		opts.Redactor = NewRedactor()
	}
	if opts.RunID == "" {
		opts.RunID = NewRunID()
	}
	if opts.Buffer <= 0 {
		opts.Buffer = 1024
	}

	tw := &Writer{
		// Sequence numbers start at 1: seq 0 belongs to the header, which is
		// written once the run knows what revision it is talking to, by
		// which time some frames have already crossed the wire.
		seq:       1,
		lines:     make(chan line, opts.Buffer),
		done:      make(chan struct{}),
		runID:     opts.RunID,
		run:       opts.Run,
		sched:     opts.Sched,
		wall:      opts.Wall,
		redactor:  opts.Redactor,
		startWall: opts.Wall.Now(),
	}
	go tw.drain(w)
	return tw, nil
}

func validateRun(r *Run) error {
	if !seed.Valid(r.Seed) {
		return fmt.Errorf("transcript: seed %q must be 6 to 16 lowercase hex digits", r.Seed)
	}
	switch r.Mode {
	case ModeProxy, ModeHostileServer, ModeStdioIngress, ModeInproc:
	default:
		return fmt.Errorf("transcript: unknown mode %q", r.Mode)
	}
	switch r.Subject.Class {
	case ClassServer, ClassClient, ClassGateway:
	default:
		return fmt.Errorf("transcript: unknown subject class %q", r.Subject.Class)
	}
	switch r.Clock {
	case clock.ModeInjected, clock.ModeReal:
	default:
		return fmt.Errorf("transcript: unknown clock mode %q", r.Clock)
	}
	if r.CharpyVersion == "" {
		return errors.New("transcript: Run.CharpyVersion is required")
	}
	if r.Peer != nil {
		if r.Peer.Module == "" || r.Peer.Version == "" {
			return errors.New("transcript: Run.Peer needs both a module and a version")
		}
		if r.Peer.Era != "" && !revision.Known(r.Peer.Era) {
			return fmt.Errorf("transcript: peer era %q is not a revision charpy knows", r.Peer.Era)
		}
	}
	if r.Clients <= 0 {
		r.Clients = 1
	}
	return nil
}

// RunID is the id stamped on every line of this transcript.
func (w *Writer) RunID() string { return w.runID }

// Err reports the first error the writer hit. A transcript that lost a line
// is not a transcript anyone should draw conclusions from, so this is a
// harness failure -- exit code 2 -- not a warning.
func (w *Writer) Err() error {
	w.mu.Lock()
	err, lost := w.err, w.lost
	w.mu.Unlock()

	w.dmu.Lock()
	err, lost = errors.Join(err, w.derr), lost+w.dwr
	w.dmu.Unlock()

	if err == nil {
		return nil
	}
	if lost > 0 {
		return fmt.Errorf("%w (%d line(s) lost)", err, lost)
	}
	return err
}

// WriteHeader writes the header line, at seq 0, ahead of everything already
// captured. It may be called once.
func (w *Writer) WriteHeader(h Header) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return errors.New("transcript: writer is closed")
	}
	if err := validateHeader(h); err != nil {
		return err
	}
	if !w.header.CompareAndSwap(false, true) {
		return errors.New("transcript: header already written; exactly one is allowed")
	}
	w.lines <- line{seq: 0, b: w.headerBytes(h)}
	return nil
}

// Frame captures one frame.
func (w *Writer) Frame(f Frame) {
	l, err := w.FrameLine(f)
	if err != nil {
		if w.fail(err); refused(err) {
			return
		}
	}
	w.emit(&l, &l.Common)
}

// Event captures one event.
func (w *Writer) Event(e Event) {
	l, err := w.EventLine(e)
	if err != nil {
		if w.fail(err); refused(err) {
			return
		}
	}
	w.emit(&l, &l.Common)
}

// Close flushes the transcript and waits for the writer to finish. It does
// not close the underlying writer.
//
// A run that ended before it ever wrote a header gets one here, recording
// what charpy knew and how = "unknown", because a headerless transcript is
// not a valid prefix of anything -- it is a file no consumer can read.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return w.Err()
	}
	if w.header.CompareAndSwap(false, true) {
		w.err = errors.Join(w.err, errors.New(
			"transcript: run ended without a header; wrote one with no negotiated revision"))
		w.lines <- line{seq: 0, b: w.headerBytes(Header{})}
	}
	w.closed = true
	close(w.lines)
	w.mu.Unlock()

	<-w.done
	return w.Err()
}

// emit stamps the Common fields, assigns a sequence number and queues the
// line.
//
// Marshalling happens under the same lock that hands out the sequence number,
// which is what keeps queue order equal to seq order. It costs microseconds
// against a subject reasoning in milliseconds, and buys the one guarantee
// every consumer of the transcript relies on.
func (w *Writer) emit(v any, c *Common) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		w.lost++
		w.err = errors.Join(w.err, errors.New("transcript: line captured after close"))
		return
	}

	c.SchemaVersion = SchemaVersion
	c.RunID = w.runID
	c.Seq = w.seq
	c.TMonoNS = int64(w.sched.Now())
	c.TWall = w.wall.Now().UTC().Format(wallFormat)

	b, err := json.Marshal(v)
	if err != nil {
		// Nothing charpy builds should fail to marshal, but a caller-supplied
		// detail or params map can carry an unmarshalable value. Spending the
		// sequence number on a note keeps the sequence dense, which the
		// oracle relies on more than it relies on any one line.
		b = w.noteBytes(c, fmt.Sprintf("line %d dropped: %v", w.seq, err))
		w.lost++
		w.err = errors.Join(w.err, err)
	}

	w.seq++
	w.lines <- b2line(c.Seq, b)
}

func b2line(seq int64, b []byte) line { return line{seq: seq, b: b} }

// fail records a defect. A refusal also counts a lost line; a repair does
// not, because the line was written -- with the offending part left out.
//
// Refusing costs one frame. Writing a line that violates the schema costs
// every consumer's trust in the file, so the two are not close.
func (w *Writer) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if refused(err) {
		w.lost++
	}
	w.err = errors.Join(w.err, err)
}

// drain writes queued lines in sequence order.
//
// It holds everything until the header arrives, because the header is seq 0
// and the run cannot know what revision it negotiated until some frames have
// already crossed the wire. Lines enter the queue in seq order, so holding
// and releasing them preserves it.
func (w *Writer) drain(out io.Writer) {
	defer close(w.done)

	bw := bufio.NewWriterSize(out, 64<<10)
	var (
		held     [][]byte
		haveHead bool
	)

	write := func(b []byte) {
		if _, err := bw.Write(b); err != nil {
			w.drainFail(err)
			return
		}
		if err := bw.WriteByte('\n'); err != nil {
			w.drainFail(err)
		}
	}
	release := func(head []byte) {
		write(head)
		for _, b := range held {
			write(b)
		}
		held, haveHead = nil, true
	}

	for ln := range w.lines {
		switch {
		case haveHead:
			write(ln.b)
		case ln.seq == 0:
			release(ln.b)
		case len(held) >= HeldCap && w.header.CompareAndSwap(false, true):
			// The run never wrote a header and the hold is unbounded
			// otherwise. Claiming it here is what stops a producer from
			// writing a second one later.
			w.drainFail(fmt.Errorf("transcript: no header after %d lines; wrote one with no negotiated revision", HeldCap))
			release(w.headerBytes(Header{}))
			write(ln.b)
		default:
			held = append(held, ln.b)
		}

		// Flush whenever the queue is empty. Under load lines batch into
		// whole buffers; when idle -- which includes the moment just before
		// a subject wedges the run -- the file is current on disk.
		if len(w.lines) == 0 {
			if err := bw.Flush(); err != nil {
				w.drainFail(err)
			}
		}
	}

	if !haveHead && len(held) > 0 {
		// Unreachable: Close writes a header before closing the queue.
		release(w.headerBytes(Header{}))
	}
	if err := bw.Flush(); err != nil {
		w.drainFail(err)
	}
}

// drainFail records a write error. It is the drain goroutine's own path, and
// deliberately does not share a lock with the producers.
func (w *Writer) drainFail(err error) {
	w.dmu.Lock()
	defer w.dmu.Unlock()
	w.dwr++
	w.derr = errors.Join(w.derr, err)
}
