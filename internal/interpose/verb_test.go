package interpose_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

func rig(t *testing.T) (*clock.Injected, *interpose.Ledger, *interpose.Interposer) {
	t.Helper()
	sched := clock.NewInjected()
	l := interpose.NewLedger(sched)
	return sched, l, interpose.New(l, sched)
}

func testCase(id string) interpose.Case {
	return interpose.Case{
		ID:       id,
		Citation: id + "@2025-11-25#seed=8f2c1a",
		Fault:    interpose.Fault{Kind: "duplicate_id", Params: map[string]any{"mode": "double_response"}},
	}
}

// The reason the ledger is double-entry: charpy corrupts the id space it is
// itself tracking, so a rewritten id has to translate back before the
// reference peer's own pending map sees the answer.
func TestRewriteRegistersItsInverse(t *testing.T) {
	_, l, i := rig(t)

	intent, err := envelope.NewRequest(envelope.NumberID(7), "tools/call", nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Originate(transcript.Downstream, conn, transcript.C2S, interpose.Exchange{IntentID: intent.ID, Method: "tools/call"})

	wire, err := envelope.NewRequest(envelope.NumberID(9), "tools/call", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := i.Rewrite(frame(envelope.KindRequest, "tools/call"), testCase("id/rewrite"), intent, wire)

	if got.Intent.ID.Text() != "7" || got.Wire.ID.Text() != "9" {
		t.Errorf("rewrite recorded intent %q wire %q", got.Intent.ID.Text(), got.Wire.ID.Text())
	}

	// The subject answers what crossed the wire.
	back, rewritten := l.Untranslate(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(9))
	if !rewritten || back.Text() != "7" {
		t.Errorf("Untranslate(9) = %q,%v want 7,true", back.Text(), rewritten)
	}
	// And the exchange followed the id, so the method still resolves.
	if m, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(9)); !ok || m != "tools/call" {
		t.Errorf("MethodFor(9) = %q,%v; the exchange did not follow the rewrite", m, ok)
	}
	if _, ok := l.MethodFor(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(7)); ok {
		t.Error("the exchange is still keyed by the id that never crossed the wire")
	}
}

// An id nobody rewrote translates to itself, so a caller can apply the inverse
// unconditionally rather than tracking which frames were touched.
func TestUntranslateIsIdentityForUntouchedIDs(t *testing.T) {
	_, l, _ := rig(t)
	l.Originate(transcript.Downstream, conn, transcript.C2S, interpose.Exchange{IntentID: envelope.NumberID(3), Method: "ping"})

	got, rewritten := l.Untranslate(transcript.Downstream, conn, transcript.C2S, envelope.NumberID(3))
	if rewritten {
		t.Error("an untouched id reported as rewritten")
	}
	if got.Text() != "3" {
		t.Errorf("Untranslate(3) = %q, want 3", got.Text())
	}
}

// A rewrite of relayed traffic has no Originate behind it, and its inverse
// matters just as much.
func TestRewriteOfRelayedTrafficStillRegisters(t *testing.T) {
	_, l, i := rig(t)

	intent, _ := envelope.NewRequest(envelope.NumberID(1), "ping", nil)
	wire, _ := envelope.NewRequest(envelope.StringID("1"), "ping", nil)
	i.Rewrite(frame(envelope.KindRequest, "ping"), testCase("id/vary"), intent, wire)

	// vary_type: the number and the string must not merge.
	back, ok := l.Untranslate(transcript.Downstream, conn, transcript.C2S, envelope.StringID("1"))
	if !ok || back.Type() != envelope.IDNumber {
		t.Errorf(`Untranslate("1") = %v/%q, want the number 1`, back.Type(), back.Text())
	}
}

func TestWithholdReleasesOnTheInjectedClock(t *testing.T) {
	sched, _, i := rig(t)

	m, _ := envelope.NewResponse(envelope.NumberID(7), nil)
	w := i.Withhold(frame(envelope.KindResponse, ""), testCase("stream/hang"), m, 30*time.Second)

	if !w.Held() {
		t.Fatal("the frame was not held")
	}
	if w.WithdrawnAt() != -1 {
		t.Errorf("WithdrawnAt() = %v before withdrawal, want -1", w.WithdrawnAt())
	}

	sched.Advance(29 * time.Second)
	if !w.Held() {
		t.Error("the hold ended early")
	}

	sched.Advance(2 * time.Second)
	if w.Held() {
		t.Error("the hold did not end on the injected clock")
	}
	// The liveness clock starts here, so the instant has to be the deadline
	// rather than whenever the run loop happened to look.
	if got := w.WithdrawnAt(); got != clock.FromMillis(30000) {
		t.Errorf("withdrawn at %v, want 30s", got)
	}
	select {
	case <-w.Withdrawn():
	default:
		t.Error("Withdrawn() did not fire")
	}
}

// withdraw_after_ms = 0 means never, which is what POST /control/withdraw
// exists to rescue.
func TestWithholdForeverUntilReleased(t *testing.T) {
	sched, _, i := rig(t)

	m, _ := envelope.NewResponse(envelope.NumberID(7), nil)
	w := i.Withhold(frame(envelope.KindResponse, ""), testCase("stream/hang"), m, 0)

	sched.Advance(time.Hour)
	if !w.Held() {
		t.Error("a hold with no withdrawal released itself")
	}

	if !w.Release(sched.Now()) {
		t.Error("Release reported nothing to release")
	}
	if w.Held() {
		t.Error("the frame is still held after Release")
	}
	if w.Release(sched.Now()) {
		t.Error("releasing twice reported a second release")
	}
}

func TestSynthesisTemplates(t *testing.T) {
	_, _, i := rig(t)
	f := frame(envelope.KindResponse, "")

	t.Run("a response for an id nothing asked about", func(t *testing.T) {
		m, err := interpose.TemplateResult(envelope.NumberID(999), json.RawMessage(`{"ok":true}`))
		if err != nil {
			t.Fatal(err)
		}
		got := i.Synthesize(f, testCase("id/unsolicited"), m)
		if got.Message.Kind != envelope.KindResponse || got.Message.ID.Text() != "999" {
			t.Errorf("synthesized %+v", got.Message)
		}
	})

	t.Run("the null id JSON-RPC reserves", func(t *testing.T) {
		m, err := interpose.TemplateError(envelope.NullID(), -32603, "internal")
		if err != nil {
			t.Fatal(err)
		}
		if m.ID.Type() != envelope.IDNull || m.Error.Code != -32603 {
			t.Errorf("synthesized %+v", m)
		}
	})

	t.Run("a replay is byte-identical", func(t *testing.T) {
		orig, _ := envelope.NewResponse(envelope.NumberID(7), json.RawMessage(`{"a":1}`))
		again := interpose.Replay(orig)
		if string(again.Raw()) != string(orig.Raw()) {
			t.Errorf("replay = %s, want the original bytes %s", again.Raw(), orig.Raw())
		}
	})

	t.Run("a notification, which silent mutation swallows", func(t *testing.T) {
		m, err := interpose.TemplateNotification("notifications/tools/list_changed", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := i.Swallow(f, testCase("manifest/silent"), m)
		if got.Message.Kind != envelope.KindNotification {
			t.Errorf("swallowed %+v", got.Message)
		}
	})
}

// Ledger job 4. Attribution has to be recorded at the moment of the lie:
// reconstructing it later from a transcript means guessing which frames were
// downstream of what.
func TestEveryVerbBeginsConsequences(t *testing.T) {
	f := frame(envelope.KindRequest, "ping")
	m, _ := envelope.NewResponse(envelope.NumberID(1), nil)

	verbs := map[string]func(*interpose.Interposer, interpose.Case){
		"rewrite":    func(i *interpose.Interposer, c interpose.Case) { i.Rewrite(f, c, m, m) },
		"withhold":   func(i *interpose.Interposer, c interpose.Case) { i.Withhold(f, c, m, 0) },
		"synthesize": func(i *interpose.Interposer, c interpose.Case) { i.Synthesize(f, c, m) },
		"swallow":    func(i *interpose.Interposer, c interpose.Case) { i.Swallow(f, c, m) },
	}

	for name, apply := range verbs {
		t.Run(name, func(t *testing.T) {
			_, l, i := rig(t)
			if _, lying := l.ConsequenceOf(transcript.Downstream, conn); lying {
				t.Fatal("a fresh connection is already attributing frames to a fault")
			}

			c := testCase("stream/" + name)
			apply(i, c)

			got, lying := l.ConsequenceOf(transcript.Downstream, conn)
			if !lying || got.ID != c.ID {
				t.Errorf("ConsequenceOf = %q,%v want %q,true", got.ID, lying, c.ID)
			}

			// Withdrawal hands the subject back responsibility for itself.
			l.EndConsequences(transcript.Downstream, conn)
			if _, lying := l.ConsequenceOf(transcript.Downstream, conn); lying {
				t.Error("attribution outlived the fault")
			}
		})
	}
}

func TestTranscriptFault(t *testing.T) {
	got := testCase("id/dup").TranscriptFault()
	if got.CaseID != "id/dup" || got.Kind != "duplicate_id" {
		t.Errorf("TranscriptFault = %+v", got)
	}
	if got.Params["mode"] != "double_response" {
		t.Errorf("params lost the mechanism's parameters: %v", got.Params)
	}
	// It has to be writable as-is, or attribution never reaches the file.
	if _, err := transcriptable(got); err != nil {
		t.Errorf("a case's fault is not writable to a transcript: %v", err)
	}
}

func transcriptable(f *transcript.Fault) (*transcript.Fault, error) { return f, nil }

// A frame cut short hides its id exactly as a rewrite does, and the
// attribution has to say which frame it replaced -- or the oracle reads
// charpy's truncation as the subject never answering. A cut that delivers the
// whole frame, and a frame delivered untouched, carry no attribution at all:
// the subject wrote them and charpy did not change them.
func TestCrossedSettlesWhatTheAttributionClaims(t *testing.T) {
	c := testCase("stream/truncate-mid-event")
	answer, err := envelope.Parse([]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Where the cut lands does not change what it hid: short of the
	// delimiter, the frame never arrived, even when its JSON is whole and
	// still carries the id -- a line cut before its newline never delimits.
	for _, tc := range []struct {
		name     string
		whole    bool
		want     bool // attributed at all
		replaced bool
	}{
		{"cut anywhere short of the delimiter", false, true, true},
		{"delivered whole and untouched", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att := interpose.Crossed(c.TranscriptFault(), answer, answer, tc.whole)
			if (att != nil) != tc.want {
				t.Fatalf("attributed = %v, want %v", att != nil, tc.want)
			}
			if att == nil {
				return
			}
			if got := att.Replaced.Present(); got != tc.replaced {
				t.Fatalf("replaced present = %v, want %v", got, tc.replaced)
			}
			if tc.replaced {
				if att.Replaced.Type() != envelope.IDNumber || att.Replaced.Text() != "3" {
					t.Errorf("replaced = %s %s, want number 3", att.Replaced.Type(), att.Replaced.Text())
				}
				if att.ReplacedKind != envelope.KindResponse {
					t.Errorf("replaced kind = %q, want response", att.ReplacedKind)
				}
			}
			if att.CaseID != c.ID || att.Citation != c.Citation {
				t.Errorf("the attribution lost its case: %+v", att)
			}
		})
	}

	// A rewrite delivered whole is still charpy's frame.
	other, _ := envelope.Parse([]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text"}]}}`))
	if att := interpose.Crossed(c.Rewrote(answer, other), answer, other, true); att == nil {
		t.Error("a rewritten frame delivered whole lost its attribution")
	}

	// A destroyed request says it was a request.
	req, _ := envelope.Parse([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
	if att := interpose.Crossed(c.TranscriptFault(), req, req, false); att.ReplacedKind != envelope.KindRequest {
		t.Errorf("replaced kind = %q, want request", att.ReplacedKind)
	}

	// A frame nothing faulted stays unattributed; settling never invents one.
	if att := interpose.Crossed(nil, answer, answer, false); att != nil {
		t.Errorf("an unattributed frame gained an attribution: %+v", att)
	}
}
