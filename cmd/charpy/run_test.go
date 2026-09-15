package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	out, dir, code := runCLI(t, "--case", "id/*", "--revision", "2025-11-25", "--seed", "8f2c1a")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	files := transcripts(t, dir)
	if len(files) != 3 {
		t.Fatalf("wrote %d transcripts, want one per id/* case\n%s", len(files), out)
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
	out, dir, code := runCLI(t, "--case", "id/duplicate-response",
		"--revision", "2025-11-25", "--seed", "8f2c1a")
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
	if !strings.Contains(out, "id/duplicate-response@2025-11-25#seed=8f2c1a") {
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

// charpy speaks first under owned stimulus, so it has to choose an era.
// "auto" is for watching somebody else's handshake.
func TestRunRefusesAutoRevisionWithACase(t *testing.T) {
	out, _, code := runCLI(t, "--case", "id/duplicate-response")
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
// subject, one case per run, and the two HTTP cases both fire.
func TestRunSubjectURLProxiesAndFaults(t *testing.T) {
	out, dir, code := runHTTP(t, httpSubject(t),
		"--case", "stream/truncate-*", "--revision", "2025-11-25", "--seed", "8f2c1a")
	if code != exitClean {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	files := transcripts(t, dir)
	// Both HTTP truncate cases apply; the stdio-only mid_line one does not and
	// is dropped at selection rather than armed to never fire.
	if len(files) != 2 {
		t.Fatalf("wrote %d transcripts, want 2 HTTP cases\n%s", len(files), out)
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
	code := cmdRun([]string{"--out", dir, "--hostile", "--revision", "2025-11-25", "--case", "stream/truncate-mid-event"}, &out)
	stderr()
	if code != exitHarness {
		t.Errorf("exit %d, want %d\n%s", code, exitHarness, out.String())
	}
}
