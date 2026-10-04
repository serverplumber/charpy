package proxy_test

import (
	"context"
	"net/http/httptest"
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

// front stands a proxy on one face of a shared run, in front of its own
// reference server, and returns the proxy's URL.
func front(t *testing.T, run *exchange.Run, sched clock.Sched, face transcript.Face, prefix string) string {
	t.Helper()
	ref, err := peer.ServerHandler(peer.Options{Era: revision.V20251125})
	if err != nil {
		t.Fatal(err)
	}
	refSrv := httptest.NewServer(ref)
	t.Cleanup(refSrv.Close)

	p, err := proxy.New(proxy.Options{
		SubjectURL: refSrv.URL, Run: run, Sched: sched, Face: face, ConnPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A gateway run is several proxies on one run: one downstream, one per
// upstream. They share a ledger and write one header, and two proxies on the
// same face name their connections apart.
func TestProxiesShareOneRun(t *testing.T) {
	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassGateway}, Clock: clock.ModeReal,
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &exchange.Run{Ledger: interpose.NewLedger(sched), Transcript: tr, Headed: transcript.Downstream}

	urls := map[string]string{
		"d-":  front(t, run, sched, transcript.Downstream, "d-"),
		"u0-": front(t, run, sched, transcript.Upstream, "u0-"),
		"u1-": front(t, run, sched, transcript.Upstream, "u1-"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, prefix := range []string{"u0-", "u1-", "d-"} {
		sess, err := peer.DialHTTP(ctx, urls[prefix], peer.Options{Era: revision.V20251125})
		if err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		if err := sess.Ping(ctx, nil); err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		_ = sess.Close()
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := transcript.Read(strings.NewReader(buf.String())) // refuses a missing or repeated header
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range got.Frames() {
		conn := ""
		if f.ConnID != nil {
			conn = *f.ConnID
		}
		prefix := ""
		for p := range urls {
			if strings.HasPrefix(conn, p) {
				prefix = p
			}
		}
		want := transcript.Upstream
		if prefix == "d-" {
			want = transcript.Downstream
		}
		switch {
		case prefix == "":
			t.Errorf("frame %d on connection %q, named by no proxy's prefix", f.Seq, conn)
		case f.Face != want:
			t.Errorf("frame %d on %q is on the %s face, want %s", f.Seq, conn, f.Face, want)
		}
		seen[prefix] = true
	}
	for p := range urls {
		if !seen[p] {
			t.Errorf("no frames from the proxy prefixed %q", p)
		}
	}
}

// A proxy joining a run uses the run's ledger and writer, and refuses others:
// two ledgers in one run would count and resolve each face apart.
func TestAProxyRefusesALedgerNotItsRuns(t *testing.T) {
	sched := clock.RealSched()
	run := &exchange.Run{Ledger: interpose.NewLedger(sched)}
	_, err := proxy.New(proxy.Options{
		SubjectURL: "http://127.0.0.1:1/", Run: run, Sched: sched,
		Ledger: interpose.NewLedger(sched),
	})
	if err == nil {
		t.Error("a proxy joined a run with a ledger of its own")
	}
}
