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
// It is a control, so it is correct by default: with nothing planted, a run
// against it must find nothing. Each place a gateway is easy to get wrong is
// got right on purpose, and lives in its own file -- an idle deadline on
// upstream calls (idle.go); answers that never name an upstream, a fresh
// upstream session when one fails, and one call at a time per upstream
// (upstream.go); and a barrier, so nothing an upstream sent during a call
// follows the gateway's answer to it (order.go).
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
	// UpstreamIdle is how long a call may hear nothing from its upstream
	// before the gateway answers it with an error. Zero takes
	// DefaultUpstreamIdle.
	UpstreamIdle time.Duration
	// UpstreamHeader is sent on every upstream request: the gateway's
	// credential for its upstreams.
	UpstreamHeader http.Header
	// Plants are the bugs planted on purpose; none by default. See [Plant].
	Plants []Plant
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
	if o.UpstreamIdle <= 0 {
		o.UpstreamIdle = DefaultUpstreamIdle
	}
	for _, p := range o.Plants {
		if !slices.Contains(Plants, p) {
			return nil, fmt.Errorf("gateway: unknown plant %q", p)
		}
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
	// about.
	progress map[string]context.Context

	// changed holds the list kinds an upstream's relist actually changed,
	// so that only those reach the downstream client as list_changed.
	changed map[string]bool

	// What the downstream client set up, to set up again on an upstream
	// that had to be reconnected: the protocol version and capabilities it
	// initialized with, the logging level it asked for, and the resources it
	// is subscribed to, by owner.
	version string
	offered mcp.ClientCapabilities
	level   *mcp.SetLoggingLevelParams
	subs    map[string]*upstream
}

func newSession(o Options) *session {
	s := &session{
		o:         o,
		tools:     map[string]*upstream{},
		prompts:   map[string]*upstream{},
		resources: map[string]*upstream{},
		templates: map[string]*upstream{},
		progress:  map[string]context.Context{},
		changed:   map[string]bool{},
		subs:      map[string]*upstream{},
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
				// A collision is the gateway's configuration, and names only
				// what the client would see listed anyway. Anything else
				// names an upstream, and goes to the log (see [session.answer]).
				var col *collision
				if errors.As(err, &col) {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: col.Error()}
				}
				s.o.Logger.Warn("opening upstreams", "err", err)
				return nil, errUnavailable
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
				s.level = p
				ups := slices.Clone(s.ups)
				s.mu.Unlock()
				// An upstream with no live link hears it when it is
				// reconnected (see [session.restore]).
				for _, u := range ups {
					if l := u.live(); l != nil {
						s.setLevel(ctx, u, l, p)
					}
				}
			}
			return res, err
		}
		return next(ctx, method, req)
	}
}

// open dials every upstream for the downstream session being initialized, and
// registers what each declared.
func (s *session) open(ctx context.Context, down *mcp.ServerSession, p *mcp.InitializeParams) error {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()

	s.version = p.ProtocolVersion
	if !slices.Contains(Sessioned(), s.version) {
		s.version = string(revision.V20251125)
	}
	if p.Capabilities != nil {
		s.offered.Sampling = p.Capabilities.Sampling
		s.offered.Elicitation = p.Capabilities.Elicitation
	}

	for _, endpoint := range s.o.Upstreams {
		u := newUpstream(endpoint, s.o.UpstreamIdle, !s.planted(PlantNoDeadline))
		l, err := s.dial(ctx, u)
		if err != nil {
			s.close()
			return err
		}
		u.set(l)
		s.mu.Lock()
		s.ups = append(s.ups, u)
		s.mu.Unlock()
	}

	for _, u := range s.ups {
		lctx, cancel := context.WithTimeout(ctx, u.idle)
		err := s.relist(lctx, u, u.live(), false)
		cancel()
		if err != nil {
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

// dial opens a session on u. It offers the upstream exactly the client
// capabilities the downstream client offered, because the gateway can only
// answer an upstream's sampling or elicitation by asking its own client.
//
// Each session gets its own client, HTTP client and barrier, so nothing one
// session read and never handled can hold up the next.
func (s *session) dial(ctx context.Context, u *upstream) (*link, error) {
	base := s.o.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if len(s.o.UpstreamHeader) > 0 {
		base = &headerTransport{base: base, header: s.o.UpstreamHeader}
	}
	b := newBarrier(base)
	hc := *s.o.HTTPClient
	hc.Transport = b
	client := mcp.NewClient(&mcp.Implementation{Name: "charpy-fixture-gateway", Version: "0"}, s.clientOptions(u, s.offered))
	if s.offered.Elicitation != nil {
		client.AddReceivingMiddleware(s.elicit(u))
	}
	// Added last, so it is outermost: a marker goes no further.
	client.AddReceivingMiddleware(b.middleware)

	// The handshake gets the same idle bound as a call: an upstream that
	// never answers initialize must not hold the downstream one forever.
	// The session itself outlives this context; the SDK detaches it.
	cctx, cancel := context.WithTimeout(ctx, u.idle)
	defer cancel()
	cs, err := client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:   u.endpoint,
		HTTPClient: &hc,
	}, &mcp.ClientSessionOptions{ProtocolVersion: s.version})
	if err != nil {
		return nil, fmt.Errorf("gateway: connecting to %s: %w", u.endpoint, err)
	}
	caps := cs.InitializeResult().Capabilities
	if caps == nil {
		caps = &mcp.ServerCapabilities{}
	}
	l := &link{cs: cs, caps: caps, barrier: b}
	// A session the upstream ends on its own is gone too.
	go func() {
		_ = cs.Wait()
		u.lose(l)
		s.cascade()
	}()
	return l, nil
}

// restore brings a reopened link back to where the downstream session had
// the last one.
func (s *session) restore(ctx context.Context, u *upstream, l *link) error {
	if err := s.relist(ctx, u, l, true); err != nil {
		return err
	}
	s.mu.Lock()
	level := s.level
	var uris []string
	for uri, owner := range s.subs {
		if owner == u {
			uris = append(uris, uri)
		}
	}
	s.mu.Unlock()
	if level != nil {
		s.setLevel(ctx, u, l, level)
	}
	if l.caps.Resources != nil && l.caps.Resources.Subscribe {
		for _, uri := range uris {
			if err := l.cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
				s.o.Logger.Warn("resubscribing", "upstream", u.endpoint, "uri", uri, "err", err)
			}
		}
	}
	return nil
}

func (s *session) setLevel(ctx context.Context, u *upstream, l *link, p *mcp.SetLoggingLevelParams) {
	if l.caps.Logging == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, u.idle)
	defer cancel()
	if err := l.cs.SetLoggingLevel(ctx, p); err != nil {
		s.o.Logger.Warn("forwarding setLevel", "upstream", u.endpoint, "err", err)
	}
}

// close ends every upstream session.
func (s *session) close() {
	s.mu.Lock()
	ups := s.ups
	s.ups = nil
	s.mu.Unlock()
	for _, u := range ups {
		if l := u.live(); l != nil {
			u.lose(l)
		}
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
		uc := u.caps()
		if uc.Logging != nil {
			caps.Logging = &mcp.LoggingCapabilities{}
		}
		if uc.Completions != nil {
			caps.Completions = &mcp.CompletionCapabilities{}
		}
		if uc.Tools != nil {
			caps.Tools = &mcp.ToolCapabilities{ListChanged: true}
		}
		if uc.Prompts != nil {
			caps.Prompts = &mcp.PromptCapabilities{ListChanged: true}
		}
		if uc.Resources != nil {
			subscribe = subscribe || uc.Resources.Subscribe
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
			u.touch()
			s.relistOrWarn(ctx, u)
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			u.touch()
			s.relistOrWarn(ctx, u)
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			u.touch()
			s.relistOrWarn(ctx, u)
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			u.touch()
			_ = s.srv.ResourceUpdated(ctx, req.Params)
		},
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			u.touch()
			if d := s.downstream(); d != nil {
				_ = d.Log(u.current(), req.Params)
			}
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			u.touch()
			key := tokenKey(req.Params.ProgressToken)
			ctx, ok := s.progressFor(key)
			if d := s.downstream(); d != nil && ok {
				_ = d.NotifyProgress(ctx, req.Params)
			}
		},
		ElicitationCompleteHandler: func(_ context.Context, req *mcp.ElicitationCompleteNotificationRequest) {
			u.touch()
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
				defer u.ask()()
				return d.CreateMessageWithTools(u.current(), req.Params)
			}
		} else {
			o.CreateMessageHandler = func(_ context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
				d, err := s.mustDownstream()
				if err != nil {
					return nil, err
				}
				defer u.ask()()
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
			defer u.ask()()
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

func (s *session) progressFor(key string) (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, ok := s.progress[key]
	return ctx, ok
}

func (s *session) relistOrWarn(ctx context.Context, u *upstream) {
	l := u.live()
	if l == nil {
		return // relisted when it is reconnected
	}
	ctx, cancel := context.WithTimeout(ctx, u.idle)
	defer cancel()
	if err := s.relist(ctx, u, l, true); err != nil {
		s.o.Logger.Warn("relisting upstream", "upstream", u.endpoint, "err", err)
	}
}

// relist reads everything u declares and brings the downstream server in line
// with it: names u no longer declares are removed, new ones added. With
// announce set, the SDK server tells the downstream client which lists
// changed; the first listing, made while the session opens, announces
// nothing.
func (s *session) relist(ctx context.Context, u *upstream, l *link, announce bool) error {
	var (
		tools     []*mcp.Tool
		prompts   []*mcp.Prompt
		resources []*mcp.Resource
		templates []*mcp.ResourceTemplate
	)
	if l.caps.Tools != nil {
		for t, err := range l.cs.Tools(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing tools on %s: %w", u.endpoint, err)
			}
			tools = append(tools, t)
		}
	}
	if l.caps.Prompts != nil {
		for p, err := range l.cs.Prompts(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing prompts on %s: %w", u.endpoint, err)
			}
			prompts = append(prompts, p)
		}
	}
	if l.caps.Resources != nil {
		for r, err := range l.cs.Resources(ctx, nil) {
			if err != nil {
				return fmt.Errorf("gateway: listing resources on %s: %w", u.endpoint, err)
			}
			resources = append(resources, r)
		}
		for t, err := range l.cs.ResourceTemplates(ctx, nil) {
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
				s.o.Logger.Warn("declared twice", c.kind, n, "upstream", o.endpoint, "and", u.endpoint)
				return &collision{kind: c.kind, name: n}
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
			return forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (*mcp.GetPromptResult, error) {
				return cs.GetPrompt(ctx, req.Params)
			})
		})
		s.prompts[p.Name] = u
	}
	read := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (*mcp.ReadResourceResult, error) {
			return cs.ReadResource(ctx, req.Params)
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

// collision is two upstreams declaring the same name. Its text names the
// name, which the downstream client would see listed anyway, and not the
// upstreams; the log has those.
type collision struct{ kind, name string }

func (c *collision) Error() string {
	return fmt.Sprintf("gateway: %s %q is declared by two upstreams", c.kind, c.name)
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
		if tok := req.Params.GetProgressToken(); tok != nil {
			key := tokenKey(tok)
			s.mu.Lock()
			s.progress[key] = ctx
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.progress, key)
				s.mu.Unlock()
			}()
		}
		return forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (*mcp.CallToolResult, error) {
			return cs.CallTool(ctx, params)
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
	if u == nil || u.caps().Completions == nil {
		return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{}}}, nil
	}
	return forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (*mcp.CompleteResult, error) {
		return cs.Complete(ctx, req.Params)
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
	if uc := u.caps(); uc.Resources == nil || !uc.Resources.Subscribe {
		return &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "upstream does not support subscriptions"}
	}
	_, err := forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (struct{}, error) {
		return struct{}{}, cs.Subscribe(ctx, req.Params)
	})
	if err == nil {
		s.mu.Lock()
		s.subs[req.Params.URI] = u
		s.mu.Unlock()
	}
	return err
}

func (s *session) unsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	u := s.owner(req.Params.URI)
	if u == nil {
		return mcp.ResourceNotFoundError(req.Params.URI)
	}
	_, err := forward(s, ctx, u, func(ctx context.Context, cs *mcp.ClientSession) (struct{}, error) {
		return struct{}{}, cs.Unsubscribe(ctx, req.Params)
	})
	if err == nil {
		s.mu.Lock()
		delete(s.subs, req.Params.URI)
		s.mu.Unlock()
	}
	return err
}
