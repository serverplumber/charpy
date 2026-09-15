package hostile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/proxy"
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
	refSrv  *http.Server
	refLn   net.Listener
	ingress *http.Server
	ln      net.Listener
}

// NewHTTP prepares a run. Nothing binds until Run.
func NewHTTP(o HTTPOptions) (*HTTPServer, error) {
	refHandler, err := peer.ServerHandler(peer.Options{Era: o.Era})
	if err != nil {
		return nil, err
	}

	refLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hostile: binding reference server: %w", err)
	}

	pr, err := proxy.New(proxy.Options{
		SubjectURL: "http://" + refLn.Addr().String() + "/",
		// The client is the subject, faced upstream. charpy's reference server
		// is correct, so only server-to-client is faulted -- the proxy already
		// faults responses, which is that direction.
		Face:       transcript.Upstream,
		Correlate:  func() transcript.Link { return transcript.Link{Via: transcript.ViaNone} },
		Cases:      o.Cases,
		Transcript: o.Transcript,
		Sched:      o.Sched,
		Ledger:     o.Ledger,
		RunSeed:    o.RunSeed,
		ClientID:   o.ClientID,
		SessionID:  o.SessionID,
		ConnID:     o.ConnID,
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

	return &HTTPServer{
		o:       o,
		proxy:   pr,
		refSrv:  &http.Server{Handler: refHandler},
		refLn:   refLn,
		ingress: &http.Server{Handler: pr},
		ln:      ln,
	}, nil
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
	refErr := make(chan error, 1)
	go func() { refErr <- h.refSrv.Serve(h.refLn) }()
	ingErr := make(chan error, 1)
	go func() { ingErr <- h.ingress.Serve(h.ln) }()

	<-ctx.Done()

	shut := func(s *http.Server) {
		sc, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(sc); err != nil {
			_ = s.Close()
		}
	}
	shut(h.ingress)
	shut(h.refSrv)

	if e := <-ingErr; e != nil && !errors.Is(e, http.ErrServerClosed) {
		return fmt.Errorf("hostile: ingress: %w", e)
	}
	<-refErr
	return nil
}
