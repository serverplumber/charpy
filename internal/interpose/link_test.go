package interpose_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func traced(t *testing.T, method, args, span string) envelope.Message {
	t.Helper()
	params := `{"_meta":{"traceparent":"00-` + traceID + `-` + span + `-01"},` + args + `}`
	m, err := envelope.NewRequest(envelope.NumberID(1), method, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func plain(t *testing.T, method, args string) envelope.Message {
	t.Helper()
	m, err := envelope.NewRequest(envelope.NumberID(1), method, json.RawMessage("{"+args+"}"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Propagated trace context is the only authoritative join for a frame the
// subject forwarded.
func TestTracedJoinIsAuthoritative(t *testing.T) {
	_, l, _ := rig(t)

	sent := traced(t, "tools/call", `"name":"search"`, "00f067aa0ba902b7")
	out, err := l.Originated("c7f1a2", sent, transcript.ViaTraced)
	if err != nil {
		t.Fatal(err)
	}
	if out.CharpyID != "c7f1a2" || out.TraceID != traceID {
		t.Errorf("originated link = %+v", out)
	}

	// The gateway forwards it under a new id, having propagated the trace.
	fwd := traced(t, "tools/call", `"name":"search"`, "00f067aa0ba902b8")
	got := l.LinkFor(fwd)

	if got.Via != transcript.ViaTraced {
		t.Errorf("via = %q, want traced", got.Via)
	}
	if got.CharpyID != "c7f1a2" {
		t.Errorf("charpy_id = %q, want the originating key", got.CharpyID)
	}
}

// The population that forces the inferred regime is gateways that strip
// _meta, so the digest must not depend on it -- nor on the id, which a
// gateway rewrites freely.
func TestInferredJoinSurvivesAStrippedMetaAndARewrittenID(t *testing.T) {
	_, l, _ := rig(t)

	sent := traced(t, "tools/call", `"name":"search","query":"seed-8f2c1a"`, "00f067aa0ba902b7")
	if _, err := l.Originated("c7f1a2", sent, transcript.ViaTraced); err != nil {
		t.Fatal(err)
	}

	// Forwarded with _meta gone, a different id, and the keys re-ordered.
	fwd, err := envelope.NewRequest(envelope.StringID("anon-91"), "tools/call",
		json.RawMessage(`{"query":"seed-8f2c1a","name":"search"}`))
	if err != nil {
		t.Fatal(err)
	}

	got := l.LinkFor(fwd)
	if got.Via != transcript.ViaInferred {
		t.Fatalf("via = %q, want inferred", got.Via)
	}
	if got.CharpyID != "c7f1a2" {
		t.Errorf("charpy_id = %q, want the originating key", got.CharpyID)
	}
	if got.Confidence != interpose.ConfidenceUniqueContent {
		t.Errorf("confidence = %v, want %v", got.Confidence, interpose.ConfidenceUniqueContent)
	}
	if got.Confidence >= 1 {
		t.Error("a content recall claimed authority")
	}
}

// Distinct content is what makes the recall work, so different arguments must
// not join.
func TestInferredJoinDoesNotMatchDifferentContent(t *testing.T) {
	_, l, _ := rig(t)

	if _, err := l.Originated("c7f1a2", plain(t, "tools/call", `"query":"alpha"`), transcript.ViaForwarded); err != nil {
		t.Fatal(err)
	}
	if got := l.LinkFor(plain(t, "tools/call", `"query":"beta"`)); got.Via != transcript.ViaNone {
		t.Errorf("via = %q, want none: different arguments are different calls", got.Via)
	}
	// Nor across methods carrying identical arguments.
	if got := l.LinkFor(plain(t, "tools/list", `"query":"alpha"`)); got.Via != transcript.ViaNone {
		t.Errorf("via = %q, want none: the method is part of the content", got.Via)
	}
}

// Ordering is a tie-breaker, not evidence, and the lower confidence records
// exactly that.
func TestIdenticalContentIsToldApartByOrder(t *testing.T) {
	_, l, _ := rig(t)

	for _, id := range []string{"aaa111", "bbb222"} {
		if _, err := l.Originated(id, plain(t, "tools/call", `"query":"same"`), transcript.ViaForwarded); err != nil {
			t.Fatal(err)
		}
	}

	first := l.LinkFor(plain(t, "tools/call", `"query":"same"`))
	second := l.LinkFor(plain(t, "tools/call", `"query":"same"`))

	if first.CharpyID != "aaa111" || second.CharpyID != "bbb222" {
		t.Errorf("claimed %q then %q, want the oldest first", first.CharpyID, second.CharpyID)
	}
	for _, got := range []transcript.Link{first, second} {
		if got.Confidence != interpose.ConfidenceOrderedContent {
			t.Errorf("confidence = %v, want the lower ordered value", got.Confidence)
		}
	}

	// A third copy of content charpy sent twice is not a join charpy can make.
	if third := l.LinkFor(plain(t, "tools/call", `"query":"same"`)); third.Via != transcript.ViaNone {
		t.Errorf("via = %q, want none: every candidate was already claimed", third.Via)
	}
}

// A subject that drops trace context and changes the payload leaves nothing to
// join on, and none is the correct answer -- an invariant needing correlation
// is skipped rather than evaluated on a guess.
func TestNoJoinIsAnAnswer(t *testing.T) {
	_, l, _ := rig(t)
	if got := l.LinkFor(plain(t, "tools/call", `"query":"never-sent"`)); got.Via != transcript.ViaNone {
		t.Errorf("via = %q, want none", got.Via)
	}
}

// charpy minted the key itself, so recording the origin as a guess would be a
// lie in the other direction.
func TestOriginatedRefusesANonAuthoritativeRegime(t *testing.T) {
	_, l, _ := rig(t)
	for _, via := range []transcript.Via{transcript.ViaInferred, transcript.ViaNone, "sideways"} {
		if _, err := l.Originated("c7f1a2", plain(t, "ping", `"a":1`), via); err == nil {
			t.Errorf("Originated accepted via = %q", via)
		}
	}
}

func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		in   string
		want bool
		why  string
	}{
		{"00-" + traceID + "-00f067aa0ba902b7-01", true, ""},
		{"00-" + traceID + "-00f067aa0ba902b7-00", true, "an unsampled trace is still a trace"},
		{"01-" + traceID + "-00f067aa0ba902b7-01", false, "only version 00 is recognised"},
		{"00-" + traceID + "-00f067aa0ba902b7", false, "too few fields"},
		{"00-tooshort-00f067aa0ba902b7-01", false, "the trace id is a fixed width"},
		{"00-" + traceID + "-short-01", false, "the span id is a fixed width"},
		{"00-zzzz2f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false, "not hex"},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", false, "an all-zero trace id is invalid and would merge everything"},
		{"00-" + traceID + "-0000000000000000-01", false, "an all-zero span id is invalid"},
		{"", false, "empty"},
	}

	for _, tc := range tests {
		got, ok := interpose.ParseTraceparent(tc.in)
		if ok != tc.want {
			t.Errorf("ParseTraceparent(%q) = %v, want %v. %s", tc.in, ok, tc.want, tc.why)
		}
		if ok && got.FormatTraceparent() != tc.in {
			t.Errorf("traceparent did not round trip: %q vs %q", got.FormatTraceparent(), tc.in)
		}
	}
}

func TestContentDigest(t *testing.T) {
	same := plain(t, "tools/call", `{"b":2,"a":1}`[1:len(`{"b":2,"a":1}`)-1])
	reordered := plain(t, "tools/call", `"a":1,"b":2`)
	if interpose.ContentDigest(same) != interpose.ContentDigest(reordered) {
		t.Error("key order changed the digest; a forwarder may re-serialise freely")
	}

	// A frame with nothing a forwarder carries across cannot be content-joined.
	errFrame, err := envelope.NewError(envelope.NumberID(1), -32600, "bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := interpose.ContentDigest(errFrame); got != "" {
		t.Errorf("an error frame produced a digest %q; error bodies repeat across unrelated exchanges", got)
	}
}

// The join tables are the ledger's only run-scoped state, and they are windows
// for the same reason every other table is.
func TestJoinTableIsAWindow(t *testing.T) {
	sched := clock.NewInjected()
	l := interpose.NewLedger(sched, interpose.WithJoinWindow(4))

	args := func(i int) string { return fmt.Sprintf(`"query":"seed-%02d"`, i) }
	for i := range 20 {
		if _, err := l.Originated(fmt.Sprintf("id%04d", i), plain(t, "tools/call", args(i)), transcript.ViaForwarded); err != nil {
			t.Fatal(err)
		}
	}

	if got := l.LinkFor(plain(t, "tools/call", args(0))); got.Via != transcript.ViaNone {
		t.Error("the oldest originated frame is still joinable; the window is not bounding")
	}
	if got := l.LinkFor(plain(t, "tools/call", args(19))); got.Via != transcript.ViaInferred {
		t.Errorf("the newest originated frame was evicted: via = %q", got.Via)
	}
}
