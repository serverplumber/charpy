package hostile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// HTTPOptions configures a hostile-server run over HTTP.
type HTTPOptions struct {
	// Listen is the address charpy's ingress binds -- where the client under
	// test connects. Empty takes an ephemeral loopback port.
	Listen string
	// Era is the revision charpy's server claims.
	Era revision.Revision

	Cases      []interpose.Case
	Transcript *transcript.Writer
	Sched      clock.Sched
	Ledger     *interpose.Ledger

	RunSeed   string
	ClientID  string
	SessionID string
	ConnID    string
}

// HTTPServer is the hostile server over HTTP.
//
// It is the proxy pointed at charpy's own reference server rather than a remote
// one: the client under test connects to the ingress, charpy forwards to an
// in-process reference peer, and faults the responses toward the client. The
// client is the subject, faced upstream; the correlation is ViaNone, matching
// the stdio hostile driver, because the one face under test is the client and
// there is nothing to join. Reconnect is a real reconnect -- a fresh reference
// server per request -- which is what lets capability_flip fire on the second
// connection's initialize result and catch a client reusing cached capabilities.
type HTTPServer struct {
	o       HTTPOptions
	proxy   *proxy.Proxy
	ref     *peer.HTTPServer
	refSrv  *http.Server
	refLn   net.Listener
	ingress *http.Server
	ln      net.Listener

	// askCtx bounds the follow-up pings; Run sets it before serving and ends
	// it before shutting down. asking holds the sessions with a ping in
	// flight, one per session for the reason the stdio driver keeps one.
	askCtx context.Context
	asks   sync.WaitGroup
	mu     sync.Mutex
	asking map[string]bool
}

// NewHTTP prepares a run. Nothing binds until Run.
func NewHTTP(o HTTPOptions) (*HTTPServer, error) {
	ref, err := peer.ServerHandler(peer.Options{Era: o.Era})
	if err != nil {
		return nil, err
	}

	refLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hostile: binding reference server: %w", err)
	}

	h := &HTTPServer{o: o, ref: ref, asking: map[string]bool{}}
	// charpy serves here, so it knows the declarations a schema_violation
	// against the declared output breaks. A failed listing leaves them nil,
	// and such a case not applicable rather than guessing.
	schemas, _ := peer.OutputSchemas()
	pr, err := proxy.New(proxy.Options{
		SubjectURL: "http://" + refLn.Addr().String() + "/",
		// The client is the subject, faced upstream. charpy's reference server
		// is correct, so only server-to-client is faulted -- the proxy already
		// faults responses, which is that direction.
		Face:       transcript.Upstream,
		Correlate:  func(envelope.Message) transcript.Link { return transcript.Link{Via: transcript.ViaNone} },
		Cases:      o.Cases,
		Transcript: o.Transcript,
		Sched:      o.Sched,
		Ledger:     o.Ledger,
		RunSeed:    o.RunSeed,
		ClientID:   o.ClientID,
		SessionID:  o.SessionID,
		ConnID:     o.ConnID,
		Applied:    h.ask,

		OutputSchemas: schemas,
	})
	if err != nil {
		_ = refLn.Close()
		return nil, err
	}

	addr := o.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = refLn.Close()
		return nil, fmt.Errorf("hostile: binding ingress: %w", err)
	}

	h.proxy, h.refLn, h.ln = pr, refLn, ln
	h.refSrv = &http.Server{Handler: ref}
	h.ingress = &http.Server{Handler: pr}
	return h, nil
}

// ask has charpy's reference server put one ping to the client holding
// sessionID, after a fault acted on a response in that session. It is the
// proxy's Applied hook, so it runs on the handler that applied the fault and
// must not block it: the ping goes out on a stream that same proxy relays.
//
// The SDK sends it on the session's standalone GET stream, which the proxy
// relays like any other event stream; it is marked as the follow-up first, so
// the armed case cannot match it there. Over HTTP a then = "close" ends one
// response stream and not the session, so unlike stdio it is still asked
// (proxy.Proxy.Askable). A client that opened no standalone stream cannot be
// asked by any server, and the SDK's refusal is noted rather than hidden.
func (h *HTTPServer) ask(sessionID string) {
	h.mu.Lock()
	if h.asking[sessionID] || h.askCtx == nil {
		h.mu.Unlock()
		return
	}
	h.asking[sessionID] = true
	ctx := h.askCtx
	h.asks.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.asks.Done()
		defer func() {
			h.mu.Lock()
			delete(h.asking, sessionID)
			h.mu.Unlock()
		}()

		withdraw := h.proxy.Ask(sessionID, transcript.S2C)
		defer withdraw()
		err := h.ref.Ping(ctx, sessionID)
		switch {
		case err == nil, ctx.Err() != nil, errors.Is(err, peer.ErrCannotAsk):
		default:
			h.proxy.Note(sessionID, fmt.Sprintf("follow-up ping: %v", err))
		}
	}()
}

// Endpoint is the URL the client under test connects to, valid once Run has
// bound the ingress.
func (h *HTTPServer) Endpoint() string {
	if h.ln == nil {
		return ""
	}
	return "http://" + h.ln.Addr().String() + "/"
}

// Run serves until the context is cancelled. The listeners are already bound
// (see NewHTTP), so Endpoint is valid before Run is called. It is relay-shaped:
// the client drives, so a signal ends the run.
func (h *HTTPServer) Run(ctx context.Context) error {
	askCtx, stopAsking := context.WithCancel(ctx)
	h.mu.Lock()
	h.askCtx = askCtx
	h.mu.Unlock()

	refErr := make(chan error, 1)
	go func() { refErr <- h.refSrv.Serve(h.refLn) }()
	ingErr := make(chan error, 1)
	go func() { ingErr <- h.ingress.Serve(h.ln) }()

	<-ctx.Done()
	// No ping starts after this, so the Wait below cannot race an Add.
	h.mu.Lock()
	h.askCtx = nil
	h.mu.Unlock()
	stopAsking()

	shut := func(s *http.Server) {
		sc, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(sc); err != nil {
			_ = s.Close()
		}
	}
	shut(h.ingress)
	shut(h.refSrv)
	h.asks.Wait()

	if e := <-ingErr; e != nil && !errors.Is(e, http.ErrServerClosed) {
		return fmt.Errorf("hostile: ingress: %w", e)
	}
	<-refErr
	return nil
}
