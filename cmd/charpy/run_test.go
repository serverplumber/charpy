package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// The subject: a conforming MCP server over stdio, re-exec'd from this test
// binary. charpy assumes its subject already passes conformance, so the
// fixture is built on the same SDK as the reference peer and anything the run
// provokes is charpy's doing.
const runSubjectEnv = "CHARPY_RUN_TEST_SUBJECT"

func TestMain(m *testing.M) {
	if os.Getenv(runSubjectEnv) == "" {
		os.Exit(m.Run())
	}

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
		fmt.Fprintf(os.Stderr, "subject: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func subjectArgs() []string {
	return []string{"--", os.Args[0], "-test.run=TestMain"}
}

// runCLI invokes the run command exactly as a shell would, with the fixture
// server as the subject.
func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()

	t.Setenv(runSubjectEnv, "1")
	var out bytes.Buffer
	stderr := captureStderr(t, &out)

	full := append([]string{"--out", dir}, args...)
	full = append(full, subjectArgs()...)
	code := cmdRun(full, &out)
	stderr()

	return out.String(), dir, code
}

// captureStderr redirects os.Stderr into w for the duration of a call, since
// charpy writes everything there on purpose: in a shim charpy is the server,
// and a line on stdout would be a frame the client cannot parse.
func captureStderr(t *testing.T, w *bytes.Buffer) func() {
	t.Helper()
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = pw

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		defer close(done)
		_, _ = buf.ReadFrom(r)
	}()

	return func() {
		os.Stderr = old
		_ = pw.Close()
		<-done
		w.Write(buf.Bytes())
	}
}

func transcripts(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// One case per run means one transcript per case, which is ADR-012 as seen
// from the command line: a glob is several runs, not one session with
// several faults armed.
func TestRunCaseGlobWritesOneTranscriptPerCase(t *testing.T) {
	// frame/malformed-unbalanced corrupts the handshake, so its peer waits out
	// the script deadline rather than finishing: a short one keeps that case's
	// ending in the suite without paying thirty seconds for it.
	out, dir, code := runCLI(t, "--case", "*", "--revision", "2025-11-25",
		"--seed", "8f2c1a", "--timeout", "2s")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	// The four the catalogue puts to a server over stdio on this revision:
	// frame/malformed-request, id/duplicate-request-inflight,
	// id/unsolicited-to-server and lifecycle/server-request-unanswered. A
	// fifth would belong here rather than silently widening the glob.
	files := transcripts(t, dir)
	if len(files) != 4 {
		t.Fatalf("wrote %d transcripts, want one per applicable case\n%s", len(files), out)
	}

	// Every transcript is a complete run in its own right: its own header,
	// its own seed, its own subject.
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		first := strings.SplitN(strings.TrimSpace(string(body)), "\n", 2)[0]
		var hdr map[string]any
		if err := json.Unmarshal([]byte(first), &hdr); err != nil {
			t.Fatalf("%s: first line is not JSON: %v", f, err)
		}
		if hdr["type"] != "header" {
			t.Errorf("%s: first line is %v, want a header", f, hdr["type"])
		}
		if hdr["seed"] != "8f2c1a" {
			t.Errorf("%s: seed = %v", f, hdr["seed"])
		}
		// Owned stimulus, so the transcript names what spoke in it.
		p, ok := hdr["peer"].(map[string]any)
		if !ok {
			t.Fatalf("%s: header names no peer", f)
		}
		if p["era"] != "2025-11-25" {
			t.Errorf("%s: peer era = %v", f, p["era"])
		}
	}
}

// A single case is a single run, and the fault it declares actually fires.
func TestRunASingleCaseInjectsIt(t *testing.T) {
	out, dir, code := runCLI(t, "--case", "frame/malformed-request",
		"--revision", "2025-11-25", "--seed", "8f2c1a", "--timeout", "2s")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	files := transcripts(t, dir)
	if len(files) != 1 {
		t.Fatalf("wrote %d transcripts, want 1\n%s", len(files), out)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"fault_applied"`) {
		t.Errorf("the case never fired:\n%s", out)
	}

	// The run line quotes the citation, not the bare id: a bug report needs
	// the revision and the seed to be reproducible from.
	if !strings.Contains(out, "frame/malformed-request@2025-11-25#seed=8f2c1a") {
		t.Errorf("output does not quote the citation:\n%s", out)
	}
}

// A selection matching nothing is a run that would pass by not running, which
// is the failure mode the loader exists to prevent.
func TestRunRefusesACaseGlobThatMatchesNothing(t *testing.T) {
	out, dir, code := runCLI(t, "--case", "nosuch/*", "--revision", "2025-11-25")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
	if len(transcripts(t, dir)) != 0 {
		t.Error("a selection that matched nothing still wrote a transcript")
	}
}

// A seed is checked when the flag is read, not when the first transcript is
// opened: by then the run has been announced, the seed is in every citation,
// and an empty transcript is already on disk.
func TestRunRefusesAMalformedSeed(t *testing.T) {
	for _, s := range []string{"1", "8f2c1", "8F2C1A", "8f2c1g", "8f2c1a8f2c1a8f2c1"} {
		t.Run(s, func(t *testing.T) {
			out, dir, code := runCLI(t, "--case", "frame/malformed-request",
				"--revision", "2025-11-25", "--seed", s)
			if code != exitHarness {
				t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
			}
			if !strings.Contains(out, "seed") {
				t.Errorf("the refusal does not name the seed:\n%s", out)
			}
			if n := len(transcripts(t, dir)); n != 0 {
				t.Errorf("a malformed seed still wrote %d transcripts", n)
			}
		})
	}
}

// Which tool a script calls is the subject's registration order unless the run
// says otherwise, which is fine against a fixture and not against a subject
// whose first tool does something.
func TestRunPinsTheToolAndItsArguments(t *testing.T) {
	out, dir, code := runCLI(t, "--case", "id/unsolicited-to-server", "--revision", "2025-11-25",
		"--seed", "8f2c1a", "--tool", "nosuch", "--args", `{"x":1}`)
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	files := transcripts(t, dir)
	if len(files) != 1 {
		t.Fatalf("wrote %d transcripts, want 1\n%s", len(files), out)
	}
	frames := rawFrames(t, files[0])
	// The fixture lists echo, so a call naming anything else can only have
	// come from --tool rather than from tools/list.
	if !strings.Contains(frames, `"name":"nosuch"`) {
		t.Errorf("--tool was not honoured; the script called the listed tool:\n%s", out)
	}
	if !strings.Contains(frames, `"x":1`) {
		t.Errorf("--args did not reach the call:\n%s", out)
	}
}

// rawFrames returns every frame body in a transcript, decoded. Raw bytes are
// carried base64 so a frame the subject wrote survives the transcript exactly.
func rawFrames(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var frames strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var l struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil || l.Raw == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(l.Raw)
		if err != nil {
			t.Fatalf("%s: raw is not base64: %v", path, err)
		}
		frames.Write(raw)
		frames.WriteByte('\n')
	}
	return frames.String()
}

// Arguments that are not a JSON object are refused before the subject is
// spawned, for the same reason a malformed seed is.
func TestRunRefusesMalformedToolArguments(t *testing.T) {
	for _, args := range []string{"{", `["x"]`, "3"} {
		t.Run(args, func(t *testing.T) {
			out, dir, code := runCLI(t, "--case", "frame/malformed-request",
				"--revision", "2025-11-25", "--tool", "echo", "--args", args)
			if code != exitHarness {
				t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
			}
			if n := len(transcripts(t, dir)); n != 0 {
				t.Errorf("malformed --args still wrote %d transcripts", n)
			}
		})
	}
}

// --tool and --args say what charpy originates, which only a scripted run
// does: under a relay the client under test chooses its own traffic.
func TestRunRefusesToolWithoutACase(t *testing.T) {
	out, _, code := runCLI(t, "--tool", "echo")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
	if !strings.Contains(out, "--case") {
		t.Errorf("the refusal does not say what is missing:\n%s", out)
	}
}

// charpy speaks first under owned stimulus, so it has to choose an era.
// "auto" is for watching somebody else's handshake.
func TestRunRefusesAutoRevisionWithACase(t *testing.T) {
	out, _, code := runCLI(t, "--case", "frame/malformed-request")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
	if !strings.Contains(out, "--revision") {
		t.Errorf("the refusal does not say what to do:\n%s", out)
	}
}

// Driving a client subject means charpy serves, which is a different driver.
func TestRunRefusesANonServerSubjectWithACase(t *testing.T) {
	out, _, code := runCLI(t, "--case", "id/*", "--revision", "2025-11-25", "--subject", "client")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
}

// httpSubject stands up a conforming Streamable HTTP server for the proxy CLI
// tests and returns its URL.
func httpSubject(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL + "/"
}

func runHTTP(t *testing.T, url string, args ...string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	full := append([]string{"--out", dir, "--subject-url", url}, args...)
	code := cmdRun(full, &out)
	stderr()
	return out.String(), dir, code
}

// The proxy driver from the command line: charpy proxies a running HTTP
// subject, one case per run, and every case it puts to an HTTP server fires.
func TestRunSubjectURLProxiesAndFaults(t *testing.T) {
	out, dir, code := runHTTP(t, httpSubject(t),
		"--case", "*", "--revision", "2025-11-25", "--seed", "8f2c1a", "--timeout", "5s")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	files := transcripts(t, dir)
	// The four cases put to a server over HTTP: frame/malformed-request-body,
	// stream/truncate-request-body, id/duplicate-request-inflight and
	// id/unsolicited-to-server. The stdio-only frame/malformed-request, and
	// every case put to a client, are dropped at selection rather than armed
	// to never fire.
	if len(files) != 4 {
		t.Fatalf("wrote %d transcripts, want 4 HTTP server cases\n%s", len(files), out)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"fault_applied"`) {
			t.Errorf("%s: the case never fired", f)
		}
		if !strings.Contains(string(body), `"transport":"http"`) {
			t.Errorf("%s: frames are not http transport", f)
		}
	}
}

// A subject URL is owned stimulus, so it needs a case and an explicit era, for
// the same reason the stdio --case path does.
func TestRunSubjectURLRefusesWithoutACase(t *testing.T) {
	out, _, code := runHTTP(t, httpSubject(t), "--revision", "2025-11-25")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
}

func TestRunSubjectURLRefusesAutoRevision(t *testing.T) {
	out, _, code := runHTTP(t, httpSubject(t), "--case", "stream/*")
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out)
	}
	if !strings.Contains(out, "--revision") {
		t.Errorf("the refusal does not say what to do:\n%s", out)
	}
}

// A subject command and a subject URL are two different drivers; asking for
// both is a mistake worth naming rather than silently preferring one.
func TestRunSubjectURLAndCommandConflict(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	code := cmdRun([]string{"--out", dir, "--subject-url", httpSubject(t),
		"--case", "stream/*", "--revision", "2025-11-25", "--", "some-command"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
}

// --hostile needs an explicit era, because charpy claims one to the client.
func TestRunHostileRefusesAutoRevision(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	code := cmdRun([]string{"--out", dir, "--hostile", "--case", "id/*"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
	if !strings.Contains(out.String(), "--revision") {
		t.Errorf("the refusal does not say what to do:\n%s", out.String())
	}
}

// --hostile serves the client that spawned charpy; a subject command is a
// different driver.
func TestRunHostileAndCommandConflict(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	code := cmdRun([]string{"--out", dir, "--hostile", "--revision", "2025-11-25", "--", "some-command"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
}

// A glob that matches no client-applicable case is refused rather than serving
// as a plain correct server that tests nothing.
func TestRunHostileRefusesAGlobMatchingNoClientCase(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	// stream/truncate-mid-event is server/gateway + http; no client subject.
	code := cmdRun([]string{"--out", dir, "--hostile", "--revision", "2025-11-25", "--case", "frame/malformed-request"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
}

// A shipped case run end to end asks the subject one more question once its
// fault has acted, on the same session, and the subject's answer crosses back
// clean. The corrupted request never reaches the server as a request, charpy
// answers its own peer so the session survives, and the server is asked again
// -- which is the answer the reaction layer judges.
func TestRunAsksAFollowUpAfterTheFault(t *testing.T) {
	out, dir, code := runHTTP(t, httpSubject(t), "--case", "frame/malformed-request-body",
		"--revision", "2025-11-25", "--seed", "8f2c1a", "--tool", "echo", "--timeout", "5s")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	files := transcripts(t, dir)
	if len(files) != 1 {
		t.Fatalf("wrote %d transcripts, want 1\n%s", len(files), out)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	tr := drivertest.Read(t, string(body))
	q, _ := drivertest.FollowUpAnswered(t, tr)
	if q.MethodName() != "ping" {
		t.Errorf("follow-up was %q, want ping at 2025-11-25", q.MethodName())
	}
	drivertest.ReactionAnswered(t, tr)

	// The case declares a recovery budget, so a fresh session is tried too.
	probes := tr.Events(transcript.Probe)
	if len(probes) != 1 || probes[0].Detail["outcome"] != string(transcript.ProbeOK) {
		t.Errorf("want one probe that recovered; got %d", len(probes))
	}
}

// An auto run compiles each case once per revision the handshake could settle
// on, cited against that revision, and arms nothing up front. A case whose
// range excludes a revision is not among that revision's cases: the old
// selection armed the union and cited everything against the oldest revision
// it allowed, so a reproduction from the citation spoke the wrong era.
func TestAutoCompilesEachCaseForTheRevisionItWillBeCitedUnder(t *testing.T) {
	cfg, code := runConfig("auto", "8f2c1a", "client", string(transcript.TransportStdio))
	if code != exitClean {
		t.Fatalf("runConfig exit %d", code)
	}
	if len(cfg.cases) != 0 {
		t.Errorf("an auto run armed %d cases before the handshake", len(cfg.cases))
	}

	for _, r := range revision.All() {
		for _, c := range cfg.byRevision[r] {
			if want := "@" + string(r) + "#"; !strings.Contains(c.Citation, want) {
				t.Errorf("under %s, %s is cited as %s", r, c.ID, c.Citation)
			}
		}
	}

	// schema/output-schema-violated applies from 2025-06-18: absent before,
	// present from then on.
	has := func(r revision.Revision) bool {
		for _, c := range cfg.byRevision[r] {
			if c.ID == "schema/output-schema-violated" {
				return true
			}
		}
		return false
	}
	if has(revision.V20241105) {
		t.Error("a >=2025-06-18 case is armed for a 2024-11-05 handshake")
	}
	if !has(revision.V20251125) {
		t.Error("a >=2025-06-18 case is missing for a 2025-11-25 handshake")
	}
}

// A case whose answer nothing here can see is dropped at selection, and asking
// for it by name says why, rather than that it does not apply. Whether a
// client takes "7" for 7 leaves nothing on the wire; the differential is what
// would see it, and it is not built.
func TestRunNamesACaseNothingCanObserve(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	stderr := captureStderr(t, &out)
	code := cmdRun([]string{"--out", dir, "--hostile", "--revision", "2025-11-25",
		"--case", "id/duplicate-response-vary-type"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
	if !strings.Contains(out.String(), "its answer is seen by differential") {
		t.Errorf("the refusal does not say what could see the answer:\n%s", out.String())
	}
}

// --gateway needs everything a gateway run is made of, and says which part is
// missing rather than starting half a run.
func TestGatewayRunRefusesWhatItCannotRun(t *testing.T) {
	dir := t.TempDir()
	gw := []string{"--", "gw", "-upstream", "{upstream0}"}
	for name, args := range map[string][]string{
		"no subject URL": append([]string{"--gateway", "--case", "*", "--revision", "2025-11-25"}, gw...),
		"no command":     {"--gateway", "--subject-url", "http://127.0.0.1:1/", "--case", "*", "--revision", "2025-11-25"},
		"no case":        append([]string{"--gateway", "--subject-url", "http://127.0.0.1:1/", "--revision", "2025-11-25"}, gw...),
		"auto revision":  append([]string{"--gateway", "--subject-url", "http://127.0.0.1:1/", "--case", "*"}, gw...),
		"stdio gateway":  {"--subject", "gateway", "--", "gw"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := cmdRun(append([]string{"--out", dir}, args...), &out); code != exitHarness {
				t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
			}
		})
	}
}
