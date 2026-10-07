// Package gateway drives a gateway under test with charpy on both sides of
// it: charpy's own peer as the gateway's client, charpy's own reference
// servers as its upstreams, and a proxy on every hop, all on one run.
//
//	peer ─► downstream proxy ─► GATEWAY ─► upstream proxy u0 ─► reference server u0
//	                                    └► upstream proxy u1 ─► reference server u1
//
// charpy spawns the gateway, one process per case, with its upstream URLs
// filled into the command line, so every case meets a gateway that has
// learned nothing from an earlier one -- gateways cache what their upstreams
// declared. Attaching to a gateway already running is a different case, with
// different limits, and is not built yet.
//
// Each upstream is its own reference server behind its own proxy, on its own
// listener, so the gateway sees two real endpoints. The alternative -- one
// upstream proxy routing to both servers by URL path -- was rejected: it
// needs the proxy to route and rewrite paths, the transcript records no path,
// and one listener with two paths is a configuration fewer gateways meet in
// production than two hosts. It may come back if a gateway turns out to
// address its upstreams by path under one host; see open-problems.md.
//
// The two upstreams serve tools under different prefixes (u0_, u1_), so which
// upstream received a call is read off the call. That is a kludge, and an open
// problem: real upstreams do not prefix their tools, and a gateway that
// rewrites names would see charpy's choice, not its own.
//
// Frames are joined across the gateway through the run's ledger: charpy
// stamps a trace on every request it originates (ADR-005), a request the
// gateway forwards with the trace intact joins as traced, one it forwards
// without joins at best as inferred by content, and an answer takes its
// request's join. No gateway verdict may rest on an inferred join
// (transcript.md section 4).
//
// The script and the recovery probe repeat what proxy.Script does on
// purpose, and the questions after a fault already differ: a server is asked
// one, a gateway three (ADR-015).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
	"github.com/serverplumber/charpy/internal/transcript"
)

// DefaultTimeout bounds a run whose gateway has stopped answering, as the
// other scripted drivers do.
const DefaultTimeout = 30 * time.Second

// DefaultTool is the tool the script calls when none is named: upstream 0's
// typed tool, with a declared outputSchema for a schema_violation to break.
// It is charpy's own name for charpy's own tool, not one read off the
// gateway's listing.
const DefaultTool = "u0_add_numbers"

// Options configures one case against one spawned gateway.
type Options struct {
	// Command starts the gateway. Every {upstreamN} in it is replaced by the
	// URL of charpy's Nth upstream; the placeholders it names, numbered from
	// 0 without gaps, are the upstreams the run serves.
	Command []string
	// Env is the gateway's environment. Nil inherits charpy's.
	Env []string
	// SubjectURL is where the gateway serves its clients once it is up.
	SubjectURL string
	// Errs receives the gateway's stdout and stderr. Nil discards them.
	Errs io.Writer

	// Case is the one case armed for this run (ADR-012).
	Case interpose.Case
	// Era is the revision charpy's peers speak, on both faces.
	Era revision.Revision
	// Stimulus shapes the script; an empty Tool takes DefaultTool.
	Stimulus scenario.Options
	// Timeout bounds the gateway's start and the script each. Zero takes
	// DefaultTimeout.
	Timeout time.Duration

	Transcript *transcript.Writer
	Sched      clock.Sched
	Wall       clock.Wall
	Ledger     *interpose.Ledger
	RunSeed    string
}

// Driver is one case against one spawned gateway.
type Driver struct {
	o      Options
	run    *exchange.Run
	cases  []interpose.Case
	life   *exchange.Conn
	down   *proxy.Proxy
	ups    []*upstream
	script scenario.Scenario

	servers []*http.Server
	served  chan error
	downURL string
	// trace stamps charpy's client's requests; each upstream server has a
	// stream of its own.
	trace func() string
}

// upstream is one of charpy's upstreams: a reference server and its proxy.
// The proxy is built once the server listens, since it needs the address.
type upstream struct {
	name    string // u0, u1, ...
	ref     http.Handler
	schemas map[string]json.RawMessage
	proxy   *proxy.Proxy
	url     string // the proxy's, which the gateway is given
}

var placeholder = regexp.MustCompile(`\{upstream(\d+)\}`)

// upstreamsNamed is how many upstreams the command names: placeholders
// numbered from 0 without gaps.
func upstreamsNamed(command []string) (int, error) {
	seen := map[int]bool{}
	for _, arg := range command {
		for _, m := range placeholder.FindAllStringSubmatch(arg, -1) {
			n, _ := strconv.Atoi(m[1])
			seen[n] = true
		}
	}
	if len(seen) == 0 {
		return 0, errors.New("gateway: the command names no {upstream0}; the gateway would never reach charpy")
	}
	for i := range len(seen) {
		if !seen[i] {
			return 0, fmt.Errorf("gateway: the command names upstreams out of order; {upstream%d} is missing", i)
		}
	}
	return len(seen), nil
}

// New prepares a run. Nothing binds, spawns or connects until Run.
func New(o Options) (*Driver, error) {
	if len(o.Command) == 0 {
		return nil, errors.New("gateway: no command to start the gateway")
	}
	if o.SubjectURL == "" {
		return nil, errors.New("gateway: no subject URL for the gateway's clients")
	}
	if o.Case.ID == "" || o.Case.Fault.Kind == "" {
		return nil, errors.New("gateway: a run needs a case with a fault to arm")
	}
	n, err := upstreamsNamed(o.Command)
	if err != nil {
		return nil, err
	}
	if o.Stimulus.Tool == "" {
		o.Stimulus.Tool = DefaultTool
		// Each call distinct inside values the tool accepts, so a forward
		// that drops the trace still recalls by content to one call rather
		// than guessing among identical ones (interposer.md section 6).
		if o.Stimulus.Arguments == nil {
			o.Stimulus.ArgumentsFor = func(call int) map[string]any {
				return map[string]any{"a": call + 1, "b": 0}
			}
		}
	}

	cases := []interpose.Case{o.Case}
	run := &exchange.Run{
		Ledger: o.Ledger, Transcript: o.Transcript, Cases: cases,
		// The header is the revision the gateway offers its clients; its
		// upstream handshake can finish first.
		Headed: transcript.Downstream,
	}
	d := &Driver{o: o, run: run, cases: cases, trace: interpose.TraceFor(o.RunSeed, o.Case.ID, "client")}
	// The gateway's lifecycle events: not frames, so nothing to join.
	d.life = run.Face(transcript.Downstream, transcript.TransportHTTP, nil).Conn("c0", "s-0", "d-c-0")

	for i := range n {
		name := "u" + strconv.Itoa(i)
		ref, err := peer.ServerHandler(peer.Options{
			Era: o.Era, ToolPrefix: name + "_", Trace: interpose.TraceFor(o.RunSeed, o.Case.ID, name),
		})
		if err != nil {
			return nil, err
		}
		schemas, err := peer.OutputSchemasFor(name + "_")
		if err != nil {
			return nil, err
		}
		d.ups = append(d.ups, &upstream{name: name, ref: ref, schemas: schemas})
	}

	d.down, err = proxy.New(proxy.Options{
		SubjectURL: o.SubjectURL,
		Run:        run, Sched: o.Sched, Wall: o.Wall, Cases: cases, RunSeed: o.RunSeed,
		Face: transcript.Downstream, ConnPrefix: "d-", Join: true,
		// Owned stimulus, as in proxy.Script: a fault that destroys charpy's
		// own request is answered by charpy, so the script can go on.
		AnswerDestroyed: true,
	})
	if err != nil {
		return nil, err
	}

	if err := scenario.Reaches(o.Case.Match, transcript.ClassGateway); err != nil {
		return nil, err
	}
	d.script, err = scenario.Basic(o.Case.Match, o.Stimulus)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// serve binds a listener for h and serves it until shutdown.
func (d *Driver) serve(h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: h}
	d.servers = append(d.servers, srv)
	go func() { d.served <- srv.Serve(ln) }()
	return "http://" + ln.Addr().String() + "/", nil
}

// Run stands charpy up on both sides, starts the gateway, plays the script
// through it, and stops everything.
func (d *Driver) Run(ctx context.Context) error {
	d.served = make(chan error, 2+3*len(d.ups))
	defer d.shutdown()

	// Upstreams first: the gateway may dial them as it starts.
	for _, u := range d.ups {
		refURL, err := d.serve(u.ref)
		if err != nil {
			return fmt.Errorf("gateway: reference server %s: %w", u.name, err)
		}
		u.proxy, err = proxy.New(proxy.Options{
			SubjectURL: refURL,
			Run:        d.run, Sched: d.o.Sched, Wall: d.o.Wall, Cases: d.cases, RunSeed: d.o.RunSeed,
			Face: transcript.Upstream, ConnPrefix: u.name + "-", Join: true,
			// charpy serves here, so it knows the declarations a
			// schema_violation against the declared output breaks.
			OutputSchemas: u.schemas,
		})
		if err != nil {
			return err
		}
		if u.url, err = d.serve(u.proxy); err != nil {
			return fmt.Errorf("gateway: upstream proxy %s: %w", u.name, err)
		}
		// Which connections are which upstream's, stated once: the proxy
		// names them with the upstream's prefix.
		d.run.Face(transcript.Upstream, transcript.TransportHTTP, nil).Conn("c0", "s-0", u.name+"-c-0").
			Event(transcript.ConnOpen, map[string]any{
				"upstream": u.name, "url": u.url, "conn_prefix": u.name + "-", "tool_prefix": u.name + "_",
			})
	}
	var err error
	if d.downURL, err = d.serve(d.down); err != nil {
		return fmt.Errorf("gateway: downstream proxy: %w", err)
	}

	timeout := d.o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	// The gateway, killed when the run is over: a context of its own, so the
	// script's ending is not the subject's.
	subjCtx, kill := context.WithCancel(context.Background())
	defer kill()
	cmd, exited, err := d.spawn(subjCtx)
	if err != nil {
		return err
	}
	defer d.reap(cmd, exited, kill)

	if err := d.ready(ctx, exited, timeout); err != nil {
		return err
	}

	sctx, halt := scenario.Interruptible(ctx)
	expiry := time.AfterFunc(timeout, halt.Expired)
	defer expiry.Stop()

	sess, err := peer.DialHTTP(sctx, d.downURL, peer.Options{Era: d.o.Era, Trace: d.trace})
	if err != nil {
		halt.Done()
		// A gateway that cannot complete a handshake under a fault is the
		// fault working; the transcript holds what happened.
		if cleanEnd(err) || sctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("gateway: %w", err)
	}

	scriptErr := d.script(sctx, sess)

	// Three questions after a fault on either face (ADR-015). A script that
	// ran out of time still leaves them worth asking here: they run under the
	// run's context with deadlines of their own, and a gateway that hung its
	// client's call is the reaction they exist to tell apart from a dead one.
	askable := scenario.Askable(sctx, scriptErr) ||
		errors.Is(scenario.Why(sctx), scenario.ErrDeadline) && ctx.Err() == nil
	if askable && d.anyApplied() {
		d.questions(ctx, sess, timeout)
	}
	if d.o.Case.LivenessWithinMS > 0 && d.anyApplied() {
		d.livenessProbe()
	}

	halt.Done()
	_ = sess.Close()

	if !cleanEnd(scriptErr) {
		return scriptErr
	}
	return nil
}

// questions asks the gateway three things after a fault, on the session the
// script used (ADR-015). A ping cannot ask about an upstream -- it is
// hop-by-hop, and a gateway answers it itself -- so only a call that has to
// travel can:
//
//   - ping: is the gateway itself alive?
//   - a call through the upstream the fault reached: does that path still
//     work -- answered, an error, or a hang?
//   - a call through another upstream: did the damage stay where it was put?
//
// The upstream a fault reached is the one whose proxy applied it; a fault on
// the downstream face reached no upstream, and the calls go through u0 and
// u1 as they come. Each question has a deadline of its own, so a path that
// hangs does not keep the healthy one from being asked; and each is kept out
// of the matcher (exchange.Conn.Ask). The gateway's forwarded copy upstream
// cannot be faulted either: a case fires once, and it already has.
func (d *Driver) questions(ctx context.Context, sess *mcp.ClientSession, wait time.Duration) {
	path, healthy := d.ups[0].name, ""
	for _, u := range d.ups {
		if u.proxy != nil && u.proxy.Askable() {
			path = u.name
			break
		}
	}
	for _, u := range d.ups {
		if u.name != path {
			healthy = u.name
			break
		}
	}

	// The transcript holds each answer, or its absence. The error is noted
	// too, because a question can fail without crossing at all -- a session
	// charpy's client already gave up on fails it locally -- and then the
	// note is the only record of why it was not asked.
	ask := func(name string, q func(context.Context) error) {
		qctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		withdraw := d.down.Ask(sess.ID(), transcript.C2S)
		defer withdraw()
		if err := q(qctx); err != nil {
			d.down.QuestionFailed(sess.ID(), name, err)
		}
	}
	call := func(upstream string) func(context.Context) error {
		return func(c context.Context) error {
			_, err := sess.CallTool(c, &mcp.CallToolParams{Name: upstream + "_echo", Arguments: map[string]any{}})
			return err
		}
	}
	ask("ping", func(c context.Context) error { return sess.Ping(c, nil) })
	ask("call through "+path, call(path))
	if healthy != "" {
		ask("call through "+healthy, call(healthy))
	}
}

// anyApplied reports whether a fault acted on either face.
func (d *Driver) anyApplied() bool {
	if d.down.Askable() {
		return true
	}
	return slices.ContainsFunc(d.ups, func(u *upstream) bool { return u.proxy != nil && u.proxy.Askable() })
}

// spawn starts the gateway with charpy's upstream URLs in its command line.
func (d *Driver) spawn(ctx context.Context) (*exec.Cmd, chan error, error) {
	urls := map[string]string{}
	for _, u := range d.ups {
		urls[u.name] = u.url
	}
	args := make([]string, len(d.o.Command))
	for i, a := range d.o.Command {
		args[i] = placeholder.ReplaceAllStringFunc(a, func(m string) string {
			return urls["u"+placeholder.FindStringSubmatch(m)[1]]
		})
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = d.o.Env
	if d.o.Errs != nil {
		cmd.Stdout, cmd.Stderr = d.o.Errs, d.o.Errs
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("gateway: starting %s: %w", args[0], err)
	}
	d.life.Event(transcript.ConnOpen, map[string]any{"command": args[0], "subject_url": d.o.SubjectURL})

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited, nil
}

// ready waits for the gateway to answer HTTP at its subject URL: any status
// will do, since a GET on an MCP endpoint is allowed to refuse. A gateway that
// exits first, or never answers, never started.
func (d *Driver) ready(ctx context.Context, exited chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case err := <-exited:
			exited <- err // reap reads it again
			return fmt.Errorf("gateway: exited before serving %s: %v", d.o.SubjectURL, err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.o.SubjectURL, nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gateway: nothing answered at %s within %s", d.o.SubjectURL, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reap stops the gateway and records how it ended. A gateway charpy killed
// at the end and one that died during the run are different events, and the
// second is a finding (see the stdio shim's stop).
func (d *Driver) reap(cmd *exec.Cmd, exited chan error, kill context.CancelFunc) {
	var killed bool
	select {
	case <-exited:
	default:
		killed = true
		kill()
		<-exited
	}
	st := cmd.ProcessState
	detail := map[string]any{"exit_code": st.ExitCode(), "killed_by_charpy": killed}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		detail["signal"] = ws.Signal().String()
	}
	d.life.Event(transcript.SubjectExit, detail)
}

// livenessProbe opens a fresh session through the gateway and asks whether it
// is serving again, as proxy.Script does for a server.
func (d *Driver) livenessProbe() {
	budget := time.Duration(d.o.Case.LivenessWithinMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	method := scenario.ProbeMethod(d.o.Era)
	start := time.Now()
	sess, err := peer.DialHTTP(ctx, d.downURL, peer.Options{Era: d.o.Era, Trace: d.trace})
	if err != nil {
		d.down.RecordProbe(method, scenario.Classify(ctx, err), time.Since(start).Nanoseconds())
		return
	}
	defer sess.Close()
	res := scenario.Probe(ctx, sess, d.o.Era)
	d.down.RecordProbe(res.Method, res.Outcome, time.Since(start).Nanoseconds())
}

// shutdown stops every listener charpy stood up, forcing what a held stream
// keeps open.
func (d *Driver) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, srv := range d.servers {
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
	}
}

func cleanEnd(err error) bool {
	return err == nil ||
		errors.Is(err, scenario.ErrScriptDone) ||
		errors.Is(err, scenario.ErrWithdrawn) ||
		errors.Is(err, scenario.ErrDeadline) ||
		errors.Is(err, scenario.ErrStimulusInterrupted)
}
