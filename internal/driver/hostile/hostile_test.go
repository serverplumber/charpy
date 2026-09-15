package hostile_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

type run struct {
	lines []map[string]any
}

func (r run) ofType(k string) []map[string]any {
	var out []map[string]any
	for _, l := range r.lines {
		if l["type"] == k {
			out = append(out, l)
		}
	}
	return out
}
func (r run) events(k string) []map[string]any {
	var out []map[string]any
	for _, l := range r.ofType("event") {
		if l["event_kind"] == k {
			out = append(out, l)
		}
	}
	return out
}

// drive stands charpy up as a hostile server, connects a real SDK client under
// test to it, runs a short session, and returns the transcript. The client is
// correct -- charpy assumes conformance -- so whatever the fault provokes is
// charpy's doing.
func drive(t *testing.T, c interpose.Case, era revision.Revision, session func(*testing.T, *mcp.ClientSession)) run {
	t.Helper()

	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeHostileServer,
			Subject: transcript.Subject{Class: transcript.ClassClient},
			Clock:   clock.ModeReal,
			Peer:    peer.Describe(era),
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two pipes wire the client under test to charpy's server side.
	clientReads, hostileWrites := io.Pipe() // charpy responses -> client
	hostileReads, clientWrites := io.Pipe() // client requests -> charpy

	h, err := hostile.New(hostile.Options{
		In:         hostileReads,
		Out:        hostileWrites,
		Era:        era,
		Cases:      []interpose.Case{c},
		Transcript: tr,
		Sched:      sched,
		Ledger:     interpose.NewLedger(sched),
		RunSeed:    "8f2c1a",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "subject", Title: "client under test"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{
		Reader: clientReads, Writer: clientWrites, MaxLineLength: -1,
	}, &mcp.ClientSessionOptions{ProtocolVersion: string(era)})
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	session(t, cs)
	_ = cs.Close()

	cancel()
	_ = clientWrites.Close()
	<-done
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

// The whole driver end to end: a real client talks to charpy's reference-peer
// server, and charpy corrupts a response toward it. The fault fires against a
// subject faced upstream -- the client, not the server.
func TestAResponseIsFaultedTowardTheClient(t *testing.T) {
	c := interpose.Case{
		ID:       "frame/malformed-unbalanced",
		Citation: "frame/malformed-unbalanced@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		Fault: interpose.Fault{Kind: "malformed_json"},
	}
	r := drive(t, c, revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
		// The call's response is mangled, so the client's own call errors --
		// which is the fault working, not a test failure. Bounded so a broken
		// response cannot hang the test.
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
	})

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the fault never fired; events: %v", r.ofType("event"))
	}

	var faultedUpstream bool
	for _, l := range r.ofType("frame") {
		if l["fault"] != nil && l["face"] == string(transcript.Upstream) {
			faultedUpstream = true
		}
	}
	if !faultedUpstream {
		t.Error("no faulted frame recorded on the upstream (client) face")
	}
}

// unsolicited-after-resolved: charpy injects a response for an id the client
// already had answered, testing its pending-map. The synthesized frame is a
// second answer the server never sent.
func TestAnUnsolicitedResponseIsInjected(t *testing.T) {
	c := interpose.Case{
		ID:       "id/unsolicited-after-resolved",
		Citation: "id/unsolicited-after-resolved@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:     interpose.ParseGlob("tools/call"),
			Direction:  transcript.S2C,
			Occurrence: 2, // the second response, by when the first has resolved
		},
		Fault: interpose.Fault{Kind: "unsolicited_response", Params: map[string]any{"id_source": "already_resolved"}},
	}
	r := drive(t, c, revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
		// A second call gives the mechanism a resolved id to reuse.
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
	})

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the unsolicited-response fault never fired; events: %v", r.events(string(transcript.FaultScheduled)))
	}
}

// charpy claims an era, and the header records which -- the thing the hostile
// driver can do that the relay shim cannot.
func TestTheHeaderRecordsTheClaimedEra(t *testing.T) {
	c := interpose.Case{
		ID:       "frame/malformed-unbalanced",
		Citation: "frame/malformed-unbalanced@2025-06-18#seed=8f2c1a",
		Match:    interpose.Match{Method: interpose.ParseGlob("resources/read"), Direction: transcript.S2C},
		Fault:    interpose.Fault{Kind: "malformed_json"},
	}
	r := drive(t, c, revision.V20250618, func(t *testing.T, cs *mcp.ClientSession) {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
	})

	hdr := r.ofType("header")
	if len(hdr) != 1 {
		t.Fatalf("want one header, got %d", len(hdr))
	}
	p, ok := hdr[0]["peer"].(map[string]any)
	if !ok || p["era"] != string(revision.V20250618) {
		t.Errorf("header peer era = %v, want %s", hdr[0]["peer"], revision.V20250618)
	}
	if hdr[0]["subject"].(map[string]any)["class"] != "client" {
		t.Errorf("subject class = %v, want client", hdr[0]["subject"])
	}
}
