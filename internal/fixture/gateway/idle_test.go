package gateway_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

// idle is short so the tests are; what matters is how it compares with what
// each upstream does.
const idle = 150 * time.Millisecond

// through stands the gateway up with the given idle bound in front of one
// upstream, and returns a downstream session and the upstream's server.
func through(t *testing.T, up *mcp.Server, opts *mcp.ClientOptions) (*mcp.ClientSession, *httptest.Server) {
	t.Helper()
	ups := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, nil))
	t.Cleanup(ups.Close)
	h, err := gateway.Handler(gateway.Options{Upstreams: []string{ups.URL}, UpstreamIdle: idle})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "down"}, opts).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, ups
}

// gatewayError is the JSON-RPC error a downstream call failed with, checked
// to say what went wrong and to name no upstream.
func gatewayError(t *testing.T, err error, ups *httptest.Server, want string) {
	t.Helper()
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) {
		t.Fatalf("err = %v, want a JSON-RPC error", err)
	}
	if rpc.Message != want {
		t.Errorf("message = %q, want %q", rpc.Message, want)
	}
	if host := strings.TrimPrefix(ups.URL, "http://"); strings.Contains(err.Error(), host) {
		t.Errorf("the error names the upstream %s: %v", host, err)
	}
}

func TestAQuietUpstreamTimesOut(t *testing.T) {
	cancelled := make(chan struct{})
	up := mcp.NewServer(&mcp.Implementation{Name: "slow"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "slow"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		select {
		case <-ctx.Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
		return &mcp.CallToolResult{}, nil, nil
	})
	cs, ups := through(t, up, nil)

	start := time.Now()
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow"})
	if took := time.Since(start); took > 10*idle {
		t.Errorf("the call took %v against an idle bound of %v", took, idle)
	}
	gatewayError(t, err, ups, "gateway: upstream timed out")

	// Giving up downstream should cancel upstream, not leave it working.
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Error("the upstream call was never cancelled")
	}
}

func TestProgressKeepsACallAlive(t *testing.T) {
	up := mcp.NewServer(&mcp.Implementation{Name: "steady"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "steady"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		for i := range 6 { // 6 x idle/2: three idle intervals in all
			time.Sleep(idle / 2)
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: req.Params.GetProgressToken(), Progress: float64(i + 1), Total: 6,
			})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
	})
	cs, _ := through(t, up, nil)

	params := &mcp.CallToolParams{Name: "steady"}
	params.SetProgressToken("tok")
	if _, err := cs.CallTool(t.Context(), params); err != nil {
		t.Fatalf("a call reporting progress was cut off: %v", err)
	}
}

func TestWaitingOnTheClientIsNotIdle(t *testing.T) {
	up := mcp.NewServer(&mcp.Implementation{Name: "asks"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "asks"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Message: "well?"})
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil, nil
	})
	// A person takes a while to answer: several idle intervals.
	cs, _ := through(t, up, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			time.Sleep(3 * idle)
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "asks"}); err != nil {
		t.Fatalf("a call waiting on its client's answer was cut off: %v", err)
	}
}

func TestAnUpstreamGoneMidSessionIsNotNamed(t *testing.T) {
	cs, ups := through(t, upstreamB(), nil)
	ups.CloseClientConnections()
	ups.Close()

	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "b_echo", Arguments: map[string]any{"say": "x"}})
	gatewayError(t, err, ups, "gateway: upstream unavailable")
}

func TestAnUnreachableUpstreamIsNotNamed(t *testing.T) {
	// A port nothing listens on: bound, noted, closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	h, err := gateway.Handler(gateway.Options{Upstreams: []string{"http://" + addr + "/"}, UpstreamIdle: idle})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	_, err = mcp.NewClient(&mcp.Implementation{Name: "down"}, nil).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err == nil {
		t.Fatal("initialize succeeded with no upstream to reach")
	}
	if !strings.Contains(err.Error(), "gateway: upstream unavailable") {
		t.Errorf("err = %v, want the gateway's own account", err)
	}
	if strings.Contains(err.Error(), addr) {
		t.Errorf("the error names the upstream %s: %v", addr, err)
	}
}
