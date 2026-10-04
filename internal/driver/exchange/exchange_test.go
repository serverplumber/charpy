package exchange_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// gatewayRun is a run with one face on each side of a gateway, and a func
// that closes it and reads back what it wrote.
func gatewayRun(t *testing.T, headed transcript.Face) (down, up *exchange.Conn, finish func() *transcript.Transcript) {
	t.Helper()
	var buf bytes.Buffer
	sched := clock.NewInjected()
	w, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassGateway}, Clock: clock.ModeInjected,
		},
		Sched: sched,
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &exchange.Run{Ledger: interpose.NewLedger(sched), Transcript: w, Headed: headed}
	// Both faces' connections are "c-0", as two proxies each default them.
	down = run.Face(transcript.Downstream, transcript.TransportHTTP, nil).Conn("c0", "s-0", "c-0")
	up = run.Face(transcript.Upstream, transcript.TransportHTTP, nil).Conn("c0", "s-0", "c-0")
	return down, up, func() *transcript.Transcript {
		t.Helper()
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		tr, err := transcript.Read(&buf)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
}

func msg(t *testing.T, raw string) envelope.Message {
	t.Helper()
	m, err := envelope.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// handshake puts an initialize exchange that settles rev across a connection,
// observed and recorded the way a driver does it.
func handshake(t *testing.T, n *exchange.Conn, rev revision.Revision) {
	t.Helper()
	for _, step := range []struct {
		dir transcript.Direction
		raw string
	}{
		{transcript.C2S, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + string(rev) + `","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`},
		{transcript.S2C, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` + string(rev) + `","capabilities":{},"serverInfo":{"name":"s","version":"0"}}}`},
	} {
		n.Observe(msg(t, step.raw), step.dir)
		n.Frame(step.dir, []byte(step.raw), nil, nil)
	}
}

// revisions is what each face's handshake answer recorded.
func revisions(tr *transcript.Transcript) map[transcript.Face]revision.Revision {
	out := map[transcript.Face]revision.Revision{}
	for _, f := range tr.Frames() {
		if f.Direction == transcript.S2C {
			out[f.Face] = f.Revision
		}
	}
	return out
}

// A gateway's upstream handshake can finish first -- the fixture dials its
// upstreams while answering its client's initialize -- but the header is the
// downstream face's: what the gateway offers its clients.
func TestTheHeadedFaceWritesTheOneHeader(t *testing.T) {
	down, up, finish := gatewayRun(t, transcript.Downstream)
	handshake(t, up, revision.V20250618)
	handshake(t, down, revision.V20251125)
	tr := finish() // Read refuses a missing or repeated header

	if got := tr.Header.Revision.Negotiated; got != revision.V20251125 {
		t.Errorf("header revision = %s, want the downstream face's 2025-11-25", got)
	}
	want := map[transcript.Face]revision.Revision{
		transcript.Upstream: revision.V20250618, transcript.Downstream: revision.V20251125,
	}
	if got := revisions(tr); got[transcript.Upstream] != want[transcript.Upstream] ||
		got[transcript.Downstream] != want[transcript.Downstream] {
		t.Errorf("frames carry %v, want each face's own %v", got, want)
	}

	var noted bool
	for _, e := range tr.Events(transcript.Note) {
		if h, _ := e.Detail["harness"].(string); strings.Contains(h, "different revisions") {
			noted = true
		}
	}
	if !noted {
		t.Error("faces settling different revisions went unnoted")
	}
}

// With no face named, the first to settle heads the run -- for a one-faced
// run, its only face.
func TestTheFirstFaceHeadsAnUnheadedRun(t *testing.T) {
	down, up, finish := gatewayRun(t, "")
	handshake(t, up, revision.V20250618)
	handshake(t, down, revision.V20250618)
	tr := finish()

	if got := tr.Header.Revision.Negotiated; got != revision.V20250618 {
		t.Errorf("header revision = %s, want the first face's", got)
	}
	if n := len(tr.Events(transcript.Note)); n != 0 {
		t.Errorf("%d notes for faces that agreed", n)
	}
}

// Two proxies both name their first connection "c-0". A follow-up question
// asked on one face must not claim a request crossing the other.
func TestFollowUpsOnTwoFacesDoNotCollide(t *testing.T) {
	down, up, _ := gatewayRun(t, transcript.Downstream)
	withdraw := down.Ask(transcript.C2S)
	defer withdraw()

	ping := msg(t, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if up.FollowUp(ping, transcript.C2S) {
		t.Error("the upstream face claimed the downstream face's follow-up question")
	}
	if !down.FollowUp(ping, transcript.C2S) {
		t.Error("the downstream face did not recognise its own follow-up question")
	}
}
