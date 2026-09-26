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
	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
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
	raw   string
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

	return run{lines: decode(t, buf.String()), raw: buf.String()}
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
			// The remnant parses to no id, so the attribution is the only
			// record of which answer it was.
			f, _ := l["fault"].(map[string]any)
			if rep, _ := f["replaced"].(map[string]any); rep["id"] == nil || rep["kind"] != "response" {
				t.Errorf("the cut frame does not name the answer it replaced: %v", l["fault"])
			}
		}
	}
	if !malformed {
		t.Error("no truncated frame reached the transcript")
	}

	// And so the oracle does not file charpy's cut against the subject. The
	// server answered the call; charpy cut the answer short. Reporting the id
	// as never answered is the finding that would have gone into somebody
	// else's tracker.
	for _, f := range invariant.Check(drivertest.Read(t, r.raw)).Findings {
		if f.Check != "id-resolves-once" {
			continue
		}
		if f.Verdict == oracle.Observed {
			t.Errorf("charpy's truncation was reported against the subject: %s", f.Summary)
		}
		if f.Verdict == oracle.Inconclusive && f.Reason != "charpy-replaced-the-answer" {
			t.Errorf("reason = %q, want charpy-replaced-the-answer", f.Reason)
		}
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

// Liveness end to end: a truncation breaks the client's call, then charpy
// probes the subject on the same HTTP session -- which survives a broken
// response stream -- and the subject answers. The oracle reads the probe as a
// recovery.
func TestATruncationIsFollowedByARecoveryProbe(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "stream/truncate-mid-event",
		Citation: "stream/truncate-mid-event@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		Fault:            interpose.Fault{Kind: "truncate", Params: map[string]any{"cut_at": "mid_event", "then": "close"}},
		LivenessWithinMS: 10000,
	}, revision.V20251125)

	probes := r.events(string(transcript.Probe))
	if len(probes) != 1 {
		t.Fatalf("want one probe after the truncation, got %d", len(probes))
	}
	d, _ := probes[0]["detail"].(map[string]any)
	if d["outcome"] != "ok" {
		t.Errorf("probe outcome = %v, want ok (the subject recovered on the surviving session)", d["outcome"])
	}
	if d["method"] != "ping" {
		t.Errorf("probe method = %v, want ping on 2025-11-25", d["method"])
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
	// The delivered event is the subject's answer, whole and untouched, so it
	// carries no attribution: marking it would drop the subject's real answer
	// from every layer's evidence. The fault is the close after it, which
	// fault_applied and charpy's own stream_close record.
	var answers int
	for _, l := range r.ofType("frame") {
		if l["direction"] != string(transcript.S2C) || l["method"] != "tools/call" {
			continue
		}
		answers++
		if l["kind"] != "response" {
			t.Errorf("the delivered event is %v, not a whole response", l["kind"])
		}
		if l["fault"] != nil {
			t.Errorf("an untouched answer carries an attribution: %v", l["fault"])
		}
	}
	if answers == 0 {
		t.Error("event_boundary should deliver a whole, well-formed event")
	}
	var closed bool
	for _, l := range r.events(string(transcript.StreamClose)) {
		if d, _ := l["detail"].(map[string]any); d["reason"] == string(transcript.CharpyClose) {
			closed = true
		}
	}
	if !closed {
		t.Error("the close that is the fault was not recorded as charpy's")
	}
}

// After the fault acts, charpy's peer asks the subject one more question on
// the same session, and the subject's answer crosses back untouched.
//
// The case matches every response, with no method and occurrence_every = 1,
// so it would fault the follow-up's answer too if the matcher saw it. The fault here
// reaches charpy's peer rather than the server, so the reaction layer rightly
// skips it; what this pins is that the question is asked and comes back clean,
// which is the driver's half whoever the fault was put to.
func TestAFollowUpIsAskedOnTheSameSession(t *testing.T) {
	c := interpose.Case{
		ID:       "id/unsolicited-after-response",
		Citation: "id/unsolicited-after-response@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Direction: transcript.S2C, Kind: "response", Every: 1},
		Fault:    interpose.Fault{Kind: "unsolicited_response", Params: map[string]any{"id_source": "never_used"}},
	}
	r := script(t, c, revision.V20251125)

	q, _ := drivertest.FollowUpAnswered(t, drivertest.Read(t, r.raw))
	if q.MethodName() != "ping" {
		t.Errorf("follow-up was %q, want ping at %s", q.MethodName(), revision.V20251125)
	}
}

// A server subject receives c2s, so that is where its questions are put. A
// request corrupted on its way to a real HTTP server crosses without its id,
// the attribution says it was a request, and fault_applied says which way it
// went. The server survives it, and answers the question after the fault.
func TestAMalformedRequestIsPutToTheServer(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "frame/malformed-request",
		Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method: interpose.ParseGlob("tools/call"), Direction: transcript.C2S, Kind: "request",
		},
		Fault: interpose.Fault{Kind: "malformed_json"},
	}, revision.V20251125)
	tr := drivertest.Read(t, r.raw)

	var destroyed bool
	for _, f := range tr.Frames() {
		if f.Direction == transcript.C2S && f.Fault != nil && f.Fault.Replaced != nil &&
			f.Fault.Replaced.Kind == envelope.KindRequest {
			destroyed = true
		}
	}
	if !destroyed {
		t.Fatal("no corrupted request reached the transcript")
	}
	applied := tr.Events(transcript.FaultApplied)
	if len(applied) != 1 {
		t.Fatalf("fault_applied %d times, want once", len(applied))
	}
	if d, _ := applied[0].AppliedDirection(); d != transcript.C2S {
		t.Errorf("fault_applied direction = %q, want c2s", d)
	}
	drivertest.FollowUpAnswered(t, tr)
	drivertest.ReactionAnswered(t, tr)
}

// A frame charpy synthesizes beside a request goes to the subject as a request
// of its own. A duplicate sent while the original is in flight puts the same
// id to the server twice at once; the copy is charpy's, the original is not.
func TestADuplicateRequestIsSentBesideTheOriginal(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "id/duplicate-request-inflight",
		Citation: "id/duplicate-request-inflight@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method: interpose.ParseGlob("tools/call"), Direction: transcript.C2S, Kind: "request",
		},
		Fault: interpose.Fault{Kind: "duplicate_id", Params: map[string]any{"mode": "concurrent_request"}},
	}, revision.V20251125)
	tr := drivertest.Read(t, r.raw)

	byID := map[string][]*transcript.FrameLine{}
	for _, f := range tr.Frames() {
		if f.Direction == transcript.C2S && f.MethodName() == "tools/call" {
			byID[f.Key()] = append(byID[f.Key()], f)
		}
	}
	var twice bool
	for id, fs := range byID {
		if len(fs) != 2 {
			continue
		}
		twice = true
		if fs[0].Tampered() == fs[1].Tampered() {
			t.Errorf("id %s: want the original untouched and only the copy charpy's", id)
		}
	}
	if !twice {
		t.Fatalf("no tools/call id was sent twice: %v", byID)
	}
	if d, _ := tr.Events(transcript.FaultApplied)[0].AppliedDirection(); d != transcript.C2S {
		t.Errorf("fault_applied direction = %q, want c2s", d)
	}
}
