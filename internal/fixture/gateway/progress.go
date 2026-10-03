package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// The SDK client settles a call the moment its answer is read, but queues the
// notifications read ahead of it and runs their handlers later, on another
// goroutine. So the call can return before the progress an upstream sent
// during it has been forwarded -- and progress forwarded after the gateway's
// own answer is progress for a finished request, which the spec forbids and
// charpy would rightly report. Holding a fault's release a few milliseconds
// is enough to make it happen, so the fixture cannot leave it to timing.
//
// Nothing in the SDK's API says how many notifications preceded an answer, so
// the count is taken where the bytes are: every POST carrying a tools/call
// with a progress token has its event stream read through progressWatch,
// which counts that token's progress ahead of the answer. The call then waits
// until that many have been forwarded.

// progressWatch is the RoundTripper of one upstream session.
type progressWatch struct {
	base http.RoundTripper

	mu    sync.Mutex
	ahead map[string]int // token key -> progress read before the answer
}

func newProgressWatch(base http.RoundTripper) *progressWatch {
	if base == nil {
		base = http.DefaultTransport
	}
	return &progressWatch{base: base, ahead: map[string]int{}}
}

// Ahead is how many progress notifications for the token were read ahead of
// its call's answer, once the answer has been read.
func (w *progressWatch) Ahead(key string) (int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, ok := w.ahead[key]
	return n, ok
}

// Forget drops a finished call's count.
func (w *progressWatch) Forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.ahead, key)
}

func (w *progressWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	key, ok := w.progressCall(req)
	resp, err := w.base.RoundTrip(req)
	if err != nil || !ok {
		return resp, err
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		resp.Body = &eventCounter{ReadCloser: resp.Body, w: w, key: key}
	}
	return resp, nil
}

// progressCall reports the token of a tools/call request carrying one,
// leaving the body readable for the real transport.
func (w *progressWatch) progressCall(req *http.Request) (string, bool) {
	if req.Method != http.MethodPost || req.Body == nil {
		return "", false
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	if err != nil {
		return "", false
	}
	var call struct {
		Method string `json:"method"`
		Params struct {
			Meta struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &call) != nil || call.Method != "tools/call" || call.Params.Meta.ProgressToken == nil {
		return "", false
	}
	return tokenKey(call.Params.Meta.ProgressToken), true
}

// eventCounter passes a call's event stream through unchanged, counting its
// token's progress until the answer goes by. Events are counted as their
// terminating blank line is read, which is before the SDK, reading the same
// bytes after this returns them, can act on the answer.
type eventCounter struct {
	io.ReadCloser
	w    *progressWatch
	key  string
	line []byte
	data strings.Builder
	n    int
	done bool
}

func (e *eventCounter) Read(p []byte) (int, error) {
	n, err := e.ReadCloser.Read(p)
	if !e.done {
		e.scan(p[:n])
	}
	return n, err
}

func (e *eventCounter) scan(b []byte) {
	for len(b) > 0 && !e.done {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			e.line = append(e.line, b...)
			return
		}
		e.line = append(e.line, b[:i]...)
		b = b[i+1:]
		line := strings.TrimSuffix(string(e.line), "\r")
		e.line = e.line[:0]
		switch {
		case line == "":
			e.dispatch()
		case strings.HasPrefix(line, "data:"):
			e.data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			e.data.WriteByte('\n')
		}
	}
}

func (e *eventCounter) dispatch() {
	data := e.data.String()
	e.data.Reset()
	if data == "" {
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProgressToken any `json:"progressToken"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(data), &m) != nil {
		return
	}
	switch {
	case m.Method == "notifications/progress" && m.Params.ProgressToken != nil && tokenKey(m.Params.ProgressToken) == e.key:
		e.n++
	case m.Method == "" && len(m.ID) > 0:
		e.done = true
		e.w.mu.Lock()
		e.w.ahead[e.key] = e.n
		e.w.mu.Unlock()
	}
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
