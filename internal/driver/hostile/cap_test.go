package hostile_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// A client whose line runs past the read cap is recorded, its stream closed
// for the cap, and the run ends as a client going away ends it -- not with
// charpy failing.
func TestAClientLinePastTheCapIsRecorded(t *testing.T) {
	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeHostileServer,
			Subject: transcript.Subject{Class: transcript.ClassClient},
			Clock:   clock.ModeReal,
			Peer:    peer.Describe(revision.V20251125),
		},
		Sched: sched,
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// No handshake completes, so nothing else writes the header.
	if err := tr.WriteHeader(transcript.Header{Revision: &transcript.Negotiation{
		Negotiated: revision.V20251125, How: transcript.HowInitialize,
	}}); err != nil {
		t.Fatal(err)
	}

	clientReads, hostileWrites := io.Pipe()
	hostileReads, clientWrites := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, clientReads) }()

	h, err := hostile.New(hostile.Options{
		In: hostileReads, Out: hostileWrites, Era: revision.V20251125,
		Transcript: tr, Sched: sched, Ledger: interpose.NewLedger(sched), RunSeed: "8f2c1a",
	})
	if err != nil {
		t.Fatal(err)
	}

	// One line past the cap, which does end, and then the client leaves.
	go func() {
		_, _ = io.WriteString(clientWrites, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"`+
			strings.Repeat("x", wire.MaxFrame+1<<20)+`"}}`+"\n")
		_ = clientWrites.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the run ended at its deadline, not when the client left")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	r := run{lines: decode(t, buf.String())}
	capped := r.events(string(transcript.FrameCapped))
	if len(capped) != 1 || capped[0]["detail"].(map[string]any)["direction"] != "c2s" {
		t.Fatalf("frame_capped events: %v, want one c2s", capped)
	}
	var closed bool
	for _, e := range r.events(string(transcript.StreamClose)) {
		if e["detail"].(map[string]any)["reason"] == string(transcript.FrameCap) {
			closed = true
		}
	}
	if !closed {
		t.Error("no stream_close with reason frame_cap")
	}
}
