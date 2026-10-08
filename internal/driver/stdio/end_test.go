package stdio_test

import (
	"errors"
	"io"
	"testing"

	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// closes returns the stream_close reasons a run recorded, by direction of the
// relay that recorded them: the c2s relay's closes toward the subject, the
// s2c relay's toward the peer.
func closeReasons(r run) []string {
	var out []string
	for _, e := range r.events(string(transcript.StreamClose)) {
		out = append(out, e["detail"].(map[string]any)["reason"].(string))
	}
	return out
}

func count(xs []string, x string) int {
	n := 0
	for _, s := range xs {
		if s == x {
			n++
		}
	}
	return n
}

// A relay that ends on a read error says so, with the error's text, rather
// than ending as though its sender had simply stopped.
func TestShimRecordsAReadError(t *testing.T) {
	in, w := io.Pipe()
	go func() {
		_, _ = io.WriteString(w, initialize+"\n")
		_ = w.CloseWithError(errors.New("boom"))
	}()
	r := driveWith(t, func(o *stdio.Options) { o.In = in })

	var found bool
	for _, e := range r.events(string(transcript.StreamClose)) {
		d := e["detail"].(map[string]any)
		if d["reason"] == string(transcript.CloseError) {
			found = true
			if d["error"] != "boom" {
				t.Errorf("error = %v, want the read error's text", d["error"])
			}
		}
	}
	if !found {
		t.Fatalf("no stream_close with reason error; closes: %v", closeReasons(r))
	}
}

// Each side's own end of stream is recorded as that side's close, once.
func TestShimRecordsEachSidesEnd(t *testing.T) {
	r := drive(t, nil, initialize, toolsCall(2))
	got := closeReasons(r)
	if count(got, string(transcript.PeerClose)) != 1 || count(got, string(transcript.SubjectClose)) != 1 {
		t.Errorf("closes = %v, want one peer_close and one subject_close", got)
	}
}

// charpy closing its peer's pipe at the end of a scripted run is charpy's
// teardown. The read error it can cause is not a broken stream, and no run
// that ended normally records one. Run several times: which of the session's
// own close and charpy's pipe close reaches the relay first is a race.
func TestAScriptedRunsTeardownIsNotAReadError(t *testing.T) {
	for range 5 {
		r := script(t, quiet(), revision.V20251125)
		if got := closeReasons(r); count(got, string(transcript.CloseError)) != 0 {
			t.Fatalf("closes = %v: charpy's own teardown was recorded as a read error", got)
		}
	}
}
