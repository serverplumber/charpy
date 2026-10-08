package main

import (
	"bytes"
	"encoding/json"
	"flag"
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
const misbehaving = "../../testdata/transcripts/misbehaving.jsonl"

// The expected output of every golden transcript, one file per format, named
// after the transcript. Not beside it: a .jsonl here would be globbed as a
// transcript.
const verdictsDir = "../../testdata/verdicts"

var update = flag.Bool("update", false, "rewrite testdata/verdicts from the current oracle")

var formats = []string{"text", "jsonl"}

func goldenTranscripts(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../../testdata/transcripts/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A gate over no files passes, so an empty or moved testdata/ must fail.
	if len(paths) == 0 {
		t.Fatal("no golden transcripts found")
	}
	return paths
}

func verdictsFile(transcript, format string) string {
	return filepath.Join(verdictsDir, strings.TrimSuffix(filepath.Base(transcript), ".jsonl")+"."+format)
}

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

// The guarantee ADR-001 makes: verdicts from a transcript do not change.
// Each golden transcript's output is checked in, so a commit that changes a
// verdict fails here until the new output is regenerated with just verdicts
// and committed -- the change is then a diff someone read, not a drift.
func TestReplayMatchesCheckedInVerdicts(t *testing.T) {
	for _, path := range goldenTranscripts(t) {
		for _, format := range formats {
			want := verdictsFile(path, format)
			t.Run(filepath.Base(want), func(t *testing.T) {
				got, _ := replay(t, "--format", format, path)
				if *update {
					if err := os.WriteFile(want, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				expected, err := os.ReadFile(want)
				if err != nil {
					t.Fatalf("%v: every golden transcript needs its verdicts checked in; run just verdicts", err)
				}
				if got != string(expected) {
					t.Errorf("verdicts differ from %s:\n--- want\n%s\n--- got\n%s", want, expected, got)
				}
			})
		}
	}
}

// A verdicts file whose transcript is gone would never be compared again.
func TestNoOrphanedVerdicts(t *testing.T) {
	expected := map[string]bool{}
	for _, path := range goldenTranscripts(t) {
		for _, format := range formats {
			expected[verdictsFile(path, format)] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(verdictsDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !expected[f] {
			t.Errorf("%s matches no golden transcript and format", f)
		}
	}
}

// The golden verdicts pin only what a golden transcript reaches, so every
// check and reason in testdata/vocabulary.txt must appear in some golden
// verdict. There is no list of exceptions: one would be the first thing
// reached for, and a verdict no transcript exercises is one whose behaviour
// nothing pins. A new verdict lands with the transcript that reaches it.
func TestEveryVerdictIsReached(t *testing.T) {
	reached := map[string]bool{}
	for _, path := range goldenTranscripts(t) {
		body, err := os.ReadFile(verdictsFile(path, "jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var f struct{ Layer, Check, Reason string }
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatalf("%s: %v", verdictsFile(path, "jsonl"), err)
			}
			reached[f.Layer+" check "+f.Check] = true
			if f.Reason != "" {
				reached[f.Layer+" reason "+f.Reason] = true
			}
		}
	}

	for _, v := range readLines(t, "../../testdata/vocabulary.txt") {
		if !reached[v] {
			t.Errorf("no golden transcript reaches %s; add one to testdata/transcripts/", v)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// The checked-in verdicts catch a change between builds; this catches
// nondeterminism within one -- map order, a clock read -- reliably, where a
// single comparison against a checked-in file would only flake.
func TestReplayIsByteIdentical(t *testing.T) {
	for _, path := range goldenTranscripts(t) {
		for _, format := range formats {
			t.Run(filepath.Base(path)+"/"+format, func(t *testing.T) {
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
}

func TestReplayFindsWhatTheSubjectDidWrong(t *testing.T) {
	out, code := replay(t, misbehaving)

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
	body, err := os.ReadFile(misbehaving)
	if err != nil {
		t.Fatal(err)
	}
	clean := strings.Join(strings.Split(string(body), "\n")[:2], "\n") + "\n"
	out, code := replay(t, writeTranscript(t, clean))

	if code != exitClean {
		t.Errorf("exit %d on a transcript with nothing wrong:\n%s", code, out)
	}
}

func TestReplayFormats(t *testing.T) {
	path := misbehaving

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
	path := misbehaving

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
