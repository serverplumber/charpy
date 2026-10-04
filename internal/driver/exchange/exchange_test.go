package exchange_test

import (
	"bytes"
	"strconv"
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

// joinedRun is a gateway run whose two faces join, and a func that closes it
// and returns each frame's link in order.
func joinedRun(t *testing.T) (down, up *exchange.Conn, finish func() []transcript.LinkLine) {
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
	run := &exchange.Run{Ledger: interpose.NewLedger(sched), Transcript: w, Headed: transcript.Downstream}
	down = run.JoinedFace(transcript.Downstream, transcript.TransportHTTP).Conn("c0", "s-0", "d-c-1")
	up = run.JoinedFace(transcript.Upstream, transcript.TransportHTTP).Conn("c0", "s-0", "u0-c-1")
	return down, up, func() []transcript.LinkLine {
		t.Helper()
		_ = w.Close() // no handshake here, so no header: the reader is what matters
		tr, err := transcript.Read(&buf)
		if err != nil {
			t.Fatal(err)
		}
		var out []transcript.LinkLine
		for _, f := range tr.Frames() {
			out = append(out, f.Link)
		}
		return out
	}
}

// cross records one frame the way a driver does: observe, then record.
func cross(t *testing.T, n *exchange.Conn, dir transcript.Direction, raw string) {
	t.Helper()
	n.Observe(msg(t, raw), dir)
	n.Frame(dir, []byte(raw), nil, nil)
}

func call(id int, meta, a string) string {
	params := `"name":"u0_add_numbers","arguments":{"a":` + a + `,"b":0}`
	if meta != "" {
		params = `"_meta":{"traceparent":"` + meta + `"},` + params
	}
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{` + params + `}}`
}

func answer(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":{"content":[]}}`
}

const (
	trace1 = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	trace2 = "00-1af7651916cd43dd8448eb211c80319c-c7ad6b7169203331-01"
)

// A call charpy makes and the gateway forwards joins across the gateway:
// traced when the trace survives, inferred by content when it does not, and
// each answer by its own request. What matches nothing joins nothing.
func TestJoinedFacesCorrelateAcrossTheGateway(t *testing.T) {
	down, up, finish := joinedRun(t)

	cross(t, down, transcript.C2S, call(1, trace1, "1")) // 0: charpy's call
	cross(t, up, transcript.C2S, call(7, trace1, "1"))   // 1: forwarded, trace kept
	cross(t, up, transcript.S2C, answer(7))              // 2: charpy's upstream answers
	cross(t, down, transcript.S2C, answer(1))            // 3: the gateway answers charpy
	cross(t, down, transcript.C2S, call(2, trace2, "2")) // 4: charpy's second call
	cross(t, up, transcript.C2S, call(8, "", "2"))       // 5: forwarded, trace dropped
	cross(t, up, transcript.C2S, call(9, "", "99"))      // 6: nothing charpy sent
	links := finish()
	if len(links) != 7 {
		t.Fatalf("%d frames, want 7", len(links))
	}

	id := func(i int) string {
		if links[i].CharpyID == nil {
			return ""
		}
		return *links[i].CharpyID
	}
	for _, c := range []struct {
		frame int
		via   transcript.Via
		same  int // the frame whose charpy_id this one must share; -1 for none
	}{
		{0, transcript.ViaTraced, -1},
		{1, transcript.ViaTraced, 0},
		{2, transcript.ViaTraced, 0},
		{3, transcript.ViaTraced, 0},
		{4, transcript.ViaTraced, -1},
		{5, transcript.ViaInferred, 4},
		{6, transcript.ViaNone, -1},
	} {
		if got := links[c.frame].Via; got != c.via {
			t.Errorf("frame %d joined via %s, want %s", c.frame, got, c.via)
		}
		if c.same >= 0 && (id(c.frame) == "" || id(c.frame) != id(c.same)) {
			t.Errorf("frame %d has charpy_id %q, want frame %d's %q", c.frame, id(c.frame), c.same, id(c.same))
		}
	}
	if id(0) == id(4) {
		t.Error("two calls share one join")
	}
	if links[5].Confidence >= 1 || links[5].Confidence <= 0 {
		t.Errorf("the inferred join's confidence = %v, want between 0 and 1", links[5].Confidence)
	}
}

// charpy's notifications carry a trace like its requests (ADR-014), so on
// the downstream face they keep their join key; and a gateway's own
// notifications/initialized upstream -- not forwarded, the same contentless
// bytes as charpy's -- joins nothing, rather than charpy's by content.
func TestCharpysNotificationsKeepTheirJoin(t *testing.T) {
	down, up, finish := joinedRun(t)
	cross(t, down, transcript.C2S, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"traceparent":"`+trace1+`"}}}`)
	cross(t, up, transcript.C2S, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	links := finish()
	if len(links) != 2 {
		t.Fatalf("%d frames, want 2", len(links))
	}
	if links[0].Via != transcript.ViaTraced || links[0].CharpyID == nil {
		t.Errorf("charpy's notification joined %+v, want traced with an id", links[0])
	}
	if links[1].Via != transcript.ViaNone {
		t.Errorf("the gateway's own initialized joined %+v, want none", links[1])
	}
}
