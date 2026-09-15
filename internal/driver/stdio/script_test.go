package stdio_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/peer"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// script runs one case against a conforming SDK subject with charpy's own peer
// driving it, and returns everything the run produced.
func script(t *testing.T, c interpose.Case, era revision.Revision) run {
	t.Helper()

	var transcriptBuf, subjErr strings.Builder
	sched := clock.RealSched()

	tr, err := transcript.New(&transcriptBuf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode:    transcript.ModeStdioIngress,
			Subject: transcript.Subject{Class: transcript.ClassServer},
			Clock:   clock.ModeReal,
			Peer:    peer.Describe(era),
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}

	sc, err := stdio.NewScript(stdio.ScriptOptions{
		Case:    c,
		Era:     era,
		Timeout: 5 * time.Second,
		Options: stdio.Options{
			Command:    subjectCommand(),
			Env:        append(os.Environ(), sdkSubjectEnv+"=1"),
			Errs:       &subjErr,
			Transcript: tr,
			Sched:      sched,
			Ledger:     interpose.NewLedger(sched),
			Face:       transcript.Downstream,
			RunSeed:    "8f2c1a",
		},
	})
	if err != nil {
		t.Fatalf("NewScript: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := sc.Run(ctx); err != nil {
		t.Fatalf("script: %v\nsubject stderr:\n%s", err, subjErr.String())
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	return run{
		lines:   decodeLines(t, transcriptBuf.String()),
		subjErr: subjErr.String(),
	}
}

// A case whose matcher never fires, so a run is clean end to end. Every case
// must name a mechanism -- an unarmed one is refused -- so the way to get a
// quiet run is a match nothing originates, which is also what UNTRIGGERED
// looks like from the driver's side.
func quiet() interpose.Case {
	return interpose.Case{
		ID:       "test/never-matches",
		Citation: "test/never-matches#seed=8f2c1a",
		// An error frame never crosses toward the subject, so nothing
		// matches. A method the scenario cannot originate would be refused
		// when the scenario is built, which is a different guard.
		Match: interpose.Match{Kind: "error"},
		Fault: interpose.Fault{
			Kind:   "truncate",
			Params: map[string]any{"cut_at": "mid_frame", "then": "stall"},
		},
	}
}

// Item 10.5 end to end: charpy spawns a conforming subject, drives it with its
// own peer, and writes a transcript of traffic it originated. Nothing is
// faulted here -- this is the loop working before a fault is asked of it.
func TestAScriptedRunOriginatesAndTranscribes(t *testing.T) {
	c := quiet()
	c.Match = interpose.Match{Method: interpose.ParseGlob("tools/call"), Occurrence: 2, Kind: "error"}
	r := script(t, c, revision.V20251125)

	var calls int
	for _, l := range r.ofType("frame") {
		if m, ok := l["method"].(string); ok && m == "tools/call" && l["direction"] == "c2s" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("charpy originated %d tools/call frames, want the 2 the case asked for", calls)
	}

	// The header names the peer, which is what makes an owned-stimulus
	// transcript reproducible from its own contents (ADR-011).
	hdr := r.ofType("header")
	if len(hdr) != 1 {
		t.Fatalf("want exactly one header, got %d", len(hdr))
	}
	p, ok := hdr[0]["peer"].(map[string]any)
	if !ok {
		t.Fatalf("header names no peer: %v", hdr[0])
	}
	if p["era"] != string(revision.V20251125) {
		t.Errorf("peer era = %v, want %s", p["era"], revision.V20251125)
	}
	if hdr[0]["revision"].(map[string]any)["negotiated"] != string(revision.V20251125) {
		t.Errorf("negotiated %v, want %s", hdr[0]["revision"], revision.V20251125)
	}
}

// The stateless handshake settles the header too. A driver watching only for
// initialize would sit behind an unwritten header for a whole 2026-07-28 run.
func TestTheStatelessHandshakeSettlesTheHeader(t *testing.T) {
	r := script(t, quiet(), revision.V20260728)

	hdr := r.ofType("header")
	if len(hdr) != 1 {
		t.Fatalf("want exactly one header, got %d", len(hdr))
	}
	rev, ok := hdr[0]["revision"].(map[string]any)
	if !ok {
		t.Fatalf("no revision settled on a stateless run: %v", hdr[0])
	}
	if rev["how"] != string(transcript.HowServerDiscover) {
		t.Errorf("how = %v, want %s", rev["how"], transcript.HowServerDiscover)
	}
	if rev["negotiated"] != string(revision.V20260728) {
		t.Errorf("negotiated = %v, want %s", rev["negotiated"], revision.V20260728)
	}
}

// The point of the whole item: a fault charpy injected, into traffic charpy
// originated, recorded against the case that asked for it and delivered to a
// real subject.
//
// The fault is aimed at a notification because the shim injects toward the
// subject only, and a notification expects no answer -- so the corruption
// lands without the script waiting on a reply that a broken frame will never
// produce. Which direction the shipped catalogue's cases actually want is a
// separate and unresolved question; see the note in notes/status.md.
func TestAFaultLandsOnOwnedStimulus(t *testing.T) {
	r := script(t, interpose.Case{
		ID:       "frame/truncate-notification",
		Citation: "frame/truncate-notification@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Kind: "notification"},
		Fault: interpose.Fault{
			Kind:   "truncate",
			Params: map[string]any{"cut_at": "mid_frame", "then": "stall"},
		},
	}, revision.V20251125)

	var applied []map[string]any
	for _, l := range r.ofType("event") {
		if l["event_kind"] == string(transcript.FaultApplied) {
			applied = append(applied, l)
		}
	}
	if len(applied) == 0 {
		t.Fatalf("no fault was applied; events: %v", r.ofType("event"))
	}
	if d, _ := applied[0]["detail"].(map[string]any); d["verb"] != "rewrite" {
		t.Errorf("verb = %v, want rewrite", d["verb"])
	}

	// The recorded frame is what crossed, not what was meant: a truncated
	// frame has no envelope left, which is what the malformed kind carries.
	var truncated bool
	for _, l := range r.ofType("frame") {
		if l["fault"] != nil && l["kind"] == "malformed" {
			truncated = true
		}
	}
	if !truncated {
		t.Errorf("no truncated frame reached the transcript")
	}

	// The subject died of charpy's fault, and the transcript says so without
	// the run failing: a crash under injection is the finding, and reporting
	// it as a harness error would fail exactly the runs that worked.
	var exit map[string]any
	for _, l := range r.ofType("event") {
		if l["event_kind"] == string(transcript.SubjectExit) {
			exit, _ = l["detail"].(map[string]any)
		}
	}
	if exit == nil {
		t.Fatal("no subject_exit event")
	}
	if exit["killed_by_charpy"] != false {
		t.Errorf("the subject died of the fault, not of shutdown: %v", exit)
	}
	if code, _ := exit["exit_code"].(float64); code == 0 {
		t.Errorf("subject exited 0 after its parser was broken: %v", exit)
	}
}

// The catalogue, unmodified, against a real subject. Every directional case
// charpy ships is s2c, and until the driver stopped deciding the direction
// for itself not one of them could fire.
func TestAShippedCaseRunsAsWritten(t *testing.T) {
	loaded := shippedCase(t, "id/duplicate-response")

	r := script(t, loaded, revision.V20251125)

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the shipped case never fired; events: %v", r.ofType("event"))
	}

	// The fault is "the same request id is answered twice", so the transcript
	// must show an id resolving twice -- which is what invariant I1 reads a
	// transcript for, and the reason the case exists.
	seen := map[string]int{}
	for _, l := range r.ofType("frame") {
		if l["direction"] != "s2c" || l["kind"] != "response" {
			continue
		}
		if id, ok := l["id"].(string); ok {
			seen[id]++
		}
	}
	doubled := false
	for _, n := range seen {
		if n > 1 {
			doubled = true
		}
	}
	if !doubled {
		t.Errorf("no id was answered twice; responses: %v", seen)
	}
}

// A scripted run ends because the script ended, not because somebody signalled
// it. That is the difference that lets this mode exit with a verdict.
func TestAScriptedRunEndsOnItsOwn(t *testing.T) {
	r := script(t, quiet(), revision.V20251125)

	var exits []map[string]any
	for _, l := range r.ofType("event") {
		if l["event_kind"] == string(transcript.SubjectExit) {
			exits = append(exits, l)
		}
	}
	if len(exits) != 1 {
		t.Fatalf("want one subject_exit, got %d", len(exits))
	}
	d, _ := exits[0]["detail"].(map[string]any)
	if d["killed_by_charpy"] != false {
		t.Errorf("subject_exit says charpy killed it; the script ended instead: %v", d)
	}
}

// shippedCase loads one case from the real catalogue and compiles it, so the
// test is held to what charpy ships rather than to a hand-built stand-in.
func shippedCase(t *testing.T, id string) interpose.Case {
	t.Helper()

	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		t.Fatalf("loading the shipped catalogue: %v", err)
	}
	compiled, err := cat.CompileAll(revision.V20251125, "8f2c1a")
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	for _, c := range compiled {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("case %s is not in the shipped catalogue", id)
	return interpose.Case{}
}
