package liveness_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/liveness"
	"github.com/serverplumber/charpy/internal/transcript"
)

func read(t *testing.T, lines ...string) *transcript.Transcript {
	t.Helper()
	got, err := transcript.Read(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return got
}

const header = `{"schema_version":1,"type":"header","run_id":"01JBW3K9F2Q7XN4TCHARPY01","seq":0,` +
	`"t_mono_ns":0,"t_wall":"2026-09-15T00:00:00.000000000Z","charpy_version":"test",` +
	`"seed":"8f2c1a","mode":"proxy","subject":{"class":"server"},` +
	`"revision":{"negotiated":"2025-11-25"},"redaction":"on","clock":"real","fleet":{"clients":1}}`

func probe(seq int, outcome string, elapsedNS int64) string {
	return `{"schema_version":1,"type":"event","run_id":"01JBW3K9F2Q7XN4TCHARPY01","seq":` +
		itoa(seq) + `,"t_mono_ns":1,"t_wall":"2026-09-15T00:00:00.000000001Z",` +
		`"event_kind":"probe","face":"downstream","transport":"http",` +
		`"client_id":"c0","session_id":"s-0","conn_id":"c-0","stream_id":null,"link":null,` +
		`"detail":{"method":"tools/list","outcome":"` + outcome + `","elapsed_mono_ns":` + itoa64(elapsedNS) + `}}`
}

func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A probe that answered is a recovery, and the layer reports it -- reporting
// recovery is the point, not a silence.
func TestAnAnsweredProbeIsRecovery(t *testing.T) {
	rep := liveness.Check(read(t, header, probe(1, "ok", 1_200_000_000)))
	if len(rep.Findings) != 1 {
		t.Fatalf("findings: %d, want 1", len(rep.Findings))
	}
	f := rep.Findings[0]
	if f.Verdict != oracle.Observed {
		t.Errorf("verdict = %s, want OBSERVED", f.Verdict)
	}
	if !strings.Contains(f.Summary, "recovered") {
		t.Errorf("summary does not report recovery: %q", f.Summary)
	}
	if !strings.Contains(f.Detail, "1200ms") {
		t.Errorf("detail does not report the elapsed: %q", f.Detail)
	}
}

// A probe that timed out is a subject that did not recover in the budget.
func TestATimedOutProbeIsNoRecovery(t *testing.T) {
	rep := liveness.Check(read(t, header, probe(1, "timeout", 30_000_000_000)))
	if len(rep.Findings) != 1 {
		t.Fatalf("findings: %d, want 1", len(rep.Findings))
	}
	f := rep.Findings[0]
	// OBSERVED, not MUST: liveness has no generated artifact behind it, so it
	// reports the fact for a human rather than failing the build.
	if f.Verdict != oracle.Observed {
		t.Errorf("verdict = %s, want OBSERVED", f.Verdict)
	}
	if !strings.Contains(f.Summary, "did not recover") {
		t.Errorf("summary does not report the failure: %q", f.Summary)
	}
}

// A run that probed nothing yields no liveness finding.
func TestNoProbesYieldNothing(t *testing.T) {
	if got := liveness.Check(read(t, header)); len(got.Findings) != 0 {
		t.Errorf("findings: %v, want none", got.Findings)
	}
}
