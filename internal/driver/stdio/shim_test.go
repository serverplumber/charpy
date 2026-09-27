package stdio_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// The subject. Re-execing the test binary keeps the fixture in one file and
// needs no build step; the env var is what tells it to be a server instead of
// a test.
const subjectEnv = "CHARPY_TEST_SUBJECT"

// sdkSubjectEnv selects a real SDK server instead of the hand-rolled fixture.
// The scripted driver connects charpy's own SDK peer, and a peer completes a
// handshake only against something that actually implements one -- so the
// scripted tests need a conforming subject rather than a frame echo.
const sdkSubjectEnv = "CHARPY_TEST_SDK_SUBJECT"

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(sdkSubjectEnv) != "":
		os.Exit(sdkSubject())
	case os.Getenv(subjectEnv) != "":
		os.Exit(subject())
	default:
		os.Exit(m.Run())
	}
}

// sdkSubject is a conforming MCP server over stdio, built on the same SDK as
// the reference peer. charpy assumes its subject already passes conformance
// (README), so anything the run provokes here is charpy's doing.
func sdkSubject() int {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	// ask_model asks its client for a completion mid-call, which is how a
	// server comes to have a request of its own outstanding.
	mcp.AddTool(srv, &mcp.Tool{Name: "ask_model", Description: "asks the client's model"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			got, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
				MaxTokens: 16,
				Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}},
			})
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{got.Content}}, nil, nil
		})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "sdk subject: %v\n", err)
		return 1
	}
	return 0
}

// subject is a minimal MCP-shaped server: it answers initialize with a
// protocol version and everything else with an empty result, and it reports
// what it actually received so a test can tell a truncated frame from a whole
// one.
func subject() int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for sc.Scan() {
		line := sc.Bytes()

		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// A frame the subject cannot parse is exactly what a truncation
			// produces, and saying so on stderr is how the test sees it.
			fmt.Fprintf(os.Stderr, "subject: unparseable frame: %d bytes\n", len(line))
			continue
		}
		if probe.Method == "" || len(probe.ID) == 0 {
			continue
		}

		result := `{}`
		if probe.Method == "initialize" {
			result = `{"protocolVersion":"2025-11-25","capabilities":{},` +
				`"serverInfo":{"name":"fixture","version":"1"}}`
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", probe.ID, result)
	}
	return 0
}

func subjectCommand() []string {
	return []string{os.Args[0], "-test.run=TestMain"}
}

type run struct {
	lines   []map[string]any
	raw     string
	toPeer  string
	subjErr string
}

// drive relays the given client frames through a shim and returns everything
// the run produced.
func drive(t *testing.T, cases []interpose.Case, clientFrames ...string) run {
	t.Helper()
	return driveWith(t, func(o *stdio.Options) { o.Cases = cases }, clientFrames...)
}

// driveWith is drive with the shim's options open to the test, for the runs
// that arm some other way than a fixed case list.
func driveWith(t *testing.T, set func(*stdio.Options), clientFrames ...string) run {
	t.Helper()

	var transcriptBuf, peer, subjErr bytes.Buffer
	sched := clock.RealSched()

	tr, err := transcript.New(&transcriptBuf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeStdioIngress,
			Subject: transcript.Subject{Class: transcript.ClassServer},
			Clock:   clock.ModeReal,
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}

	o := stdio.Options{
		Command:    subjectCommand(),
		Env:        append(os.Environ(), subjectEnv+"=1"),
		In:         strings.NewReader(strings.Join(clientFrames, "\n") + "\n"),
		Out:        nopCloser{&peer},
		Errs:       &subjErr,
		Transcript: tr,
		Sched:      sched,
		Ledger:     interpose.NewLedger(sched),
		Face:       transcript.Downstream,
		RunSeed:    "8f2c1a",
	}
	set(&o)
	sh, err := stdio.New(o)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sh.Run(ctx); err != nil {
		t.Fatalf("shim: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	return run{
		lines:   decodeLines(t, transcriptBuf.String()),
		raw:     transcriptBuf.String(),
		toPeer:  peer.String(),
		subjErr: subjErr.String(),
	}
}

func decodeLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("transcript line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func (r run) ofType(t string) []map[string]any {
	var out []map[string]any
	for _, l := range r.lines {
		if l["type"] == t {
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

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func toolsCall(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"search"}}`, id)
}

// The whole point of the shim: a real client's traffic reaches a real server
// through charpy, and charpy writes down what crossed.
func TestShimRelaysAndTranscribes(t *testing.T) {
	got := drive(t, nil, initialize, toolsCall(2))

	if len(got.lines) == 0 {
		t.Fatal("no transcript")
	}
	if got.lines[0]["type"] != "header" {
		t.Fatalf("first line is %v, want the header", got.lines[0]["type"])
	}

	// The header is written when the initialize exchange settles the
	// revision, which is after several frames have already been captured --
	// which is exactly what the writer's hold-until-header exists for.
	rev, _ := got.lines[0]["revision"].(map[string]any)
	if rev == nil || rev["negotiated"] != "2025-11-25" {
		t.Errorf("header revision = %v, want the observed 2025-11-25", got.lines[0]["revision"])
	}
	if rev["how"] != "initialize" {
		t.Errorf("revision.how = %v, want initialize", rev["how"])
	}

	frames := got.ofType("frame")
	if len(frames) != 4 {
		t.Fatalf("transcribed %d frames, want 4 (two each way)", len(frames))
	}
	for _, f := range frames {
		if f["face"] != "downstream" {
			t.Errorf("face = %v, want downstream: a server subject has one face", f["face"])
		}
		if f["transport"] != "stdio" {
			t.Errorf("transport = %v", f["transport"])
		}
	}

	// The client got real answers back.
	if strings.Count(got.toPeer, `"result"`) != 2 {
		t.Errorf("the client received:\n%s", got.toPeer)
	}
	if len(got.events("conn_open")) != 1 || len(got.events("subject_exit")) != 1 {
		t.Error("the subject's lifecycle is not in the transcript")
	}

	// A response carries no method of its own, so a method in that column can
	// only have come from the ledger resolving the id. That is ledger job 2,
	// and without it every response in every transcript is unattributable to
	// what asked for it.
	for _, f := range frames {
		if f["direction"] != "s2c" {
			continue
		}
		if f["method"] == nil {
			t.Errorf("response to id %v carries no method; the ledger echo is not wired", f["id"])
		}
	}
	if frames[2]["method"] != "initialize" || frames[3]["method"] != "tools/call" {
		t.Errorf("responses resolved to %v and %v", frames[2]["method"], frames[3]["method"])
	}
}

// A fault reaches the subject: the case matches, the bytes are cut, and the
// server says it could not parse what arrived.
func TestShimInjectsATruncation(t *testing.T) {
	truncate := interpose.Case{
		ID:       "stream/truncate-mid-frame",
		Citation: "stream/truncate-mid-frame@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method: interpose.ParseGlob("tools/call"),
			Kind:   "request",
		},
		Fault: interpose.Fault{
			Kind:   "truncate",
			Params: map[string]any{"cut_at": "mid_frame", "then": "stall"},
		},
	}

	got := drive(t, []interpose.Case{truncate}, initialize, toolsCall(2))

	if len(got.events("fault_scheduled")) != 1 {
		t.Errorf("fault_scheduled events: %d, want 1", len(got.events("fault_scheduled")))
	}
	applied := got.events("fault_applied")
	if len(applied) != 1 {
		t.Fatalf("fault_applied events: %d, want 1", len(applied))
	}
	if d, _ := applied[0]["detail"].(map[string]any); d["verb"] != "rewrite" {
		t.Errorf("verb = %v, want rewrite", d["verb"])
	}

	// The subject saw a frame it could not parse, which is the fault landing.
	if !strings.Contains(got.subjErr, "unparseable frame") {
		t.Errorf("the subject parsed everything it received:\n%s", got.subjErr)
	}

	// And the transcript carries the truncated bytes with their attribution.
	var cut map[string]any
	for _, f := range got.ofType("frame") {
		if f["fault"] != nil {
			cut = f
		}
	}
	if cut == nil {
		t.Fatal("no frame carries fault attribution")
	}
	if cut["kind"] != "malformed" {
		t.Errorf("kind = %v, want malformed: a cut frame has no envelope", cut["kind"])
	}
	raw, err := base64.StdEncoding.DecodeString(cut["raw"].(string))
	if err != nil {
		t.Fatal(err)
	}
	whole := toolsCall(2)
	if len(raw) >= len(whole) {
		t.Errorf("raw is %d bytes, not a truncation of %d", len(raw), len(whole))
	}
	if !strings.HasPrefix(whole, string(raw)) {
		t.Errorf("raw is not a prefix of the frame: %q", raw)
	}
	f, _ := cut["fault"].(map[string]any)
	if f["case_id"] != "stream/truncate-mid-frame" || f["kind"] != "truncate" {
		t.Errorf("attribution = %v", cut["fault"])
	}
	// The cut bytes parse to no id, so the attribution is the only record of
	// which request they were.
	if rep, _ := f["replaced"].(map[string]any); rep["id"] != "2" || rep["id_type"] != "number" || rep["kind"] != "request" {
		t.Errorf("replaced = %v, want the request's id, number 2, and that it was a request", f["replaced"])
	}
}

// A case whose matcher never fires leaves the traffic alone. Under relay that
// is the normal outcome, and it is UNTRIGGERED rather than a failure.
func TestShimLeavesUnmatchedTrafficAlone(t *testing.T) {
	never := interpose.Case{
		ID:       "stream/never",
		Citation: "stream/never@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Method: interpose.ParseGlob("resources/read")},
		Fault:    interpose.Fault{Kind: "truncate", Params: map[string]any{}},
	}

	got := drive(t, []interpose.Case{never}, initialize, toolsCall(2))

	if n := len(got.events("fault_applied")); n != 0 {
		t.Errorf("%d faults applied, want none", n)
	}
	if strings.Contains(got.subjErr, "unparseable") {
		t.Errorf("the subject saw a corrupted frame:\n%s", got.subjErr)
	}
	for _, f := range got.ofType("frame") {
		if f["fault"] != nil {
			t.Errorf("a frame carries attribution with no fault applied: %v", f["fault"])
		}
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// armer stands in for --revision auto's selection: it records which revision
// the shim asked about, and answers with cases cited against it.
type armer struct {
	mu    sync.Mutex
	asked []revision.Revision
	match interpose.Match
}

func (a *armer) arm(r revision.Revision) []interpose.Case {
	a.mu.Lock()
	a.asked = append(a.asked, r)
	a.mu.Unlock()
	return []interpose.Case{{
		ID:       "frame/malformed-unbalanced",
		Citation: "frame/malformed-unbalanced@" + string(r) + "#seed=8f2c1a",
		Match:    a.match,
		Fault:    interpose.Fault{Kind: "malformed_json"},
	}}
}

// An auto run arms when the handshake settles, against the revision it settled
// on. That revision is what the citation names -- on the armed list and on the
// faulted frame -- because it is what a reader reproduces the run with.
func TestAnAutoRunArmsAgainstTheNegotiatedRevision(t *testing.T) {
	a := &armer{match: interpose.Match{Method: interpose.ParseGlob("tools/call"), Direction: transcript.S2C}}
	got := driveWith(t, func(o *stdio.Options) { o.Arm = a.arm }, initialize, toolsCall(2))

	if len(a.asked) != 1 || a.asked[0] != revision.V20251125 {
		t.Fatalf("armed for %v, want once for the negotiated 2025-11-25", a.asked)
	}

	want := "frame/malformed-unbalanced@2025-11-25#seed=8f2c1a"
	hdr := got.ofType("header")
	if len(hdr) != 1 {
		t.Fatalf("headers: %d", len(hdr))
	}
	if cases, _ := hdr[0]["cases"].([]any); len(cases) != 1 || cases[0] != want {
		t.Errorf("armed = %v, want [%s]", hdr[0]["cases"], want)
	}

	var cited bool
	for _, f := range got.ofType("frame") {
		if fa, _ := f["fault"].(map[string]any); fa != nil {
			cited = true
			if fa["citation"] != want {
				t.Errorf("faulted frame cites %v, want %s", fa["citation"], want)
			}
		}
	}
	if !cited {
		t.Error("the case armed at settlement never fired on the call after it")
	}
}

// Before the handshake settles nothing is armed, so nothing is faulted: charpy
// does not yet know which cases apply or what revision a citation would name.
// The client's initialize request is the frame that always crosses first.
func TestAnAutoRunFaultsNothingBeforeTheHandshake(t *testing.T) {
	a := &armer{match: interpose.Match{Method: interpose.ParseGlob("initialize"), Direction: transcript.C2S}}
	got := driveWith(t, func(o *stdio.Options) { o.Arm = a.arm }, initialize, toolsCall(2))

	if n := len(got.events(string(transcript.FaultApplied))); n != 0 {
		t.Errorf("%d faults applied before the revision was known", n)
	}
	// The subject still settled the run, so the case was armed and is on the
	// record -- as a case that never fired, which coverage reports.
	if len(a.asked) != 1 {
		t.Errorf("armed %d times, want once", len(a.asked))
	}
}

// A run arms once. Both a fixed list and a deferred one is a caller that has
// not decided which, and guessing would arm the wrong set.
func TestCasesAndArmAreExclusive(t *testing.T) {
	_, err := stdio.New(stdio.Options{
		Command: subjectCommand(),
		Cases:   []interpose.Case{{ID: "x"}},
		Arm:     func(revision.Revision) []interpose.Case { return nil },
	})
	if err == nil {
		t.Error("New accepted both Cases and Arm")
	}
}

// A plan that delivers the matched frame untouched beside what it injects
// leaves that frame the subject's. Attributed, it read as tampered, and every
// layer that judges the subject -- schema, invariants, reaction, all through
// SubjectOriginated -- dropped the subject's real answer from its evidence.
func TestAnUntouchedAnswerBesideADuplicateIsStillTheSubjects(t *testing.T) {
	dup := interpose.Case{
		ID:       "id/duplicate-response",
		Citation: "id/duplicate-response@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Method: interpose.ParseGlob("tools/call"), Direction: transcript.S2C},
		Fault:    interpose.Fault{Kind: "duplicate_id", Params: map[string]any{"mode": "double_response"}},
	}
	got := drive(t, []interpose.Case{dup}, initialize, toolsCall(2))

	var own, charpys int
	for _, f := range drivertest.Read(t, got.raw).Frames() {
		if f.Direction != transcript.S2C || f.MethodName() != "tools/call" {
			continue
		}
		if oracle.SubjectOriginated(transcript.ClassServer, f) {
			own++
		} else {
			charpys++
		}
	}
	if own != 1 || charpys != 1 {
		t.Errorf("the subject's answers %d, charpy's %d; want the original the subject's and only the copy charpy's",
			own, charpys)
	}
}
