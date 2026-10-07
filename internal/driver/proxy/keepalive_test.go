package proxy_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// stream collects what a client reads off an SSE response, and whether the
// response ended.
type stream struct {
	mu   sync.Mutex
	body strings.Builder
	err  error
	done bool
}

func (s *stream) read(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		s.mu.Lock()
		s.body.Write(buf[:n])
		if err != nil {
			s.err, s.done = err, true
		}
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *stream) state() (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String(), s.done, s.err
}

// A stall with keep-alives, through the proxy: the subject's answer is cut
// mid-event, and the stream then stays open with comments arriving on the
// wall clock's cadence and no data, so it looks healthy to the client the
// whole time.
func TestAStallKeepsTheStreamAliveWithComments(t *testing.T) {
	era := revision.V20251125
	sched := clock.RealSched()
	wall := clock.NewFixedWall(time.Unix(0, 0))

	var buf strings.Builder
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassServer},
			Clock:   clock.ModeReal,
			Peer:    peer.Describe(era),
		},
		Sched: sched,
		Wall:  wall,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	p, err := proxy.New(proxy.Options{
		SubjectURL: subject(t),
		Transcript: tr,
		Sched:      sched,
		Wall:       wall,
		Ledger:     interpose.NewLedger(sched),
		Face:       transcript.Downstream,
		RunSeed:    "8f2c1a",
		Cases: []interpose.Case{{
			ID:       "stream/truncate-then-stall-alive",
			Citation: "stream/truncate-then-stall-alive@2025-11-25#seed=8f2c1a",
			Match: interpose.Match{
				Method:    interpose.ParseGlob("initialize"),
				Direction: transcript.S2C,
			},
			Fault: interpose.Fault{Kind: "truncate", Params: map[string]any{
				"cut_at": "mid_event", "then": "stall", "keepalive": "comments",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+string(era)+
			`","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an event stream to stall", ct)
	}

	var s stream
	go s.read(resp.Body)
	defer resp.Body.Close()

	// Advance the wall one cadence at a time until three keep-alives have
	// arrived. The stall arms just after the cut is flushed, so an advance
	// that lands before it fires nothing and the next one catches it.
	const want = 3
	comment := string(wire.EncodeComment("").Bytes)
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, done, err := s.state()
		if n := strings.Count(body, comment); n >= want {
			break
		}
		if done {
			t.Fatalf("the stream ended during the stall (%v); client read %q", err, body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d keep-alives arrived; client read %q", want, body)
		}
		wall.Advance(wire.DefaultKeepaliveEvery)
		time.Sleep(10 * time.Millisecond)
	}

	body, done, err := s.state()
	if done {
		t.Errorf("the stream ended after the keep-alives (%v); a stall holds it open", err)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("client saw EOF; read %q", body)
	}
	// No data after the cut: everything past the remnant is comments.
	cut := strings.Index(body, comment)
	if rest := strings.ReplaceAll(body[cut:], comment, ""); rest != "" {
		t.Errorf("data crossed during the stall: %q", rest)
	}
}
