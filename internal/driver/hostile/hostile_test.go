package hostile_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

type run struct {
	lines []map[string]any
	raw   string
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
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
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
	return run{lines: decode(t, buf.String()), raw: buf.String()}
}

// shipped compiles a case as the catalogue ships it, so a test exercises the
// TOML a user runs rather than a hand-built copy that can drift from it.
func shipped(t *testing.T, id string) interpose.Case {
	t.Helper()
	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		t.Fatalf("the shipped catalogue does not load: %v", err)
	}
	for _, cs := range cat.Applicable(revision.V20251125) {
		if cs.ID == id {
			c, err := cs.Compile(revision.V20251125, "8f2c1a")
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	t.Fatalf("no shipped case %s applies to %s", id, revision.V20251125)
	return interpose.Case{}
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

// A response cut short toward the client parses to no id, so the attribution
// must name the answer it replaced. Without it the oracle sees the client's
// call as never answered and reports it -- a finding about the exchange that
// is entirely charpy's doing.
func TestACutResponseNamesTheAnswerItReplaced(t *testing.T) {
	c := interpose.Case{
		ID:       "stream/truncate-mid-frame",
		Citation: "stream/truncate-mid-frame@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		// mid_frame, not mid_line: mid_line withholds only the newline, so
		// the bytes that cross are a whole frame still carrying its id, and
		// there is nothing to name.
		Fault: interpose.Fault{Kind: "truncate", Params: map[string]any{"cut_at": "mid_frame", "then": "close"}},
	}
	r := drive(t, c, revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
		// The answer is cut, so the call errors or times out: the fault
		// working, bounded so it cannot hang the test.
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
	})

	var callID any
	var cut map[string]any
	for _, l := range r.ofType("frame") {
		if l["direction"] == string(transcript.C2S) && l["method"] == "tools/call" {
			callID = l["id"]
		}
		if l["fault"] != nil && l["kind"] == "malformed" {
			cut = l
		}
	}
	if cut == nil {
		t.Fatalf("no cut frame reached the transcript; events: %v", r.events(string(transcript.FaultApplied)))
	}
	f, _ := cut["fault"].(map[string]any)
	rep, _ := f["replaced"].(map[string]any)
	if callID == nil || rep["id"] != callID {
		t.Errorf("replaced = %v, want the id of the client's call, %v", f["replaced"], callID)
	}

	for _, fd := range invariant.Check(drivertest.Read(t, r.raw)).Findings {
		if fd.Check == "id-resolves-once" && fd.Verdict == oracle.Observed {
			t.Errorf("charpy's cut was reported as a finding: %s", fd.Summary)
		}
	}
}

// unsolicited-after-resolved: charpy injects a response for an id the client
// already had answered, testing its pending-map. The synthesized frame is a
// second answer the server never sent.
//
// The shipped case, not a copy: its matcher is what says "the second call",
// and a matcher that left that out would fire on every answer after the
// first resolved.
func TestAnUnsolicitedResponseIsInjected(t *testing.T) {
	r := drive(t, shipped(t, "id/unsolicited-after-resolved"), revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		// Three calls: the first resolves an id to reuse, the second is the
		// one the case names, the third is one it must leave alone.
		for range 3 {
			_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
		}
	})

	applied := r.events(string(transcript.FaultApplied))
	if len(applied) != 1 {
		t.Fatalf("fault_applied %d times, want once, beside the second call", len(applied))
	}

	// Beside the second call's answer -- which the mechanism delivers under
	// the same attribution -- crosses a late answer naming the first call's
	// id, the one already resolved.
	var calls, faulted []any
	for _, l := range r.ofType("frame") {
		if l["direction"] == string(transcript.C2S) && l["method"] == "tools/call" {
			calls = append(calls, l["id"])
		}
		if l["fault"] != nil {
			faulted = append(faulted, l["id"])
		}
	}
	if len(calls) < 2 {
		t.Fatalf("calls %v", calls)
	}
	var late bool
	for _, id := range faulted {
		if id == calls[0] {
			late = true
		}
		if len(calls) > 2 && id == calls[2] {
			t.Errorf("the third call's answer was touched; the case names the second")
		}
	}
	if !late {
		t.Errorf("no late answer for the first call's id %v among the faulted frames %v", calls[0], faulted)
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

// A case lands on the frame its summary means. Both of these once named only a
// direction, so they fired on the first frame going that way -- the handshake
// answer -- and nothing survived to be asked whether the parser or the
// connection had wedged, which is what the cases are about. They name the call
// now, and the handshake crosses untouched.
func TestShippedCasesLandOnTheCallNotTheHandshake(t *testing.T) {
	for _, id := range []string{"frame/malformed-unbalanced", "stream/truncate-mid-line"} {
		t.Run(id, func(t *testing.T) {
			r := drive(t, shipped(t, id), revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
				// The answer is corrupted or never delimits, so the call
				// fails or waits: bounded, because either is the fault working.
				cctx, cc := context.WithTimeout(context.Background(), time.Second)
				defer cc()
				_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
			})

			if n := len(r.events(string(transcript.FaultApplied))); n != 1 {
				t.Errorf("fault_applied %d times, want once", n)
			}
			// Corrupted, or cut before its newline, the answer never arrived
			// as a frame, so it names the id it carried -- even when, as for
			// mid_line, its JSON is whole and still parses to that id.
			for _, l := range r.ofType("frame") {
				f, _ := l["fault"].(map[string]any)
				if f == nil || l["direction"] != string(transcript.S2C) {
					continue
				}
				if rep, _ := f["replaced"].(map[string]any); rep["kind"] != "response" {
					t.Errorf("the damaged answer does not name what it replaced: %v", f)
				}
			}
			var initAnswered bool
			for _, l := range r.ofType("frame") {
				if l["direction"] != string(transcript.S2C) || l["method"] != "initialize" {
					continue
				}
				if l["fault"] != nil {
					t.Error("the fault landed on the handshake")
				} else {
					initAnswered = true
				}
			}
			if !initAnswered {
				t.Error("the handshake answer did not cross untouched")
			}
		})
	}
}

// Where charpy serves, the declaration is its own: add_numbers declares a
// numeric sum, and the shipped case breaks structuredContent against it. What
// the client then does is its reaction, and the run's to report, not this
// test's to require: the spec says clients SHOULD validate structured results
// (2025-11-25 server/tools), and the pinned Go SDK client does not -- it
// accepts the broken sum.
func TestADeclaredOutputSchemaIsBrokenTowardTheClient(t *testing.T) {
	var callErr error
	r := drive(t, shipped(t, "schema/output-schema-violated"), revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		// A client can only validate against a schema it has seen.
		if _, err := cs.ListTools(cctx, nil); err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		_, callErr = cs.CallTool(cctx, &mcp.CallToolParams{
			Name: "add_numbers", Arguments: map[string]any{"a": 5, "b": 3},
		})
	})

	if n := len(r.events(string(transcript.FaultApplied))); n != 1 {
		t.Fatalf("fault_applied %d times, want once; scheduled: %v", n, r.events(string(transcript.FaultScheduled)))
	}
	var broken bool
	for _, l := range r.ofType("frame") {
		if l["fault"] == nil || l["direction"] != string(transcript.S2C) {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(l["raw"].(string))
		var got struct {
			Result struct {
				StructuredContent map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("the violation is not valid JSON: %v", err)
		}
		if _, isNumber := got.Result.StructuredContent["sum"].(float64); !isNumber {
			broken = true
		}
	}
	if !broken {
		t.Error("no answer with structuredContent broken against the declared schema")
	}
	t.Logf("the client's call returned error %v", callErr)
}
