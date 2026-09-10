package envelope_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
)

func TestParseClassifies(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		kind   envelope.Kind
		id     string
		idType envelope.IDType
		method string
	}{
		{
			name: "request", in: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`,
			kind: envelope.KindRequest, id: "7", idType: envelope.IDNumber, method: "tools/call",
		},
		{
			name: "notification", in: `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
			kind: envelope.KindNotification, idType: envelope.IDAbsent,
			method: "notifications/tools/list_changed",
		},
		{
			name: "response", in: `{"jsonrpc":"2.0","id":"abc","result":{"content":[]}}`,
			kind: envelope.KindResponse, id: "abc", idType: envelope.IDString,
		},
		{
			name: "error", in: `{"jsonrpc":"2.0","id":7,"error":{"code":-32020,"message":"HeaderMismatch"}}`,
			kind: envelope.KindError, id: "7", idType: envelope.IDNumber,
		},
		{
			name: "request with a null id is a request, not a notification",
			in:   `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
			kind: envelope.KindRequest, idType: envelope.IDNull, method: "ping",
		},
		{
			name: "error with a null id", in: `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`,
			kind: envelope.KindError, idType: envelope.IDNull,
		},
		{
			name: "result beside a null error is a response",
			in:   `{"jsonrpc":"2.0","id":1,"result":{},"error":null}`,
			kind: envelope.KindResponse, id: "1", idType: envelope.IDNumber,
		},
		{
			name: "an object id is invalid, not unrepresentable",
			in:   `{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
			kind: envelope.KindRequest, id: `{"a":1}`, idType: envelope.IDInvalid, method: "ping",
		},
		{
			name: "missing jsonrpc still classifies; the schema layer judges it",
			in:   `{"id":3,"method":"ping"}`,
			kind: envelope.KindRequest, id: "3", idType: envelope.IDNumber, method: "ping",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := envelope.Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if m.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", m.Kind, tc.kind)
			}
			if m.ID.Text() != tc.id {
				t.Errorf("id text = %q, want %q", m.ID.Text(), tc.id)
			}
			if m.ID.Type() != tc.idType {
				t.Errorf("id type = %q, want %q", m.ID.Type(), tc.idType)
			}
			if m.Method != tc.method {
				t.Errorf("method = %q, want %q", m.Method, tc.method)
			}
			if string(m.Raw()) != tc.in {
				t.Errorf("raw = %q, want the exact input bytes", m.Raw())
			}
		})
	}
}

// A malformed message carries every byte and no envelope. The transcript
// schema requires exactly that shape, and it should be impossible to build
// any other one.
func TestMalformedKeepsBytesAndNothingElse(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"unbalanced", `{"jsonrpc":"2.0","id":7,"result":{"content":[`},
		{"not an object", `[1,2,3]`},
		{"empty", ``},
		{"whitespace only", "  \n"},
		{"trailing garbage", `{"jsonrpc":"2.0","id":1,"result":{}} not json`},
		{"two frames in one", `{"jsonrpc":"2.0","id":1,"result":{}}{"jsonrpc":"2.0","id":2,"result":{}}`},
		{"nothing to dispatch on", `{"jsonrpc":"2.0","id":7}`},
		{"error member with no code", `{"jsonrpc":"2.0","id":7,"error":{"message":"nope"}}`},
		{"bare truncated sse payload", `event: message\ndata: {"jsonrpc":"2.0"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := envelope.Parse([]byte(tc.in))
			if err == nil {
				t.Fatalf("Parse(%q) returned no error; malformed input must say why", tc.in)
			}
			if m.Kind != envelope.KindMalformed {
				t.Errorf("kind = %q, want malformed", m.Kind)
			}
			if string(m.Raw()) != tc.in {
				t.Errorf("raw = %q, want the exact input bytes", m.Raw())
			}
			if m.ID.Type() != envelope.IDAbsent {
				t.Errorf("id type = %q, want absent", m.ID.Type())
			}
			if m.ID.Text() != "" || m.Method != "" || m.Result != nil || m.Error != nil {
				t.Errorf("malformed message carries an envelope: %+v", m)
			}
		})
	}
}

// A frame carrying more than one dispatchable member is not a message, and
// charpy declines to pick one. Guessing would write an assertion about
// someone else's frame into the kind column that I1, I2 and I3 count.
func TestSeveralDispatchMembersIsNotAMessage(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		carries string
	}{
		{"method and result", `{"jsonrpc":"2.0","id":1,"method":"ping","result":{}}`, "method and result"},
		{"method and error", `{"jsonrpc":"2.0","id":1,"method":"ping","error":{"code":-1,"message":"x"}}`, "method and error"},
		{"result and error", `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-1,"message":"x"}}`, "error and result"},
		{"all three", `{"jsonrpc":"2.0","id":1,"method":"ping","result":{},"error":{"code":-1,"message":"x"}}`, "method and error and result"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := envelope.Parse([]byte(tc.in))
			if err == nil {
				t.Fatal("Parse accepted a frame carrying several dispatchable members")
			}
			if m.Kind != envelope.KindMalformed {
				t.Errorf("kind = %q, want malformed", m.Kind)
			}
			// The diagnostic replaces the columns charpy declines to fill.
			if !strings.Contains(err.Error(), tc.carries) {
				t.Errorf("error %q does not name what the frame carried (%q)", err, tc.carries)
			}
			if string(m.Raw()) != tc.in {
				t.Error("an unreadable frame still keeps every byte")
			}
		})
	}
}

// Exactly one dispatchable member is a message. The asymmetry between a null
// result and a null error is JSON-RPC's: a result may be null, an error must
// be an object.
func TestExactlyOneDispatchMemberIsAMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind envelope.Kind
	}{
		{"null result is a result", `{"jsonrpc":"2.0","id":1,"result":null}`, envelope.KindResponse},
		{"null error is not an error", `{"jsonrpc":"2.0","id":1,"result":{},"error":null}`, envelope.KindResponse},
		{"error member that is not an object", `{"jsonrpc":"2.0","id":1,"result":{},"error":"boom"}`, envelope.KindResponse},
		{"error with no integer code cannot be recorded", `{"jsonrpc":"2.0","id":1,"error":{"message":"x"}}`, envelope.KindMalformed},
		{"empty method cannot be recorded", `{"jsonrpc":"2.0","id":1,"method":""}`, envelope.KindMalformed},
		{"non-string method cannot be recorded", `{"jsonrpc":"2.0","id":1,"method":7}`, envelope.KindMalformed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := envelope.Parse([]byte(tc.in))
			if m.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", m.Kind, tc.kind)
			}
		})
	}
}

// transcript.md section 5: the number 7 and the string "7" collide in the id
// column deliberately, and must not collide in charpy's own bookkeeping.
func TestIDCollidesInTextAndNotInKey(t *testing.T) {
	num := envelope.NumberID(7)
	str := envelope.StringID("7")

	if num.Text() != str.Text() {
		t.Errorf("text %q vs %q: the transcript column is meant to collide", num.Text(), str.Text())
	}
	if num.Key() == str.Key() {
		t.Errorf("key %q collides; the ledger would confuse a vary_type duplicate_id", num.Key())
	}
	if num.Equal(str) {
		t.Error("a number id must not equal a string id")
	}
	if got := num.AsString(); got.Type() != envelope.IDString || got.Text() != "7" {
		t.Errorf("AsString() = %v/%q, want string/7", got.Type(), got.Text())
	}
}

func TestIDNullIsNotAbsent(t *testing.T) {
	null := envelope.ParseID(json.RawMessage("null"))
	absent := envelope.ParseID(nil)

	if !null.Present() {
		t.Error("a null id is present: JSON-RPC permits it on an error response")
	}
	if absent.Present() {
		t.Error("no id member means not present")
	}
	if null.Type() == absent.Type() {
		t.Error("null and absent must be distinguishable in id_type")
	}
	if null.Text() != "" || absent.Text() != "" {
		t.Error("neither null nor absent has canonical text")
	}
}

func TestParseIDNumbers(t *testing.T) {
	tests := []struct {
		in   string
		typ  envelope.IDType
		text string
	}{
		{"7", envelope.IDNumber, "7"},
		{"-1", envelope.IDNumber, "-1"},
		{"0", envelope.IDNumber, "0"},
		// Not collapsed to 7: to some implementations these are different
		// ids, and the transcript should not decide otherwise.
		{"7.0", envelope.IDNumber, "7.0"},
		{"1e2", envelope.IDNumber, "1e2"},
		{"true", envelope.IDInvalid, "true"},
		{"[1,2]", envelope.IDInvalid, "[1,2]"},
		{`{ "a" : 1 }`, envelope.IDInvalid, `{"a":1}`},
		{`"7"`, envelope.IDString, "7"},
		{`""`, envelope.IDString, ""},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			id := envelope.ParseID(json.RawMessage(tc.in))
			if id.Type() != tc.typ {
				t.Errorf("type = %q, want %q", id.Type(), tc.typ)
			}
			if id.Text() != tc.text {
				t.Errorf("text = %q, want %q", id.Text(), tc.text)
			}
		})
	}
}

// An empty string id is a string id, not an absent one. Anything keyed on
// emptiness rather than on the type tag gets this wrong.
func TestEmptyStringIDIsPresent(t *testing.T) {
	id := envelope.StringID("")
	if !id.Present() || id.Type() != envelope.IDString {
		t.Errorf("empty string id: present = %v type = %q", id.Present(), id.Type())
	}
}

// Parse must not alias the caller's buffer: the transcript writer holds these
// bytes across an asynchronous write, and the wire layer reuses read buffers.
func TestParseClonesInput(t *testing.T) {
	buf := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	m, err := envelope.Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	before := string(m.Raw())
	for i := range buf {
		buf[i] = 'x'
	}
	if string(m.Raw()) != before {
		t.Error("Parse aliased the caller's buffer; an async transcript write would see mangled bytes")
	}
}

func TestConstructorsRoundTrip(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		m, err := envelope.NewRequest(envelope.NumberID(7), "tools/call", json.RawMessage(`{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != envelope.KindRequest || m.Method != "tools/call" || m.ID.Text() != "7" {
			t.Errorf("round trip lost the envelope: %+v", m)
		}
		const want = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x"}}`
		if string(m.Raw()) != want {
			t.Errorf("raw = %s, want %s", m.Raw(), want)
		}
	})

	t.Run("notification has no id member", func(t *testing.T) {
		m, err := envelope.NewNotification("notifications/initialized", nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != envelope.KindNotification || m.ID.Present() {
			t.Errorf("kind = %q id present = %v", m.Kind, m.ID.Present())
		}
	})

	t.Run("error", func(t *testing.T) {
		m, err := envelope.NewError(envelope.StringID("a"), -32020, "HeaderMismatch", nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != envelope.KindError || m.Error.Code != -32020 {
			t.Errorf("kind = %q error = %+v", m.Kind, m.Error)
		}
	})

	t.Run("an id JSON-RPC forbids still goes on the wire", func(t *testing.T) {
		m, err := envelope.NewRequest(envelope.InvalidID(json.RawMessage(`[1,2]`)), "ping", nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID.Type() != envelope.IDInvalid {
			t.Errorf("id type = %q, want invalid", m.ID.Type())
		}
		const want = `{"jsonrpc":"2.0","id":[1,2],"method":"ping"}`
		if string(m.Raw()) != want {
			t.Errorf("raw = %s, want %s", m.Raw(), want)
		}
	})

	t.Run("invalid params are refused rather than emitted", func(t *testing.T) {
		if _, err := envelope.NewRequest(envelope.NumberID(1), "ping", json.RawMessage(`{oops`)); err == nil {
			t.Error("NewRequest accepted params that are not JSON")
		}
	})
}

func TestResultType(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		present bool
	}{
		{"complete", `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`, "complete", true},
		{"input_required", `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required"}}`, "input_required", true},
		// Not defaulted to complete: the transcript records the wire, and a
		// server that omitted the field should be visible as one that did.
		{"absent stays absent", `{"jsonrpc":"2.0","id":1,"result":{}}`, "", false},
		// Returned as it came, for the schema layer to reject.
		{"unknown value survives", `{"jsonrpc":"2.0","id":1,"result":{"resultType":"banana"}}`, "banana", true},
		{"not a response", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := envelope.Parse([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := m.ResultType()
			if got != tc.want || ok != tc.present {
				t.Errorf("ResultType() = %q,%v want %q,%v", got, ok, tc.want, tc.present)
			}
		})
	}
}
