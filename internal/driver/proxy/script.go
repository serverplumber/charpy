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

	// Liveness: after a fault has acted, does the subject serve again within
	// the budget? Only when the case asked (a budget) and a fault actually
	// interrupted the stimulus -- a clean script had nothing to recover from.
	if s.o.Case.LivenessWithinMS > 0 && faultInterrupted(scriptErr) {
		s.livenessProbe()
	}

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

// livenessProbe opens a fresh session to the subject and asks whether it is
// serving again. Fresh, not the scenario's session: after a truncation the
// client's own session may be wedged, and liveness is about the subject
// resuming service, not that session surviving. Dialing is part of the probe --
// a subject that will not accept a new connection has not recovered -- so a
// failed dial is a probe outcome, not an error to return.
func (s *Script) livenessProbe() {
	budget := time.Duration(s.o.Case.LivenessWithinMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	method := scenario.ProbeMethod(s.o.Era)
	start := time.Now()

	sess, err := peer.DialHTTP(ctx, s.Endpoint(), peer.Options{Era: s.o.Era})
	if err != nil {
		s.proxy.RecordProbe(method, scenario.Classify(ctx, err), time.Since(start).Nanoseconds())
		return
	}
	defer sess.Close()

	res := scenario.Probe(ctx, sess, s.o.Era)
	s.proxy.RecordProbe(res.Method, res.Outcome, time.Since(start).Nanoseconds())
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

// faultInterrupted reports whether a fault broke the stimulus -- a call the
// wire cut, or a hold released. A finished script had nothing to recover from,
// so it gets no probe.
func faultInterrupted(err error) bool {
	return errors.Is(err, scenario.ErrStimulusInterrupted) || errors.Is(err, scenario.ErrWithdrawn)
}

func cleanEnd(err error) bool {
	return err == nil ||
		errors.Is(err, scenario.ErrScriptDone) ||
		errors.Is(err, scenario.ErrWithdrawn) ||
		errors.Is(err, scenario.ErrDeadline) ||
		errors.Is(err, scenario.ErrStimulusInterrupted)
}
