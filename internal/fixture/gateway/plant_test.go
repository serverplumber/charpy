package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

const secret = "Bearer planted-secret-7f3a"

// planted stands the gateway up in front of up with the given plants and the
// planted credential, and returns a downstream session, the upstream's server
// and what Authorization header it was sent.
func planted(t *testing.T, up *mcp.Server, plants ...gateway.Plant) (*mcp.ClientSession, *httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var auth string
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, nil)
	ups := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ups.Close)
	h, err := gateway.Handler(gateway.Options{
		Upstreams:      []string{ups.URL},
		UpstreamIdle:   idle,
		UpstreamHeader: http.Header{"Authorization": {secret}},
		Plants:         plants,
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return down(t, gw.URL, nil), ups, func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
}

// quiet is an upstream whose one tool answers only when released.
func quiet(t *testing.T) (*mcp.Server, func()) {
	release := make(chan struct{})
	var once sync.Once
	up := mcp.NewServer(&mcp.Implementation{Name: "quiet"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "quiet"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &mcp.CallToolResult{}, nil, nil
	})
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	return up, free
}

func TestUpstreamHeaderIsSent(t *testing.T) {
	cs, _, auth := planted(t, upstreamB())
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "b_echo", Arguments: map[string]any{"say": "x"}}); err != nil {
		t.Fatal(err)
	}
	if got := auth(); got != secret {
		t.Errorf("upstream saw Authorization %q, want %q", got, secret)
	}
}

// leaks reports whether a downstream error carries the upstream's address or
// the planted credential.
func leaks(err error, ups *httptest.Server) (addr, cred bool) {
	text := err.Error()
	return strings.Contains(text, strings.TrimPrefix(ups.URL, "http://")), strings.Contains(text, secret)
}

func TestLeak(t *testing.T) {
	gone := func(t *testing.T, plants ...gateway.Plant) (error, *httptest.Server) {
		cs, ups, _ := planted(t, upstreamB(), plants...)
		ups.CloseClientConnections()
		ups.Close()
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "b_echo", Arguments: map[string]any{"say": "x"}})
		if err == nil {
			t.Fatal("the call succeeded with its upstream gone")
		}
		return err, ups
	}
	hung := func(t *testing.T, plants ...gateway.Plant) (error, *httptest.Server) {
		up, _ := quiet(t)
		cs, ups, _ := planted(t, up, plants...)
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "quiet"})
		if err == nil {
			t.Fatal("the call succeeded with its upstream hung")
		}
		return err, ups
	}

	for name, fault := range map[string]func(*testing.T, ...gateway.Plant) (error, *httptest.Server){
		"upstream gone": gone,
		"upstream hung": hung,
	} {
		t.Run(name, func(t *testing.T) {
			err, ups := fault(t)
			if addr, cred := leaks(err, ups); addr || cred {
				t.Errorf("with nothing planted the error leaks (address %v, credential %v): %v", addr, cred, err)
			}
			err, ups = fault(t, gateway.PlantLeak)
			if addr, cred := leaks(err, ups); !addr || !cred {
				t.Errorf("planted, the error does not leak (address %v, credential %v): %v", addr, cred, err)
			}
		})
	}
}

func TestLeakShowsOnlyUnderAFault(t *testing.T) {
	cs, _, _ := planted(t, upstreamA(), gateway.PlantLeak)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "a_fail"})
	if err != nil {
		t.Fatal(err)
	}
	// An upstream's own error is the upstream's, and passes untouched.
	if got := text(t, res); strings.Contains(got, secret) || !res.IsError {
		t.Errorf("an upstream's own error was rewritten: %q", got)
	}
}

func TestNoDeadline(t *testing.T) {
	up, free := quiet(t)
	cs, _, _ := planted(t, up, gateway.PlantNoDeadline)

	// The caller gives up long after the idle bound would have answered.
	ctx, cancel := context.WithTimeout(t.Context(), 10*idle)
	defer cancel()
	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "quiet"})
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) {
		t.Fatalf("planted, the gateway answered a hung call anyway: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's own deadline", err)
	}

	// Once the upstream answers, so does the gateway: it hangs with its
	// upstream, nothing more.
	free()
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "quiet"}); err != nil {
		t.Errorf("after the upstream recovered: %v", err)
	}
}

func TestCascade(t *testing.T) {
	a := upstreamA()
	cs, _, _ := planted(t, a, gateway.PlantCascade)
	echo := func() error {
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "a_echo", Arguments: map[string]any{"say": "x"}})
		return err
	}
	// Nothing shows before the fault.
	if err := echo(); err != nil {
		t.Fatal(err)
	}

	for ss := range a.Sessions() {
		_ = ss.Close()
	}

	// The call that finds the upstream gone may still be answered; after it,
	// the downstream session is over and nothing is.
	_ = echo()
	eventually(t, "the downstream session ended", func() bool {
		err := echo()
		var rpc *jsonrpc.Error
		return err != nil && !errors.As(err, &rpc)
	})
	n := 0
	for range a.Sessions() {
		n++
	}
	if n != 0 {
		t.Errorf("planted, the gateway reopened %d upstream session(s)", n)
	}
}

// The sanity check for every plant at once: none of them shows on a correct
// sequence. Conformance makes the same point at scale (`just
// fixture-conformance leak,nodeadline,cascade`).
func TestPlantsAreSilentWithoutAFault(t *testing.T) {
	cs, _, _ := planted(t, upstreamA(), gateway.Plants...)
	params := &mcp.CallToolParams{Name: "a_busy"}
	params.SetProgressToken("tok")
	if _, err := cs.CallTool(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "a_echo", Arguments: map[string]any{"say": "x"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * idle) // past where a deadline would have fired, had one been due
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "a_echo", Arguments: map[string]any{"say": "x"}}); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownPlantIsRefused(t *testing.T) {
	if _, err := gateway.ParsePlants("leak,leek"); err == nil {
		t.Error("a misspelled plant was accepted")
	}
	if _, err := gateway.Handler(gateway.Options{Upstreams: []string{"http://x"}, Plants: []gateway.Plant{"leek"}}); err == nil {
		t.Error("Handler accepted a misspelled plant")
	}
}
