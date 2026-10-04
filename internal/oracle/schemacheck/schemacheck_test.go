package schemacheck_test

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/oracle"
	"github.com/serverplumber/charpy/internal/oracle/oracletest"
	"github.com/serverplumber/charpy/internal/oracle/schemacheck"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

func check(t *testing.T, tr *transcript.Transcript) oracle.Report {
	t.Helper()
	rep, err := schemacheck.Check(tr)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

const (
	goodReq = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	goodRes = `{"jsonrpc":"2.0","id":1,"result":{}}`
	// jsonrpc is a const in the schema, so this is rejected by a generated
	// artifact rather than by charpy's opinion of it.
	badVersion = `{"jsonrpc":"3.0","id":1,"result":{}}`
)

func TestConformingFramesProduceNothing(t *testing.T) {
	got := check(t, oracletest.New(t, transcript.ClassServer).
		ToSubject(goodReq).FromSubject(goodRes).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("a conforming exchange produced findings: %+v", got.Findings)
	}
}

// The only layer allowed to say MUST, and only because it cites the artifact
// that did the rejecting.
func TestSchemaViolationIsAMustWithACitation(t *testing.T) {
	got := check(t, oracletest.New(t, transcript.ClassServer).
		ToSubject(goodReq).FromSubject(badVersion).
		Done())

	if len(got.Findings) == 0 {
		t.Fatal("a frame the schema rejects produced no finding")
	}
	f := got.Findings[0]
	if f.Verdict != oracle.Must {
		t.Errorf("verdict = %q, want MUST", f.Verdict)
	}
	if !strings.HasPrefix(f.Check, "schema:2025-11-25#") {
		t.Errorf("citation = %q; it must name the artifact and the subschema", f.Check)
	}
	if f.Detail == "" {
		t.Error("no detail; a MUST that does not say what was wrong is not reproducible")
	}
	if f.Seq < 0 {
		t.Error("the finding does not point at a transcript line")
	}
}

// A frame charpy broke on purpose cannot be held against the subject. Without
// this exclusion every truncation case would report itself as a MUST
// violation by the subject.
func TestFramesCharpyCorruptedAreExcluded(t *testing.T) {
	got := check(t, oracletest.New(t, transcript.ClassServer).
		Corrupted(badVersion).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("charpy's own corruption was reported against the subject: %+v", got.Findings)
	}
}

// What charpy sent is charpy's, whatever shape it was in.
func TestFramesTowardTheSubjectAreExcluded(t *testing.T) {
	got := check(t, oracletest.New(t, transcript.ClassServer).
		ToSubject(badVersion).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("a frame charpy sent was judged: %+v", got.Findings)
	}
}

// Which direction belongs to the subject depends on what is under test.
func TestSubjectDirectionFollowsTheClass(t *testing.T) {
	server := check(t, oracletest.New(t, transcript.ClassServer).FromSubject(badVersion).Done())
	client := check(t, oracletest.New(t, transcript.ClassClient).FromSubject(badVersion).Done())

	if len(server.Findings) == 0 || len(client.Findings) == 0 {
		t.Errorf("a client subject's own frames were not judged: server=%d client=%d",
			len(server.Findings), len(client.Findings))
	}
}

// Bytes no parser accepts are not a schema finding: the malformed kind already
// records that, and saying it twice would be two findings for one fact.
func TestUnparseableBytesAreNotASchemaFinding(t *testing.T) {
	got := check(t, oracletest.New(t, transcript.ClassServer).
		FromSubject(`{"jsonrpc":"2.0","id":1,"result":{`).
		Done())

	if len(got.Findings) != 0 {
		t.Errorf("unparseable bytes produced a schema finding: %+v", got.Findings)
	}
}

// Without a revision there is no artifact to validate against, and guessing
// one would produce confident findings about the wrong specification.
func TestNoRevisionSkips(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).FromSubject(badVersion).Done()
	tr.Header.Revision = nil

	got := check(t, tr)
	if len(got.Findings) != 1 || got.Findings[0].Verdict != oracle.Skipped {
		t.Fatalf("findings = %+v, want one SKIPPED", got.Findings)
	}
	if got.Findings[0].Reason == "" {
		t.Error("a skip with no reason reads as a pass")
	}
}

// Beyond the transcript's byte cap the tail is missing because charpy stopped
// recording, not because the subject stopped sending.
func TestCappedFramesAreInconclusive(t *testing.T) {
	tr := oracletest.New(t, transcript.ClassServer).FromSubject(goodRes).Done()
	tr.Frames()[0].RawTruncated = true

	got := check(t, tr)
	if len(got.Findings) != 1 || got.Findings[0].Verdict != oracle.Inconclusive {
		t.Fatalf("findings = %+v, want one INCONCLUSIVE", got.Findings)
	}
}

// The root is an anyOf over four message shapes, so every violation fails all
// four branches. Reporting them all is complete and useless -- one wrong byte
// becomes eight MUST findings, seven about shapes the frame never claimed to
// be -- and it inflates the count the exit code is derived from.
//
// The branch reported is the one whose objection reaches deepest into the
// frame. Counting failures alone ties a wrong-typed result against a request
// with no method, and picks whichever the library listed first.
func TestOneFindingCitingTheSubschemaThatObjected(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		cite  string
		says  string
	}{
		{
			name:  "a version the schema fixes",
			frame: `{"jsonrpc":"3.0","id":1,"result":{}}`,
			cite:  "#/$defs/JSONRPCResultResponse/properties/jsonrpc/const",
			says:  "value must be '2.0'",
		},
		{
			name:  "a result of the wrong type",
			frame: `{"jsonrpc":"2.0","id":1,"result":"a string where an object belongs"}`,
			cite:  "#/$defs/Result/type",
			says:  "got string, want object",
		},
		{
			name:  "a method that is not a string",
			frame: `{"jsonrpc":"2.0","id":1,"method":7}`,
			cite:  "#/$defs/JSONRPCRequest/properties/method/type",
			says:  "got number, want string",
		},
		{
			name:  "an error with no code",
			frame: `{"jsonrpc":"2.0","id":1,"error":{"message":"no code"}}`,
			cite:  "#/$defs/Error/required",
			says:  "missing property 'code'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := check(t, oracletest.New(t, transcript.ClassServer).FromSubject(tc.frame).Done())

			if len(got.Findings) != 1 {
				t.Fatalf("%d findings for one violation:\n%+v", len(got.Findings), got.Findings)
			}
			f := got.Findings[0]
			if !strings.HasSuffix(f.Check, tc.cite) {
				t.Errorf("cited %q, want it to end %q", f.Check, tc.cite)
			}
			if !strings.Contains(f.Detail, tc.says) {
				t.Errorf("detail = %q, want it to mention %q", f.Detail, tc.says)
			}
		})
	}
}

// A batch response is valid JSON-RPC in 2025-03-26 and gone from 2025-06-18 on,
// so the same bytes are fine or a MUST depending on which revision they are
// held to.
const batchRes = `[{"jsonrpc":"2.0","id":1,"result":{}}]`

// Each frame is held to the revision its own face negotiated. A gateway can
// settle 2025-03-26 with a server while the header -- its clients' side --
// says 2025-11-25, and a frame on that face is judged by what was agreed on
// it.
func TestAFrameIsHeldToItsOwnFacesRevision(t *testing.T) {
	ownFace := check(t, oracletest.New(t, transcript.ClassServer).
		Revision("2025-03-26").
		ToSubject(goodReq).FromSubject(batchRes).
		Done())
	if len(ownFace.Findings) != 0 {
		t.Errorf("a batch on a face that negotiated 2025-03-26 was held to the header: %+v", ownFace.Findings)
	}

	header := check(t, oracletest.New(t, transcript.ClassServer).
		Revision("").
		ToSubject(goodReq).FromSubject(batchRes).
		Done())
	if len(header.Findings) == 0 || header.Findings[0].Verdict != oracle.Must {
		t.Errorf("a batch on a frame naming no revision was not held to the header's 2025-11-25: %+v", header.Findings)
	} else if !strings.HasPrefix(header.Findings[0].Check, "schema:2025-11-25#") {
		t.Errorf("citation = %q, want the header's revision", header.Findings[0].Check)
	}
}

// Every vendored revision's schema must compile and pass a conforming frame.
// The schemas up to 2025-06-18 keep their types under "definitions", not
// "$defs", and a root that assumed one made the others uncheckable: Check
// returned an error for every 2025-06-18 transcript.
func TestEveryRevisionCanBeChecked(t *testing.T) {
	for _, r := range revision.All() {
		t.Run(string(r), func(t *testing.T) {
			// From 2026-07-28 every result says what kind it is.
			res := goodRes
			if r.Stateless() {
				res = `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`
			}
			rep, err := schemacheck.Check(oracletest.New(t, transcript.ClassServer).
				Revision(r).
				ToSubject(goodReq).FromSubject(res).
				Done())
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Findings) != 0 {
				t.Errorf("a conforming exchange produced findings: %+v", rep.Findings)
			}
		})
	}
}
