package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

// upstreamA declares tools, a prompt, a resource and a template, logs and
// reports progress, and asks its client for sampling -- every way an
// upstream reaches back through the gateway.
func upstreamA() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "a"}, &mcp.ServerOptions{
		CompletionHandler: func(_ context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{"from-a:" + req.Params.Ref.Name}}}, nil
		},
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "a_echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Say string `json:"say"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a:" + in.Say}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "a_fail"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("a failed on purpose")
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "a_sample"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}},
			MaxTokens: 10,
		})
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{res.Content}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "a_busy"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		_ = req.Session.Log(ctx, &mcp.LoggingMessageParams{Level: "info", Data: "working"})
		for _, p := range []float64{0, 50, 100} {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: req.Params.GetProgressToken(), Progress: p, Total: 100,
			})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
	})
	srv.AddPrompt(&mcp.Prompt{Name: "a_prompt", Arguments: []*mcp.PromptArgument{{Name: "topic"}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "about " + req.Params.Arguments["topic"]}},
			}}, nil
		})
	srv.AddResource(&mcp.Resource{URI: "test://a/doc", Name: "doc"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "a doc"}}}, nil
		})
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "test://a/item/{id}", Name: "item"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "item " + req.Params.URI}}}, nil
		})
	return srv
}

// upstreamB is plain: one tool, and no logging or completions, so what the
// gateway advertises is the union and not either upstream's alone.
func upstreamB() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "b"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})
	mcp.AddTool(srv, &mcp.Tool{Name: "b_echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Say string `json:"say"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "b:" + in.Say}}}, nil, nil
	})
	return srv
}

func serve(t *testing.T, srv *mcp.Server) string {
	t.Helper()
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

// downstream records what reaches the client through the gateway.
type downstream struct {
	mu       sync.Mutex
	logs     []string
	progress []float64
	updated  []string
	changed  chan struct{}
}

func (d *downstream) options() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		CreateMessageHandler: func(context.Context, *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{Role: "assistant", Model: "m", Content: &mcp.TextContent{Text: "sampled"}}, nil
		},
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.logs = append(d.logs, req.Params.Data.(string))
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.progress = append(d.progress, req.Params.Progress)
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.updated = append(d.updated, req.Params.URI)
		},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case d.changed <- struct{}{}:
			default:
			}
		},
	}
}

// connect stands the gateway up in front of the given upstreams and opens a
// downstream session through it.
func connect(t *testing.T, upstreams ...string) (*mcp.ClientSession, *downstream) {
	t.Helper()
	h, err := gateway.Handler(gateway.Options{Upstreams: upstreams})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	d := &downstream{changed: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "down"}, d.options()).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, d
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want text content, got %T", res.Content[0])
	}
	return tc.Text
}

func TestMergesAndRoutes(t *testing.T) {
	cs, _ := connect(t, serve(t, upstreamA()), serve(t, upstreamB()))
	ctx := t.Context()

	var tools []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool.Name)
	}
	slices.Sort(tools)
	if want := []string{"a_busy", "a_echo", "a_fail", "a_sample", "b_echo"}; !slices.Equal(tools, want) {
		t.Fatalf("tools = %v, want %v", tools, want)
	}

	for name, want := range map[string]string{"a_echo": "a:x", "b_echo": "b:x"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"say": "x"}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := text(t, res); got != want {
			t.Errorf("%s answered %q, want %q: routed to the wrong upstream", name, got, want)
		}
	}

	p, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "a_prompt", Arguments: map[string]string{"topic": "gateways"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Messages[0].Content.(*mcp.TextContent).Text; got != "about gateways" {
		t.Errorf("prompt = %q", got)
	}

	for uri, want := range map[string]string{"test://a/doc": "a doc", "test://a/item/7": "item test://a/item/7"} {
		r, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if got := r.Contents[0].Text; got != want {
			t.Errorf("%s = %q, want %q", uri, got, want)
		}
	}

	var templates []string
	for tmpl, err := range cs.ResourceTemplates(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		templates = append(templates, tmpl.URITemplate)
	}
	if !slices.Equal(templates, []string{"test://a/item/{id}"}) {
		t.Errorf("templates = %v", templates)
	}
}

func TestAdvertisesTheUnion(t *testing.T) {
	cs, _ := connect(t, serve(t, upstreamB()))
	caps := cs.InitializeResult().Capabilities
	if caps.Logging != nil || caps.Completions != nil || caps.Prompts != nil || caps.Resources != nil {
		t.Errorf("advertised what no upstream has: %+v", caps)
	}
	if caps.Tools == nil {
		t.Error("tools not advertised")
	}

	cs, _ = connect(t, serve(t, upstreamA()), serve(t, upstreamB()))
	caps = cs.InitializeResult().Capabilities
	if caps.Logging == nil || caps.Completions == nil || caps.Prompts == nil ||
		caps.Resources == nil || !caps.Resources.Subscribe {
		t.Errorf("did not advertise what an upstream has: %+v", caps)
	}
}

func TestErrorsPassThrough(t *testing.T) {
	cs, _ := connect(t, serve(t, upstreamA()))
	ctx := t.Context()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "a_fail"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(t, res), "failed on purpose") {
		t.Errorf("upstream's tool error not passed through: %+v", res)
	}

	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "nobody_has_this"})
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) || rpc.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("unknown tool: err = %v, want a %d", err, jsonrpc.CodeInvalidParams)
	}
}

func TestUpstreamAsksTheDownstreamClient(t *testing.T) {
	cs, _ := connect(t, serve(t, upstreamA()))
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "a_sample"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "sampled" {
		t.Errorf("a_sample = %q: the sampling request did not reach the downstream client", got)
	}
}

func TestProgressAndLogsReachTheCaller(t *testing.T) {
	cs, d := connect(t, serve(t, upstreamA()))
	ctx := t.Context()
	if err := cs.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "info"}); err != nil {
		t.Fatal(err)
	}
	params := &mcp.CallToolParams{Name: "a_busy"}
	params.SetProgressToken("tok")
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}

	// Notifications travel ahead of the result on the call's own stream, but
	// the handlers run asynchronously; give them a moment.
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		logs, progress := slices.Clone(d.logs), slices.Clone(d.progress)
		d.mu.Unlock()
		if len(logs) == 1 && len(progress) == 3 {
			if logs[0] != "working" || !slices.Equal(progress, []float64{0, 50, 100}) {
				t.Errorf("logs = %v, progress = %v", logs, progress)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs = %v, progress = %v; want one log and three progress notifications", logs, progress)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCompletionRoutesToTheOwner(t *testing.T) {
	cs, _ := connect(t, serve(t, upstreamA()), serve(t, upstreamB()))
	res, err := cs.Complete(t.Context(), &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "a_prompt"},
		Argument: mcp.CompleteParamsArgument{Name: "topic", Value: "g"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Completion.Values, []string{"from-a:a_prompt"}) {
		t.Errorf("completion = %v", res.Completion.Values)
	}
}

func TestSubscriptionUpdatesReachTheSubscriber(t *testing.T) {
	a := upstreamA()
	cs, d := connect(t, serve(t, a))
	ctx := t.Context()
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "test://a/doc"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "test://a/doc"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		updated := slices.Clone(d.updated)
		d.mu.Unlock()
		if slices.Equal(updated, []string{"test://a/doc"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updated = %v", updated)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: "test://a/doc"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamListChangeReachesTheDownstream(t *testing.T) {
	b := upstreamB()
	cs, d := connect(t, serve(t, upstreamA()), serve(t, b))
	ctx := t.Context()

	// Registering the first listings is not a change the client can have
	// missed; the SDK's debounce would send it within 10ms.
	select {
	case <-d.changed:
		t.Fatal("tools/list_changed sent as the session opened")
	case <-time.After(100 * time.Millisecond):
	}

	mcp.AddTool(b, &mcp.Tool{Name: "b_new"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "new"}}}, nil, nil
	})
	select {
	case <-d.changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed reached the downstream client")
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "b_new"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "new" {
		t.Errorf("b_new = %q", got)
	}

	b.RemoveTools("b_echo")
	select {
	case <-d.changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed after a removal")
	}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "b_echo" {
			t.Error("b_echo still listed after its upstream removed it")
		}
	}
}

func TestCollidingNamesAreRefused(t *testing.T) {
	h, err := gateway.Handler(gateway.Options{Upstreams: []string{serve(t, upstreamB()), serve(t, upstreamB())}})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	_, err = mcp.NewClient(&mcp.Implementation{Name: "down"}, nil).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), `tool "b_echo" is declared by both`) {
		t.Fatalf("err = %v, want the collision named", err)
	}
}

// titledMulti is a titled multi-select as the SDK's own conformance server
// sends it: items typed "string" and carrying anyOf. The 2025-11-25 schema
// requires only anyOf, so this is valid; the SDK client's validator refuses
// it, and a gateway must not let that stop the question reaching its client.
var titledMulti = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"pick": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":  "string",
				"anyOf": []any{map[string]any{"const": "v1", "title": "One"}},
			},
		},
	},
}

func TestElicitationIsPassedOnAsAsked(t *testing.T) {
	up := mcp.NewServer(&mcp.Implementation{Name: "e"}, nil)
	mcp.AddTool(up, &mcp.Tool{Name: "e_ask"}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Message: "pick", RequestedSchema: titledMulti})
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil, nil
	})

	h, err := gateway.Handler(gateway.Options{Upstreams: []string{serve(t, up)}})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	// The downstream client is the Go SDK too, so it records the question in
	// middleware, where its own validator has not yet run.
	asked := make(chan *mcp.ElicitParams, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "down"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{}},
	})
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if p, ok := req.GetParams().(*mcp.ElicitParams); ok && method == "elicitation/create" {
				asked <- p
				return &mcp.ElicitResult{Action: "decline"}, nil
			}
			return next(ctx, method, req)
		}
	})
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "e_ask"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "decline" {
		t.Fatalf("e_ask = %q, want the downstream client's decline", got)
	}
	if p := <-asked; p.Message != "pick" {
		t.Errorf("asked %q", p.Message)
	}
}
