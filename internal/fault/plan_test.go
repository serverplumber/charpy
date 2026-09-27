package fault_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/wire"
)

const runSeed = "8f2c1a"

func caseOf(kind string, params map[string]any) interpose.Case {
	if params == nil {
		params = map[string]any{}
	}
	return interpose.Case{
		ID:       "stream/" + strings.ReplaceAll(kind, "_", "-"),
		Citation: "stream/x@2025-11-25#seed=" + runSeed,
		Fault:    interpose.Fault{Kind: kind, Params: params},
	}
}

func parse(t *testing.T, s string) envelope.Message {
	t.Helper()
	m, err := envelope.Parse([]byte(s))
	if err != nil {
		t.Fatalf("test frame does not parse: %v", err)
	}
	return m
}

func apply(t *testing.T, kind string, params map[string]any, m envelope.Message) fault.Plan {
	t.Helper()
	p, err := fault.Apply(caseOf(kind, params), m, fault.Context{RunSeed: runSeed})
	if err != nil {
		t.Fatalf("Apply(%s): %v", kind, err)
	}
	return p
}

const (
	request   = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search"}}`
	response  = `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`
	toolsList = `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
		`{"name":"search","inputSchema":{"type":"object"}},` +
		`{"name":"fetch","inputSchema":{"type":"object"}}]}}`
	initialize = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"s","version":"1"}}}`
)

// Every mechanism the registry declares must have an implementation behind it.
// A registry entry with nothing behind it is a case that loads, matches, and
// then does nothing -- the silent pass the whole design is built to prevent.
func TestEveryRegisteredMechanismPlans(t *testing.T) {
	frames := map[string]envelope.Message{}
	for _, k := range fault.Kinds() {
		t.Run(k, func(t *testing.T) {
			// Give each mechanism the frame kind its cases match.
			var m envelope.Message
			switch k {
			case "manifest_mutate":
				m = parse(t, toolsList)
			case "capability_flip":
				m = parse(t, initialize)
			case "schema_violation", "duplicate_id":
				m = parse(t, response)
			default:
				m = parse(t, request)
			}
			frames[k] = m

			_, err := fault.Apply(caseOf(k, nil), m, fault.Context{RunSeed: runSeed})
			if err != nil && !errors.Is(err, fault.ErrNotApplicable) {
				t.Errorf("default parameters do not plan: %v", err)
			}
		})
	}
}

func TestUnknownMechanismIsAnError(t *testing.T) {
	_, err := fault.Apply(caseOf("teleport", nil), parse(t, request), fault.Context{RunSeed: runSeed})
	if err == nil {
		t.Error("an unimplemented mechanism planned silently")
	}
}

func TestHang(t *testing.T) {
	p := apply(t, "hang", map[string]any{
		"withdraw_after_ms": int64(30000),
		"then":              "close",
		"scope":             "stream",
		"keepalive":         "comments",
	}, parse(t, response))

	if p.Verb() != interpose.VerbWithhold {
		t.Errorf("verb = %q, want withhold", p.Verb())
	}
	if p.Deliver != nil {
		t.Error("a held frame must not also be delivered")
	}
	if p.Hold.For.Milliseconds() != 30000 || p.Hold.Then != "close" || p.Hold.Scope != "stream" {
		t.Errorf("hold = %+v", p.Hold)
	}
	if p.Keepalive != wire.KeepaliveComments {
		t.Errorf("keepalive = %q", p.Keepalive)
	}

	// withdraw_after_ms = 0 means never, which is what the control plane's
	// withdraw exists to rescue.
	if got := apply(t, "hang", nil, parse(t, response)); got.Hold.For != 0 {
		t.Errorf("default hold = %v, want indefinite", got.Hold.For)
	}
}

func TestTruncateCarriesTheSeedToTheCut(t *testing.T) {
	p := apply(t, "truncate", map[string]any{"cut_at": "mid_frame", "then": "stall"}, parse(t, response))

	if p.Verb() != interpose.VerbRewrite || p.Deliver == nil {
		t.Fatalf("verb = %q deliver = %v", p.Verb(), p.Deliver)
	}
	if p.Then != fault.StreamStall {
		t.Errorf("then = %q, want stall", p.Then)
	}
	if p.Cut.Opts.Pick == nil {
		t.Fatal("no seeded picker; the cut point would not reproduce from a citation")
	}

	// The cut must actually land, and land in the same place twice.
	enc := wire.EncodeLine(p.Deliver.Raw())
	first, err := enc.Cut(p.Cut.At, p.Cut.Opts)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	again := apply(t, "truncate", map[string]any{"cut_at": "mid_frame"}, parse(t, response))
	second, err := enc.Cut(again.Cut.At, again.Cut.Opts)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same case and seed cut at %d then %d", first, second)
	}
	if first <= 0 || first >= enc.Len() {
		t.Errorf("cut at %d, outside the frame", first)
	}
}

func TestMalformedJSON(t *testing.T) {
	tests := []struct {
		how   string
		check func(*testing.T, []byte)
	}{
		{"unbalanced", func(t *testing.T, b []byte) {
			if json.Valid(b) {
				t.Error("still parses")
			}
		}},
		{"trailing_garbage", func(t *testing.T, b []byte) {
			if json.Valid(b) {
				t.Error("still parses")
			}
			if !strings.HasPrefix(string(b), response[:10]) {
				t.Error("the original frame was not preserved ahead of the garbage")
			}
		}},
		{"bad_utf8", func(t *testing.T, b []byte) {
			if json.Valid(b) {
				t.Error("still parses")
			}
		}},
		{"nul_byte", func(t *testing.T, b []byte) {
			if json.Valid(b) {
				t.Error("still parses")
			}
		}},
		{"deep_nest", func(t *testing.T, b []byte) {
			if strings.Count(string(b), "[") < 100 {
				t.Error("not deep enough to test a recursion limit")
			}
		}},
		{"duplicate_key", func(t *testing.T, b []byte) {
			// Technically valid JSON with undefined semantics: that is the
			// point, and a variant that produced invalid JSON would be a
			// different fault.
			if !json.Valid(b) {
				t.Errorf("duplicate_key must stay valid JSON: %s", b)
			}
			if strings.Count(string(b), `"jsonrpc"`) < 2 {
				t.Errorf("no duplicated key: %s", b)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.how, func(t *testing.T) {
			p := apply(t, "malformed_json", map[string]any{"how": tc.how}, parse(t, response))
			if p.Deliver == nil {
				t.Fatal("nothing to deliver")
			}
			tc.check(t, p.Deliver.Raw())
		})
	}
}

// The interesting one: it passes every parser and fails only the oracle.
func TestSchemaViolationStaysValidJSON(t *testing.T) {
	for _, target := range []string{"envelope", "result"} {
		for _, how := range []string{"wrong_type", "missing_required", "extra_required_absent", "enum_out_of_range"} {
			t.Run(target+"/"+how, func(t *testing.T) {
				p := apply(t, "schema_violation",
					map[string]any{"target": target, "how": how}, parse(t, response))
				if !json.Valid(p.Deliver.Raw()) {
					t.Errorf("not valid JSON: %s", p.Deliver.Raw())
				}
				if string(p.Deliver.Raw()) == response {
					t.Error("the frame was not changed")
				}
			})
		}
	}
}

// The two targets that break a shape the subject declared cannot be planned
// until charpy has read the declaration. Not applicable beats a weaker fault
// delivered quietly.
func TestSchemaViolationNeedsTheDeclarationFirst(t *testing.T) {
	for _, target := range []string{"declared_output_schema", "tool_input_schema"} {
		_, err := fault.Apply(caseOf("schema_violation", map[string]any{"target": target}),
			parse(t, response), fault.Context{RunSeed: runSeed})
		if !errors.Is(err, fault.ErrNotApplicable) {
			t.Errorf("target %q planned as %v, want ErrNotApplicable", target, err)
		}
	}
}

func TestDuplicateID(t *testing.T) {
	t.Run("double_response answers the same id twice", func(t *testing.T) {
		p := apply(t, "duplicate_id", map[string]any{"mode": "double_response"}, parse(t, response))
		if p.Verb() != interpose.VerbSynthesize {
			t.Errorf("verb = %q, want synthesize", p.Verb())
		}
		if len(p.After) != 1 || p.After[0].ID.Text() != "7" {
			t.Errorf("after = %+v", p.After)
		}
	})

	t.Run("vary_type sends 7 then \"7\"", func(t *testing.T) {
		p := apply(t, "duplicate_id",
			map[string]any{"mode": "double_response", "vary_type": true}, parse(t, response))
		if got := p.After[0].ID; got.Type() != envelope.IDString || got.Text() != "7" {
			t.Errorf("second id = %v/%q, want the string 7", got.Type(), got.Text())
		}
		if p.Deliver.ID.Type() != envelope.IDNumber {
			t.Error("the first copy should keep the number")
		}
	})

	t.Run("a mode that does not fit the matched frame is refused", func(t *testing.T) {
		_, err := fault.Apply(caseOf("duplicate_id", map[string]any{"mode": "concurrent_request"}),
			parse(t, response), fault.Context{RunSeed: runSeed})
		if !errors.Is(err, fault.ErrNotApplicable) {
			t.Errorf("err = %v, want ErrNotApplicable: a response is not a concurrent request", err)
		}
	})

	t.Run("reuse_after_close spans a connection", func(t *testing.T) {
		_, err := fault.Apply(caseOf("duplicate_id", map[string]any{"mode": "reuse_after_close"}),
			parse(t, response), fault.Context{RunSeed: runSeed})
		if !errors.Is(err, fault.ErrNotApplicable) {
			t.Errorf("err = %v, want ErrNotApplicable", err)
		}
	})
}

func TestUnsolicitedResponse(t *testing.T) {
	t.Run("never_used is seeded and reproducible", func(t *testing.T) {
		a := apply(t, "unsolicited_response", map[string]any{"id_source": "never_used"}, parse(t, request))
		b := apply(t, "unsolicited_response", map[string]any{"id_source": "never_used"}, parse(t, request))
		if a.Before[0].ID.Text() != b.Before[0].ID.Text() {
			t.Errorf("the same case and seed chose %q then %q",
				a.Before[0].ID.Text(), b.Before[0].ID.Text())
		}
	})

	t.Run("reserved_null", func(t *testing.T) {
		p := apply(t, "unsolicited_response", map[string]any{"id_source": "reserved_null"}, parse(t, request))
		if p.Before[0].ID.Type() != envelope.IDNull {
			t.Errorf("id type = %q, want null", p.Before[0].ID.Type())
		}
	})

	// The frame charpy matched on is traffic the subject is waiting to see
	// answered. Swallowing it makes every subject look like it stopped
	// answering, which is a finding charpy would be inventing.
	t.Run("the matched frame still crosses", func(t *testing.T) {
		for _, source := range []string{"never_used", "reserved_null"} {
			p := apply(t, "unsolicited_response", map[string]any{"id_source": source}, parse(t, request))
			if p.Deliver == nil {
				t.Fatalf("%s: the matched frame was dropped, not delivered beside the fault", source)
			}
			if got := string(p.Deliver.Raw()); got != request {
				t.Errorf("%s: delivered %s, want the matched frame unchanged", source, got)
			}
			if p.Swallow {
				t.Errorf("%s: the matched frame was swallowed", source)
			}
		}
	})

	t.Run("already_resolved needs one", func(t *testing.T) {
		_, err := fault.Apply(caseOf("unsolicited_response", map[string]any{"id_source": "already_resolved"}),
			parse(t, request), fault.Context{RunSeed: runSeed})
		if !errors.Is(err, fault.ErrNotApplicable) {
			t.Errorf("err = %v, want ErrNotApplicable with nothing resolved", err)
		}

		p, err := fault.Apply(caseOf("unsolicited_response", map[string]any{"id_source": "already_resolved"}),
			parse(t, request), fault.Context{RunSeed: runSeed, Resolved: envelope.NumberID(3)})
		if err != nil {
			t.Fatal(err)
		}
		if p.Before[0].ID.Text() != "3" {
			t.Errorf("answered %q, want the resolved id 3", p.Before[0].ID.Text())
		}
	})
}

func TestManifestMutate(t *testing.T) {
	t.Run("removing an entry from the forwarded list", func(t *testing.T) {
		p := apply(t, "manifest_mutate", map[string]any{"list": "tools", "op": "remove"}, parse(t, toolsList))
		if strings.Contains(string(p.Deliver.Raw()), "search") {
			t.Errorf("the entry was not removed: %s", p.Deliver.Raw())
		}
		if !strings.Contains(string(p.Deliver.Raw()), "fetch") {
			t.Error("the rest of the list did not survive")
		}
	})

	for _, op := range []string{"add", "rename", "change_schema"} {
		t.Run(op, func(t *testing.T) {
			p := apply(t, "manifest_mutate", map[string]any{"op": op}, parse(t, toolsList))
			if string(p.Deliver.Raw()) == toolsList {
				t.Error("the list was not changed")
			}
		})
	}

	// notify = "silent" is not adversarial: it matches deployed reality, where
	// notification delivery is already distrusted.
	t.Run("silent swallows the notification", func(t *testing.T) {
		note := parse(t, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
		p := apply(t, "manifest_mutate", map[string]any{"notify": "silent"}, note)
		if !p.Swallow || p.Deliver != nil {
			t.Errorf("swallow = %v deliver = %v", p.Swallow, p.Deliver)
		}
		if p.Verb() != interpose.VerbSynthesize {
			t.Errorf("verb = %q, want synthesize", p.Verb())
		}
	})
}

func TestCapabilityFlip(t *testing.T) {
	// narrow is the dangerous one: the subject cached a capability and keeps
	// using it after the peer stopped offering it.
	t.Run("narrow empties what was offered", func(t *testing.T) {
		p := apply(t, "capability_flip",
			map[string]any{"field": "capabilities", "direction_of_change": "narrow"}, parse(t, initialize))
		if strings.Contains(string(p.Deliver.Raw()), "listChanged") {
			t.Errorf("capabilities were not narrowed: %s", p.Deliver.Raw())
		}
	})

	for _, field := range []string{"capabilities", "protocol_version", "server_info", "extensions"} {
		t.Run(field, func(t *testing.T) {
			p := apply(t, "capability_flip", map[string]any{"field": field}, parse(t, initialize))
			if string(p.Deliver.Raw()) == initialize {
				t.Errorf("field %q was not flipped", field)
			}
			if !json.Valid(p.Deliver.Raw()) {
				t.Errorf("not valid JSON: %s", p.Deliver.Raw())
			}
		})
	}
}

// With the declaration in hand -- where charpy serves, it is its own -- the
// violation lands on structuredContent: the frame is still valid JSON-RPC and
// a valid CallToolResult, wrong only against the outputSchema.
func TestSchemaViolationBreaksTheDeclaredOutput(t *testing.T) {
	const declared = `{"type":"object","required":["sum"],"properties":{"sum":{"type":"number"}}}`
	const answer = `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"sum\":8}"}],"structuredContent":{"sum":8}}}`
	ctx := fault.Context{RunSeed: runSeed, OutputSchema: []byte(declared)}

	for how, check := range map[string]func(map[string]any) bool{
		"wrong_type":       func(sc map[string]any) bool { _, isString := sc["sum"].(string); return isString },
		"missing_required": func(sc map[string]any) bool { _, present := sc["sum"]; return !present },
	} {
		t.Run(how, func(t *testing.T) {
			p, err := fault.Apply(caseOf("schema_violation", map[string]any{
				"target": "declared_output_schema", "how": how,
			}), parse(t, answer), ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID     int `json:"id"`
				Result struct {
					StructuredContent map[string]any `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(p.Deliver.Raw(), &got); err != nil {
				t.Fatalf("the violation is not valid JSON: %v", err)
			}
			if got.ID != 2 {
				t.Errorf("id = %d; the envelope must be untouched", got.ID)
			}
			if !check(got.Result.StructuredContent) {
				t.Errorf("structuredContent = %v, not broken by %s", got.Result.StructuredContent, how)
			}
		})
	}

	// A result with nothing structured to break does not apply.
	_, err := fault.Apply(caseOf("schema_violation", map[string]any{"target": "declared_output_schema"}),
		parse(t, response), ctx)
	if !errors.Is(err, fault.ErrNotApplicable) {
		t.Errorf("a result without structuredContent planned as %v, want ErrNotApplicable", err)
	}
}
