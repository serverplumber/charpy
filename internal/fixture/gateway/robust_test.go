package gateway_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

// down opens a downstream session on the gateway at url.
func down(t *testing.T, url string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "down"}, opts).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func gatewayAt(t *testing.T, upstreams ...string) string {
	t.Helper()
	h, err := gateway.Handler(gateway.Options{Upstreams: upstreams})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw.URL
}

// eventually polls cond until it holds or two seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 2s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An upstream session that ends must not take the downstream session with
// it: the next call opens a new one, set up as the downstream client left
// the last -- logging level and subscriptions included.
func TestReconnectsWhenTheUpstreamSessionEnds(t *testing.T) {
	a := upstreamA()
	var (
		mu      sync.Mutex
		logs    []string
		updated []string
	)
	cs := down(t, gatewayAt(t, serve(t, a)), &mcp.ClientOptions{
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, req.Params.Data.(string))
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			mu.Lock()
			defer mu.Unlock()
			updated = append(updated, req.Params.URI)
		},
	})
	ctx := t.Context()
	if err := cs.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "info"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "test://a/doc"}); err != nil {
		t.Fatal(err)
	}
	echo := func() error {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "a_echo", Arguments: map[string]any{"say": "x"}})
		return err
	}
	if err := echo(); err != nil {
		t.Fatal(err)
	}

	for ss := range a.Sessions() {
		_ = ss.Close()
	}

	// The call that finds the session gone may fail; the one after it must
	// not.
	if err := echo(); err != nil {
		t.Logf("first call after the upstream session ended: %v", err)
		if err := echo(); err != nil {
			t.Fatalf("the gateway did not reconnect: %v", err)
		}
	}

	mu.Lock()
	logs, updated = nil, nil
	mu.Unlock()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "a_busy"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "log level restored on the new upstream session", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(logs, "working")
	})
	if err := a.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "test://a/doc"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "subscription restored on the new upstream session", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(updated, "test://a/doc")
	})
}

// junkAhead puts, ahead of every response an upstream streams, frames the SDK
// client rejects before any middleware sees them: an unknown method, and a
// known one whose params do not parse. charpy injects frames like these.
type junkAhead struct{ http.Handler }

var junk = []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/bogus\",\"params\":{}}\n\n" +
	"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":\"oops\"}\n\n")

func (j junkAhead) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j.Handler.ServeHTTP(&junkWriter{ResponseWriter: w}, r)
}

type junkWriter struct {
	http.ResponseWriter
	held []byte
}

func (w *junkWriter) Write(b []byte) (int, error) {
	w.held = append(w.held, b...)
	for {
		i := bytes.Index(w.held, []byte("\n\n"))
		if i < 0 {
			return len(b), nil
		}
		event := w.held[:i+2]
		if bytes.Contains(event, []byte(`"result"`)) || bytes.Contains(event, []byte(`"error"`)) {
			if _, err := w.ResponseWriter.Write(junk); err != nil {
				return 0, err
			}
		}
		if _, err := w.ResponseWriter.Write(event); err != nil {
			return 0, err
		}
		w.held = w.held[i+2:]
	}
}

func (w *junkWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Frames the SDK drops unseen must not hold up the gateway's answers.
func TestJunkAheadOfAnAnswerDoesNotStall(t *testing.T) {
	b := upstreamB()
	ups := httptest.NewServer(junkAhead{mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return b }, nil)})
	t.Cleanup(ups.Close)
	cs := down(t, gatewayAt(t, ups.URL), nil)

	const calls = 10
	start := time.Now()
	for range calls {
		if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "b_echo", Arguments: map[string]any{"say": "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("%d calls took %v with junk ahead of each answer", calls, took)
	}
}

// Calls to one upstream are admitted one at a time, so what the upstream
// sends during a call belongs to that call.
func TestCallsToOneUpstreamAreSerial(t *testing.T) {
	var inFlight, most atomic.Int64
	up := mcp.NewServer(&mcp.Implementation{Name: "slow"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "slow"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		return &mcp.CallToolResult{}, nil, nil
	})
	cs := down(t, gatewayAt(t, serve(t, up)), nil)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if m := most.Load(); m != 1 {
		t.Errorf("the upstream saw %d calls at once from one downstream session", m)
	}
}

// A client answering an upstream's sampling request may call through the
// gateway to do it. Queueing that call behind the one waiting on it would
// deadlock.
func TestANestedCallIsAdmitted(t *testing.T) {
	up := mcp.NewServer(&mcp.Implementation{Name: "nest"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "asks"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}},
			MaxTokens: 10,
		})
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{res.Content}}, nil, nil
	})
	mcp.AddTool(up, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "nested"}}}, nil, nil
	})

	var cs *mcp.ClientSession
	cs = down(t, gatewayAt(t, serve(t, up)), &mcp.ClientOptions{
		CreateMessageHandler: func(ctx context.Context, _ *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
			if err != nil {
				return nil, err
			}
			return &mcp.CreateMessageResult{Role: "assistant", Model: "m", Content: res.Content[0]}, nil
		},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "asks"})
	if err != nil {
		t.Fatalf("a call made to answer the upstream's sampling request: %v", err)
	}
	if got := text(t, res); got != "nested" {
		t.Errorf("asks = %q", got)
	}
}

// Downstream sessions come and go by the hundred in a run with two fixtures
// over the catalogue; each must leave nothing running behind it.
func TestSessionsLeaveNothingBehind(t *testing.T) {
	url := gatewayAt(t, serve(t, upstreamA()), serve(t, upstreamB()))
	once := func() {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "down"}, nil).
			Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "b_echo", Arguments: map[string]any{"say": "x"}}); err != nil {
			t.Fatal(err)
		}
		_ = cs.Close()
	}
	settled := func() int {
		runtime.GC()
		return runtime.NumGoroutine()
	}

	// Warm up the connection pools, so the baseline is what one session
	// costs at rest, not what the first one costs.
	for range 5 {
		once()
	}
	time.Sleep(200 * time.Millisecond)
	base := settled()

	const sessions = 200
	for range sessions {
		once()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := settled()
		if n <= base+10 {
			t.Logf("%d sessions; goroutines %d before, %d after", sessions, base, n)
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d sessions left %d goroutines behind (%d before, %d after)\n%s",
				sessions, n-base, base, n, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
