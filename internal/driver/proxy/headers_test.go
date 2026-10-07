package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/exchange"
	"github.com/serverplumber/charpy/internal/driver/proxy"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// A header sent on two field lines and one sent on one line holding a comma
// are different messages on the wire, and the transcript keeps them apart on
// both directions. A gateway that appends a mirrored header instead of
// replacing it is caught only if this holds. HTTP/1.1 only; the HTTP/2 gap is
// scoped in docs/open-problems.md.
func TestRepeatedHeadersCrossAsSeparateValues(t *testing.T) {
	ref, err := peer.ServerHandler(peer.Options{Era: revision.V20251125})
	if err != nil {
		t.Fatal(err)
	}
	subject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Twice", "a")
		w.Header().Add("X-Twice", "b")
		w.Header().Set("X-Once", "a, b")
		ref.ServeHTTP(w, r)
	}))
	t.Cleanup(subject.Close)

	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassServer}, Clock: clock.ModeReal,
		},
		Sched: sched,
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &exchange.Run{Ledger: interpose.NewLedger(sched), Transcript: tr, Headed: transcript.Downstream}
	p, err := proxy.New(proxy.Options{
		SubjectURL: subject.URL, Run: run, Sched: sched, Wall: clock.NewFixedWall(time.Unix(0, 0)), Face: transcript.Downstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	// Built by hand: no SDK client sends a header twice, and the point is
	// what crosses the wire, not what a client would choose to send.
	req, err := http.NewRequest(http.MethodPost, front.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header["Mcp-Param-Q"] = []string{"a", "b"}
	req.Header["Mcp-Param-R"] = []string{"a, b"}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize through the proxy: %s", resp.Status)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := transcript.Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	var c2s, s2c *transcript.HTTPLine
	for _, f := range got.Frames() {
		if f.HTTP == nil {
			continue
		}
		switch f.Direction {
		case transcript.C2S:
			c2s = f.HTTP
		case transcript.S2C:
			s2c = f.HTTP
		}
	}
	if c2s == nil || s2c == nil {
		t.Fatalf("want a frame each way with http detail; transcript:\n%s", buf.String())
	}

	for _, tc := range []struct {
		side string
		h    *transcript.HTTPLine
		name string
		want []string
	}{
		{"request", c2s, "mcp-param-q", []string{"a", "b"}},
		{"request", c2s, "mcp-param-r", []string{"a, b"}},
		{"response", s2c, "x-twice", []string{"a", "b"}},
		{"response", s2c, "x-once", []string{"a, b"}},
	} {
		if !slices.Equal(tc.h.Headers[tc.name], tc.want) {
			t.Errorf("%s %s = %q, want %q", tc.side, tc.name, tc.h.Headers[tc.name], tc.want)
		}
	}
}
