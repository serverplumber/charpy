// Package oracle is the vocabulary every layer reports in.
//
// The oracle runs over a finished transcript, never during a run. That is what
// makes verdicts exactly reproducible even though runs are not, and it is why
// a layer here takes a file rather than a subject: invariants written next
// month re-run against transcripts captured today. See docs/design/oracle.md.
package oracle

import (
	"cmp"
	"slices"

	"github.com/serverplumber/charpy/internal/transcript"
)

// Verdict is what a check concluded. Two buckets and three non-verdicts, with
// nothing between them.
type Verdict string

const (
	// Must is schema-mechanical only: a generated normative artifact rejected
	// the frame, and the report cites the subschema that did it. charpy
	// asserts nothing of its own.
	Must Verdict = "MUST"
	// Observed is everything else. This is what happened.
	Observed Verdict = "OBSERVED"

	// Skipped means the check does not apply -- wrong revision, wrong subject
	// class, degraded correlation. It carries a reason.
	Skipped Verdict = "SKIPPED"
	// Inconclusive means it applied but the transcript cannot support a
	// conclusion: truncated capture, subject died first, join too weak.
	Inconclusive Verdict = "INCONCLUSIVE"
	// Untriggered means the case applied and the run finished, but no frame
	// ever matched, so nothing was injected. It is the normal outcome of a
	// relayed run whose traffic never went where the matcher pointed.
	Untriggered Verdict = "UNTRIGGERED"
)

// Passed reports whether a verdict is one a suite may treat as a pass.
//
// It is deliberately narrow. A suite that reports skips as passes acquires
// false confidence, which is how test suites become worthless over time --
// so Observed is the only thing that qualifies, and the three non-verdicts
// are none of them failures either.
func (v Verdict) Passed() bool { return v == Observed }

// Finding is one thing a layer concluded.
type Finding struct {
	Verdict Verdict
	// Layer names which of the four produced this: schema, invariant,
	// differential or reaction.
	Layer string
	// Check is the invariant name, or the subschema path for a MUST.
	Check string
	// Citation is the case this belongs to, where a case caused it.
	Citation string

	// Seq is the transcript line the finding points at, or -1.
	Seq int64
	// Summary is one sentence. Detail is what makes it reproducible.
	Summary string
	Detail  string
	// Reason says why, for a Skipped or Inconclusive verdict. A non-verdict
	// without one is indistinguishable from a pass to whoever reads it.
	Reason string
}

// Report is everything an oracle run concluded.
type Report struct {
	RunID    string
	Findings []Finding
}

// Add appends a finding.
func (r *Report) Add(f Finding) { r.Findings = append(r.Findings, f) }

// Sort puts findings in a stable order: by layer, then check, then sequence.
//
// Determinism is the product here. `charpy replay` twice over one transcript
// must produce byte-identical verdicts, and map iteration inside a layer must
// not be able to reorder a report.
func (r *Report) Sort() {
	slices.SortStableFunc(r.Findings, func(a, b Finding) int {
		if c := cmp.Compare(a.Layer, b.Layer); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Check, b.Check); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Seq, b.Seq); c != 0 {
			return c
		}
		return cmp.Compare(a.Summary, b.Summary)
	})
}

// Violations counts findings that are not passes and not non-verdicts.
func (r *Report) Violations() (must, observed int) {
	for _, f := range r.Findings {
		switch f.Verdict {
		case Must:
			must++
		case Observed:
			observed++
		}
	}
	return must, observed
}

// SubjectReceives reports whether a frame travelling dir across face is one
// the subject receives rather than sends. A fault on such a frame is a
// question put to the subject; a fault on any other is put to charpy's own
// peer, and nothing the subject does afterwards answers it
// (docs/design/decisions.md ADR-013).
func SubjectReceives(class transcript.Class, face transcript.Face, dir transcript.Direction) bool {
	switch class {
	case transcript.ClassServer:
		return dir == transcript.C2S
	case transcript.ClassClient:
		return dir == transcript.S2C
	case transcript.ClassGateway:
		// Mirror of SubjectOriginated: what arrives on either side of itself.
		return (face == transcript.Upstream && dir == transcript.S2C) ||
			(face == transcript.Downstream && dir == transcript.C2S)
	default:
		return false
	}
}

// SubjectOriginated reports whether a frame came from the subject rather than
// from charpy.
//
// Only the subject's own frames may be held against it, and which direction
// that is depends on what is under test: a server is faced downstream and
// speaks s2c, a client is faced upstream and speaks c2s. A gateway speaks on
// both of its faces.
//
// A frame charpy tampered with is never the subject's, whichever way it went.
func SubjectOriginated(class transcript.Class, f *transcript.FrameLine) bool {
	if f.Tampered() {
		return false
	}
	switch class {
	case transcript.ClassServer:
		return f.Direction == transcript.S2C
	case transcript.ClassClient:
		return f.Direction == transcript.C2S
	case transcript.ClassGateway:
		// The gateway is on both sides of itself: what it sends downstream and
		// what it sends upstream are both its own.
		return (f.Face == transcript.Downstream && f.Direction == transcript.S2C) ||
			(f.Face == transcript.Upstream && f.Direction == transcript.C2S)
	default:
		return false
	}
}
