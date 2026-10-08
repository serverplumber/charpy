package transcript_test

import (
	"os"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/transcript"
)

func TestReadGoldenTranscript(t *testing.T) {
	f, err := os.Open("../../testdata/transcripts/truncate-mid-event.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := transcript.Read(f)
	if err != nil {
		t.Fatalf("the hand-written golden does not read back: %v", err)
	}
	if got.Header == nil || got.Header.Seed != "8f2c1a" {
		t.Fatalf("header = %+v", got.Header)
	}
	if n := len(got.Frames()); n != 14 {
		t.Errorf("read %d frames, want 14", n)
	}
	if n := len(got.Events(transcript.FaultApplied)); n != 1 {
		t.Errorf("read %d fault_applied events, want 1", n)
	}
	// A truncate holds nothing, so nothing is withdrawn.
	if n := len(got.Events(transcript.FaultWithdrawn)); n != 0 {
		t.Errorf("read %d fault_withdrawn events, want 0", n)
	}

	// The truncated frame carries no envelope and keeps its bytes, which is
	// the shape the malformed kind exists for. It is the first malformed frame
	// on the upstream face; the gateway forwards the same bytes downstream,
	// untouched by charpy and so not attributed.
	var cut *transcript.FrameLine
	for _, f := range got.Frames() {
		if f.Kind == "malformed" && f.Face == transcript.Upstream && cut == nil {
			cut = f
		}
	}
	if cut == nil {
		t.Fatal("no malformed frame")
	}
	if _, ok := cut.IDText(); ok {
		t.Error("a malformed frame reported an id")
	}
	if !cut.Tampered() {
		t.Error("the truncated frame is not attributed to its case")
	}
	raw, err := cut.Bytes()
	if err != nil || !strings.HasPrefix(string(raw), "event: message") {
		t.Errorf("raw = %q, %v", raw, err)
	}
}

// What a run writes must be what the oracle reads. Without this the two halves
// of the product drift apart a field at a time.
func TestWriterOutputReadsBack(t *testing.T) {
	r := newRig(t)
	r.header(t)
	r.w.Frame(transcript.Frame{
		Face: transcript.Downstream, Direction: transcript.C2S, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s1", ConnID: "c-1",
		Message: req(t, 7, "tools/call"),
		Link:    transcript.Link{Via: transcript.ViaNone},
	})
	r.w.Event(transcript.Event{Kind: transcript.Note, Detail: map[string]any{"x": 1}})
	r.close(t)

	got, err := transcript.Read(strings.NewReader(r.buf.String()))
	if err != nil {
		t.Fatalf("what the writer produced does not read back: %v", err)
	}
	if len(got.Entries) != 3 {
		t.Fatalf("read %d entries, want 3", len(got.Entries))
	}
	f := got.Frames()[0]
	if f.MethodName() != "tools/call" {
		t.Errorf("method = %q", f.MethodName())
	}
	if f.Key() != "number:7" {
		t.Errorf("key = %q, want number:7", f.Key())
	}
	if f.Tampered() {
		t.Error("an untouched frame reported as tampered")
	}
}

// The guarantees every consumer reads a transcript under are checked, not
// assumed: a truncated capture should support fewer conclusions, not wrong
// ones.
func TestReadRefusesWhatIsNotATranscript(t *testing.T) {
	const header = `{"schema_version":1,"type":"header","run_id":"r","seq":0,"t_mono_ns":0,` +
		`"t_wall":"2026-09-08T14:03:10Z","charpy_version":"t","seed":"8f2c1a","mode":"proxy",` +
		`"subject":{"class":"server"},"redaction":"on","clock":"injected","fleet":{"clients":1}}`
	frame := func(seq int, raw string) string {
		return `{"schema_version":1,"type":"frame","run_id":"r","seq":` + itoa(seq) +
			`,"t_mono_ns":1,"t_wall":"2026-09-08T14:03:10Z","face":"downstream","direction":"c2s",` +
			`"transport":"stdio","client_id":"c0","session_id":"s-0","conn_id":"c-0","kind":"notification",` +
			`"id":null,"id_type":"absent","method":"notifications/initialized","raw":"` + raw + `","raw_len":3,` +
			`"raw_truncated":false,"http":null,"link":{"via":"none","confidence":0}}`
	}
	note := func(seq int) string {
		return `{"schema_version":1,"type":"event","run_id":"r","seq":` + itoa(seq) +
			`,"t_mono_ns":1,"t_wall":"2026-09-08T14:03:10Z","event_kind":"note"}`
	}

	tests := []struct {
		name string
		in   string
		why  string
	}{
		{"no header", note(0), "a file whose first line is not a header is not a transcript"},
		{"header not first", note(0) + "\n" + header, "exactly one header, first"},
		{"a gap in the sequence", header + "\n" + note(2), "seq must be dense"},
		{"out of order", header + "\n" + note(2) + "\n" + note(1), "file order must equal seq order"},
		{"a future schema version", strings.Replace(header, `"schema_version":1`, `"schema_version":2`, 1),
			"a v1 reader must refuse rather than guess"},
		{"an unknown line type", strings.Replace(header, `"type":"header"`, `"type":"sideways"`, 1), ""},
		{"not JSON", "{", ""},
		{"a frame whose bytes are not base64", header + "\n" + frame(1, "not base64!"),
			"charpy writes every frame's bytes as base64"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := transcript.Read(strings.NewReader(tc.in)); err == nil {
				t.Errorf("accepted: %s", tc.why)
			}
		})
	}
}

// A withdrawal is the release of a hold, so one with nothing held is a file
// charpy did not write. That is a stale fixture, and it must fail as one
// rather than replay into a finding.
func TestReadRefusesAWithdrawalNothingWasHeldFor(t *testing.T) {
	const header = `{"schema_version":1,"type":"header","run_id":"r","seq":0,"t_mono_ns":0,` +
		`"t_wall":"2026-09-08T14:03:10Z","charpy_version":"t","seed":"8f2c1a","mode":"proxy",` +
		`"subject":{"class":"server"},"redaction":"on","clock":"injected","fleet":{"clients":1}}`
	type ev struct {
		kind, caseID, verb string
	}
	file := func(events ...ev) string {
		lines := []string{header}
		for i, e := range events {
			detail := "{}"
			if e.verb != "" {
				detail = `{"verb":"` + e.verb + `","direction":"c2s"}`
			}
			lines = append(lines, `{"schema_version":1,"type":"event","run_id":"r","seq":`+itoa(i+1)+
				`,"t_mono_ns":1,"t_wall":"2026-09-08T14:03:10Z","event_kind":"`+e.kind+`","detail":`+detail+
				`,"fault":{"case_id":"`+e.caseID+`","citation":"`+e.caseID+`@2025-11-25#seed=8f2c1a",`+
				`"kind":"hang","params":{}}}`)
		}
		return strings.Join(lines, "\n") + "\n"
	}
	withhold := string(interpose.VerbWithhold)
	const a, b = "lifecycle/hang-a", "lifecycle/hang-b"

	refused := []struct {
		name string
		in   string
	}{
		{"a withdrawal alone", file(ev{"fault_withdrawn", a, ""})},
		{"scheduled, then withdrawn", file(ev{"fault_scheduled", a, ""}, ev{"fault_withdrawn", a, ""})},
		{"withdrawn before it was applied", file(
			ev{"fault_scheduled", a, ""}, ev{"fault_withdrawn", a, ""}, ev{"fault_applied", a, withhold})},
		{"a rewrite withdrawn", file(ev{"fault_applied", a, string(interpose.VerbRewrite)}, ev{"fault_withdrawn", a, ""})},
		{"another case's hold", file(ev{"fault_applied", a, withhold}, ev{"fault_withdrawn", b, ""})},
		{"one hold withdrawn twice", file(
			ev{"fault_applied", a, withhold}, ev{"fault_withdrawn", a, ""}, ev{"fault_withdrawn", a, ""})},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := transcript.Read(strings.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), "bad fixture") {
				t.Errorf("err = %v, want a refusal naming the file a bad fixture", err)
			}
		})
	}

	accepted := []struct {
		name string
		in   string
	}{
		{"a hold, withdrawn", file(
			ev{"fault_scheduled", a, ""}, ev{"fault_applied", a, withhold}, ev{"fault_withdrawn", a, ""})},
		// What a driver writes when a matched case cannot be applied.
		{"scheduled, never applied", file(ev{"fault_scheduled", a, ""})},
		{"an applied fault that records no verb", file(ev{"fault_applied", a, ""}, ev{"fault_withdrawn", a, ""})},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := transcript.Read(strings.NewReader(tc.in)); err != nil {
				t.Errorf("refused a sequence charpy writes: %v", err)
			}
		})
	}
}

// A fault is put to whoever receives its frame (ADR-013), and the catalogue
// refuses any case whose fault would land on charpy's own peer. A file with
// such a fault_applied is not one charpy wrote.
func TestReadRefusesAFaultPutToCharpysOwnPeer(t *testing.T) {
	file := func(class transcript.Class, face transcript.Face, dir transcript.Direction) string {
		return `{"schema_version":1,"type":"header","run_id":"r","seq":0,"t_mono_ns":0,` +
			`"t_wall":"2026-09-08T14:03:10Z","charpy_version":"t","seed":"8f2c1a","mode":"proxy",` +
			`"subject":{"class":"` + string(class) + `"},"redaction":"on","clock":"injected","fleet":{"clients":1}}` + "\n" +
			`{"schema_version":1,"type":"event","run_id":"r","seq":1,"t_mono_ns":1,"t_wall":"2026-09-08T14:03:10Z",` +
			`"event_kind":"fault_applied","face":"` + string(face) + `","detail":{"verb":"rewrite","direction":"` + string(dir) + `"},` +
			`"fault":{"case_id":"frame/x","citation":"frame/x@2025-11-25#seed=8f2c1a","kind":"malformed_json"}}` + "\n"
	}
	const (
		up, down = transcript.Upstream, transcript.Downstream
		c2s, s2c = transcript.C2S, transcript.S2C
	)
	for _, tc := range []struct {
		class   transcript.Class
		face    transcript.Face
		dir     transcript.Direction
		refused bool
	}{
		{transcript.ClassServer, down, c2s, false},
		{transcript.ClassServer, down, s2c, true},
		{transcript.ClassClient, up, s2c, false},
		{transcript.ClassClient, up, c2s, true},
		{transcript.ClassGateway, up, s2c, false},
		{transcript.ClassGateway, down, c2s, false},
		{transcript.ClassGateway, up, c2s, true},
		{transcript.ClassGateway, down, s2c, true},
	} {
		t.Run(string(tc.class)+"/"+string(tc.face)+"/"+string(tc.dir), func(t *testing.T) {
			_, err := transcript.Read(strings.NewReader(file(tc.class, tc.face, tc.dir)))
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), "bad fixture")):
				t.Errorf("err = %v, want a refusal naming the file a bad fixture", err)
			case !tc.refused && err != nil:
				t.Errorf("refused a fault charpy puts to the subject: %v", err)
			}
		})
	}
}

func itoa(n int) string {
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
