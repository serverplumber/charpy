// Package gateway is charpy's fixture gateway: a minimal MCP gateway on the
// reference SDK, built to be pointed at by the gateway driver (brief §9).
//
// It is a test instrument, not a deliverable, and it must not grow into one.
// But it is held to the same precondition as any subject: it passes
// modelcontextprotocol/conformance on its downstream face, fronting a server
// that does. A resilience finding against a gateway that was already wrong
// under a correct sequence could not be told apart from the bug it already
// had, so "minimal" is spent on what it leaves out -- auth, stdio upstreams,
// the 2026-07-28 era -- and never on what conformance asks of a server.
//
// The shape is one downstream session to one session on each upstream. Every
// downstream session gets its own mcp.Server, and that server's tools,
// resources and prompts are the union of what its upstreams listed. One-to-one
// is what makes forwarding exact: an upstream's request for sampling, its log
// lines and its progress all belong to the one downstream session that holds
// the upstream session they arrived on.
//
// Names pass through unchanged, with no per-upstream prefix. Conformance calls
// tools by the names its server declares, and charpy gives each of its
// upstreams disjoint names, so a call reaching the wrong upstream is a fact on
// the wire rather than an inference about somebody's prefixing scheme. Two
// upstreams declaring the same name is refused when the session opens.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"

	"github.com/serverplumber/charpy/internal/revision"
)

// Options configures the fixture gateway.
type Options struct {
	// Upstreams are the Streamable HTTP endpoints the gateway fronts. Each
	// downstream session opens its own session on every one of them.
	Upstreams []string
	// HTTPClient dials the upstreams. Nil takes http.DefaultClient.
	HTTPClient *http.Client
	// Logger receives the gateway's own diagnostics. Nil discards them.
	Logger *slog.Logger
}

// Sessioned is the set of revisions the gateway speaks on both faces. The
// stateless era has no session to map one-to-one, so it is out until a case
// needs it.
func Sessioned() []string {
	var out []string
	for _, r := range revision.All() {
		if !r.Stateless() && r != revision.Draft {
			out = append(out, string(r))
		}
	}
	return out
}

// Handler returns the gateway's downstream face as a Streamable HTTP handler.
//
// The SDK asks for a server on every request, not only on the initialize that
// opens a session, so the server it is handed is a cheap shell: nothing dials
// an upstream until that server's own initialize arrives.
func Handler(o Options) (http.Handler, error) {
	if len(o.Upstreams) == 0 {
		return nil, errors.New("gateway: no upstreams")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return newSession(o).srv
	}, nil), nil
}

// session is one downstream session and the upstream sessions behind it.
type session struct {
	o   Options
	srv *mcp.Server

	mu   sync.Mutex
	down *mcp.ServerSession
	ups  []*upstream

	// owners maps each name the downstream sees to the upstream that
	// declared it, one table per kind of name.
	tools, prompts, resources, templates map[string]*upstream

	// progress maps a downstream progress token to the call that carried it,
	// so an upstream's progress lands on the stream of the request it is
	// about; forwarded counts what has been sent on, and bump is closed and
	// replaced whenever it grows (see [session.drain]).
	progress  map[string]context.Context
	forwarded map[string]int
	bump      chan struct{}

	// changed holds the list kinds an upstream's relist actually changed,
	// so that only those reach the downstream client as list_changed.
	changed map[string]bool
}

// upstream is one upstream session, and what its server declared.
type upstream struct {
	endpoint string
	cs       *mcp.ClientSession
	caps     *mcp.ServerCapabilities
	watch    *progressWatch

	mu sync.Mutex
	// calls are the downstream requests in flight to this upstream, newest
	// last. See [upstream.current].
	calls []context.Context

	tools, prompts, resources, templates []string
}

func newSession(o Options) *session {
	s := &session{
		o:         o,
		tools:     map[string]*upstream{},
		prompts:   map[string]*upstream{},
		resources: map[string]*upstream{},
		templates: map[string]*upstream{},
		progress:  map[string]context.Context{},
		forwarded: map[string]int{},
		bump:      make(chan struct{}),
		changed:   map[string]bool{},
	}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "charpy-fixture-gateway", Version: "0"}, &mcp.ServerOptions{
		Logger:                    o.Logger,
		SupportedProtocolVersions: Sessioned(),
		CompletionHandler:         s.complete,
		SubscribeHandler:          s.subscribe,
		UnsubscribeHandler:        s.unsubscribe,
	})
	s.srv.AddReceivingMiddleware(s.intercept)
	s.srv.AddSendingMiddleware(s.quiet)
	return s
}

// quiet holds back a list_changed that no relist caused. Registering the
// first listings while the downstream session initializes counts as a
// change to the SDK, which would tell a client that has not yet asked for a
// list that the list changed.
func (s *session) quiet(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "notifications/tools/list_changed", "notifications/prompts/list_changed", "notifications/resources/list_changed":
			s.mu.Lock()
			send := s.changed[method]
			delete(s.changed, method)
			s.mu.Unlock()
			if !send {
				return nil, nil
			}
		}
		return next(ctx, method, req)
	}
}

// intercept handles the two downstream requests the gateway must act on
// before or after the SDK does: initialize, which opens the upstreams, and
// logging/setLevel, which every upstream with logging must hear too.
func (s *session) intercept(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "initialize":
			ss, _ := req.GetSession().(*mcp.ServerSession)
			p, _ := req.GetParams().(*mcp.InitializeParams)
			if ss == nil || p == nil {
				return next(ctx, method, req)
			}
			if err := s.open(ctx, ss, p); err != nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
			}
			res, err := next(ctx, method, req)
			if ir, ok := res.(*mcp.InitializeResult); ok && err == nil {
				s.advertise(ir)
			}
			return res, err

		case "logging/setLevel":
			res, err := next(ctx, method, req)
			if p, ok := req.GetParams().(*mcp.SetLoggingLevelParams); ok && err == nil {
				s.mu.Lock()
				ups := slices.Clone(s.ups)
				s.mu.Unlock()
				for _, u := range ups {
					if u.caps.Logging != nil {
						if err := u.cs.SetLoggingLevel(ctx, p); err != nil {
							s.o.Logger.Warn("forwarding setLevel", "upstream", u.endpoint, "err", err)
						}
					}
				}
			}
			return res, err
		}
		return next(ctx, method, req)
	}
}

// open dials every upstream for the downstream session being initialized, and
// registers what each declared. It offers each upstream exactly the client
// capabilities the downstream client offered, because the gateway can only
// answer an upstream's sampling or elicitation by asking its own client.
func (s *session) open(ctx context.Context, down *mcp.ServerSession, p *mcp.InitializeParams) error {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()

	version := p.ProtocolVersion
	if !slices.Contains(Sessioned(), version) {
		version = string(revision.V20251125)
	}
	var offered mcp.ClientCapabilities
	if p.Capabilities != nil {
		offered.Sampling = p.Capabilities.Sampling
		offered.Elicitation = p.Capabilities.Elicitation
	}

	for _, endpoint := range s.o.Upstreams {
		u := &upstream{endpoint: endpoint, watch: newProgressWatch(s.o.HTTPClient.Transport)}
		hc := *s.o.HTTPClient
		hc.Transport = u.watch
		client := mcp.NewClient(&mcp.Implementation{Name: "charpy-fixture-gateway", Version: "0"}, s.clientOptions(u, offered))
		if offered.Elicitation != nil {
			client.AddReceivingMiddleware(s.elicit(u))
		}
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: &hc,
		}, &mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			s.close()
			return fmt.Errorf("gateway: connecting to %s: %w", endpoint, err)
		}
		u.cs = cs
		u.caps = cs.InitializeResult().Capabilities
		if u.caps == nil {
			u.caps = &mcp.ServerCapabilities{}
		}
		s.mu.Lock()
		s.ups = append(s.ups, u)
		s.mu.Unlock()
	}

	for _, u := range s.ups {
		if err := s.relist(ctx, u, false); err != nil {
			s.close()
			return err
		}
	}

	go func() {
		_ = down.Wait()
		s.close()
	}()
	return nil
}

// close ends every upstream session.
func (s *session) close() {
	s.mu.Lock()
	ups := s.ups
	s.ups = nil
	s.mu.Unlock()
	for _, u := range ups {
		_ = u.cs.Close()
	}
}

// advertise narrows the downstream initialize result to what the upstreams
// can back. The SDK infers capabilities from the handlers the server was
// built with, and those are set unconditionally because the server exists
// before any upstream answers; a capability no upstream has would be a
// promise the gateway cannot keep.
func (s *session) advertise(ir *mcp.InitializeResult) {
	caps := ir.Capabilities
	if caps == nil {
		caps = &mcp.ServerCapabilities{}
		ir.Capabilities = caps
	}
	caps.Logging, caps.Completions = nil, nil
	var subscribe bool
	for _, u := range s.ups {
		if u.caps.Logging != nil {
			caps.Logging = &mcp.LoggingCapabilities{}
		}
		if u.caps.Completions != nil {
			caps.Completions = &mcp.CompletionCapabilities{}
		}
		if u.caps.Tools != nil {
			caps.Tools = &mcp.ToolCapabilities{ListChanged: true}
		}
		if u.caps.Prompts != nil {
			caps.Prompts = &mcp.PromptCapabilities{ListChanged: true}
		}
		if u.caps.Resources != nil {
			subscribe = subscribe || u.caps.Resources.Subscribe
			caps.Resources = &mcp.ResourceCapabilities{ListChanged: true}
		}
	}
	if caps.Resources != nil {
		caps.Resources.Subscribe = subscribe
	}
}

// clientOptions are the upstream client's handlers: everything an upstream
// can send its client is passed to the downstream client, on the stream of
// the downstream request it belongs to.
func (s *session) clientOptions(u *upstream, offered mcp.ClientCapabilities) *mcp.ClientOptions {
	o := &mcp.ClientOptions{
		Logger:       s.o.Logger,
		Capabilities: &offered,

		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			s.relistOrWarn(ctx, u)
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			s.relistOrWarn(ctx, u)
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			s.relistOrWarn(ctx, u)
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			_ = s.srv.ResourceUpdated(ctx, req.Params)
		},
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			if d := s.downstream(); d != nil {
				_ = d.Log(u.current(), req.Params)
			}
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			key := tokenKey(req.Params.ProgressToken)
			ctx, ok := s.progressFor(key)
			if d := s.downstream(); d != nil && ok {
				_ = d.NotifyProgress(ctx, req.Params)
				s.mu.Lock()
				s.forwarded[key]++
				close(s.bump)
				s.bump = make(chan struct{})
				s.mu.Unlock()
			}
		},
		ElicitationCompleteHandler: func(_ context.Context, req *mcp.ElicitationCompleteNotificationRequest) {
			if d := s.downstream(); d != nil {
				_ = d.NotifyElicitationComplete(u.current(), req.Params)
			}
		},
	}

	// A handler the SDK finds set adds its capability, so only those the
	// downstream client offered are set.
	if offered.Sampling != nil {
		if offered.Sampling.Tools != nil {
			o.CreateMessageWithToolsHandler = func(_ context.Context, req *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error) {
				d, err := s.mustDownstream()
				if err != nil {
					return nil, err
				}
				return d.CreateMessageWithTools(u.current(), req.Params)
			}
		} else {
			o.CreateMessageHandler = func(_ context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
				d, err := s.mustDownstream()
				if err != nil {
					return nil, err
				}
				return d.CreateMessage(u.current(), req.Params)
			}
		}
	}
	// Elicitation is not a handler: see [session.elicit]. The capability is
	// offered through Capabilities, which the SDK leaves alone when set.
	return o
}

// elicit passes an upstream's elicitation/create to the downstream client
// ahead of the SDK client's own handling, which validates the requested
// schema first and is stricter than the spec: it reads array items typed
// "string" as an untitled enum and refuses them without an enum, though the
// 2025-11-25 schema requires only anyOf of a titled multi-select. The SDK's
// own conformance server sends exactly that shape. A gateway owes its client
// the upstream's question, not its SDK's opinion of it.
func (s *session) elicit(u *upstream) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			p, ok := req.GetParams().(*mcp.ElicitParams)
			if method != "elicitation/create" || !ok {
				return next(ctx, method, req)
			}
			d, err := s.mustDownstream()
			if err != nil {
				return nil, err
			}
			return d.Elicit(u.current(), p)
		}
	}
}

func (s *session) downstream() *mcp.ServerSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

func (s *session) mustDownstream() (*mcp.ServerSession, error) {
	if d := s.downstream(); d != nil {
		return d, nil
	}
	return nil, errors.New("gateway: no downstream session")
}

// current is the context an upstream-originated message is sent downstream
// under. Its value is what the SDK reads to put the message on the stream of
// a request, so it is the newest downstream call still in flight to this
// upstream, or a bare context -- the standalone stream -- when none is.
//
// Newest is a guess when two calls to one upstream overlap: the upstream
// session does not say which of its requests an incoming one relates to. The
// SDK client reads the stream it arrived on and drops that, and the fixture
// will not reach past it. Progress is exact, because a token names its
// request.
func (u *upstream) current() context.Context {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := len(u.calls); n > 0 {
		return u.calls[n-1]
	}
	return context.Background()
}

// track records a downstream request in flight to u, and returns the
// function that ends it.
func (u *upstream) track(ctx context.Context) func() {
	u.mu.Lock()
	u.calls = append(u.calls, ctx)
	u.mu.Unlock()
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		for i, c := range u.calls {
			if c == ctx {
				u.calls = slices.Delete(u.calls, i, i+1)
				return
			}
		}
	}
}

func (s *session) progressFor(key string) (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, ok := s.progress[key]
	return ctx, ok
}

// progressWait bounds how long a finished call waits for progress the
// upstream sent ahead of its answer. The handlers are already queued when
// the call returns, so a working gateway never reaches it; it keeps one that
// stopped forwarding from also never answering.
const progressWait = 2 * time.Second

// drain waits until the progress u's upstream sent ahead of the call's answer
// has all been forwarded, so none of it follows the gateway's own answer.
func (s *session) drain(ctx context.Context, u *upstream, key string) {
	want, ok := u.watch.Ahead(key)
	if !ok {
		return
	}
	timeout := time.NewTimer(progressWait)
	defer timeout.Stop()
	for {
		s.mu.Lock()
		got, bump := s.forwarded[key], s.bump
		s.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-bump:
		case <-ctx.Done():
			return
		case <-timeout.C:
			s.o.Logger.Warn("progress not forwarded before the answer", "upstream", u.endpoint, "want", want, "got", got)
			return
		}
	}
}

// forward runs a downstream request against its owning upstream, tracking it
// as in flight for the messages the upstream sends back while it runs.
func forward[R any](ctx context.Context, u *upstream, call func(context.Context) (R, error)) (R, error) {
	defer u.track(ctx)()
	r, err := call(ctx)
	return r, passError(err)
}

// passError returns an upstream's JSON-RPC error as the gateway's own, code
// intact. The SDK client wraps it; a downstream client is owed the error the
// server sent, not the gateway's account of having received one.
func passError(err error) error {
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) {
		return rpc
	}
	return err
}

func (s *session) relistOrWarn(ctx context.Context, u *upstream) {
	if err := s.relist(ctx, u, true); err != nil {
		s.o.Logger.Warn("relisting upstream", "upstream", u.endpoint, "err", err)
	}
}

// relist reads everything u declares and brings the downstream server in line
// with it: names u no longer declares are removed, new ones added. With
// announce set, the SDK server tells the downstream client which lists
// changed; the first listing, made while the session opens, announces
// nothing.
func (s *session) relist(ctx context.Context, u *upstream, announce bool) error {
	var (
		tools     []*mcp.Tool
		prompts   []*mcp.Prompt
		resources []*mcp.Resource
		templates []*mcp.ResourceTemplate
	)
	if u.caps.Tools != nil {
		for t, err := range u.cs.Tools(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing tools on %s: %w", u.endpoint, err)
			}
			tools = append(tools, t)
		}
	}
	if u.caps.Prompts != nil {
		for p, err := range u.cs.Prompts(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing prompts on %s: %w", u.endpoint, err)
			}
			prompts = append(prompts, p)
		}
	}
	if u.caps.Resources != nil {
		for r, err := range u.cs.Resources(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing resources on %s: %w", u.endpoint, err)
			}
			resources = append(resources, r)
		}
		for t, err := range u.cs.ResourceTemplates(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing resource templates on %s: %w", u.endpoint, err)
			}
			templates = append(templates, t)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	toolNames := names(tools, func(t *mcp.Tool) string { return t.Name })
	promptNames := names(prompts, func(p *mcp.Prompt) string { return p.Name })
	resourceURIs := names(resources, func(r *mcp.Resource) string { return r.URI })
	templateURIs := names(templates, func(t *mcp.ResourceTemplate) string { return t.URITemplate })
	for _, c := range []struct {
		kind  string
		owner map[string]*upstream
		names []string
	}{
		{"tool", s.tools, toolNames},
		{"prompt", s.prompts, promptNames},
		{"resource", s.resources, resourceURIs},
		{"resource template", s.templates, templateURIs},
	} {
		for _, n := range c.names {
			if o, ok := c.owner[n]; ok && o != u {
				return fmt.Errorf("gateway: %s %q is declared by both %s and %s", c.kind, n, o.endpoint, u.endpoint)
			}
		}
	}

	if announce {
		for method, differs := range map[string]bool{
			"notifications/tools/list_changed":     !sameSet(u.tools, toolNames),
			"notifications/prompts/list_changed":   !sameSet(u.prompts, promptNames),
			"notifications/resources/list_changed": !sameSet(u.resources, resourceURIs) || !sameSet(u.templates, templateURIs),
		} {
			if differs {
				s.changed[method] = true
			}
		}
	}

	drop(s.tools, u, u.tools, toolNames, s.srv.RemoveTools)
	drop(s.prompts, u, u.prompts, promptNames, s.srv.RemovePrompts)
	drop(s.resources, u, u.resources, resourceURIs, s.srv.RemoveResources)
	drop(s.templates, u, u.templates, templateURIs, s.srv.RemoveResourceTemplates)
	u.tools, u.prompts, u.resources, u.templates = toolNames, promptNames, resourceURIs, templateURIs

	skip := func(kind, name string, err error) {
		s.o.Logger.Warn("skipping declaration", "upstream", u.endpoint, kind, name, "err", err)
	}
	for _, t := range tools {
		if err := guard(func() { s.srv.AddTool(t, s.callTool(u)) }); err != nil {
			skip("tool", t.Name, err)
			continue
		}
		s.tools[t.Name] = u
	}
	for _, p := range prompts {
		s.srv.AddPrompt(p, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return forward(ctx, u, func(ctx context.Context) (*mcp.GetPromptResult, error) {
				return u.cs.GetPrompt(ctx, req.Params)
			})
		})
		s.prompts[p.Name] = u
	}
	read := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return forward(ctx, u, func(ctx context.Context) (*mcp.ReadResourceResult, error) {
			return u.cs.ReadResource(ctx, req.Params)
		})
	}
	for _, r := range resources {
		if err := guard(func() { s.srv.AddResource(r, read) }); err != nil {
			skip("resource", r.URI, err)
			continue
		}
		s.resources[r.URI] = u
	}
	for _, t := range templates {
		if err := guard(func() { s.srv.AddResourceTemplate(t, read) }); err != nil {
			skip("template", t.URITemplate, err)
			continue
		}
		s.templates[t.URITemplate] = u
	}
	return nil
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func names[T any](xs []T, name func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, name(x))
	}
	return out
}

// drop removes what u declared before and no longer does. Callers hold s.mu.
func drop(owner map[string]*upstream, u *upstream, before, now []string, remove func(...string)) {
	var gone []string
	for _, n := range before {
		if !slices.Contains(now, n) && owner[n] == u {
			gone = append(gone, n)
			delete(owner, n)
		}
	}
	if len(gone) > 0 {
		remove(gone...)
	}
}

// guard turns the SDK's panic on a declaration it will not accept -- a tool
// schema that is not an object, a resource URI or template that does not
// parse -- into an error. An upstream is somebody else's server, and under
// charpy it is one whose listings get faulted: a gateway that died on a bad
// declaration would be a planted bug nobody planted.
func guard(register func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	register()
	return nil
}

// callTool forwards a call as the downstream client made it: arguments
// unparsed, _meta intact so a progress token reaches the upstream, and the
// upstream's result -- isError included -- returned unchanged.
func (s *session) callTool(u *upstream) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params := &mcp.CallToolParams{Meta: req.Params.Meta, Name: req.Params.Name}
		if len(req.Params.Arguments) > 0 {
			params.Arguments = req.Params.Arguments
		}
		tok := req.Params.GetProgressToken()
		if tok == nil {
			return forward(ctx, u, func(ctx context.Context) (*mcp.CallToolResult, error) {
				return u.cs.CallTool(ctx, params)
			})
		}

		key := tokenKey(tok)
		s.mu.Lock()
		s.progress[key] = ctx
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.progress, key)
			delete(s.forwarded, key)
			s.mu.Unlock()
			u.watch.Forget(key)
		}()
		return forward(ctx, u, func(ctx context.Context) (*mcp.CallToolResult, error) {
			res, err := u.cs.CallTool(ctx, params)
			s.drain(ctx, u, key)
			return res, err
		})
	}
}

// complete routes a completion to the upstream that owns what it refers to: a
// prompt by name, a resource template or resource by URI.
func (s *session) complete(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	var u *upstream
	if ref := req.Params.Ref; ref != nil {
		s.mu.Lock()
		switch ref.Type {
		case "ref/prompt":
			u = s.prompts[ref.Name]
		case "ref/resource":
			u = s.templates[ref.URI]
			if u == nil {
				u = s.resources[ref.URI]
			}
		}
		s.mu.Unlock()
	}
	if u == nil || u.caps.Completions == nil {
		return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{}}}, nil
	}
	return forward(ctx, u, func(ctx context.Context) (*mcp.CompleteResult, error) {
		return u.cs.Complete(ctx, req.Params)
	})
}

// owner is the upstream serving a resource URI: the one that listed it, or
// the one whose template matches it.
func (s *session) owner(uri string) *upstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.resources[uri]; ok {
		return u
	}
	for tmpl, u := range s.templates {
		// The SDK's own matcher, which is unexported.
		if t, err := uritemplate.New(tmpl); err == nil && t.Regexp().MatchString(uri) {
			return u
		}
	}
	return nil
}

func (s *session) subscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	u := s.owner(req.Params.URI)
	if u == nil {
		return mcp.ResourceNotFoundError(req.Params.URI)
	}
	if u.caps.Resources == nil || !u.caps.Resources.Subscribe {
		return &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "upstream does not support subscriptions"}
	}
	_, err := forward(ctx, u, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, u.cs.Subscribe(ctx, req.Params)
	})
	return err
}

func (s *session) unsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	u := s.owner(req.Params.URI)
	if u == nil {
		return mcp.ResourceNotFoundError(req.Params.URI)
	}
	_, err := forward(ctx, u, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, u.cs.Unsubscribe(ctx, req.Params)
	})
	return err
}
