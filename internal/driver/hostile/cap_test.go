package hostile_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/hostile"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
	"github.com/serverplumber/charpy/internal/wire"
)

// driveRaw runs the hostile driver against a client that is only bytes: feed
// writes what the client sends and closes the pipe however the test wants. It
// fails the test if the run errors, or ends at its deadline rather than when
// the client's side ended.
func driveRaw(t *testing.T, feed func(*io.PipeWriter)) run {
	t.Helper()
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
	go feed(clientWrites)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the run ended at its deadline, not when the client's side ended")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	return run{lines: decode(t, buf.String())}
}

func closesWith(r run, reason transcript.CloseReason) []map[string]any {
	var out []map[string]any
	for _, e := range r.events(string(transcript.StreamClose)) {
		if d := e["detail"].(map[string]any); d["reason"] == string(reason) {
			out = append(out, d)
		}
	}
	return out
}

// A client whose line runs past the read cap is recorded, its stream closed
// for the cap, and the run ends as a client going away ends it -- not with
// charpy failing.
func TestAClientLinePastTheCapIsRecorded(t *testing.T) {
	r := driveRaw(t, func(w *io.PipeWriter) {
		// One line past the cap, which does end, and then the client leaves.
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"`+
			strings.Repeat("x", wire.MaxFrame+1<<20)+`"}}`+"\n")
		_ = w.Close()
	})

	capped := r.events(string(transcript.FrameCapped))
	if len(capped) != 1 || capped[0]["detail"].(map[string]any)["direction"] != "c2s" {
		t.Fatalf("frame_capped events: %v, want one c2s", capped)
	}
	if len(closesWith(r, transcript.FrameCap)) != 1 {
		t.Error("no stream_close with reason frame_cap")
	}
}

// A client stream that breaks on a read error is recorded as one, with the
// error's text, not as the client closing.
func TestAClientReadErrorIsRecorded(t *testing.T) {
	r := driveRaw(t, func(w *io.PipeWriter) { _ = w.CloseWithError(errors.New("boom")) })

	errs := closesWith(r, transcript.CloseError)
	if len(errs) != 1 || errs[0]["error"] != "boom" {
		t.Fatalf("error closes: %v, want one carrying the read error", errs)
	}
	if n := len(closesWith(r, transcript.SubjectClose)); n != 0 {
		t.Errorf("%d subject_close beside the error: a broken stream is not the client closing", n)
	}
}

// A client that leaves is recorded as the subject closing its stream.
func TestAClientThatLeavesIsRecorded(t *testing.T) {
	r := driveRaw(t, func(w *io.PipeWriter) { _ = w.Close() })
	if n := len(closesWith(r, transcript.SubjectClose)); n != 1 {
		t.Errorf("subject_close: %d, want 1", n)
	}
}

// charpy closing its reference server's output at the end of a run is
// charpy's teardown, and no normal run records a read error for it. Several
// runs, because the server's own close and charpy's race.
func TestAHostileRunsTeardownIsNotAReadError(t *testing.T) {
	for range 5 {
		r := drive(t, interpose.Case{
			ID:       "test/never-matches",
			Citation: "test/never-matches#seed=8f2c1a",
			Match:    interpose.Match{Kind: "error", Direction: transcript.S2C},
			Fault:    interpose.Fault{Kind: "malformed_json"},
		}, revision.V20251125, func(t *testing.T, cs *mcp.ClientSession) {
			if _, err := cs.ListTools(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
		})
		if errs := closesWith(r, transcript.CloseError); len(errs) != 0 {
			t.Fatalf("error closes %v: charpy's own teardown was recorded as a read error", errs)
		}
	}
}
