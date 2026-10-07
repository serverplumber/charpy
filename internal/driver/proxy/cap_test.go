package proxy_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// runaway is the fixture subject, except that it answers tools/call with a
// body that never ends: head, then filler until charpy hangs up.
func runaway(t *testing.T, contentType, head string) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !bytes.Contains(body, []byte(`"tools/call"`)) {
			sdk.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, head); err != nil {
			return
		}
		filler := bytes.Repeat([]byte("x"), 32<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(filler); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// observe matches nothing that crosses, so the run is a plain relay.
var observe = interpose.Case{
	ID:       "test/observe",
	Citation: "test/observe@2025-11-25#seed=8f2c1a",
	Match:    interpose.Match{Kind: "error"},
	Fault:    interpose.Fault{Kind: "malformed_json"},
}

// A subject whose answer never ends is a finding, not charpy's failure: the
// run completes (scriptAt fails the test on a Run error), and the transcript
// says what the subject did.
func TestAJSONBodyThatNeverEndsIsRecordedNotFatal(t *testing.T) {
	r := scriptAt(t, observe, revision.V20251125,
		runaway(t, "application/json", `{"jsonrpc":"2.0","id":2,"result":{"blob":"`))

	capped := r.events(string(transcript.FrameCapped))
	if len(capped) != 1 {
		t.Fatalf("frame_capped events: %d, want 1", len(capped))
	}
	d := capped[0]["detail"].(map[string]any)
	if d["direction"] != "s2c" {
		t.Errorf("direction = %v, want s2c", d["direction"])
	}
	if n, _ := d["bytes_read"].(float64); n <= float64(4<<20) {
		t.Errorf("bytes_read = %v, want past the cap", d["bytes_read"])
	}
	// The frame never crossed, so no frame line pretends it did.
	for _, f := range r.ofType("frame") {
		if f["direction"] == "s2c" && f["raw_len"].(float64) > float64(transcript.RawCap) {
			t.Errorf("a frame line carries the capped answer: raw_len %v", f["raw_len"])
		}
	}
}

// The same over an event stream, where the unit that never ends is a data
// line with no newline. The stream is closed for the cap, and says so.
func TestAnEventThatNeverEndsClosesTheStreamForTheCap(t *testing.T) {
	r := scriptAt(t, observe, revision.V20251125,
		runaway(t, "text/event-stream", "event: message\ndata: "))

	if n := len(r.events(string(transcript.FrameCapped))); n != 1 {
		t.Fatalf("frame_capped events: %d, want 1", n)
	}
	var closed bool
	for _, e := range r.events(string(transcript.StreamClose)) {
		if e["detail"].(map[string]any)["reason"] == string(transcript.FrameCap) {
			closed = true
		}
	}
	if !closed {
		t.Error("no stream_close with reason frame_cap")
	}
}

// A client whose request never ends is the client's finding. The subject never
// sees it, the client's exchange is broken rather than answered by charpy, and
// the proxy goes on serving.
func TestARequestThatNeverEndsIsRecordedAndNotForwarded(t *testing.T) {
	var forwarded atomic.Int32
	subj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(subj.Close)

	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassServer}, Clock: clock.ModeReal,
		},
		Sched: sched,
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.WriteHeader(transcript.Header{Revision: &transcript.Negotiation{
		Negotiated: revision.V20251125, How: transcript.HowInitialize,
	}}); err != nil {
		t.Fatal(err)
	}
	pr, err := proxy.New(proxy.Options{
		SubjectURL: subj.URL, Transcript: tr, Sched: sched,
		Wall: clock.NewFixedWall(time.Unix(0, 0)), Ledger: interpose.NewLedger(sched),
		RunSeed: "8f2c1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ingress := httptest.NewServer(pr)
	t.Cleanup(ingress.Close)

	body := io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"`),
		io.LimitReader(endlessX{}, 5<<20))
	if resp, err := http.Post(ingress.URL, "application/json", body); err == nil {
		resp.Body.Close()
		t.Errorf("the client got an answer (%s); want its exchange broken", resp.Status)
	}

	// Still serving.
	resp, err := http.Post(ingress.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("the proxy stopped serving after a capped request: %v", err)
	}
	resp.Body.Close()
	if n := forwarded.Load(); n != 1 {
		t.Errorf("subject saw %d requests, want 1: the capped one must not be forwarded", n)
	}

	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	r := run{lines: decode(t, buf.String())}
	capped := r.events(string(transcript.FrameCapped))
	if len(capped) != 1 || capped[0]["detail"].(map[string]any)["direction"] != "c2s" {
		t.Fatalf("frame_capped events: %v, want one c2s", capped)
	}
}

type endlessX struct{}

func (endlessX) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
