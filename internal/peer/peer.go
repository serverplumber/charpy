package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Module is the reference peer's import path, as recorded in the transcript
// header. It is a constant rather than a build-time lookup because the
// transcript's claim is about the pin in go.mod, which is what ADR-011 fixed.
const Module = "github.com/modelcontextprotocol/go-sdk"

// Version is the pinned version, kept in step with go.mod by TestPinMatchesGoMod.
const Version = "v1.8.0-pre.2"

// Options configures a client peer.
type Options struct {
	// Era is the revision to ask the subject for. Empty asks for the latest
	// the SDK knows, which is what the SDK does on its own.
	//
	// It is an ask, not an outcome: the subject may negotiate something else,
	// and a fault may rewrite it in flight. What the conversation settled on
	// is the transcript's revision.negotiated; this is its peer.era.
	Era revision.Revision

	// Name and Title identify charpy to the subject. Both default.
	Name  string
	Title string
}

// Client is the reference peer as a client: a correct MCP client whose bytes
// charpy owns.
//
// The SDK writes newline-delimited JSON into Out and reads it from In, and
// those are charpy's ends of two pipes. Nothing between the SDK and charpy
// re-encodes anything, which is the whole point of the byte seam (ADR-011):
// the bytes the ledger records as intent are the bytes the SDK authored, not
// charpy's re-rendering of a parsed message.
type Client struct {
	// Out carries frames the peer originated. Read it, transcribe it, decide
	// what the wire sees.
	Out io.ReadCloser
	// In carries frames toward the peer. Write what you want it to believe.
	In io.WriteCloser

	client *mcp.Client
	era    revision.Revision
	t      *mcp.IOTransport
}

// NewClient prepares a client peer. Nothing connects until Connect.
func NewClient(o Options) (*Client, error) {
	if o.Era != "" {
		if !revision.Known(o.Era) {
			return nil, fmt.Errorf("peer: era %q is not a revision charpy knows", o.Era)
		}
		// The peer speaks the dated revisions and not draft, so a draft-only
		// case has no peer to carry it. That is a SKIPPED with a reason, and
		// the run must not discover it by watching a handshake fail.
		if o.Era == revision.Draft {
			return nil, fmt.Errorf("peer: no reference peer speaks %s", revision.Draft)
		}
	}

	name := o.Name
	if name == "" {
		name = "charpy"
	}
	title := o.Title
	if title == "" {
		title = "charpy reference peer"
	}

	// The pipes exist from here, not from Connect, because Connect blocks on
	// the handshake: whoever owns Out and In has to be relaying before it is
	// called, and cannot be if the fields are filled in by the call it is
	// waiting on.
	//
	// MaxLineLength is negative -- the cap disabled -- because a size limit
	// inside charpy's own peer would turn a subject's oversized frame into a
	// peer-side error instead of an observation.
	peerReads, charpyWrites := io.Pipe()
	charpyReads, peerWrites := io.Pipe()

	return &Client{
		Out:    charpyReads,
		In:     charpyWrites,
		client: mcp.NewClient(&mcp.Implementation{Name: name, Title: title}, clientOptions()),
		era:    o.Era,
		t: &mcp.IOTransport{
			Reader:        peerReads,
			Writer:        peerWrites,
			MaxLineLength: -1,
		},
	}, nil
}

// Connect opens a session over the pipes.
//
// The transport is mcp.IOTransport rather than an mcp.Transport charpy
// implements: Transport deals in jsonrpc.Message, already parsed, and every
// byte-level fault charpy has needs the bytes.
//
// Connect blocks until the handshake completes, and the handshake crosses Out
// and In, so whoever owns them must already be relaying when it is called.
// It returns once the peer is ready to be scripted.
func (c *Client) Connect(ctx context.Context) (*mcp.ClientSession, error) {
	var opts *mcp.ClientSessionOptions
	if c.era != "" {
		opts = &mcp.ClientSessionOptions{ProtocolVersion: string(c.era)}
	}

	sess, err := c.client.Connect(ctx, c.t, opts)
	if err != nil {
		return nil, fmt.Errorf("peer: connecting at era %q: %w", c.eraLabel(), err)
	}
	return sess, nil
}

// Era is the revision charpy asked the peer to speak, for the transcript
// header. Empty when charpy asked for nothing and took the SDK's latest.
func (c *Client) Era() revision.Revision { return c.era }

// Describe is the transcript header's account of the reference peer.
//
// It is a free function of the era rather than a method on a constructed
// peer, because the transcript writer is built before the run and a method
// would make the two circular: the header needs the peer, and the driver
// needs the header's writer. Module and Version are constants, so nothing has
// to exist yet for this to be true.
func Describe(era revision.Revision) *transcript.Peer {
	return &transcript.Peer{Module: Module, Version: Version, Era: era}
}

func (c *Client) eraLabel() string {
	if c.era == "" {
		return "latest"
	}
	return string(c.era)
}

// Server is the reference peer as a server: a correct MCP server whose
// response bytes charpy owns and corrupts before a client under test sees them.
//
// It is the mirror of Client. The SDK server reads requests from In and writes
// responses to Out -- charpy's ends of two pipes -- and nothing between the
// server and charpy re-encodes anything, the byte seam ADR-011 chose. Where a
// client peer originates the traffic, a server peer answers it: the client
// under test drives, and the server reacts, which is why the hostile driver is
// relay-shaped (docs/design/interposer.md section 5).
type Server struct {
	// Out carries frames the server originated -- responses toward the client.
	// Read it, transcribe it, decide what the client sees.
	Out io.ReadCloser
	// In carries frames toward the server -- the client's requests, relayed.
	In io.WriteCloser

	srv *mcp.Server
	t   *mcp.IOTransport
	era revision.Revision
	ask *asker
}

// NewServer prepares a reference-peer server at an era, with a minimal tool
// surface for a client to exercise. Nothing runs until Serve.
//
// The surface is deliberately small for v0: a client initializes, perhaps
// lists tools, perhaps calls one, and charpy faults the answers. A richer or
// configurable surface is a later concern; what matters here is that the
// server is correct, so anything the client mishandles is charpy's fault to
// have injected, not the server's to have emitted.
func NewServer(o Options) (*Server, error) {
	if o.Era == revision.Draft {
		return nil, fmt.Errorf("peer: no reference peer speaks %s", revision.Draft)
	}
	if o.Era != "" && !revision.Known(o.Era) {
		return nil, fmt.Errorf("peer: era %q is not a revision charpy knows", o.Era)
	}

	name, title := o.Name, o.Title
	if name == "" {
		name = "charpy"
	}
	if title == "" {
		title = "charpy reference peer"
	}

	ask := newAsker(o.Era, false)
	srv := newMCPServer(name, title, o.Era, ask)

	serverReads, charpyWrites := io.Pipe()
	charpyReads, serverWrites := io.Pipe()

	return &Server{
		Out: charpyReads,
		In:  charpyWrites,
		srv: srv,
		era: o.Era,
		ask: ask,
		t: &mcp.IOTransport{
			Reader:        serverReads,
			Writer:        serverWrites,
			MaxLineLength: -1,
		},
	}, nil
}

// Serve runs the server over the pipes until the context is cancelled or the
// pipes close. It blocks, so callers run it in a goroutine and relay In and Out.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.srv.Run(ctx, s.t); err != nil && ctx.Err() == nil {
		return fmt.Errorf("peer: server: %w", err)
	}
	return nil
}

// Era is the revision the server was built to speak, for the transcript header.
func (s *Server) Era() revision.Revision { return s.era }

// Close tears down charpy's ends of the pipes, which ends the server's loop.
func (s *Server) Close() error {
	return errors.Join(s.In.Close(), s.Out.Close())
}

// Ping has the server ask the client under test a ping, once the client has
// initialized its session. See [ErrCannotAsk] for when it will not.
//
// The server asks rather than charpy writing a ping into the pipe, so the
// question is the SDK's own bytes crossing the interposer like any other
// correct frame, and the client's answer goes back to the session that asked
// -- which is what makes it an answer rather than a stray response charpy
// would have to swallow.
func (s *Server) Ping(ctx context.Context) error { return s.ask.ping(ctx, "") }

// ErrCannotAsk is returned when a reference server has no way to put a
// question to the client: the era is 2026-07-28 or later, where a server
// originates no requests, or the era is unset and nothing says the session
// will be a sessioned one.
var ErrCannotAsk = errors.New("peer: this server cannot originate a request")

// asker holds the sessions a reference server's clients have initialized, so
// a driver can put a question to one by id.
//
// It waits on initialization rather than on connection because a question put
// before notifications/initialized is one the client may rightly refuse, and
// a handshake the fault broke never initializes at all -- which is exactly the
// case with no session to ask on. Keying by session id lets one asker serve
// the HTTP handler's fresh server per session; over stdio there is one
// session and its id is empty.
type asker struct {
	era revision.Revision
	// stream says a question also waits for the client's standalone GET
	// stream, which is where the SDK sends a request a server originates
	// over Streamable HTTP. Stdio has one pipe and nothing to wait for.
	stream bool

	mu     sync.Mutex
	ready  map[string]chan struct{}
	listen map[string]chan struct{}
	sess   map[string]*mcp.ServerSession
}

func newAsker(era revision.Revision, stream bool) *asker {
	return &asker{
		era: era, stream: stream,
		ready: map[string]chan struct{}{}, listen: map[string]chan struct{}{},
		sess: map[string]*mcp.ServerSession{},
	}
}

// gateLocked returns the channel in m for id, closed when that gate opens.
// Callers hold mu.
func gateLocked(m map[string]chan struct{}, id string) chan struct{} {
	ch, ok := m[id]
	if !ok {
		ch = make(chan struct{})
		m[id] = ch
	}
	return ch
}

// open closes id's gate in m, once.
func (a *asker) open(m map[string]chan struct{}, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := gateLocked(m, id)
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// wait blocks until id's gate in m opens or ctx ends.
func (a *asker) wait(ctx context.Context, m map[string]chan struct{}, id string) error {
	a.mu.Lock()
	ch := gateLocked(m, id)
	a.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// initialized is the server's InitializedHandler.
func (a *asker) initialized(_ context.Context, req *mcp.InitializedRequest) {
	id := req.Session.ID()
	a.mu.Lock()
	if _, seen := a.sess[id]; !seen {
		a.sess[id] = req.Session
	}
	a.mu.Unlock()
	a.open(a.ready, id)
}

// ping waits for session id to initialize, bounded by ctx, and pings it.
//
// A server may ping its client in every sessioned revision; from 2026-07-28 a
// server originates no requests at all, and the stateless handshake has no
// initialized notification to wait for. An unset era is refused with it: the
// SDK's own latest is the stateless one, so waiting would wait for nothing.
func (a *asker) ping(ctx context.Context, id string) error {
	if a.era == "" || a.era.Stateless() {
		return ErrCannotAsk
	}
	if err := a.wait(ctx, a.ready, id); err != nil {
		return err
	}
	if a.stream {
		if err := a.wait(ctx, a.listen, id); err != nil {
			return err
		}
	}
	a.mu.Lock()
	ss := a.sess[id]
	a.mu.Unlock()

	// The GET is seen before the SDK attaches it to the session, so the first
	// ping can still find no stream there. The SDK refuses such a write before
	// anything crosses the wire, which makes asking again safe; the bound is
	// generous against an attach that takes microseconds.
	for range 50 {
		err := ss.Ping(ctx, nil)
		if !errors.Is(err, errRejected) || ctx.Err() != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ss.Ping(ctx, nil)
}

// clientOptions configures the reference client. It answers sampling, so a
// server under test that asks its client for a completion mid-call gets an
// answer -- and a case can withhold that answer, which is the only way to put
// to a server the question of a request of its own that goes unanswered. The
// answer is canned: charpy is not a model, and nothing judges what it says.
func clientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		CreateMessageHandler: func(context.Context, *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Role:    "assistant",
				Model:   "charpy-reference-peer",
				Content: &mcp.TextContent{Text: "ok"},
			}, nil
		},
	}
}

// errRejected is the SDK's jsonrpc2.ErrRejected, which it does not export: a
// message its transport refused to send. WireError matches on code alone.
var errRejected = &jsonrpc.Error{Code: -32005}

// newMCPServer builds the reference server: the SDK server with a minimal tool
// surface, restricted to one era. Claiming another era is a fault
// (capability_flip on the initialize result), not a second configuration
// (docs/design/revisions.md section 4).
func newMCPServer(name, title string, era revision.Revision, ask *asker) *mcp.Server {
	opts := &mcp.ServerOptions{InitializedHandler: ask.initialized}
	if era != "" {
		opts.SupportedProtocolVersions = []string{string(era)}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Title: title}, opts)
	addTools(srv)
	return srv
}

// addTools registers the reference server's tools. It is its own function so
// that OutputSchemas can list the same set from a server no client under test
// will ever reach.
func addTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes its argument"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	// add_numbers is the tool the SDK's conformance client calls in its
	// tools_call scenario, so a real client under test has a call to make and
	// a fault aimed at tools/call has an answer to land on. Its result is
	// typed, so the SDK declares an outputSchema for it -- the declaration a
	// schema_violation with target declared_output_schema breaks.
	mcp.AddTool(srv, &mcp.Tool{Name: "add_numbers", Description: "adds two numbers"},
		func(ctx context.Context, req *mcp.CallToolRequest, args AddArgs) (*mcp.CallToolResult, AddResult, error) {
			return nil, AddResult{Sum: args.A + args.B}, nil
		})
}

// OutputSchemas is the outputSchema each reference tool declares, by tool
// name, exactly as a client's tools/list sees it -- which is what a
// schema_violation against the declared output has to break. The SDK derives
// a schema from a tool's result type when the tool is added, and keeps it to
// itself, so the only faithful copy is the one it lists. It lists from a
// throwaway server with the same tools, over the SDK's in-memory transport,
// so no session a client under test could be confused with ever exists.
func OutputSchemas() (map[string]json.RawMessage, error) {
	schemasOnce.Do(func() { schemas, schemasErr = listOutputSchemas() })
	return schemas, schemasErr
}

var (
	schemasOnce sync.Once
	schemas     map[string]json.RawMessage
	schemasErr  error
)

func listOutputSchemas() (map[string]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "charpy-schemas"}, nil)
	addTools(srv)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		return nil, fmt.Errorf("peer: listing reference tools: %w", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "charpy-schemas"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("peer: listing reference tools: %w", err)
	}
	defer cs.Close()

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("peer: listing reference tools: %w", err)
	}
	out := map[string]json.RawMessage{}
	for _, t := range listed.Tools {
		if t.OutputSchema == nil {
			continue
		}
		raw, err := json.Marshal(t.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("peer: %s outputSchema: %w", t.Name, err)
		}
		out[t.Name] = raw
	}
	return out, nil
}

// AddArgs is add_numbers' input.
type AddArgs struct {
	A float64 `json:"a" jsonschema:"the first addend"`
	B float64 `json:"b" jsonschema:"the second addend"`
}

// AddResult is add_numbers' structured output.
type AddResult struct {
	Sum float64 `json:"sum" jsonschema:"a plus b"`
}

// HTTPServer is the reference server over Streamable HTTP: the handler to
// serve, and a way to have it ask a client a question.
type HTTPServer struct {
	handler http.Handler
	ask     *asker
}

// ServeHTTP serves the SDK's handler, noting each session's standalone GET
// stream as it arrives: that stream is the only way a question from this
// server reaches the client.
func (h *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if sid := r.Header.Get("Mcp-Session-Id"); r.Method == http.MethodGet && sid != "" {
		h.ask.open(h.ask.listen, sid)
	}
	h.handler.ServeHTTP(w, r)
}

// Ping has the server ask the client holding sessionID a ping, once that
// session has initialized and opened its standalone GET stream. The SDK sends
// a request a server originates on that stream, so a client that never opens
// one cannot be asked, and Ping waits until ctx ends.
func (h *HTTPServer) Ping(ctx context.Context, sessionID string) error {
	return h.ask.ping(ctx, sessionID)
}

// ServerHandler is the reference server as a Streamable HTTP handler, for the
// hostile driver's HTTP mode: charpy stands this up and a proxy in front of it
// faults its responses toward the client under test. A fresh server per
// request keeps each of the client's connections independent, which is what
// makes a reconnect a real reconnect.
func ServerHandler(o Options) (*HTTPServer, error) {
	if o.Era == revision.Draft {
		return nil, fmt.Errorf("peer: no reference peer speaks %s", revision.Draft)
	}
	if o.Era != "" && !revision.Known(o.Era) {
		return nil, fmt.Errorf("peer: era %q is not a revision charpy knows", o.Era)
	}
	name, title := o.Name, o.Title
	if name == "" {
		name = "charpy"
	}
	if title == "" {
		title = "charpy reference peer"
	}
	ask := newAsker(o.Era, true)
	return &HTTPServer{
		handler: mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return newMCPServer(name, title, o.Era, ask) }, nil,
		),
		ask: ask,
	}, nil
}

// DialHTTP connects the reference peer to an MCP server over Streamable HTTP
// and returns the open session.
//
// Unlike the stdio Client, this exposes no pipes: charpy does not sit on the
// peer's transport here, it sits at a listener the peer connects to. The
// endpoint is charpy's proxy, which forwards to the real subject; the peer is
// an ordinary correct HTTP client and never knows charpy is in the path.
//
// DisableStandaloneSSE is set: the peer does request/response only and opens
// no standalone GET stream. v0's HTTP cases are all POST-response faults, and
// the standalone stream is optional per spec (the SDK says so), so declining
// it keeps the proxy to the one method the cases exercise rather than the
// whole transport.
func DialHTTP(ctx context.Context, endpoint string, o Options) (*mcp.ClientSession, error) {
	if o.Era == revision.Draft {
		return nil, fmt.Errorf("peer: no reference peer speaks %s", revision.Draft)
	}
	if o.Era != "" && !revision.Known(o.Era) {
		return nil, fmt.Errorf("peer: era %q is not a revision charpy knows", o.Era)
	}

	name, title := o.Name, o.Title
	if name == "" {
		name = "charpy"
	}
	if title == "" {
		title = "charpy reference peer"
	}

	client := mcp.NewClient(&mcp.Implementation{Name: name, Title: title}, clientOptions())
	t := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
	}

	var opts *mcp.ClientSessionOptions
	if o.Era != "" {
		opts = &mcp.ClientSessionOptions{ProtocolVersion: string(o.Era)}
	}

	sess, err := client.Connect(ctx, t, opts)
	if err != nil {
		return nil, fmt.Errorf("peer: connecting to %s: %w", endpoint, err)
	}
	return sess, nil
}

// Close tears down charpy's ends of the pipes. The peer's ends close with
// them, which is what ends its read loop.
func (c *Client) Close() error {
	var errs []error
	if c.In != nil {
		errs = append(errs, c.In.Close())
	}
	if c.Out != nil {
		errs = append(errs, c.Out.Close())
	}
	return errors.Join(errs...)
}
