package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
)

// DefaultTimeout bounds a scripted run the subject has stopped answering,
// matching the stdio driver and the catalogue's own liveness budgets.
const DefaultTimeout = 30 * time.Second

// ScriptOptions configures an owned-stimulus HTTP run.
type ScriptOptions struct {
	Options

	// Case is the one case armed for this run (ADR-012).
	Case interpose.Case
	// Era is the revision to ask the subject for. Empty takes the SDK latest.
	Era revision.Revision
	// Scenario overrides the derived script. Nil derives it from the case.
	Scenario scenario.Scenario
	// Timeout bounds the script. Zero takes DefaultTimeout.
	Timeout time.Duration
	// Listen is the address charpy's ingress binds. Empty takes an ephemeral
	// loopback port, which is what a test and a one-shot run want.
	Listen string
}

// Script is owned stimulus over HTTP: charpy stands up an ingress, drives the
// subject through it with its own peer, and corrupts the responses in flight.
//
// It is the proxy's analog of stdio.Script and ends the same way -- when the
// script does -- so it can exit with a verdict rather than on a signal.
type Script struct {
	o     ScriptOptions
	proxy *Proxy
	run   scenario.Scenario
	srv   *http.Server
	ln    net.Listener
}

// NewScript prepares a run. Nothing binds or connects until Run.
func NewScript(o ScriptOptions) (*Script, error) {
	if o.Case.ID == "" {
		return nil, errors.New("proxy: a scripted run needs a case to arm")
	}
	if o.Case.Fault.Kind == "" {
		return nil, fmt.Errorf("proxy: case %s names no fault mechanism", o.Case.ID)
	}

	inner := o.Options
	inner.Cases = []interpose.Case{o.Case}
	pr, err := New(inner)
	if err != nil {
		return nil, err
	}

	run := o.Scenario
	if run == nil {
		run, err = scenario.Basic(o.Case.Match, scenario.Options{})
		if err != nil {
			return nil, err
		}
	}

	return &Script{o: o, proxy: pr, run: run}, nil
}

// Endpoint is the URL the peer connects to, valid once Run has bound the
// listener.
func (s *Script) Endpoint() string {
	if s.ln == nil {
		return ""
	}
	return "http://" + s.ln.Addr().String() + "/"
}

// Run binds the ingress, connects the peer through it, and plays the script.
func (s *Script) Run(ctx context.Context) error {
	addr := s.o.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy: binding ingress: %w", err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s.proxy}

	served := make(chan error, 1)
	go func() { served <- s.srv.Serve(ln) }()

	sctx, halt := scenario.Interruptible(ctx)
	timeout := s.o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	expiry := time.AfterFunc(timeout, halt.Expired)
	defer expiry.Stop()

	sess, err := peer.DialHTTP(sctx, s.Endpoint(), peer.Options{Era: s.o.Era})
	if err != nil {
		s.shutdown()
		<-served
		return fmt.Errorf("proxy: %w", err)
	}

	scriptErr := s.run(sctx, sess)
	halt.Done()
	_ = sess.Close()

	s.shutdown()
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("proxy: ingress: %w", err)
	}

	if !cleanEnd(scriptErr) {
		return scriptErr
	}
	return nil
}

// shutdown stops the ingress, giving in-flight handlers a moment to finish and
// then forcing them: a held stream would otherwise keep a graceful shutdown
// waiting for a response charpy is deliberately withholding.
func (s *Script) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		_ = s.srv.Close()
	}
}

func cleanEnd(err error) bool {
	return err == nil ||
		errors.Is(err, scenario.ErrScriptDone) ||
		errors.Is(err, scenario.ErrWithdrawn) ||
		errors.Is(err, scenario.ErrDeadline) ||
		errors.Is(err, scenario.ErrStimulusInterrupted)
}
