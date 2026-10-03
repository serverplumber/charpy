package gateway

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// upstream is one upstream server as one downstream session sees it: the link
// to it, the calls in flight on that link, and what the server declared.
type upstream struct {
	endpoint string
	idle     time.Duration

	// dialing serializes (re)connecting, so concurrent callers that find the
	// link down open one session between them, not one each.
	dialing sync.Mutex

	mu sync.Mutex
	// link is the live session, nil while there is none. known is the last
	// capabilities a session declared, kept while the link is down so the
	// routing that depends on them still has an answer.
	link  *link
	known *mcp.ServerCapabilities
	// calls are the downstream requests in flight to this upstream, newest
	// last. See [upstream.start] and [upstream.current].
	calls []*call
	// asking counts the upstream's requests the downstream client has yet to
	// answer; while there are any, the upstream is waiting, not idle, and a
	// nested call may be admitted (see [upstream.start]).
	asking int
	// freed is closed and replaced whenever a waiting call might be
	// admitted.
	freed chan struct{}

	// What the upstream declared, held under the session's lock.
	tools, prompts, resources, templates []string
}

// link is one session on an upstream. A failure outside the protocol ends it,
// and the next call opens another; nothing of one link's ordering carries
// over to the next (see [barrier]).
type link struct {
	cs      *mcp.ClientSession
	caps    *mcp.ServerCapabilities
	barrier *barrier
}

func newUpstream(endpoint string, idle time.Duration) *upstream {
	return &upstream{endpoint: endpoint, idle: idle, freed: make(chan struct{}), known: &mcp.ServerCapabilities{}}
}

// live is the current link, or nil.
func (u *upstream) live() *link {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.link
}

// caps is what the upstream last declared.
func (u *upstream) caps() *mcp.ServerCapabilities {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.known
}

func (u *upstream) set(l *link) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.link, u.known = l, l.caps
}

// lose ends l, if it is still the current link. Closing tells the upstream the
// session is over rather than leaving it to time it out.
func (u *upstream) lose(l *link) {
	u.mu.Lock()
	current := u.link == l
	if current {
		u.link = nil
	}
	u.mu.Unlock()
	if current {
		_ = l.cs.Close()
	}
}

// Calls are admitted one at a time per upstream. The upstream session does
// not say which of its requests a message it sends relates to, so with two
// calls in flight the gateway could only guess which caller a log line or a
// sampling request belongs to; with one, it is a fact. A downstream session
// maps to one upstream session and charpy's scripts are serial, so the queue
// costs nothing in practice.
//
// The exception is a call made while the upstream is waiting on the
// downstream client: a client answering an upstream's sampling request may
// itself call through the gateway, and making that wait for the call that is
// waiting on it would deadlock. It is admitted, and is the newest call, which
// is where what the upstream sends next most likely belongs.

// start admits a downstream request to u and arms its idle deadline.
func (u *upstream) start(ctx context.Context) (*call, error) {
	for {
		u.mu.Lock()
		if len(u.calls) == 0 || u.asking > 0 {
			cctx, cancel := context.WithCancelCause(ctx)
			c := &call{ctx: cctx, cancel: cancel}
			c.timer = time.AfterFunc(u.idle, func() { u.expire(c) })
			u.calls = append(u.calls, c)
			u.mu.Unlock()
			return c, nil
		}
		wait := u.freed
		u.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// finish ends a call, its deadline with it, and admits the next.
func (u *upstream) finish(c *call) {
	c.timer.Stop()
	c.cancel(nil)
	u.mu.Lock()
	defer u.mu.Unlock()
	if i := slices.Index(u.calls, c); i >= 0 {
		u.calls = slices.Delete(u.calls, i, i+1)
	}
	u.free()
}

// free wakes the calls waiting for admission. Callers hold u.mu.
func (u *upstream) free() {
	close(u.freed)
	u.freed = make(chan struct{})
}

// current is the context an upstream-originated message is sent downstream
// under. Its value is what the SDK reads to put the message on the stream of
// a request, so it is the call in flight to this upstream -- the newest, if a
// nested one was admitted -- or a bare context, the standalone stream, when
// there is none.
func (u *upstream) current() context.Context {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := len(u.calls); n > 0 {
		return u.calls[n-1].ctx
	}
	return context.Background()
}

// link returns u's live link, opening one if there is none. A reopened link
// is brought back to where the downstream session left the last one: lists
// re-read, the logging level and subscriptions sent again. An upstream that
// lost its session lost all three with it.
func (s *session) link(ctx context.Context, u *upstream) (*link, error) {
	u.dialing.Lock()
	defer u.dialing.Unlock()
	if l := u.live(); l != nil {
		return l, nil
	}
	l, err := s.dial(ctx, u)
	if err != nil {
		return nil, err
	}
	u.set(l)
	if err := s.restore(ctx, u, l); err != nil {
		u.lose(l)
		return nil, err
	}
	return l, nil
}

// forward runs a downstream request against its owning upstream: admitted,
// under the idle deadline, on a live link, and answered only once what the
// upstream sent during it has been passed on.
func forward[R any](s *session, ctx context.Context, u *upstream, do func(context.Context, *mcp.ClientSession) (R, error)) (R, error) {
	var zero R
	c, err := u.start(ctx)
	if err != nil {
		return zero, err
	}
	defer u.finish(c)
	l, err := s.link(c.ctx, u)
	if err != nil {
		return zero, s.answer(c.ctx, u, nil, err)
	}
	r, err := do(c.ctx, l.cs)
	// Still in flight while the barrier waits, so what the upstream sent
	// during the call is routed onto the call's own stream.
	l.barrier.wait(c.ctx, s.o.Logger.Warn)
	return r, s.answer(c.ctx, u, l, err)
}

// answer is the error a downstream client gets for a failed upstream call.
//
// An upstream's own JSON-RPC error is passed on, code intact: the client is
// owed the error the server sent. Anything else -- a refused connection, a
// stream cut short, an upstream gone quiet -- is the gateway's failure, and
// its text is the gateway's to write. Go's own errors name the upstream's URL
// and the SDK's name its session, and a gateway that put them in an answer
// would be handing its callers its internal topology. The detail goes to the
// gateway's log instead.
//
// A failure outside the protocol also ends the link, so the next call opens a
// fresh session rather than failing on a dead one forever. A call that merely
// went quiet does not: a slow tool is no reason to drop a session.
func (s *session) answer(ctx context.Context, u *upstream, l *link, err error) error {
	if err == nil {
		return nil
	}
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) && !slices.Contains(sdkLocal, rpc.Code) {
		return rpc
	}
	if errors.Is(context.Cause(ctx), errIdle) {
		s.o.Logger.Warn("upstream idle", "upstream", u.endpoint, "after", u.idle)
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "gateway: upstream timed out"}
	}
	if ctx.Err() != nil {
		// The downstream request ended first; nobody is waiting for this.
		return ctx.Err()
	}
	s.o.Logger.Warn("upstream call failed", "upstream", u.endpoint, "err", err)
	if l != nil {
		u.lose(l)
	}
	return errUnavailable
}

// sdkLocal are the codes the SDK's JSON-RPC layer raises itself, about its own
// connection -- unknown, client closing, server closing, rejected by
// transport -- in the same type as an error a server sent. MCP gives none of
// them a meaning, so one is taken as the SDK's account of a failed upstream,
// not the upstream's answer; an upstream that really sent one is reported as
// unavailable, which is no loss.
var sdkLocal = []int64{-32001, -32003, -32004, -32005}

// errUnavailable is what a downstream client is told when an upstream cannot
// be reached or fails outside the protocol.
var errUnavailable = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "gateway: upstream unavailable"}
