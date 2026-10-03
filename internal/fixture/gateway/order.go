package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The SDK client settles a call the moment its answer is read, but queues the
// requests and notifications read ahead of it and handles them later, on
// another goroutine (go-sdk #1337). So a call can return before the progress
// and log messages the upstream sent during it have been forwarded -- and
// once the gateway has answered, there is no request left to forward them on.
// Progress after an answer is forbidden outright; a log message after one
// misses the stream its client is reading. Conformance's logging scenario
// lost one about one run in seven that way.
//
// So the gateway waits for the queue before it answers: a barrier. The
// queue is first in, first out, and handles one message at a time, so the
// barrier is a message of the gateway's own. Each link's RoundTripper puts a
// marker notification into the event stream just ahead of every response,
// and a receiving middleware takes it back out. When a call returns, every
// marker put so far -- its own included -- is waited for, and once the
// marker ahead of its response has been handled, so has everything read
// before it.
//
// Counting messages instead, and waiting for as many to be handled, cannot be
// made exact: the SDK rejects an unknown method, a misplaced id or params it
// cannot parse before any middleware sees it, and every frame of that kind --
// the kind charpy injects -- would leave the count short and stall each later
// answer for the whole bound. The marker is well formed by construction, so
// it always arrives.
//
// The marker exists only between the gateway's HTTP client and its SDK
// client. Neither face's wire carries it.

// barrierLogger names the marker, a notifications/message no upstream sends.
const barrierLogger = "charpy-fixture-gateway/barrier"

// barrierWait bounds the barrier. The marker is queued before the call
// returns, so a working gateway never reaches it; it keeps one whose marker
// went missing from also never answering.
const barrierWait = 2 * time.Second

// barrier is one link's: markers put ahead of responses, and markers handled.
type barrier struct {
	base http.RoundTripper

	mu   sync.Mutex
	put  int64
	seen int64
	bump chan struct{} // closed and replaced when seen grows
}

func newBarrier(base http.RoundTripper) *barrier {
	if base == nil {
		base = http.DefaultTransport
	}
	return &barrier{base: base, bump: make(chan struct{})}
}

func (b *barrier) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	// Only an event stream can carry anything ahead of a response; a JSON
	// body is the response alone.
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		resp.Body = &markedStream{ReadCloser: resp.Body, b: b}
	}
	return resp, nil
}

// middleware takes the markers back out, counting them.
func (b *barrier) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if p, ok := req.GetParams().(*mcp.LoggingMessageParams); ok && method == "notifications/message" && p.Logger == barrierLogger {
			b.mu.Lock()
			b.seen++
			close(b.bump)
			b.bump = make(chan struct{})
			b.mu.Unlock()
			return nil, nil
		}
		return next(ctx, method, req)
	}
}

// wait returns once every marker put so far has been handled.
func (b *barrier) wait(ctx context.Context, log func(msg string, args ...any)) {
	b.mu.Lock()
	want := b.put
	b.mu.Unlock()
	timeout := time.NewTimer(barrierWait)
	defer timeout.Stop()
	for {
		b.mu.Lock()
		seen, bump := b.seen, b.bump
		b.mu.Unlock()
		if seen >= want {
			return
		}
		select {
		case <-bump:
		case <-ctx.Done():
			return
		case <-timeout.C:
			log("barrier marker not handled", "put", want, "seen", seen)
			return
		}
	}
}

func (b *barrier) marker() []byte {
	b.mu.Lock()
	b.put++
	n := b.put
	b.mu.Unlock()
	return fmt.Appendf(nil,
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"debug\",\"logger\":%q,\"data\":%d}}\n\n",
		barrierLogger, n)
}

// markedStream passes an event stream through event by event, putting a
// marker ahead of each response. A partial event is held until it completes;
// at the end of the stream whatever is held is passed on as it stands, so a
// stream cut mid-event reaches the SDK cut in the same place.
type markedStream struct {
	io.ReadCloser
	b   *barrier
	buf []byte // the underlying stream's reads land here
	in  []byte // read, not yet a whole event
	out []byte // ready for the reader
	err error  // the underlying stream's end, once reached
}

func (m *markedStream) Read(p []byte) (int, error) {
	if m.buf == nil {
		m.buf = make([]byte, 32<<10)
	}
	for len(m.out) == 0 && m.err == nil {
		n, err := m.ReadCloser.Read(m.buf)
		m.in = append(m.in, m.buf[:n]...)
		m.events()
		if err != nil {
			m.out = append(m.out, m.in...)
			m.in = nil
			m.err = err
		}
	}
	if len(m.out) > 0 {
		n := copy(p, m.out)
		m.out = m.out[n:]
		return n, nil
	}
	return 0, m.err
}

// events moves every complete event from in to out, marked if it is a
// response.
func (m *markedStream) events() {
	for {
		end, size := eventEnd(m.in)
		if end < 0 {
			return
		}
		event := m.in[:end+size]
		if isResponse(event) {
			m.out = append(m.out, m.b.marker()...)
		}
		m.out = append(m.out, event...)
		m.in = m.in[end+size:]
	}
}

// eventEnd finds the blank line that ends the first event: its offset, and
// the length of the line break pair, or -1.
func eventEnd(b []byte) (int, int) {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf < 0 || (lf >= 0 && lf < crlf):
		return lf, 2
	default:
		return crlf, 4
	}
}

// isResponse reports whether an event's data is a JSON-RPC response: an id
// and no method.
func isResponse(event []byte) bool {
	var data bytes.Buffer
	for line := range bytes.Lines(event) {
		line = bytes.TrimRight(line, "\r\n")
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data.Write(bytes.TrimPrefix(rest, []byte(" ")))
			data.WriteByte('\n')
		}
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal(data.Bytes(), &m) == nil && len(m.ID) > 0 && m.Method == ""
}

// tokenKey keys a progress token. A token is a string or a number, and the two
// must not collide -- "7" and 7 are different tokens -- while a number must
// key the same however a decoder typed it.
func tokenKey(t any) string {
	if s, ok := t.(string); ok {
		return "s:" + s
	}
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Sprintf("?:%v", t)
	}
	return "n:" + string(b)
}
