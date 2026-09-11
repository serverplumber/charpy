package transcript_test

import (
	"os"
	"strings"
	"testing"

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
	if n := len(got.Frames()); n != 3 {
		t.Errorf("read %d frames, want 3", n)
	}
	if n := len(got.Events(transcript.FaultWithdrawn)); n != 1 {
		t.Errorf("read %d fault_withdrawn events, want 1", n)
	}

	// The truncated frame carries no envelope and keeps its bytes, which is
	// the shape the malformed kind exists for.
	var cut *transcript.FrameLine
	for _, f := range got.Frames() {
		if f.Kind == "malformed" {
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := transcript.Read(strings.NewReader(tc.in)); err == nil {
				t.Errorf("accepted: %s", tc.why)
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
