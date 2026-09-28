package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A transcript of a server misbehaving three ways: a result of the wrong
// type, the same id answered twice, and an answer to an id nobody asked for.
// Written by hand rather than captured, because an oracle tested only on
// files charpy produced would agree with charpy about anything charpy got
// wrong.
const misbehaving = `{"schema_version":1,"type":"header","run_id":"01TEST","seq":0,"t_mono_ns":0,"t_wall":"2026-09-11T00:00:00.000000000Z","charpy_version":"t","seed":"8f2c1a","mode":"stdio-ingress","subject":{"class":"server"},"revision":{"negotiated":"2025-11-25","how":"initialize"},"redaction":"on","clock":"real","fleet":{"clients":1}}
{"schema_version":1,"type":"frame","run_id":"01TEST","seq":1,"t_mono_ns":1,"t_wall":"2026-09-11T00:00:00.000000000Z","face":"downstream","direction":"c2s","transport":"stdio","client_id":"c0","session_id":"s-1","conn_id":"c-1","kind":"request","id":"2","id_type":"number","method":"tools/call","raw":"eyJqc29ucnBjIjoiMi4wIiwiaWQiOjIsIm1ldGhvZCI6InRvb2xzL2NhbGwifQ==","raw_len":45,"raw_truncated":false,"http":null,"link":{"via":"none","confidence":0}}
{"schema_version":1,"type":"frame","run_id":"01TEST","seq":2,"t_mono_ns":2,"t_wall":"2026-09-11T00:00:00.000000000Z","face":"downstream","direction":"s2c","transport":"stdio","client_id":"c0","session_id":"s-1","conn_id":"c-1","kind":"response","id":"2","id_type":"number","method":"tools/call","raw":"eyJqc29ucnBjIjoiMi4wIiwiaWQiOjIsInJlc3VsdCI6ImEgc3RyaW5nIn0=","raw_len":43,"raw_truncated":false,"http":null,"link":{"via":"none","confidence":0}}
{"schema_version":1,"type":"frame","run_id":"01TEST","seq":3,"t_mono_ns":3,"t_wall":"2026-09-11T00:00:00.000000000Z","face":"downstream","direction":"s2c","transport":"stdio","client_id":"c0","session_id":"s-1","conn_id":"c-1","kind":"response","id":"2","id_type":"number","method":"tools/call","raw":"eyJqc29ucnBjIjoiMi4wIiwiaWQiOjIsInJlc3VsdCI6e319","raw_len":36,"raw_truncated":false,"http":null,"link":{"via":"none","confidence":0}}
{"schema_version":1,"type":"frame","run_id":"01TEST","seq":4,"t_mono_ns":4,"t_wall":"2026-09-11T00:00:00.000000000Z","face":"downstream","direction":"s2c","transport":"stdio","client_id":"c0","session_id":"s-1","conn_id":"c-1","kind":"response","id":"9999","id_type":"number","method":null,"raw":"eyJqc29ucnBjIjoiMi4wIiwiaWQiOjk5OTksInJlc3VsdCI6e319","raw_len":39,"raw_truncated":false,"http":null,"link":{"via":"none","confidence":0}}
`

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func replay(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := cmdReplay(args, &out)
	return out.String(), code
}

// The determinism guarantee charpy actually ships. ADR-001 calls this a CI
// gate rather than an aspiration: runs against a live subject cannot be made
// reproducible, and verdicts must be, forever.
func TestReplayIsByteIdentical(t *testing.T) {
	path := writeTranscript(t, misbehaving)

	for _, format := range []string{"text", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			first, code := replay(t, "--format", format, path)
			second, again := replay(t, "--format", format, path)

			if first != second {
				t.Errorf("two replays of one transcript differ:\n--- first\n%s\n--- second\n%s", first, second)
			}
			if code != again {
				t.Errorf("exit codes differ: %d then %d", code, again)
			}
		})
	}
}

func TestReplayFindsWhatTheSubjectDidWrong(t *testing.T) {
	out, code := replay(t, writeTranscript(t, misbehaving))

	// A MUST fails the build; OBSERVED findings are reported without charpy
	// deciding whether they are acceptable.
	if code != exitMustViolate {
		t.Errorf("exit %d, want %d", code, exitMustViolate)
	}
	for _, want := range []string{
		"id-resolves-once",        // answered twice
		"no-unsolicited-response", // answered an id nobody asked for
		"schema:2025-11-25#",      // the artifact that rejected the frame
		"1 MUST, 2 OBSERVED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

func TestReplayCleanTranscriptExitsZero(t *testing.T) {
	clean := strings.Join(strings.Split(misbehaving, "\n")[:2], "\n") + "\n"
	out, code := replay(t, writeTranscript(t, clean))

	if code != exitClean {
		t.Errorf("exit %d on a transcript with nothing wrong:\n%s", code, out)
	}
}

func TestReplayFormats(t *testing.T) {
	path := writeTranscript(t, misbehaving)

	t.Run("jsonl is one finding per line", func(t *testing.T) {
		out, _ := replay(t, "--format", "jsonl", path)
		for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
			var f map[string]any
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatalf("line %d is not JSON: %v", i+1, err)
			}
			if f["verdict"] == nil || f["check"] == nil || f["run_id"] != "01TEST" {
				t.Errorf("line %d is missing identity: %v", i+1, f)
			}
		}
	})

	t.Run("an unknown format is refused", func(t *testing.T) {
		if _, code := replay(t, "--format", "yaml", path); code != exitHarness {
			t.Error("an unknown format was accepted")
		}
	})
}

func TestReplayRefusesWhatIsNotATranscript(t *testing.T) {
	if _, code := replay(t, writeTranscript(t, "not a transcript\n")); code != exitHarness {
		t.Error("garbage was replayed")
	}
	if _, code := replay(t, "/nonexistent/run.jsonl"); code != exitHarness {
		t.Error("a missing file was replayed")
	}
	if _, code := replay(t); code != exitHarness {
		t.Error("replay with no argument was accepted")
	}
}

// A single layer can be run alone, which is how an invariant written next
// month is checked against transcripts captured today.
func TestReplayOneLayer(t *testing.T) {
	path := writeTranscript(t, misbehaving)

	schemaOnly, _ := replay(t, "--oracle", "schema", path)
	if strings.Contains(schemaOnly, "id-resolves-once") {
		t.Error("--oracle schema ran the invariant layer")
	}
	invariantOnly, code := replay(t, "--oracle", "invariant", path)
	if strings.Contains(invariantOnly, "schema:") {
		t.Error("--oracle invariant ran the schema layer")
	}
	if code != exitClean {
		t.Errorf("exit %d with no schema layer; the MUST came from schema", code)
	}
	if _, code := replay(t, "--oracle", "telepathy", path); code != exitHarness {
		t.Error("an unknown oracle was accepted")
	}
}
