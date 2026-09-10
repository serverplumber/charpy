package transcript_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

var runStart = time.Date(2026, 9, 8, 14, 3, 10, 0, time.UTC)

type rig struct {
	w     *transcript.Writer
	buf   *bytes.Buffer
	sched *clock.Injected
	wall  *clock.FixedWall
}

func newRig(t *testing.T, mutate ...func(*transcript.Options)) *rig {
	t.Helper()

	r := &rig{
		buf:   &bytes.Buffer{},
		sched: clock.NewInjected(),
		wall:  clock.NewFixedWall(runStart),
	}
	opts := transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "0.1.0-dev",
			CharpyCommit:  "abc1234",
			Seed:          "8f2c1a",
			Mode:          transcript.ModeProxy,
			Subject: transcript.Subject{
				Class:      transcript.ClassGateway,
				Descriptor: "http://localhost:8080/mcp",
			},
			Clock: clock.ModeInjected,
		},
		Sched: r.sched,
		Wall:  r.wall,
		RunID: "01JBW3K9F2Q7XN4TCHARPY01",
	}
	for _, m := range mutate {
		m(&opts)
	}

	w, err := transcript.New(r.buf, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.w = w
	return r
}

// header writes a representative header.
func (r *rig) header(t *testing.T) {
	t.Helper()
	err := r.w.WriteHeader(transcript.Header{
		Revision: &transcript.Negotiation{
			Negotiated: revision.V20251125,
			Offered:    []revision.Revision{revision.V20260728, revision.V20251125},
			How:        transcript.HowInitialize,
		},
		PolicyDigest: "sha256:1f0d2c",
		Cases:        []string{"stream/truncate-mid-event@2025-11-25#seed=8f2c1a"},
	})
	if err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
}

// close finishes the transcript and returns its lines.
func (r *rig) close(t *testing.T) []string {
	t.Helper()
	if err := r.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return splitLines(r.buf.String())
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, line)
	}
	return m
}

func validateAll(t *testing.T, sch *jsonschema.Schema, lines []string) {
	t.Helper()
	for i, line := range lines {
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(line))
		if err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("line %d does not validate against the transcript schema:\n%s\n%v", i+1, line, err)
		}
	}
}

func req(t *testing.T, id int64, method string) envelope.Message {
	t.Helper()
	m, err := envelope.NewRequest(envelope.NumberID(id), method, json.RawMessage(`{"name":"search"}`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The writer's own output is held to the same schema the hand-written golden
// transcripts are. Nothing else in the package matters if this fails.
func TestWrittenLinesValidateAgainstTheSchema(t *testing.T) {
	sch := compileSchema(t)
	r := newRig(t)

	r.w.Event(transcript.Event{
		Kind:      transcript.ConnOpen,
		Face:      transcript.Downstream,
		Transport: transcript.TransportHTTP,
		ConnID:    "c-04",
		Detail:    map[string]any{"remote": "127.0.0.1:54112"},
	})
	r.sched.Advance(time.Millisecond)

	r.header(t)

	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportHTTP,
		ClientID: "c0", SessionID: "s-4f2a", ConnID: "c-04", StreamID: "s-11",
		Message:  req(t, 7, "tools/call"),
		Revision: revision.V20251125,
		HTTP: &transcript.HTTP{
			Headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer hunter2",
			},
		},
		Link: transcript.Link{
			CharpyID: "c7f1a2",
			Via:      transcript.ViaForwarded,
			TraceID:  "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:   "00f067aa0ba902b7",
		},
		Session: &transcript.Session{MCPSessionID: "sess-aaa", Identity: "tenant-a"},
	})
	r.sched.Advance(time.Millisecond)

	// A frame charpy truncated: no envelope, every byte kept.
	cut, _ := envelope.Parse([]byte(`event: message` + "\n" + `data: {"jsonrpc":"2.0","id":7,"result":{`))
	fault := &transcript.Fault{
		CaseID:   "stream/truncate-mid-event",
		Citation: "stream/truncate-mid-event@2025-11-25#seed=8f2c1a",
		Kind:     "truncate",
		Params:   map[string]any{"cut_at": "mid_event", "after_bytes": 91},
	}
	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.S2C, Transport: transcript.TransportHTTP,
		ClientID: "c0", SessionID: "s-4f2a", ConnID: "c-04", StreamID: "s-11",
		Message:  cut,
		Revision: revision.V20251125,
		HTTP: &transcript.HTTP{
			Status:   200,
			Headers:  map[string]string{"content-type": "text/event-stream"},
			SSEEvent: "message",
			SSEID:    "42",
		},
		Link:  transcript.Link{CharpyID: "c7f1a2", Via: transcript.ViaForwarded},
		Fault: fault,
	})

	r.w.Event(transcript.Event{
		Kind: transcript.StreamClose, Face: transcript.Downstream, Transport: transcript.TransportHTTP,
		ConnID: "c-04", StreamID: "s-11",
		Detail: transcript.CloseDetail(transcript.CharpyClose, 91),
		Fault:  fault,
	})
	r.w.Event(transcript.Event{
		Kind: transcript.FaultWithdrawn, Face: transcript.Downstream, Fault: fault,
	})
	r.w.Event(transcript.Event{
		Kind: transcript.Probe, Face: transcript.Downstream, Transport: transcript.TransportHTTP,
		Detail: transcript.ProbeDetail("ping", transcript.ProbeOK, clock.FromMillis(120)),
	})

	// A stdio frame, an error frame with a ledger-resolved method, and an
	// inferred join: the shapes the schema constrains differently.
	errFrame, err := envelope.NewError(envelope.StringID("anon-91"), -32020, "HeaderMismatch", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.w.Frame(transcript.Frame{
		Face: transcript.Upstream, Direction: transcript.S2C, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-4f2a", ConnID: "c-05",
		Message:  errFrame,
		Method:   "tools/call",
		Revision: revision.V20251125,
		Link:     transcript.Link{CharpyID: "c7f1a2", Via: transcript.ViaInferred, Confidence: 0.8},
	})

	lines := r.close(t)
	if len(lines) != 8 {
		t.Fatalf("wrote %d lines, want 8", len(lines))
	}
	validateAll(t, sch, lines)
}

// Guarantee 1: file order equals seq order, seq is dense from 0, and the
// header is line one even though it was written after the run began.
func TestHeaderIsFirstAndSequenceIsDense(t *testing.T) {
	r := newRig(t)

	for i := range 5 {
		r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"n": i}})
	}
	r.header(t)
	for i := range 5 {
		r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"n": 5 + i}})
	}

	lines := r.close(t)
	if len(lines) != 11 {
		t.Fatalf("wrote %d lines, want 11", len(lines))
	}

	for i, line := range lines {
		m := decode(t, line)
		if got := int(m["seq"].(float64)); got != i {
			t.Errorf("line %d has seq %d, want %d", i+1, got, i)
		}
		if i == 0 && m["type"] != "header" {
			t.Errorf("first line is %q, want header", m["type"])
		}
		if i > 0 && m["type"] == "header" {
			t.Errorf("line %d is a second header", i+1)
		}
	}

	// The notes captured before the header keep the order they were captured
	// in, immediately after it.
	for i := range 10 {
		m := decode(t, lines[i+1])
		if got := int(m["detail"].(map[string]any)["n"].(float64)); got != i {
			t.Errorf("line %d carries note %d, want %d", i+2, got, i)
		}
	}
}

// The header describes the run, so it carries the run's start, not the
// moment it was written -- otherwise the file's first line sorts after its
// second on the clock every consumer orders by.
func TestHeaderCarriesRunStart(t *testing.T) {
	r := newRig(t)
	r.sched.Advance(5 * time.Second)
	r.wall.Advance(5 * time.Second)
	r.header(t)

	m := decode(t, r.close(t)[0])
	if got := m["t_mono_ns"].(float64); got != 0 {
		t.Errorf("header t_mono_ns = %v, want 0", got)
	}
	if got := m["t_wall"].(string); got != "2026-09-08T14:03:10.000000000Z" {
		t.Errorf("header t_wall = %q, want the run start", got)
	}
}

func TestFrameColumnsAreDerivedFromTheBytes(t *testing.T) {
	tests := []struct {
		name   string
		msg    func(*testing.T) envelope.Message
		method string
		want   map[string]any
	}{
		{
			name: "request",
			msg:  func(t *testing.T) envelope.Message { return req(t, 7, "tools/call") },
			want: map[string]any{"kind": "request", "id": "7", "id_type": "number", "method": "tools/call"},
		},
		{
			name: "string id",
			msg: func(t *testing.T) envelope.Message {
				m, err := envelope.NewRequest(envelope.StringID("7"), "ping", nil)
				if err != nil {
					t.Fatal(err)
				}
				return m
			},
			// The same id column as the number 7, distinguished by id_type.
			want: map[string]any{"kind": "request", "id": "7", "id_type": "string"},
		},
		{
			name: "null id is not the string null",
			msg: func(t *testing.T) envelope.Message {
				m, _ := envelope.Parse([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"x"}}`))
				return m
			},
			want: map[string]any{"kind": "error", "id": nil, "id_type": "null", "error_code": float64(-32700)},
		},
		{
			name: "notification has no id member",
			msg: func(t *testing.T) envelope.Message {
				m, err := envelope.NewNotification("notifications/initialized", nil)
				if err != nil {
					t.Fatal(err)
				}
				return m
			},
			want: map[string]any{"kind": "notification", "id": nil, "id_type": "absent", "method": "notifications/initialized"},
		},
		{
			name: "malformed keeps its bytes and nothing else",
			msg: func(t *testing.T) envelope.Message {
				m, _ := envelope.Parse([]byte(`{"jsonrpc":"2.0","id":7,"result":{`))
				return m
			},
			// The method the caller passed is ignored: a frame with no
			// envelope cannot claim one.
			method: "tools/call",
			want: map[string]any{
				"kind": "malformed", "id": nil, "id_type": "absent",
				"method": nil, "result_type": nil, "error_code": nil,
			},
		},
		{
			name: "the ledger's method echo lands on a response",
			msg: func(t *testing.T) envelope.Message {
				m, err := envelope.NewResponse(envelope.NumberID(7), json.RawMessage(`{"resultType":"complete"}`))
				if err != nil {
					t.Fatal(err)
				}
				return m
			},
			method: "tools/call",
			want:   map[string]any{"kind": "response", "method": "tools/call", "result_type": "complete"},
		},
		{
			name: "an unresolved response carries no method",
			msg: func(t *testing.T) envelope.Message {
				m, err := envelope.NewResponse(envelope.NumberID(7), nil)
				if err != nil {
					t.Fatal(err)
				}
				return m
			},
			want: map[string]any{"kind": "response", "method": nil, "result_type": nil},
		},
		{
			name: "a resultType outside the enum stays in the raw bytes",
			msg: func(t *testing.T) envelope.Message {
				m, err := envelope.NewResponse(envelope.NumberID(7), json.RawMessage(`{"resultType":"banana"}`))
				if err != nil {
					t.Fatal(err)
				}
				return m
			},
			want: map[string]any{"kind": "response", "result_type": nil},
		},
	}

	sch := compileSchema(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.header(t)
			msg := tc.msg(t)
			r.w.Frame(transcript.Frame{
				Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
				ClientID: "c0", SessionID: "s1",
				Message: msg, Method: tc.method,
				Link: transcript.Link{Via: transcript.ViaNone},
			})

			lines := r.close(t)
			validateAll(t, sch, lines)

			m := decode(t, lines[1])
			for k, want := range tc.want {
				if got := m[k]; got != want {
					t.Errorf("%s = %#v, want %#v", k, got, want)
				}
			}
			raw, err := base64.StdEncoding.DecodeString(m["raw"].(string))
			if err != nil {
				t.Fatalf("raw is not base64: %v", err)
			}
			if !bytes.Equal(raw, msg.Raw()) {
				t.Errorf("raw decodes to %q, want the exact frame bytes %q", raw, msg.Raw())
			}
		})
	}
}

// Guarantee 2: every byte appears in exactly one raw, except beyond the cap
// where raw_truncated marks it and raw_len still tells the truth.
func TestRawIsCappedButRawLenIsNot(t *testing.T) {
	sch := compileSchema(t)
	r := newRig(t)
	r.header(t)

	blob := strings.Repeat("x", transcript.RawCap)
	msg, err := envelope.NewResponse(envelope.NumberID(1), json.RawMessage(`{"blob":"`+blob+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.S2C, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s1", Message: msg,
		Link: transcript.Link{Via: transcript.ViaNone},
	})

	lines := r.close(t)
	validateAll(t, sch, lines)

	m := decode(t, lines[1])
	if m["raw_truncated"] != true {
		t.Error("raw_truncated is not set on an oversized frame")
	}
	if got := int(m["raw_len"].(float64)); got != msg.Len() {
		t.Errorf("raw_len = %d, want the true length %d", got, msg.Len())
	}
	raw, _ := base64.StdEncoding.DecodeString(m["raw"].(string))
	if len(raw) != transcript.RawCap {
		t.Errorf("raw holds %d bytes, want the cap %d", len(raw), transcript.RawCap)
	}
	if !bytes.Equal(raw, msg.Raw()[:transcript.RawCap]) {
		t.Error("raw is not the leading bytes of the frame")
	}
}

func TestRedaction(t *testing.T) {
	headers := map[string]string{
		"Authorization":  "Bearer hunter2",
		"Cookie":         "session=abc",
		"Content-Type":   "application/json",
		"Mcp-Session-Id": "sess-aaa",
	}

	t.Run("on by default", func(t *testing.T) {
		r := newRig(t)
		r.header(t)
		writeHTTPFrame(t, r, headers)

		got := decode(t, r.close(t)[1])["http"].(map[string]any)["headers"].(map[string]any)
		if got["authorization"] != transcript.Digest("Bearer hunter2") {
			t.Errorf("authorization = %v, want a digest", got["authorization"])
		}
		if got["cookie"] != transcript.Digest("session=abc") {
			t.Errorf("cookie = %v, want a digest", got["cookie"])
		}
		if got["content-type"] != "application/json" {
			t.Errorf("content-type = %v, want it in the clear", got["content-type"])
		}
		// The session id is carried in the clear in session.mcp_session_id,
		// so redacting the header while printing the field would be
		// incoherent rather than safe.
		if got["mcp-session-id"] != "sess-aaa" {
			t.Errorf("mcp-session-id = %v, want it in the clear", got["mcp-session-id"])
		}
		if _, ok := got["Authorization"]; ok {
			t.Error("header names are not lowercased")
		}
	})

	t.Run("off records itself in the header", func(t *testing.T) {
		r := newRig(t, func(o *transcript.Options) { o.Redactor = transcript.NoRedaction() })
		r.header(t)
		writeHTTPFrame(t, r, headers)

		lines := r.close(t)
		if decode(t, lines[0])["redaction"] != "off" {
			t.Error("a transcript written without redaction must say so in its header")
		}
		got := decode(t, lines[1])["http"].(map[string]any)["headers"].(map[string]any)
		if got["authorization"] != "Bearer hunter2" {
			t.Errorf("authorization = %v, want the raw value", got["authorization"])
		}
	})

	t.Run("the same secret digests the same on both faces", func(t *testing.T) {
		// This is the whole basis of invariant I5: equality of digests is
		// equality of exact bytes.
		if transcript.Digest("Bearer hunter2") != transcript.Digest("Bearer hunter2") {
			t.Fatal("digests are not stable")
		}
		if transcript.Digest("Bearer hunter2") == transcript.Digest("bearer hunter2") {
			t.Error("digests collide across different values")
		}
	})
}

func writeHTTPFrame(t *testing.T, r *rig, headers map[string]string) {
	t.Helper()
	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportHTTP,
		ClientID: "c0", SessionID: "s1",
		Message: req(t, 1, "ping"),
		HTTP:    &transcript.HTTP{Headers: headers},
		Link:    transcript.Link{Via: transcript.ViaNone},
	})
}

// Confidence is a property of the regime, not a free parameter, so a
// transcript cannot claim authority a guess does not have.
func TestConfidenceComesFromTheRegime(t *testing.T) {
	tests := []struct {
		via  transcript.Via
		give float64
		want float64
	}{
		{transcript.ViaForwarded, 0.2, 1.0},
		{transcript.ViaTraced, 0, 1.0},
		{transcript.ViaNone, 0.9, 0.0},
		{transcript.ViaInferred, 0.75, 0.75},
	}

	for _, tc := range tests {
		t.Run(string(tc.via), func(t *testing.T) {
			r := newRig(t)
			r.header(t)
			r.w.Frame(transcript.Frame{
				Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
				ClientID: "c0", SessionID: "s1", Message: req(t, 1, "ping"),
				Link: transcript.Link{Via: tc.via, Confidence: tc.give},
			})

			link := decode(t, r.close(t)[1])["link"].(map[string]any)
			if got := link["confidence"].(float64); got != tc.want {
				t.Errorf("confidence = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInferredJoinCannotClaimCertainty(t *testing.T) {
	sch := compileSchema(t)
	r := newRig(t)
	r.header(t)
	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s1", Message: req(t, 1, "ping"),
		Link: transcript.Link{CharpyID: "c7f1a2", Via: transcript.ViaInferred, Confidence: 1.0},
	})

	if err := r.w.Close(); err == nil {
		t.Error("an inferred join claiming confidence 1.0 was accepted silently")
	}
	lines := splitLines(r.buf.String())
	validateAll(t, sch, lines)

	// Downgraded to the regime that claims nothing, rather than given a
	// confidence charpy invented.
	link := decode(t, lines[1])["link"].(map[string]any)
	if link["via"] != string(transcript.ViaNone) {
		t.Errorf("via = %v, want none", link["via"])
	}
	if got := link["confidence"].(float64); got != 0 {
		t.Errorf("confidence = %v, want 0", got)
	}
	if link["charpy_id"] != nil {
		t.Errorf("charpy_id = %v; a disclaimed join must not offer a key", link["charpy_id"])
	}
}

// A line that cannot be made valid is refused. The sequence stays dense, the
// error survives to Close, and the run fails as a harness error rather than
// producing a transcript nobody can trust.
func TestUnwritableLinesAreRefusedNotMangled(t *testing.T) {
	sch := compileSchema(t)

	tests := []struct {
		name  string
		write func(*transcript.Writer)
	}{
		{"frame with no client_id", func(w *transcript.Writer) {
			w.Frame(transcript.Frame{
				Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
				SessionID: "s1", Link: transcript.Link{Via: transcript.ViaNone},
			})
		}},
		{"frame with no session_id", func(w *transcript.Writer) {
			w.Frame(transcript.Frame{
				Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
				ClientID: "c0", Link: transcript.Link{Via: transcript.ViaNone},
			})
		}},
		{"frame with no face", func(w *transcript.Writer) {
			w.Frame(transcript.Frame{
				Direction: transcript.C2S, Transport: transcript.TransportStdio,
				ClientID: "c0", SessionID: "s1", Link: transcript.Link{Via: transcript.ViaNone},
			})
		}},
		{"stream_close with no reason", func(w *transcript.Writer) {
			w.Event(transcript.Event{Kind: transcript.StreamClose, Detail: map[string]any{"bytes_written": 10}})
		}},
		{"probe with no outcome", func(w *transcript.Writer) {
			w.Event(transcript.Event{Kind: transcript.Probe, Detail: map[string]any{"method": "ping"}})
		}},
		{"fault event that names no case", func(w *transcript.Writer) {
			w.Event(transcript.Event{Kind: transcript.FaultApplied})
		}},
		{"unknown event kind", func(w *transcript.Writer) {
			w.Event(transcript.Event{Kind: "exploded"})
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.header(t)
			tc.write(r.w)
			r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"after": true}})

			if err := r.w.Close(); err == nil {
				t.Fatal("Close reported success after refusing a line")
			}

			lines := splitLines(r.buf.String())
			validateAll(t, sch, lines)
			if len(lines) != 2 {
				t.Fatalf("wrote %d lines, want 2 (header and the good note)", len(lines))
			}
			for i, line := range lines {
				if got := int(decode(t, line)["seq"].(float64)); got != i {
					t.Errorf("line %d has seq %d; a refused line must not spend a sequence number", i+1, got)
				}
			}
		})
	}
}

// A repairable defect costs the offending part, never the frame: the bytes
// are what the transcript exists to preserve.
func TestRepairableDefectsKeepTheFrame(t *testing.T) {
	sch := compileSchema(t)

	tests := []struct {
		name  string
		frame transcript.Frame
		check func(*testing.T, map[string]any)
	}{
		{
			name: "http detail on a stdio frame",
			frame: transcript.Frame{
				Transport: transcript.TransportStdio,
				HTTP:      &transcript.HTTP{Status: 200},
			},
			check: func(t *testing.T, m map[string]any) {
				if m["http"] != nil {
					t.Errorf("http = %v, want null on stdio", m["http"])
				}
			},
		},
		{
			name: "protocol session on a stateless revision",
			frame: transcript.Frame{
				Transport: transcript.TransportStdio,
				Revision:  revision.V20260728,
				Session:   &transcript.Session{MCPSessionID: "sess-aaa"},
			},
			check: func(t *testing.T, m map[string]any) {
				if m["session"] != nil {
					t.Errorf("session = %v, want null where the protocol has none", m["session"])
				}
			},
		},
		{
			name: "a trace id that is not one",
			frame: transcript.Frame{
				Transport: transcript.TransportStdio,
				Link:      transcript.Link{Via: transcript.ViaTraced, TraceID: "not-a-trace-id"},
			},
			check: func(t *testing.T, m map[string]any) {
				if got := m["link"].(map[string]any)["trace_id"]; got != nil {
					t.Errorf("trace_id = %v, want null", got)
				}
			},
		},
		{
			name: "a fault whose case id is not one",
			frame: transcript.Frame{
				Transport: transcript.TransportStdio,
				Fault:     &transcript.Fault{CaseID: "Not A Case", Citation: "x", Kind: "truncate"},
			},
			check: func(t *testing.T, m map[string]any) {
				if m["fault"] != nil {
					t.Errorf("fault = %v, want null", m["fault"])
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.header(t)

			f := tc.frame
			f.Face, f.Direction = transcript.Downstream, transcript.C2S
			f.ClientID, f.SessionID = "c0", "s1"
			f.Message = req(t, 1, "ping")
			r.w.Frame(f)

			if err := r.w.Close(); err == nil {
				t.Error("Close reported success after repairing a line")
			}

			lines := splitLines(r.buf.String())
			validateAll(t, sch, lines)
			if len(lines) != 2 {
				t.Fatalf("wrote %d lines, want 2: the frame must survive", len(lines))
			}
			m := decode(t, lines[1])
			if m["raw"] == "" {
				t.Error("the frame lost its bytes")
			}
			tc.check(t, m)
		})
	}
}

// A run that dies before fingerprinting settles still leaves a readable file.
// Zero lines is a valid prefix; a file whose first line is not a header is
// not.
func TestCloseWithoutAHeaderSuppliesOne(t *testing.T) {
	sch := compileSchema(t)
	r := newRig(t)
	r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"phase": "fingerprinting"}})

	if err := r.w.Close(); err == nil {
		t.Error("Close reported success on a run that never wrote a header")
	}

	lines := splitLines(r.buf.String())
	validateAll(t, sch, lines)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	m := decode(t, lines[0])
	if m["type"] != "header" {
		t.Fatalf("first line is %q, want header", m["type"])
	}
	if _, ok := m["revision"]; ok {
		t.Error("a synthesised header must not claim a negotiated revision")
	}
	if m["seed"] != "8f2c1a" || m["mode"] != "proxy" {
		t.Error("the synthesised header lost what charpy did know")
	}
}

func TestWriteHeaderIsOnceAndValidated(t *testing.T) {
	r := newRig(t)
	r.header(t)

	if err := r.w.WriteHeader(transcript.Header{}); err == nil {
		t.Error("a second header was accepted")
	}
	if err := r.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r2 := newRig(t)
	err := r2.w.WriteHeader(transcript.Header{
		Revision: &transcript.Negotiation{Negotiated: "2027-01-01", How: transcript.HowInitialize},
	})
	if err == nil {
		t.Error("a header naming a revision charpy does not know was accepted")
	}
	_ = r2.w.Close()
}

func TestNewValidatesTheRun(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*transcript.Options)
	}{
		{"no seed", func(o *transcript.Options) { o.Run.Seed = "" }},
		{"seed that is not hex", func(o *transcript.Options) { o.Run.Seed = "zzzzzz" }},
		{"unknown mode", func(o *transcript.Options) { o.Run.Mode = "sideways" }},
		{"unknown subject class", func(o *transcript.Options) { o.Run.Subject.Class = "proxy" }},
		{"unknown clock mode", func(o *transcript.Options) { o.Run.Clock = "frozen" }},
		{"no version", func(o *transcript.Options) { o.Run.CharpyVersion = "" }},
		{"no clock", func(o *transcript.Options) { o.Sched = nil }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := transcript.Options{
				Run: transcript.Run{
					CharpyVersion: "0.1.0-dev", Seed: "8f2c1a", Mode: transcript.ModeProxy,
					Subject: transcript.Subject{Class: transcript.ClassGateway}, Clock: clock.ModeInjected,
				},
				Sched: clock.NewInjected(),
			}
			tc.mutate(&opts)
			if _, err := transcript.New(&bytes.Buffer{}, opts); err == nil {
				t.Error("New accepted a run it should have refused")
			}
		})
	}
}

// The interposer writes from every connection goroutine at once. File order
// must still equal seq order, which is what the whole single-lock design is
// for. Meaningful under -race.
func TestConcurrentWritesStayInSequence(t *testing.T) {
	r := newRig(t, func(o *transcript.Options) { o.Buffer = 4 })
	r.header(t)

	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				r.w.Frame(transcript.Frame{
					Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
					ClientID: "c0", SessionID: "s1",
					Message: req(t, int64(w*each+i), "ping"),
					Link:    transcript.Link{Via: transcript.ViaNone},
				})
			}
		}()
	}
	wg.Wait()

	lines := r.close(t)
	if len(lines) != writers*each+1 {
		t.Fatalf("wrote %d lines, want %d", len(lines), writers*each+1)
	}
	for i, line := range lines {
		if got := int(decode(t, line)["seq"].(float64)); got != i {
			t.Fatalf("line %d has seq %d, want %d: file order must equal seq order", i+1, got, i)
		}
	}
}

// A small queue must slow charpy down, never drop lines. Every consumer reads
// the transcript under a completeness guarantee.
func TestABlockedQueueAppliesBackpressure(t *testing.T) {
	r := newRig(t, func(o *transcript.Options) { o.Buffer = 1 })
	r.header(t)

	const n = 500
	for i := range n {
		r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"n": i}})
	}

	lines := r.close(t)
	if len(lines) != n+1 {
		t.Fatalf("wrote %d lines, want %d: a full queue must not drop lines", len(lines), n+1)
	}
}

func TestCaptureAfterCloseIsAnError(t *testing.T) {
	r := newRig(t)
	r.header(t)
	if err := r.w.Close(); err != nil {
		t.Fatal(err)
	}

	r.w.Event(transcript.Event{Kind: transcript.Note})
	if r.w.Err() == nil {
		t.Error("a line captured after close was accepted silently")
	}
	if got := len(splitLines(r.buf.String())); got != 1 {
		t.Errorf("wrote %d lines after close, want 1", got)
	}
}

func TestRunIDIsAULID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := transcript.NewRunID()
		if len(id) != 26 {
			t.Fatalf("run id %q is %d characters, want 26", id, len(id))
		}
		if strings.ContainsAny(id, "ILOU") {
			t.Errorf("run id %q uses a character Crockford base32 excludes", id)
		}
		if seen[id] {
			t.Fatalf("run id %q repeated", id)
		}
		seen[id] = true
	}

	// ULIDs sort by creation time as strings, which is what makes a
	// directory of transcripts self-ordering.
	first := transcript.NewRunID()
	time.Sleep(2 * time.Millisecond)
	if second := transcript.NewRunID(); second <= first {
		t.Errorf("%q does not sort after %q", second, first)
	}
}

// A run that captures a great many lines before writing a header must not
// hold them all in memory forever. Past the cap the writer supplies its own
// header rather than growing until it dies -- and, critically, only one:
// Close must not add a second.
func TestTheHoldForAHeaderIsBounded(t *testing.T) {
	sch := compileSchema(t)
	r := newRig(t, func(o *transcript.Options) { o.Buffer = 64 })

	const n = transcript.HeldCap + 10
	for i := range n {
		r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"n": i}})
	}

	if err := r.w.Close(); err == nil {
		t.Error("Close reported success on a run that never wrote a header")
	}

	lines := splitLines(r.buf.String())
	if len(lines) != n+1 {
		t.Fatalf("wrote %d lines, want %d", len(lines), n+1)
	}
	validateAll(t, sch, lines[:1])

	headers := 0
	for i, line := range lines {
		m := decode(t, line)
		if m["type"] == "header" {
			headers++
			if i != 0 {
				t.Errorf("line %d is a header; the header is line one", i+1)
			}
		}
		if got := int(m["seq"].(float64)); got != i {
			t.Fatalf("line %d has seq %d, want %d", i+1, got, i)
		}
	}
	if headers != 1 {
		t.Errorf("wrote %d header lines, want exactly 1", headers)
	}
}
