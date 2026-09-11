package wire_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"

	"github.com/serverplumber/charpy/internal/wire"
)

const frame = `{"jsonrpc":"2.0","id":7,"result":{}}`

func TestEncodeLine(t *testing.T) {
	got := wire.EncodeLine([]byte(frame))
	if string(got.Bytes) != frame+"\n" {
		t.Errorf("EncodeLine = %q", got.Bytes)
	}
}

// A deliberately malformed frame must reach the subject byte for byte; the
// encoder does not parse or re-serialise what it is given.
func TestEncodeLinePassesMalformedBytesThrough(t *testing.T) {
	const broken = `{"jsonrpc":"2.0","id":7,"result":{`
	got := wire.EncodeLine([]byte(broken))
	if string(got.Bytes) != broken+"\n" {
		t.Errorf("EncodeLine mangled a malformed frame: %q", got.Bytes)
	}
}

func TestEncodeEvent(t *testing.T) {
	got := wire.EncodeEventID("message", []byte(frame), 42)
	want := "event: message\ndata: " + frame + "\nid: 42\n\n"
	if string(got.Bytes) != want {
		t.Errorf("EncodeEvent =\n%q\nwant\n%q", got.Bytes, want)
	}
}

// 2026-07-28 removed resumability, so a stream on that revision carries no
// event ids at all.
func TestEncodeEventWithoutAnID(t *testing.T) {
	got := wire.EncodeEvent("message", []byte(frame), "")
	want := "event: message\ndata: " + frame + "\n\n"
	if string(got.Bytes) != want {
		t.Errorf("EncodeEvent = %q, want %q", got.Bytes, want)
	}
}

func TestEncodeComment(t *testing.T) {
	got := wire.EncodeComment("keep-alive")
	if string(got.Bytes) != ": keep-alive\n" {
		t.Errorf("EncodeComment = %q", got.Bytes)
	}
}

// The cut points are the parameter surface of the truncate mechanism, so each
// one is pinned to the exact bytes it delivers.
func TestCutPoints(t *testing.T) {
	event := wire.EncodeEventID("message", []byte(frame), 42)
	line := wire.EncodeLine([]byte(frame))
	comment := wire.EncodeComment("keep-alive")

	tests := []struct {
		name string
		enc  wire.Encoded
		at   wire.CutPoint
		opt  wire.CutOptions
		want string
	}{
		{
			name: "byte lands exactly where it is told",
			enc:  line, at: wire.CutByte, opt: wire.CutOptions{AfterBytes: 10},
			want: frame[:10],
		},
		{
			name: "mid_line withholds only the delimiter",
			enc:  line, at: wire.CutMidLine,
			want: frame,
		},
		{
			name: "mid_frame lands inside the body at the seeded offset",
			enc:  line, at: wire.CutMidFrame, opt: wire.CutOptions{Pick: at(9)},
			want: frame[:10],
		},
		{
			name: "field_boundary delivers the event field and no data field",
			enc:  event, at: wire.CutFieldBoundary,
			want: "event: message\n",
		},
		{
			name: "event_boundary delivers a whole, well-formed event",
			enc:  event, at: wire.CutEventBoundary,
			want: "event: message\ndata: " + frame + "\nid: 42\n\n",
		},
		{
			name: "mid_event lands inside the data value at the seeded offset",
			enc:  event, at: wire.CutMidEvent, opt: wire.CutOptions{Pick: at(9)},
			want: "event: message\ndata: " + frame[:10],
		},
		{
			name: "mid_comment lands inside the comment text at the seeded offset",
			enc:  comment, at: wire.CutMidComment, opt: wire.CutOptions{Pick: at(3)},
			want: ": keep",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.enc.Cut(tc.at, tc.opt)
			if err != nil {
				t.Fatalf("Cut: %v", err)
			}
			if got := string(tc.enc.Bytes[:n]); got != tc.want {
				t.Errorf("delivered %q, want %q", got, tc.want)
			}
		})
	}
}

// A cut point that cannot land is an error, never a silent fallback to a
// nearby one. A fault charpy cannot deliver is a case that does not apply;
// degrading it quietly would be a test that passes by not running.
func TestCutPointsThatDoNotApply(t *testing.T) {
	event := wire.EncodeEventID("message", []byte(frame), 42)
	line := wire.EncodeLine([]byte(frame))
	comment := wire.EncodeComment("x")

	tests := []struct {
		name string
		enc  wire.Encoded
		at   wire.CutPoint
	}{
		{"mid_line on an SSE event", event, wire.CutMidLine},
		{"mid_event on a stdio frame", line, wire.CutMidEvent},
		{"field_boundary on a stdio frame", line, wire.CutFieldBoundary},
		{"event_boundary on a comment", comment, wire.CutEventBoundary},
		{"mid_comment on an SSE event", event, wire.CutMidComment},
		{"field_boundary with no event field", wire.EncodeEvent("", []byte(frame), ""), wire.CutFieldBoundary},
		{"mid_comment with nothing to cut into", wire.EncodeComment("x"), wire.CutMidComment},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.enc.Cut(tc.at, wire.CutOptions{}); !errors.Is(err, wire.ErrNotApplicable) {
				t.Errorf("Cut = %v, want ErrNotApplicable", err)
			}
		})
	}
}

func TestCutByteBounds(t *testing.T) {
	line := wire.EncodeLine([]byte(frame))

	if _, err := line.Cut(wire.CutByte, wire.CutOptions{AfterBytes: line.Len()}); !errors.Is(err, wire.ErrNoTruncation) {
		t.Error("a cut covering every byte was accepted as a truncation")
	}
	if _, err := line.Cut(wire.CutByte, wire.CutOptions{AfterBytes: -1}); err == nil {
		t.Error("a negative after_bytes was accepted")
	}
	if _, err := line.Cut("sideways", wire.CutOptions{}); err == nil {
		t.Error("an unknown cut point was accepted")
	}
}

// Seeded cuts are how the run seed reaches the wire, which is what makes a
// case reproducible from its citation.
func TestSeededCutUsesThePicker(t *testing.T) {
	line := wire.EncodeLine([]byte(frame))

	first, err := line.Cut(wire.CutMidFrame, wire.CutOptions{Pick: func(int) int { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	last, err := line.Cut(wire.CutMidFrame, wire.CutOptions{Pick: func(n int) int { return n - 1 }})
	if err != nil {
		t.Fatal(err)
	}

	if first != 1 {
		t.Errorf("lowest seeded cut = %d, want 1: at least one byte must be delivered", first)
	}
	if last != len(frame)-1 {
		t.Errorf("highest seeded cut = %d, want %d: at least one byte must be withheld", last, len(frame)-1)
	}

	// A picker out of range is clamped rather than panicking on a slice.
	for _, pick := range []func(int) int{func(int) int { return -5 }, func(n int) int { return n * 10 }} {
		n, err := line.Cut(wire.CutMidFrame, wire.CutOptions{Pick: pick})
		if err != nil || n <= 0 || n >= line.Len() {
			t.Errorf("out-of-range picker produced %d, %v", n, err)
		}
	}
}

func TestStdioStream(t *testing.T) {
	var sink nopCloser
	s := wire.NewStdio(&sink)

	e := wire.EncodeLine([]byte(frame))
	n, err := e.Cut(wire.CutMidLine, wire.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.EmitCut(s, e, n); err != nil {
		t.Fatal(err)
	}

	if sink.String() != frame {
		t.Errorf("wrote %q, want the frame without its delimiter", sink.String())
	}
	if s.Written() != int64(len(frame)) {
		t.Errorf("Written() = %d, want %d", s.Written(), len(frame))
	}
}

func TestEmitCutRejectsAnOffsetOutOfRange(t *testing.T) {
	var sink nopCloser
	s := wire.NewStdio(&sink)
	e := wire.EncodeLine([]byte(frame))

	if _, err := wire.EmitCut(s, e, e.Len()+1); err == nil {
		t.Error("an offset past the end was accepted")
	}
	if _, err := wire.EmitCut(s, e, -1); err == nil {
		t.Error("a negative offset was accepted")
	}
}

// The real test of the SSE design: a cut has to be visible to an actual HTTP
// client. Without the flush the bytes would sit in a buffer and arrive whole
// when the handler returned, turning every stream case into a silent pass.
func TestSSECutReachesTheClient(t *testing.T) {
	event := wire.EncodeEventID("message", []byte(frame), 42)
	n, err := event.Cut(wire.CutMidEvent, wire.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := wire.NewSSE(w)
		if _, err := wire.EmitCut(s, event, n); err != nil {
			t.Errorf("EmitCut: %v", err)
		}
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// A buffering intermediary would deliver the event whole and the cut
	// would be a no-op.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := string(event.Bytes[:n])
	if string(body) != want {
		t.Errorf("client received %q, want exactly the cut prefix %q", body, want)
	}
	if strings.HasSuffix(string(body), "\n\n") {
		t.Error("the client received a complete event; the cut did not take")
	}
}

// Over HTTP/1.1 both capabilities are there.
func TestSSECapabilitiesOverHTTP11(t *testing.T) {
	got := make(chan capabilities, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := wire.NewSSE(w)
		var c capabilities
		c.hold = s.Hold()
		_, _, c.raw = s.Raw()
		got <- c
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
	}

	c := <-got
	if c.hold != nil {
		t.Errorf("Hold() over HTTP/1.1 = %v, want nil", c.hold)
	}
	if c.raw != nil {
		t.Errorf("Raw() over HTTP/1.1 = %v, want nil", c.raw)
	}
}

// capabilities carries a handler's findings back to the test goroutine. The
// handler outlives the client's response, so a shared variable would be a
// race rather than a result.
type capabilities struct{ hold, raw error }

// And over HTTP/2 hijacking is gone, which is why the capability is probed
// rather than assumed. The specification mandates no HTTP version, so this is
// a transport a subject may really present.
func TestSSERawIsUnsupportedOverHTTP2(t *testing.T) {
	got := make(chan capabilities, 1)
	proto := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto <- r.Proto
		s := wire.NewSSE(w)
		var c capabilities
		c.hold = s.Hold()
		_, _, c.raw = s.Raw()
		got <- c
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
	}

	if p := <-proto; !strings.HasPrefix(p, "HTTP/2") {
		t.Fatalf("test served %s, wanted HTTP/2", p)
	}
	c := <-got
	if !errors.Is(c.raw, http.ErrNotSupported) {
		t.Errorf("Raw() over HTTP/2 = %v, want ErrNotSupported", c.raw)
	}
	// Holding a response, unlike hijacking it, survives the upgrade -- so a
	// hang works on both transports and only the reset-style faults are
	// scoped to HTTP/1.1.
	if c.hold != nil {
		t.Errorf("Hold() over HTTP/2 = %v, want nil", c.hold)
	}
}

// at returns a picker that always chooses offset i, so a test states the cut
// it wants rather than restating the implementation's own arithmetic.
func at(i int) func(int) int { return func(int) int { return i } }

// Whatever the seed picks, the cut must leave at least one byte on each side:
// a "mid" cut that delivered nothing, or everything, would not be one.
func TestSeededCutsLandStrictlyInsideTheBody(t *testing.T) {
	cases := []struct {
		name string
		enc  wire.Encoded
		at   wire.CutPoint
		body [2]int
	}{
		{"mid_frame", wire.EncodeLine([]byte(frame)), wire.CutMidFrame, [2]int{0, len(frame)}},
		{"mid_comment", wire.EncodeComment("keep-alive"), wire.CutMidComment, [2]int{2, 12}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 40 {
				n, err := tc.enc.Cut(tc.at, wire.CutOptions{Pick: at(i)})
				if err != nil {
					t.Fatalf("pick %d: %v", i, err)
				}
				if n <= tc.body[0] || n >= tc.body[1] {
					t.Errorf("pick %d cut at %d, outside the body (%d, %d)", i, n, tc.body[0], tc.body[1])
				}
			}
		})
	}
}

type nopCloser struct {
	strings.Builder
	closed bool
}

func (n *nopCloser) Write(b []byte) (int, error) { return n.Builder.Write(b) }
func (n *nopCloser) Close() error                { n.closed = true; return nil }

// A stall is a thing charpy does, not a call it omits. That it is a method is
// what lets a test assert on it at all.
func TestStallHoldsTheStreamOpen(t *testing.T) {
	var sink nopCloser
	s := wire.NewStdio(&sink)

	st, err := wire.Stall(s, wire.StallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sink.closed {
		t.Error("stalling closed the stream")
	}
	if got := sink.String(); got != "" {
		t.Errorf("a silent stall wrote %q", got)
	}

	// Ending a stall withdraws the fault: the stream stays open and normal
	// service resumes on it.
	if err := st.End(); err != nil {
		t.Fatal(err)
	}
	if sink.closed {
		t.Error("ending a stall closed the stream; withdrawal is not a teardown")
	}
	if _, err := wire.Emit(s, wire.EncodeLine([]byte(frame))); err != nil {
		t.Fatalf("the stream was unusable after withdrawal: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if !sink.closed {
		t.Error("Close did not close the stream")
	}
}

// The other half of the keepalive parameter: comments keep arriving while no
// data ever does, so the stream looks healthy the whole time.
func TestStallWithCommentsKeepsTheStreamLookingAlive(t *testing.T) {
	srv, body, done := sseRecorder(t, func(s *wire.SSE, wall *clock.FixedWall) {
		st, err := wire.Stall(s, wire.StallOptions{
			Keepalive: wire.KeepaliveComments,
			Every:     5 * time.Second,
			Comment:   "keep-alive",
			Wall:      wall,
		})
		if err != nil {
			t.Errorf("Stall: %v", err)
			return
		}
		wall.Advance(16 * time.Second) // three cadences
		if got := st.Comments(); got != 3 {
			t.Errorf("sent %d keep-alives over three cadences, want 3", got)
		}
		if err := st.End(); err != nil {
			t.Errorf("End: %v", err)
		}
		// Nothing further once the stall is over.
		wall.Advance(time.Minute)
		if got := st.Comments(); got != 3 {
			t.Errorf("sent %d keep-alives after End, want 3", got)
		}
	})
	defer srv.Close()
	<-done

	if got := body(); got != ": keep-alive\n: keep-alive\n: keep-alive\n" {
		t.Errorf("client received %q, want three keep-alive comments and no data", got)
	}
}

// Comments are HTTP only. Asking for them on stdio is a case that does not
// apply, not a quiet downgrade to silence.
func TestStallRejectsCommentsWhereThereAreNone(t *testing.T) {
	var sink nopCloser
	_, err := wire.Stall(wire.NewStdio(&sink), wire.StallOptions{
		Keepalive: wire.KeepaliveComments,
		Wall:      clock.NewFixedWall(time.Now()),
	})
	if !errors.Is(err, wire.ErrNotApplicable) {
		t.Errorf("Stall = %v, want ErrNotApplicable", err)
	}
}

// Keep-alives run on real time, never the injected clock: they exist to stop
// the subject's timer firing, and the subject's deadlines are real.
func TestStallWithCommentsNeedsAWallClock(t *testing.T) {
	srv, _, done := sseRecorder(t, func(s *wire.SSE, _ *clock.FixedWall) {
		if _, err := wire.Stall(s, wire.StallOptions{Keepalive: wire.KeepaliveComments}); err == nil {
			t.Error("keep-alive comments were accepted with no clock to pace them")
		}
	})
	defer srv.Close()
	<-done
}

// sseRecorder runs fn as an SSE handler and returns the body the client saw.
func sseRecorder(t *testing.T, fn func(*wire.SSE, *clock.FixedWall)) (*httptest.Server, func() string, chan struct{}) {
	t.Helper()

	done := make(chan struct{})
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fn(wire.NewSSE(w), clock.NewFixedWall(time.Unix(0, 0)))
	}))

	go func() {
		defer close(done)
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Errorf("get: %v", err)
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		got = string(b)
	}()

	return srv, func() string { return got }, done
}
