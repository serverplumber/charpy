package gateway_test

import (
	"context"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/gateway"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/reaction"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// fixture is the fixture gateway's binary, built once: the driver spawns a
// process, so the subject it is tested against has to be one. metastrip is a
// test-only gateway that drops _meta from what it forwards.
var fixture, metastrip string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "charpy-gateway-test")
	if err != nil {
		panic(err)
	}
	fixture = filepath.Join(dir, "fixture-gateway")
	metastrip = filepath.Join(dir, "metastrip")
	for out, pkg := range map[string]string{
		fixture:   "github.com/serverplumber/charpy/internal/fixture/gateway/cmd/fixture-gateway",
		metastrip: "./testdata/metastrip",
	} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			panic("building " + pkg + ": " + err.Error())
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// shipped compiles one case from the shipped catalogue.
func shipped(t *testing.T, id string) interpose.Case {
	t.Helper()
	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		t.Fatal(err)
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
	t.Fatalf("no shipped case %s at 2025-11-25", id)
	return interpose.Case{}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Logf("gateway: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// drive runs one case against a freshly spawned fixture gateway and returns
// the transcript.
func drive(t *testing.T, c interpose.Case) *transcript.Transcript {
	t.Helper()
	return driveWith(t, c, 10*time.Second, planted())
}

// planted starts the fixture gateway on two upstreams with the plants named.
func planted(plants ...string) func(addr string) []string {
	return func(addr string) []string {
		cmd := []string{fixture, "-http", addr, "-upstream", "{upstream0}", "-upstream", "{upstream1}"}
		if len(plants) > 0 {
			cmd = append(cmd, "-plant", strings.Join(plants, ","))
		}
		return cmd
	}
}

// driveWith runs one case against a gateway the command starts on addr, with
// timeout bounding its start, its script and each question.
func driveWith(t *testing.T, c interpose.Case, timeout time.Duration, command func(addr string) []string) *transcript.Transcript {
	t.Helper()
	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassGateway}, Clock: clock.ModeReal,
		},
		Sched: sched,
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	d, err := gateway.New(gateway.Options{
		Command:    command(addr),
		SubjectURL: "http://" + addr + "/",
		Errs:       logWriter{t},
		Case:       c,
		Era:        revision.V20251125,
		Timeout:    timeout,
		Transcript: tr, Sched: sched, Wall: clock.NewFixedWall(time.Unix(0, 0)), Ledger: interpose.NewLedger(sched), RunSeed: "8f2c1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := transcript.Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func conn(f *transcript.FrameLine) string {
	if f.ConnID == nil {
		return ""
	}
	return *f.ConnID
}

// What every gateway run's transcript must hold, whichever face its case
// faults: one header, at the downstream face's revision; frames on both
// faces, upstream ones named by the upstream they reached; each upstream
// stated once; and the gateway charpy spawned, killed by charpy at the end.
func checkRun(t *testing.T, tr *transcript.Transcript) {
	t.Helper()
	if tr.Header.Revision == nil || tr.Header.Revision.Negotiated != revision.V20251125 {
		t.Errorf("header revision = %+v, want 2025-11-25", tr.Header.Revision)
	}
	faces := map[transcript.Face]int{}
	for _, f := range tr.Frames() {
		faces[f.Face]++
		if f.Face == transcript.Upstream && !strings.HasPrefix(conn(f), "u0-") && !strings.HasPrefix(conn(f), "u1-") {
			t.Errorf("upstream frame %d on %q, named for no upstream", f.Seq, conn(f))
		}
		if f.Face == transcript.Downstream && !strings.HasPrefix(conn(f), "d-") {
			t.Errorf("downstream frame %d on %q", f.Seq, conn(f))
		}
	}
	if faces[transcript.Downstream] == 0 || faces[transcript.Upstream] == 0 {
		t.Errorf("frames per face = %v, want both", faces)
	}

	upstreams := map[string]bool{}
	for _, e := range tr.Events(transcript.ConnOpen) {
		if u, ok := e.Detail["upstream"].(string); ok {
			upstreams[u] = true
		}
	}
	if !upstreams["u0"] || !upstreams["u1"] {
		t.Errorf("upstreams stated = %v, want u0 and u1", upstreams)
	}

	exits := tr.Events(transcript.SubjectExit)
	if len(exits) != 1 {
		t.Fatalf("%d subject_exit events, want 1", len(exits))
	}
	if killed, _ := exits[0].Detail["killed_by_charpy"].(bool); !killed {
		t.Errorf("the gateway exited on its own: %v", exits[0].Detail)
	}
}

// notes is the transcript's harness notes, for a failure to say why a fault
// did not apply.
func notes(tr *transcript.Transcript) []string {
	var out []string
	for _, e := range tr.Events(transcript.Note) {
		if h, ok := e.Detail["harness"].(string); ok {
			out = append(out, h)
		}
	}
	return out
}

// appliedOn is the face each applied fault acted on.
func appliedOn(tr *transcript.Transcript) []transcript.Face {
	var out []transcript.Face
	for _, e := range tr.Events(transcript.FaultApplied) {
		if e.Face != nil {
			out = append(out, *e.Face)
		}
	}
	return out
}

// A case put to the gateway's downstream face: charpy's client sends it a
// request body cut short.
func TestADownstreamCaseRunsThroughTheGateway(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-request-body"))
	checkRun(t, tr)
	if got := appliedOn(tr); len(got) != 1 || got[0] != transcript.Downstream {
		t.Errorf("faults applied on %v, want once on the downstream face; notes: %q", got, notes(tr))
	}
}

// A case put to the gateway's upstream face: charpy's upstream cuts the event
// stream answering the gateway's second call, which the gateway made because
// charpy's client called through it.
func TestAnUpstreamCaseRunsThroughTheGateway(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-mid-event"))
	checkRun(t, tr)
	if got := appliedOn(tr); len(got) != 1 || got[0] != transcript.Upstream {
		t.Errorf("faults applied on %v, want once on the upstream face; notes: %q", got, notes(tr))
	}
}

func TestACommandNamingNoUpstreamIsRefused(t *testing.T) {
	for _, command := range [][]string{
		{"gw", "-http", "127.0.0.1:1"},
		{"gw", "-upstream", "{upstream1}"},
	} {
		_, err := gateway.New(gateway.Options{
			Command: command, SubjectURL: "http://127.0.0.1:1/",
			Case: shipped(t, "stream/truncate-mid-event"),
		})
		if err == nil {
			t.Errorf("%v: accepted", command)
		}
	}
}

// calls pairs each of charpy's downstream tools/call requests with the
// upstream tools/call requests that share its join.
func calls(t *testing.T, tr *transcript.Transcript) (down, up []*transcript.FrameLine) {
	t.Helper()
	for _, f := range tr.Frames() {
		if f.Kind != envelope.KindRequest || f.Method == nil || *f.Method != "tools/call" {
			continue
		}
		switch {
		case f.Face == transcript.Downstream && f.Direction == transcript.C2S:
			down = append(down, f)
		case f.Face == transcript.Upstream && f.Direction == transcript.C2S:
			up = append(up, f)
		}
	}
	if len(down) == 0 || len(up) == 0 {
		t.Fatalf("%d downstream and %d upstream tools/call requests, want both", len(down), len(up))
	}
	return down, up
}

func joinID(f *transcript.FrameLine) string {
	if f.Link.CharpyID == nil {
		return ""
	}
	return *f.Link.CharpyID
}

// The fixture forwards _meta, so each call charpy makes joins its forwarded
// copy as traced, and the answers on both faces take the same join.
func TestACallJoinsTracedThroughTheFixture(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-mid-event"))
	down, up := calls(t, tr)
	ids := map[string]bool{}
	for _, f := range down {
		if f.Link.Via != transcript.ViaTraced || joinID(f) == "" {
			t.Errorf("charpy's call %d joined %+v, want traced", f.Seq, f.Link)
		}
		ids[joinID(f)] = true
	}
	for _, f := range up {
		if f.Link.Via != transcript.ViaTraced || !ids[joinID(f)] {
			t.Errorf("forwarded call %d joined %+v, want traced to one of charpy's", f.Seq, f.Link)
		}
	}
	// Answers inherit: every frame carrying a join charpy's calls made.
	answered := 0
	for _, f := range tr.Frames() {
		if (f.Kind == envelope.KindResponse || f.Kind == envelope.KindError) && ids[joinID(f)] {
			answered++
		}
	}
	if answered == 0 {
		t.Error("no answer took its call's join")
	}
}

// A gateway that drops _meta drops the trace: its forwarded calls join only
// by content, inferred, and each to exactly the call it copies -- the calls
// are distinct, so the recall is a unique one.
func TestACallJoinsInferredWhenTheGatewayDropsTheTrace(t *testing.T) {
	tr := driveWith(t, shipped(t, "stream/truncate-mid-event"), 10*time.Second, func(addr string) []string {
		return []string{metastrip, "-http", addr, "-upstream", "{upstream0}"}
	})
	down, up := calls(t, tr)
	ids := map[string]bool{}
	for _, f := range down {
		ids[joinID(f)] = true
	}
	for _, f := range up {
		if f.Link.Via != transcript.ViaInferred || !ids[joinID(f)] {
			t.Errorf("forwarded call %d joined %+v, want inferred to one of charpy's", f.Seq, f.Link)
		}
		if f.Link.Confidence != interpose.ConfidenceUniqueContent {
			t.Errorf("forwarded call %d joined at confidence %v, want a unique match", f.Seq, f.Link.Confidence)
		}
	}
}

// answers is the reaction oracle's account of the call the fault was carrying
// and the three questions, keyed by role: "carried", "ping", "u0", "u1". Each
// is what the gateway did -- "answered", "error" or "hang" -- or "unasked"
// for a question charpy never got to put; every other finding must be
// OBSERVED.
func answers(t *testing.T, tr *transcript.Transcript) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range reaction.Check(tr).Findings {
		if f.Check != "reaction" {
			continue
		}
		if f.Reason == "question-not-asked" {
			out[role(f.Summary)] = "unasked"
			continue
		}
		if f.Verdict != oracle.Observed {
			t.Errorf("%s: %q (%s); notes: %q", f.Verdict, f.Summary, f.Reason, notes(tr))
			continue
		}
		r := role(f.Summary)
		switch {
		case strings.Contains(f.Summary, "exited"):
			out["exit"] = "exited"
		case strings.Contains(f.Summary, "did not answer"):
			out[r] = "hang"
		case strings.Contains(f.Summary, "with an error"):
			out[r] = "error"
		default:
			out[r] = "answered"
		}
	}
	return out
}

// role is the role a reaction finding's summary names: "carried", "ping",
// the upstream a call went through, or "other" for anything else -- a call
// through an upstream left unnamed, or an exit.
func role(summary string) string {
	switch {
	case strings.Contains(summary, "the call the fault was carrying"):
		return "carried"
	case strings.Contains(summary, "a ping"):
		return "ping"
	}
	for _, u := range []string{"u0", "u1"} {
		if strings.Contains(summary, "through "+u+" ") {
			return u
		}
	}
	return "other"
}

func wantAnswers(t *testing.T, tr *transcript.Transcript, want map[string]string) {
	t.Helper()
	got := answers(t, tr)
	for role, w := range want {
		if got[role] != w {
			t.Errorf("%s: %q, want %q (all: %v)", role, got[role], w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("questions judged: %v, want %v", got, want)
	}
}

// The fixture loses its upstream session to the cut and tells its client the
// call failed, then opens a new session on the next call and answers all
// three questions: the control for cascade.
func TestAGatewayAnswersAllThreeAfterAnUpstreamCut(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-mid-event"))
	wantAnswers(t, tr, map[string]string{
		"carried": "error", "ping": "answered", "u0": "answered", "u1": "answered",
	})
}

// cascade ends the downstream session with the upstream one. The ping that
// follows goes unanswered, and the calls never cross at all: charpy's client
// has already seen its session end, so only the ping is judged and both
// calls are reported as not asked. Whether the
// cut call is answered with an error first races the plant's close, so either
// is accepted -- but it is judged, on its traced join.
func TestACascadingGatewayAnswersNothingAfterAnUpstreamCut(t *testing.T) {
	tr := driveWith(t, shipped(t, "stream/truncate-mid-event"), 10*time.Second, planted("cascade"))
	got := answers(t, tr)
	if c := got["carried"]; c != "error" && c != "hang" {
		t.Errorf("carried: %q, want an error or a hang (all: %v)", c, got)
	}
	delete(got, "carried")
	want := map[string]string{"ping": "hang", "u0": "unasked", "u1": "unasked"}
	if !maps.Equal(got, want) {
		t.Errorf("questions: %v, want %v", got, want)
	}
}

// hungCall holds open u0's first answer to a tools/call, for good, on a
// stream left open: a hang no shipped case puts on a call
// (gateway/upstream-credential-downstream hangs the handshake).
func hungCall() interpose.Case {
	return interpose.Case{
		ID:       "test/upstream-call-hang",
		Citation: "test/upstream-call-hang@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method: interpose.ParseGlob("tools/call"), Face: transcript.Upstream,
			Direction: transcript.S2C, Kind: envelope.KindResponse,
		},
		Fault: interpose.Fault{Kind: "hang", Params: map[string]any{"scope": "response", "withdraw_after_ms": int64(0)}},
	}
}

// With its idle deadline the fixture gives up on the hung call and tells its
// client so, then serves every question after.
func TestAGatewayWithADeadlineErrorsTheHungCall(t *testing.T) {
	tr := driveWith(t, hungCall(), 5*time.Second, planted())
	wantAnswers(t, tr, map[string]string{
		"carried": "error", "ping": "answered", "u0": "answered", "u1": "answered",
	})
}

// nodeadline leaves the hung call unanswered until charpy's client gives up
// on it. The cancellation reaches upstream, so the questions after it are
// served -- they cannot tell this gateway from the one above; only the
// carried call does.
func TestAGatewayWithoutADeadlineHangsTheCall(t *testing.T) {
	tr := driveWith(t, hungCall(), 5*time.Second, planted("nodeadline"))
	wantAnswers(t, tr, map[string]string{
		"carried": "hang", "ping": "answered", "u0": "answered", "u1": "answered",
	})
}

// A gateway that drops the trace leaves the carried call joined only by
// content, and the oracle says so rather than judge it.
func TestACarriedCallThroughAStrippingGatewayIsInconclusive(t *testing.T) {
	tr := driveWith(t, shipped(t, "stream/truncate-mid-event"), 10*time.Second, func(addr string) []string {
		return []string{metastrip, "-http", addr, "-upstream", "{upstream0}"}
	})
	for _, f := range reaction.Check(tr).Findings {
		if f.Reason == "carried-call-join-inferred" {
			if f.Verdict != oracle.Inconclusive {
				t.Errorf("verdict = %s, want INCONCLUSIVE", f.Verdict)
			}
			return
		}
	}
	t.Errorf("no finding on the carried call's inferred join; notes: %q", notes(tr))
}

// metastrip passes the cut stream through, which ends charpy's client's
// session, so none of the questions crosses. Each is reported as not asked,
// citing the driver's note on why; none is judged, and the liveness probe's
// ping on its own connection does not stand in for the question's.
func TestQuestionsAStrippingGatewayNeverGotAreReportedUnasked(t *testing.T) {
	tr := driveWith(t, shipped(t, "stream/truncate-mid-event"), 10*time.Second, func(addr string) []string {
		return []string{metastrip, "-http", addr, "-upstream", "{upstream0}"}
	})
	unasked := map[string]bool{}
	for _, f := range reaction.Check(tr).Findings {
		if f.Check != "reaction" {
			continue
		}
		switch r := role(f.Summary); {
		case r == "carried":
		case f.Reason != "question-not-asked":
			t.Errorf("%s judged: %s %q", r, f.Verdict, f.Summary)
		default:
			if f.Verdict != oracle.Inconclusive {
				t.Errorf("%s: verdict %s, want INCONCLUSIVE", r, f.Verdict)
			}
			if !strings.Contains(f.Detail, "charpy's client: ") {
				t.Errorf("%s: detail %q cites no note from the driver", r, f.Detail)
			}
			unasked[r] = true
		}
	}
	if !unasked["ping"] || !unasked["u0"] || len(unasked) != 2 {
		t.Errorf("unasked: %v, want ping and u0; notes: %q", unasked, notes(tr))
	}
}
