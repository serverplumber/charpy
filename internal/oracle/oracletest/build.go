// Package oracletest builds transcripts for the oracle layers to judge.
//
// The oracle is tested against hand-built transcripts rather than captured
// ones, and that is the property docs/design/oracle.md section 1 puts first:
// it is the only reason a suite like this can itself be trusted. A layer
// tested only on files charpy produced would agree with charpy about anything
// charpy got wrong.
package oracletest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Builder assembles a transcript one frame at a time.
type Builder struct {
	t     *testing.T
	buf   bytes.Buffer
	w     *transcript.Writer
	class transcript.Class
	face  transcript.Face
	conn  string
	rev   revision.Revision
	link  transcript.Link
}

// New starts a transcript for a subject class.
func New(t *testing.T, class transcript.Class) *Builder {
	t.Helper()

	b := &Builder{t: t, class: class, conn: "c-1", rev: revision.V20251125,
		link: transcript.Link{Via: transcript.ViaNone}}
	b.face = transcript.Downstream
	if class == transcript.ClassClient {
		b.face = transcript.Upstream
	}

	w, err := transcript.New(&b.buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a",
			Mode: transcript.ModeStdioIngress, Subject: transcript.Subject{Class: class},
			Clock: clock.ModeInjected,
		},
		Sched: clock.NewInjected(),
		Wall:  clock.NewFixedWall(time.Unix(0, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	b.w = w

	if err := w.WriteHeader(transcript.Header{
		Revision: &transcript.Negotiation{
			Negotiated: revision.V20251125, How: transcript.HowInitialize,
		},
	}); err != nil {
		t.Fatal(err)
	}
	return b
}

// Conn switches which connection subsequent frames belong to.
func (b *Builder) Conn(id string) *Builder { b.conn = id; return b }

// Face switches which of the subject's faces subsequent frames and events
// are on; a gateway has two.
func (b *Builder) Face(f transcript.Face) *Builder { b.face = f; return b }

// Joined sets the join subsequent frames and fault events carry: id under
// via, at a confidence below certainty when inferred. Joined("", "") clears
// it.
func (b *Builder) Joined(id string, via transcript.Via) *Builder {
	b.link = transcript.Link{CharpyID: id, Via: via}
	switch via {
	case "":
		b.link.Via = transcript.ViaNone
	case transcript.ViaInferred:
		b.link.Confidence = 0.9
	}
	return b
}

// Revision sets the revision subsequent frames say their face negotiated; ""
// leaves it unsaid, as on a handshake request. The header stays 2025-11-25.
func (b *Builder) Revision(r revision.Revision) *Builder { b.rev = r; return b }

// Raw writes a frame from exact bytes, which is how a transcript carrying
// something no encoder would produce gets built.
func (b *Builder) Raw(dir transcript.Direction, raw string, fault *transcript.Fault) *Builder {
	b.t.Helper()
	m, _ := envelope.Parse([]byte(raw))
	b.w.Frame(transcript.Frame{
		Face: b.face, Direction: dir, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Message: m, Revision: b.rev,
		Link:  b.link,
		Fault: fault,
	})
	return b
}

// RawHTTP is Raw over HTTP, with the status the frame's response carried.
func (b *Builder) RawHTTP(dir transcript.Direction, raw string, status int) *Builder {
	b.t.Helper()
	m, _ := envelope.Parse([]byte(raw))
	b.w.Frame(transcript.Frame{
		Face: b.face, Direction: dir, Transport: transcript.TransportHTTP,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Message: m, Revision: b.rev,
		Link: b.link,
		HTTP: &transcript.HTTP{Status: status},
	})
	return b
}

// ToSubject writes a frame travelling toward the subject.
func (b *Builder) ToSubject(raw string) *Builder {
	return b.Raw(b.dirToSubject(), raw, nil)
}

// FromSubject writes a frame the subject originated.
func (b *Builder) FromSubject(raw string) *Builder {
	return b.Raw(b.dirFromSubject(), raw, nil)
}

// Corrupted writes a frame charpy tampered with, which no layer may hold
// against the subject.
func (b *Builder) Corrupted(raw string) *Builder {
	return b.Raw(b.dirFromSubject(), raw, &transcript.Fault{
		CaseID:   "stream/truncate-mid-frame",
		Citation: "stream/truncate-mid-frame@2025-11-25#seed=8f2c1a",
		Kind:     "truncate",
	})
}

// Replacing writes a frame charpy rewrote into raw, naming the id the
// subject's own answer carried. malformed_json is the mechanism that produces
// this shape: what crosses parses to no id at all.
func (b *Builder) Replacing(id envelope.ID, raw string) *Builder {
	return b.Raw(b.dirFromSubject(), raw, &transcript.Fault{
		CaseID:   "frame/malformed-unbalanced",
		Citation: "frame/malformed-unbalanced@2025-11-25#seed=8f2c1a",
		Kind:     "malformed_json",
		Replaced: id,
	})
}

// A subject receives what travels toward it: client to server on the face a
// client faces, server to client on the face a server faces. A server has
// only the first, a client only the second, a gateway both.
func (b *Builder) dirToSubject() transcript.Direction {
	if b.face == transcript.Upstream {
		return transcript.S2C
	}
	return transcript.C2S
}

func (b *Builder) dirFromSubject() transcript.Direction {
	if b.face == transcript.Upstream {
		return transcript.C2S
	}
	return transcript.S2C
}

// FaultToSubject records a case's fault taking effect on a frame the subject
// receives: the question the reaction layer judges its answer to.
func (b *Builder) FaultToSubject() *Builder { return b.applied(b.dirToSubject()) }

// FaultToCharpy records a fault on a frame the subject sent, which lands on
// charpy's own peer and puts no question to the subject at all.
func (b *Builder) FaultToCharpy() *Builder { return b.applied(b.dirFromSubject()) }

func (b *Builder) applied(dir transcript.Direction) *Builder {
	var link *transcript.Link
	if b.link.Via != transcript.ViaNone {
		l := b.link
		link = &l
	}
	b.w.Event(transcript.Event{
		Link: link,
		Kind: transcript.FaultApplied, Face: b.face, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Detail: transcript.AppliedDetail("rewrite", dir),
		Fault: &transcript.Fault{
			CaseID:   "frame/malformed-request",
			Citation: "frame/malformed-request@2025-11-25#seed=8f2c1a",
			Kind:     "malformed_json",
		},
	})
	return b
}

// SubjectKilled records charpy ending the subject at the close of a run,
// which is charpy's doing and not a subject that went away.
func (b *Builder) SubjectKilled() *Builder {
	b.w.Event(transcript.Event{
		Kind: transcript.SubjectExit, Face: b.face, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Detail: map[string]any{"exit_code": -1, "killed_by_charpy": true},
	})
	return b
}

// SubjectExit records the subject going away.
func (b *Builder) SubjectExit(code int) *Builder {
	b.w.Event(transcript.Event{
		Kind: transcript.SubjectExit, Face: b.face, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Detail: map[string]any{"exit_code": code},
	})
	return b
}

// QuestionFailed records, on the current connection, the note a driver
// writes when a follow-up question fails in charpy's own client.
func (b *Builder) QuestionFailed(question, why string) *Builder {
	b.w.Event(transcript.Event{
		Kind: transcript.Note, Face: b.face, Transport: transcript.TransportStdio,
		ClientID: "c0", SessionID: "s-1", ConnID: b.conn,
		Detail: transcript.QuestionDetail(question, errors.New(why)),
	})
	return b
}

// Done closes the transcript and reads it back, which also checks that
// everything built here is something a reader accepts.
func (b *Builder) Done() *transcript.Transcript {
	b.t.Helper()
	if err := b.w.Close(); err != nil {
		b.t.Fatalf("transcript: %v", err)
	}
	got, err := transcript.Read(bytes.NewReader(b.buf.Bytes()))
	if err != nil {
		b.t.Fatalf("the built transcript does not read back: %v\n%s", err, b.buf.String())
	}
	return got
}
