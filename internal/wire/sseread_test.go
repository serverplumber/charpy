package wire_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/wire"
)

// scanAll drains a scanner into its units and the error that stopped it.
func scanAll(t *testing.T, stream string) ([]wire.Encoded, error) {
	t.Helper()
	sc := wire.NewSSEScanner(strings.NewReader(stream))
	var out []wire.Encoded
	for sc.Scan() {
		out = append(out, sc.Unit())
	}
	return out, sc.Err()
}

// The reader is the dual of the writer: a unit's bytes are exactly what
// arrived, so relaying them reproduces the stream.
func TestScannerRelaysVerbatim(t *testing.T) {
	stream := "event: message\nid: 7\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"
	units, err := scanAll(t, stream)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("units: %d, want 1", len(units))
	}
	if string(units[0].Bytes) != stream {
		t.Errorf("relayed bytes differ from the stream:\n got %q\nwant %q", units[0].Bytes, stream)
	}
}

// Body recovers the JSON frame from the data field wherever the field falls,
// because SSE fixes no order and the SDK puts data last.
func TestScannerRecoversTheFrame(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":7,"result":{}}`
	for _, stream := range []string{
		"event: message\nid: 7\ndata: " + frame + "\n\n",
		"data: " + frame + "\nid: 7\nevent: message\n\n",
		"data: " + frame + "\n\n",
	} {
		units, err := scanAll(t, stream)
		if err != nil {
			t.Fatalf("Err: %v", err)
		}
		if len(units) != 1 {
			t.Fatalf("units: %d, want 1 for %q", len(units), stream)
		}
		if got := string(units[0].Body()); got != frame {
			t.Errorf("Body = %q, want %q\nstream %q", got, frame, stream)
		}
	}
}

// A mid_event cut lands inside the data value of the bytes that actually
// arrived, which is the whole reason the reader measures spans rather than
// re-encoding.
func TestAMidEventCutLandsInTheSubjectsBytes(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":7,"result":{"big":"payload"}}`
	stream := "event: message\ndata: " + frame + "\n\n"
	units, _ := scanAll(t, stream)
	if len(units) != 1 {
		t.Fatalf("units: %d", len(units))
	}

	at, err := units[0].Cut(wire.CutMidEvent, wire.CutOptions{})
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	// The cut is strictly inside the frame: some of it delivered, some not.
	dataOff := strings.Index(stream, frame)
	if at <= dataOff || at >= dataOff+len(frame) {
		t.Errorf("cut at %d is not inside the frame [%d,%d)", at, dataOff, dataOff+len(frame))
	}
}

// Comments arrive as their own units so a stalled stream's keep-alives are
// recorded and a mid_comment cut has something to land in.
func TestScannerYieldsCommentsAsUnits(t *testing.T) {
	stream := ": keep-alive\nevent: message\ndata: {}\n\n"
	units, _ := scanAll(t, stream)
	if len(units) != 2 {
		t.Fatalf("units: %d, want 2 (a comment then an event)", len(units))
	}
	if !units[0].IsComment() {
		t.Error("first unit is not a comment")
	}
	if string(units[0].Body()) != "keep-alive" {
		t.Errorf("comment body = %q", units[0].Body())
	}
	if units[1].IsComment() {
		t.Error("second unit should be an event")
	}
}

// A stream cut off mid-event -- no terminating blank line -- still yields what
// arrived. A truncated event is the datum, and dropping it would hide exactly
// what a truncate case exists to produce.
func TestAStreamCutOffMidEventStillYieldsIt(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\"" // no blank line, ends mid-frame
	units, err := scanAll(t, stream)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("units: %d, want the partial one", len(units))
	}
	if string(units[0].Bytes) != stream {
		t.Errorf("partial unit = %q, want %q", units[0].Bytes, stream)
	}
}

// An event with no data field carries no frame -- an id-only cursor bump.
func TestAnEventWithNoDataHasNoBody(t *testing.T) {
	units, _ := scanAll(t, "id: 9\n\n")
	if len(units) != 1 {
		t.Fatalf("units: %d", len(units))
	}
	if units[0].Body() != nil {
		t.Errorf("Body = %q, want nil", units[0].Body())
	}
}
