package catalogue

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The committed schema is what the #:schema directives point Taplo at, so it
// must match what the registries currently say.
func TestCaseSchemaIsCurrent(t *testing.T) {
	want, err := CaseSchema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../cases/case.schema.json")
	if err != nil {
		t.Fatalf("cases/case.schema.json missing: %v; run `just case-schema`", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("cases/case.schema.json is stale; run `just case-schema`")
	}
}

// The generated schema and the loader validate the same format from the same
// registries, so everything the loader accepts must also pass the schema.
// This is the guard that keeps the editor tooling honest.
func TestShippedCatalogueMatchesSchema(t *testing.T) {
	compiled := compileCaseSchema(t)

	manifests, err := filepath.Glob("../../cases/*.toml")
	if err != nil || len(manifests) == 0 {
		t.Fatalf("no case manifests found: %v", err)
	}
	for _, path := range manifests {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := toml.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			// Round-trip through JSON so the validator sees the value shapes
			// a JSON document would produce (float64 numbers, not int64).
			j, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var v any
			if err := json.Unmarshal(j, &v); err != nil {
				t.Fatal(err)
			}
			if err := compiled.Validate(v); err != nil {
				t.Errorf("does not validate against the generated schema:\n%v", err)
			}
		})
	}
}

// A schema that accepts everything would pass the test above vacuously, so
// check it actually rejects the mistakes it exists to catch.
func TestCaseSchemaRejects(t *testing.T) {
	compiled := compileCaseSchema(t)

	valid := func() map[string]any {
		return map[string]any{
			"schema_version": float64(1),
			"case": []any{map[string]any{
				"id": "stream/thing", "applies_to": "*",
				"subject": []any{"server"}, "verdict": "OBSERVED",
				"derives_from": "none", "summary": "s",
				"fault": map[string]any{"kind": "truncate", "cut_at": "mid_event"},
			}},
		}
	}
	if err := compiled.Validate(valid()); err != nil {
		t.Fatalf("baseline document should validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown fault param", func(d map[string]any) {
			cs := d["case"].([]any)[0].(map[string]any)
			cs["fault"].(map[string]any)["cut_after"] = float64(91)
		}},
		{"enum value from the wrong mechanism", func(d map[string]any) {
			cs := d["case"].([]any)[0].(map[string]any)
			cs["fault"].(map[string]any)["cut_at"] = "double_response"
		}},
		{"unknown case key", func(d map[string]any) {
			d["case"].([]any)[0].(map[string]any)["sujbect"] = []any{"server"}
		}},
		{"bad id shape", func(d map[string]any) {
			d["case"].([]any)[0].(map[string]any)["id"] = "stream/Truncate_MidEvent"
		}},
		{"unknown invariant", func(d map[string]any) {
			cs := d["case"].([]any)[0].(map[string]any)
			cs["expect"] = map[string]any{"invariants": []any{"id-resolves-twice"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := valid()
			tc.mutate(doc)
			if err := compiled.Validate(doc); err == nil {
				t.Error("schema accepted a document the loader would reject")
			}
		})
	}
}

func compileCaseSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := CaseSchema()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("case.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile("case.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}
