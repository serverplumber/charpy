package hostile_test

import (
	"context"
	"encoding/base64"
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

// driveHTTP stands charpy up as a hostile HTTP server, runs the given client
// sessions against its ingress, and returns the transcript.
func driveHTTP(t *testing.T, c interpose.Case, era revision.Revision, sessions func(*testing.T, string)) run {
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

	h, err := hostile.NewHTTP(hostile.HTTPOptions{
		Era:        era,
		Cases:      []interpose.Case{c},
		Transcript: tr,
		Sched:      sched,
		Ledger:     interpose.NewLedger(sched),
		RunSeed:    "8f2c1a",
	})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	// The endpoint is bound in NewHTTP, so it is ready now.
	sessions(t, h.Endpoint())

	cancel()
	<-done
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}
	return run{lines: decode(t, buf.String())}
}

func connectOnce(t *testing.T, endpoint string, era revision.Revision, call bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "subject", Title: "client under test"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true},
		&mcp.ClientSessionOptions{ProtocolVersion: string(era)})
	if err != nil {
		return // a faulted handshake can fail the connect; that is the fault working
	}
	if call {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
	}
	_ = cs.Close()
}

// The reconnect case HTTP unlocks: charpy narrows capabilities on the second
// connection's initialize result, catching a client that cached the first.
func TestCapabilityFlipFiresOnReconnect(t *testing.T) {
	c := interpose.Case{
		ID:       "lifecycle/capability-narrowed-on-reconnect",
		Citation: "lifecycle/capability-narrowed-on-reconnect@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:     interpose.ParseGlob("initialize"),
			Kind:       "response",
			Direction:  transcript.S2C,
			Occurrence: 2, // the second connection's handshake
		},
		Fault: interpose.Fault{Kind: "capability_flip", Params: map[string]any{
			"field": "capabilities", "direction_of_change": "narrow",
		}},
	}
	r := driveHTTP(t, c, revision.V20251125, func(t *testing.T, endpoint string) {
		connectOnce(t, endpoint, revision.V20251125, false) // conn 1: full caps
		connectOnce(t, endpoint, revision.V20251125, false) // conn 2: narrowed
	})

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("capability_flip never fired across two connections; scheduled: %v",
			r.events(string(transcript.FaultScheduled)))
	}

	// The faulted initialize result carries narrowed (empty) capabilities. The
	// transcript stores frame bytes base64-encoded, so decode before reading.
	var narrowed bool
	for _, l := range r.ofType("frame") {
		if l["fault"] == nil {
			continue
		}
		raw, _ := l["raw"].(string)
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), `"capabilities":{}`) {
			narrowed = true
		}
	}
	if !narrowed {
		t.Error("no faulted initialize result with narrowed capabilities was recorded")
	}
}

// A response faulted toward a real HTTP client, single connection: the same
// s2c fault the stdio hostile driver does, over the transport a reconnect
// needs.
func TestHTTPResponseFaultedTowardClient(t *testing.T) {
	c := interpose.Case{
		ID:       "frame/malformed-unbalanced",
		Citation: "frame/malformed-unbalanced@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method:    interpose.ParseGlob("tools/call"),
			Direction: transcript.S2C,
		},
		Fault: interpose.Fault{Kind: "malformed_json"},
	}
	r := driveHTTP(t, c, revision.V20251125, func(t *testing.T, endpoint string) {
		connectOnce(t, endpoint, revision.V20251125, true)
	})

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the fault never fired; events: %v", r.ofType("event"))
	}
	var upstream bool
	for _, l := range r.ofType("frame") {
		if l["fault"] != nil && l["face"] == string(transcript.Upstream) {
			upstream = true
		}
	}
	if !upstream {
		t.Error("no faulted frame on the upstream (client) face")
	}
}
