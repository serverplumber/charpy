package peer_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
)

// relay joins the peer's byte pipes to a subject, recording every frame that
// crosses in charpy's own envelope terms. It is the shape a driver takes, cut
// down to what a test needs.
type relay struct {
	mu   sync.Mutex
	seen []envelope.Message
}

func (r *relay) record(m envelope.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, m)
}

func (r *relay) frames() []envelope.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]envelope.Message(nil), r.seen...)
}

// pump reads newline-delimited frames from src, records them, and writes the
// exact bytes on to dst. Recording before forwarding is the ordering the
// transcript guarantee needs.
func (r *relay) pump(t *testing.T, src io.Reader, dst io.Writer, done chan<- struct{}) {
	t.Helper()
	go func() {
		defer close(done)
		sc := bufio.NewScanner(src)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			m, err := envelope.Parse(line)
			if err == nil {
				r.record(m)
			}
			if _, err := dst.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
}

func fixture(t *testing.T) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	return s
}

// dial stands a subject up behind charpy's pipes and hands back the peer's
// session plus what charpy saw crossing in each direction.
func dial(t *testing.T, o peer.Options) (*mcp.ClientSession, *relay, *relay) {
	t.Helper()

	c, err := peer.NewClient(o)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	subjIn, charpyToSubj := io.Pipe()
	subjOut, subjWrites := io.Pipe()
	srv := fixture(t)
	ss, err := srv.Connect(t.Context(), &mcp.IOTransport{
		Reader: subjIn, Writer: subjWrites, MaxLineLength: -1,
	}, nil)
	if err != nil {
		t.Fatalf("subject connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	// Connect blocks on the handshake and the handshake crosses these pipes,
	// so the relays run first. That ordering is the API's contract, not a
	// quirk of the test.
	out, in := &relay{}, &relay{}
	d1, d2 := make(chan struct{}), make(chan struct{})
	out.pump(t, c.Out, charpyToSubj, d1)
	in.pump(t, subjOut, c.In, d2)
	t.Cleanup(func() { _ = c.Close() })

	sess, err := c.Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, out, in
}

// The point of the byte seam: what the ledger will record as intent is what
// the SDK actually wrote, parsed by charpy's own parser, with nothing
// re-encoding it on the way (ADR-011).
func TestCharpyReadsTheBytesTheSDKWrote(t *testing.T) {
	sess, out, in := dial(t, peer.Options{})

	if _, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	var methods []string
	for _, m := range out.frames() {
		if m.Method != "" {
			methods = append(methods, m.Method)
		}
	}
	// Asking for no era takes the SDK's latest, which is the stateless one, so
	// the handshake is server/discover and there is no initialize at all. The
	// era boundary is not a version string on otherwise identical traffic.
	if !contains(methods, "server/discover") {
		t.Errorf("charpy never saw the handshake; saw %v", methods)
	}
	if !contains(methods, "tools/call") {
		t.Errorf("charpy never saw the call; saw %v", methods)
	}

	// Every recorded frame's raw bytes must be the bytes themselves, not a
	// re-render: parsing and re-marshalling would reorder keys and break the
	// content digest the inferred join is built on.
	for _, m := range out.frames() {
		var any map[string]json.RawMessage
		if err := json.Unmarshal(m.Raw(), &any); err != nil {
			t.Errorf("recorded frame is not the raw line: %q", m.Raw())
		}
	}
	if len(in.frames()) == 0 {
		t.Error("charpy saw nothing coming back from the subject")
	}
}

// The era is a constructor parameter, which is the promise ADR-011 pinned
// v1.8.0 to keep. Asking for a sessioned era must put that version on the
// wire through the legacy handshake, not the SDK's stateless latest -- and on
// v1.7.0 this test could not have passed, which is what the pin bought.
func TestTheEraAskedForIsTheEraOnTheWire(t *testing.T) {
	_, out, _ := dial(t, peer.Options{Era: revision.V20250618})

	var got string
	for _, m := range out.frames() {
		if m.Method == "server/discover" {
			t.Error("a sessioned era still probed for the stateless handshake")
		}
		if m.Method != "initialize" {
			continue
		}
		var f struct {
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.Unmarshal(m.Raw(), &f); err != nil {
			t.Fatalf("initialize does not parse: %v", err)
		}
		got = f.Params.ProtocolVersion
	}
	if got != string(revision.V20250618) {
		t.Errorf("initialize offered %q, want %s", got, revision.V20250618)
	}
}

// Item 10 in one test: a compiled case drives the reference peer over the byte
// seam, and charpy sees every frame the script originated. Nothing here is
// faulted yet -- the interposer sits between these pipes -- but this is the
// traffic it will have to work on.
func TestAScenarioDrivesThePeerAndCharpySeesIt(t *testing.T) {
	sess, out, in := dial(t, peer.Options{Era: revision.V20251125})

	run, err := scenario.Basic(interpose.Match{
		Method:     interpose.ParseGlob("tools/call"),
		Occurrence: 3,
	}, scenario.Options{})
	if err != nil {
		t.Fatalf("Basic: %v", err)
	}
	if err := run(t.Context(), sess); err != nil {
		t.Fatalf("run: %v", err)
	}

	var calls int
	for _, m := range out.frames() {
		if m.Method == "tools/call" {
			calls++
		}
	}
	if calls != 3 {
		t.Errorf("charpy saw %d tools/call frames, want the 3 the case asked for", calls)
	}

	// The matcher resolves a response's method from the ledger, so the
	// answers have to arrive as frames charpy can parse, not just as bytes.
	var answers int
	for _, m := range in.frames() {
		if m.Kind == envelope.KindResponse || m.Kind == envelope.KindError {
			answers++
		}
	}
	if answers < calls {
		t.Errorf("charpy saw %d answers for %d calls", answers, calls)
	}
}

// A draft-only case has no peer that can carry it, and the run must be told so
// up front rather than watching a handshake fail and guessing why.
func TestDraftIsRefusedWithItsReason(t *testing.T) {
	_, err := peer.NewClient(peer.Options{Era: revision.Draft})
	if err == nil {
		t.Fatal("NewClient built a peer that speaks draft")
	}
	if !strings.Contains(err.Error(), string(revision.Draft)) {
		t.Errorf("error does not name draft: %v", err)
	}
}

func TestAnUnknownEraIsRefused(t *testing.T) {
	if _, err := peer.NewClient(peer.Options{Era: "2027-01-01"}); err == nil {
		t.Fatal("NewClient accepted an era charpy does not know")
	}
}

// The version the transcript reports must be the version the build resolves,
// or a header claims a pin the binary does not have.
func TestPinMatchesGoMod(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	want := peer.Module + " " + peer.Version
	if !strings.Contains(string(b), want) {
		t.Errorf("go.mod does not require %q; peer.Version is stale", want)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// The reference server serves add_numbers -- the tool the SDK's conformance
// client calls in its tools_call scenario -- with a declared outputSchema,
// which is what a schema_violation against the declared output breaks.
func TestTheReferenceServerDeclaresAddNumbersOutput(t *testing.T) {
	h, err := peer.ServerHandler(peer.Options{Era: revision.V20251125})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var add *mcp.Tool
	for _, tl := range listed.Tools {
		if tl.Name == "add_numbers" {
			add = tl
		}
	}
	if add == nil {
		t.Fatal("the reference server does not serve add_numbers")
	}
	if add.OutputSchema == nil {
		t.Error("add_numbers declares no outputSchema")
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "add_numbers", Arguments: map[string]any{"a": 5, "b": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res.StructuredContent.(map[string]any); got["sum"] != float64(8) {
		t.Errorf("structuredContent = %v, want sum 8", res.StructuredContent)
	}
}

// A gateway run gives each reference upstream a prefix, so no two serve a
// tool by the same name, and the declarations follow the names.
func TestToolPrefixNamesEveryTool(t *testing.T) {
	h, err := peer.ServerHandler(peer.Options{Era: revision.V20251125, ToolPrefix: "u0_"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range listed.Tools {
		names = append(names, tl.Name)
		if !strings.HasPrefix(tl.Name, "u0_") {
			t.Errorf("tool %q served without the prefix", tl.Name)
		}
	}
	if !slices.Contains(names, "u0_add_numbers") {
		t.Fatalf("tools = %v, want u0_add_numbers among them", names)
	}

	base, err := peer.OutputSchemas()
	if err != nil {
		t.Fatal(err)
	}
	prefixed, err := peer.OutputSchemasFor("u0_")
	if err != nil {
		t.Fatal(err)
	}
	if string(prefixed["u0_add_numbers"]) != string(base["add_numbers"]) || len(prefixed) != len(base) {
		t.Errorf("prefixed declarations %v do not match the base ones %v", prefixed, base)
	}
}

// Every message charpy's peer originates carries the trace it was given, in
// _meta and in the traceparent header (ADR-005, ADR-014) -- a ping and
// notifications/initialized, whose params the SDK leaves nil, included.
func TestMessagesCarryTheirTrace(t *testing.T) {
	h, err := peer.ServerHandler(peer.Options{Era: revision.V20251125})
	if err != nil {
		t.Fatal(err)
	}
	type seen struct{ method, meta, header string }
	var mu sync.Mutex
	var got []seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta struct {
					Traceparent string `json:"traceparent"`
				} `json:"_meta"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &m) == nil && m.Method != "" {
			mu.Lock()
			got = append(got, seen{m.Method, m.Params.Meta.Traceparent, r.Header.Get("Traceparent")})
			mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()

	n := 0
	trace := func() string {
		n++
		return fmt.Sprintf("00-%032x-%016x-01", n, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := peer.DialHTTP(ctx, ts.URL, peer.Options{Era: revision.V20251125, Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Ping(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()

	mu.Lock()
	defer mu.Unlock()
	methods := map[string]bool{}
	traces := map[string]bool{}
	for _, s := range got {
		methods[s.method] = true
		if s.meta == "" {
			t.Errorf("%s carries no traceparent in _meta", s.method)
		}
		if s.header != s.meta {
			t.Errorf("%s: header %q, _meta %q", s.method, s.header, s.meta)
		}
		if traces[s.meta] {
			t.Errorf("%s reuses trace %s", s.method, s.meta)
		}
		traces[s.meta] = true
	}
	for _, m := range []string{"initialize", "notifications/initialized", "ping", "tools/list"} {
		if !methods[m] {
			t.Errorf("saw no %s; saw %v", m, got)
		}
	}
}
