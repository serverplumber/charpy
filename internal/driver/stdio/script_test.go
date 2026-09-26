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
	"github.com/serverplumber/charpy/internal/driver/drivertest"
	"github.com/serverplumber/charpy/internal/driver/stdio"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/reaction"
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
		raw:     transcriptBuf.String(),
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

// The catalogue, unmodified, against a real subject: a case put to a server
// arrives at the server. The fault is "the same request id arrives twice while
// the first is in flight", so the transcript must show that id cross toward
// the subject twice -- once as the peer sent it, once as charpy's copy.
func TestAShippedCaseRunsAsWritten(t *testing.T) {
	loaded := shippedCase(t, "id/duplicate-request-inflight")

	r := script(t, loaded, revision.V20251125)

	if len(r.events(string(transcript.FaultApplied))) == 0 {
		t.Fatalf("the shipped case never fired; events: %v", r.ofType("event"))
	}

	seen := map[string]int{}
	for _, l := range r.ofType("frame") {
		if l["direction"] != "c2s" || l["kind"] != "request" || l["method"] != "tools/call" {
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
		t.Errorf("no request id reached the server twice; requests: %v", seen)
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

// A fault that reaches the subject is followed by one question on the same
// session, so the reaction layer has an answer to judge rather than
// nothing-asked-after-fault.
//
// The case matches every request toward the subject, with no method and
// occurrence_every = 1, which is the shape that would fault the follow-up too
// if the matcher were allowed to see it. unsolicited_response leaves the matched
// request whole and puts an extra frame before it, so the subject can still
// answer the script and the session survives to be asked.
func TestAFollowUpIsAskedAfterTheFault(t *testing.T) {
	c := interpose.Case{
		ID:       "id/unsolicited-to-server",
		Citation: "id/unsolicited-to-server@2025-11-25#seed=8f2c1a",
		Match:    interpose.Match{Direction: transcript.C2S, Kind: "request", Every: 1},
		Fault:    interpose.Fault{Kind: "unsolicited_response", Params: map[string]any{"id_source": "never_used"}},
	}
	r := script(t, c, revision.V20251125)

	tr := drivertest.Read(t, r.raw)
	q, _ := drivertest.FollowUpAnswered(t, tr)
	// The last request the script sent is the follow-up, so every fault is
	// followed by it; and it is the era's probe method, not a second call.
	if q.MethodName() != "ping" {
		t.Errorf("follow-up was %q, want ping at %s", q.MethodName(), revision.V20251125)
	}
	drivertest.ReactionAnswered(t, tr)
}

// Nothing acted, so nothing is followed up: the script's traffic stays exactly
// what the case derives, and a clean run gains no request it did not ask for.
func TestNoFollowUpWithoutAFault(t *testing.T) {
	r := script(t, quiet(), revision.V20251125)
	for _, l := range r.ofType("frame") {
		if l["method"] == "ping" {
			t.Fatalf("a run with no fault sent a follow-up ping: %v", l)
		}
	}
}

// A c2s fault that destroys the peer's own request -- the subject never
// receives it, so it never answers it -- must not leave the script waiting out
// its deadline. charpy answers its own peer, attributed to the case, and the
// script goes on to ask the question after the fault. Whatever the subject then
// does is the reaction layer's to report: this fixture is the pinned Go SDK's
// stdio server, which exits on a malformed line, so here that is an exit; a
// subject that survives would be reported as answering.
func TestADestroyedRequestIsAnsweredByCharpyAndTheScriptGoesOn(t *testing.T) {
	c := interpose.Case{
		ID:       "frame/malformed-request",
		Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
		Match: interpose.Match{
			Method: interpose.ParseGlob("tools/call"), Direction: transcript.C2S, Kind: "request",
		},
		Fault: interpose.Fault{Kind: "malformed_json"},
	}
	start := time.Now()
	r := script(t, c, revision.V20251125)
	// The script's deadline is 5s; a peer left waiting would sit all of it out.
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the run took %s: the peer waited on a request the subject never received", took)
	}

	tr := drivertest.Read(t, r.raw)
	var callID string
	var answered, asked bool
	var faultSeq int64 = -1
	for _, e := range tr.Entries {
		if ev := e.Event; ev != nil && ev.EventKind == transcript.FaultApplied {
			faultSeq = ev.Seq
		}
		f := e.Frame
		if f == nil {
			continue
		}
		if f.Fault != nil && f.Fault.Replaced != nil && f.Fault.Replaced.Kind == envelope.KindRequest {
			callID = f.Fault.Replaced.ID
		}
		id, _ := f.IDText()
		if callID != "" && f.Direction == transcript.S2C && f.Kind == envelope.KindError && id == callID {
			answered = true
			if !f.Tampered() {
				t.Error("charpy's answer to its own peer is recorded as the subject's")
			} else if f.Fault.Replaced != nil {
				t.Errorf("charpy's own answer claims to have replaced %v", f.Fault.Replaced)
			}
		}
		if faultSeq >= 0 && f.Seq > faultSeq && f.Direction == transcript.C2S &&
			f.Kind == envelope.KindRequest && !f.Tampered() {
			asked = true
		}
	}
	if callID == "" {
		t.Fatal("no destroyed request recorded")
	}
	if !answered {
		t.Errorf("nothing answered the peer's destroyed request %s", callID)
	}
	if !asked {
		t.Error("the script asked nothing after the fault")
	}

	for _, f := range reaction.Check(tr).Findings {
		if f.Check != "reaction" {
			continue
		}
		if f.Verdict != oracle.Observed {
			t.Errorf("reaction = %s %s (%s), want an observed reaction", f.Verdict, f.Summary, f.Reason)
		}
		t.Logf("reaction: %s -- %s", f.Summary, f.Detail)
	}
}
