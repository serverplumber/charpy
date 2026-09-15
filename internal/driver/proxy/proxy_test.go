package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// subject stands up a conforming MCP server over Streamable HTTP and returns
// its URL. charpy assumes its subject already passes conformance, so the
// fixture is the SDK's own handler and anything the run provokes is charpy's.
func subject(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

type run struct {
	lines []map[string]any
}

func (r run) ofType(kind string) []map[string]any {
	var out []map[string]any
	for _, l := range r.lines {
		if l["type"] == kind {
			out = append(out, l)
		}
	}
	return out
}

func (r run) events(kind string) []map[string]any {
	var out []map[string]any
	for _, l := range r.ofType("event") {
		if l["event_kind"] == kind {
			out = append(out, l)
		}
	}
	return out
}

// script drives one case against a real HTTP subject through the proxy and
// returns the transcript it produced.
func script(t *testing.T, c interpose.Case, era revision.Revision) run {
	t.Helper()

	var buf strings.Builder
	sched := clock.RealSched()

	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassServer},
			Clock:   clock.ModeReal,
			Peer:    peer.Describe(era),
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}

	sc, err := proxy.NewScript(proxy.ScriptOptions{
		Case:    c,
		Era:     era,
		Timeout: 10 * time.Second,
		Options: proxy.Options{
			SubjectURL: subject(t),
			Transcript: tr,
			Sched:      sched,
			Ledger:     interpose.NewLedger(sched),
			Face:       transcript.Downstream,
			RunSeed:    "8f2c1a",
		},
	})
	if err != nil {
		t.Fatalf("NewScript: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	return run{lines: decode(t, buf.String())}
}

func decode(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// The whole item, end to end: charpy's peer drives a real HTTP subject through
// the proxy, and charpy sees the traffic cross. wire's SSE half carries real
// bytes for the first time.
func TestTheProxyRelaysAndTranscribes(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "test/observe",
		Citation: "test/observe@2025-11-25#seed=8f2c1a",
		// Matches nothing that crosses (an error frame never does), so the
		// run is a clean relay end to end.
		Match: interpose.Match{Kind: "error"},
		Fault: interpose.Fault{Kind: "truncate", Params: map[string]any{"cut_at": "mid_event", "then": "close"}},
	}, revision.V20251125)

	var calls, responses int
	for _, l := range r.ofType("frame") {
		if l["transport"] != "http" {
			t.Errorf("frame is not http transport: %v", l["transport"])
		}
		if m, _ := l["method"].(string); m == "tools/call" && l["direction"] == "c2s" {
			calls++
		}
		if l["direction"] == "s2c" && l["kind"] == "response" {
			responses++
		}
	}
	if calls == 0 {
		t.Errorf("charpy saw no tools/call cross; frames: %d", len(r.ofType("frame")))
	}
	if responses == 0 {
		t.Error("charpy saw no response come back")
	}

	hdr := r.ofType("header")
	if len(hdr) != 1 || hdr[0]["revision"].(map[string]any)["negotiated"] != string(revision.V20251125) {
		t.Errorf("header did not settle the revision: %v", hdr)
	}
}

// A truncation cutting a real SSE event mid-frame: the fault fires, the client
// call breaks under it, and the transcript records a malformed frame -- the
// remnant of the event the subject sent, cut in the subject's own bytes.
func TestATruncationCutsARealEventStream(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "stream/truncate-mid-event",
		Citation: "stream/truncate-mid-event@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		Fault: interpose.Fault{Kind: "truncate", Params: map[string]any{"cut_at": "mid_event", "then": "close"}},
	}, revision.V20251125)

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the truncation never fired; events: %v", r.events(string(transcript.FaultScheduled)))
	}

	var malformed bool
	for _, l := range r.ofType("frame") {
		if l["fault"] != nil && l["kind"] == "malformed" {
			malformed = true
		}
	}
	if !malformed {
		t.Error("no truncated frame reached the transcript")
	}

	// then=close: charpy ended the client's stream, recorded as its own close.
	var closed bool
	for _, l := range r.events(string(transcript.StreamClose)) {
		if d, _ := l["detail"].(map[string]any); d["reason"] == string(transcript.CharpyClose) {
			closed = true
		}
	}
	if !closed {
		t.Error("no charpy_close recorded for a then=close truncation")
	}
}

// event_boundary delivers the whole event and stops the stream: nothing
// malformed is ever seen, which is the least dramatic variant and the one most
// likely to find a subject that mishandles a clean early close.
func TestAnEventBoundaryCutDeliversAWholeEvent(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "stream/truncate-event-boundary",
		Citation: "stream/truncate-event-boundary@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		Fault: interpose.Fault{Kind: "truncate", Params: map[string]any{"cut_at": "event_boundary", "then": "close"}},
	}, revision.V20251125)

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatal("the event_boundary cut never fired")
	}
	// The delivered event is well formed: a response frame, not a malformed
	// remnant.
	var wellFormed bool
	for _, l := range r.ofType("frame") {
		if l["fault"] != nil && l["kind"] == "response" {
			wellFormed = true
		}
	}
	if !wellFormed {
		t.Error("event_boundary should deliver a whole, well-formed event")
	}
}
