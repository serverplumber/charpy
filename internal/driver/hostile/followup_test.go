package hostile_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// lockedBuf is a transcript sink a test can read while the run is still
// writing to it, which is how it knows the client's answer has crossed.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTranscript(t *testing.T, buf io.Writer, era revision.Revision, sched clock.Sched) *transcript.Writer {
	t.Helper()
	tr, err := transcript.New(buf, transcript.Options{
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
	return tr
}

// pingAnswered reports whether a transcript so far shows charpy's server
// pinging the client and the client answering it.
func pingAnswered(raw string) bool {
	tr, err := transcript.Read(strings.NewReader(raw))
	if err != nil {
		return false
	}
	asked := map[string]bool{}
	for _, fr := range tr.Frames() {
		switch {
		case fr.Kind == envelope.KindRequest && fr.Direction == transcript.S2C && fr.MethodName() == "ping":
			asked[fr.Key()] = true
		case fr.Kind == envelope.KindResponse && fr.Direction == transcript.C2S && asked[fr.Key()]:
			return true
		}
	}
	return false
}

// awaitAnswer polls until the client's answer to the follow-up is in the
// transcript, so the test does not close the client with the ping in flight
// and then read its own impatience as a client that did not answer.
func awaitAnswer(t *testing.T, buf *lockedBuf) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !pingAnswered(buf.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("the client never answered a follow-up ping\n%s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// everyS2C faults every frame toward the client, requests included, so it
// would fault the follow-up ping too if the matcher were allowed to see it.
// unsolicited_response leaves the matched frame whole and puts an extra
// response for a never-used id before it, which a conforming client drops,
// so the session survives to be asked.
func everyS2C() interpose.Case {
	return interpose.Case{
		ID:       "id/unsolicited-never-used",
		Citation: "id/unsolicited-never-used@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Direction: transcript.S2C, Every: 1},
		Fault:    interpose.Fault{Kind: "unsolicited_response", Params: map[string]any{"id_source": "never_used"}},
	}
}

// Over stdio, a fault toward the client is followed by charpy's reference
// server pinging it through the SDK, and the reaction layer then has the
// client's answer to judge.
func TestTheServerPingsTheClientAfterAFault(t *testing.T) {
	buf := &lockedBuf{}
	sched := clock.RealSched()
	tr := newTranscript(t, buf, revision.V20251125, sched)

	clientReads, hostileWrites := io.Pipe()
	hostileReads, clientWrites := io.Pipe()
	h, err := hostile.New(hostile.Options{
		In: hostileReads, Out: hostileWrites, Era: revision.V20251125,
		Cases: []interpose.Case{everyS2C()}, Transcript: tr, Sched: sched,
		Ledger: interpose.NewLedger(sched), RunSeed: "8f2c1a",
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
	}, &mcp.ClientSessionOptions{ProtocolVersion: string(revision.V20251125)})
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	awaitAnswer(t, buf)
	_ = cs.Close()

	cancel()
	_ = clientWrites.Close()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	got := drivertest.Read(t, buf.String())
	q, _ := drivertest.FollowUpAnswered(t, got)
	if q.MethodName() != "ping" {
		t.Errorf("follow-up was %q, want ping", q.MethodName())
	}
	drivertest.ReactionAnswered(t, got)
}

// httpRun serves one client session against charpy's hostile HTTP server,
// runs session in it, waits for the client to answer a follow-up ping, and
// returns the transcript read back.
func httpRun(t *testing.T, c interpose.Case, session func(context.Context, *mcp.ClientSession)) *transcript.Transcript {
	t.Helper()
	buf := &lockedBuf{}
	sched := clock.RealSched()
	tr := newTranscript(t, buf, revision.V20251125, sched)

	h, err := hostile.NewHTTP(hostile.HTTPOptions{
		Era: revision.V20251125, Cases: []interpose.Case{c}, Transcript: tr,
		Sched: sched, Ledger: interpose.NewLedger(sched), RunSeed: "8f2c1a",
	})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	cctx, cc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cc()
	// The standalone GET stream stays enabled: it is where the SDK sends a
	// request the server originates, so a client without one cannot be asked.
	cl := mcp.NewClient(&mcp.Implementation{Name: "subject", Title: "client under test"}, nil)
	cs, err := cl.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: h.Endpoint()},
		&mcp.ClientSessionOptions{ProtocolVersion: string(revision.V20251125)})
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	session(cctx, cs)
	awaitAnswer(t, buf)
	_ = cs.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}
	return drivertest.Read(t, buf.String())
}

// Over HTTP the SDK sends a request the server originates on the client's
// standalone GET stream, which the proxy relays like any other event stream.
// The case matches every frame toward the client, so this pins that the ping
// is kept out of its reach there and the client's answer comes back clean.
//
// The fault here lands on the initialize response, which the proxy records
// under the connection of a request that had no session id yet, while the
// ping travels on the session's own; so this checks the frames and leaves the
// reaction verdict to the test below.
func TestTheServerPingsAnHTTPClientAfterAFault(t *testing.T) {
	got := httpRun(t, everyS2C(), func(context.Context, *mcp.ClientSession) {})

	q, _ := drivertest.FollowUpAnswered(t, got)
	if q.MethodName() != "ping" {
		t.Errorf("follow-up was %q, want ping", q.MethodName())
	}
}

// A fault on a response inside the session is followed by a ping on that same
// connection, and the reaction layer finds the client answered.
func TestAnHTTPClientsAnswerIsJudged(t *testing.T) {
	c := everyS2C()
	c.Match = interpose.Match{Method: interpose.ParseGlob("tools/call"), Direction: transcript.S2C}
	got := httpRun(t, c, func(ctx context.Context, cs *mcp.ClientSession) {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
	})

	drivertest.FollowUpAnswered(t, got)
	drivertest.ReactionAnswered(t, got)
}

// From 2026-07-28 a server originates no requests, so there is no follow-up
// to send and nothing-asked-after-fault is the correct result, not a gap.
func TestAStatelessServerAsksNothing(t *testing.T) {
	c := everyS2C()
	c.Citation = "id/unsolicited-never-used@2026-07-28#seed=8f2c1a"
	r := drive(t, c, revision.V20260728, func(t *testing.T, cs *mcp.ClientSession) {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = cs.CallTool(cctx, &mcp.CallToolParams{Name: "echo"})
	})

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the fault never fired; events: %v", r.ofType("event"))
	}
	for _, l := range r.ofType("frame") {
		if l["direction"] == string(transcript.S2C) && l["kind"] == string(envelope.KindRequest) {
			t.Errorf("a 2026-07-28 server sent the client a request: %v", l)
		}
	}
}
