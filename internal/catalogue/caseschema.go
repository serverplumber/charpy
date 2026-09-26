package catalogue

import (
	"encoding/json"
	"fmt"

	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
	"github.com/serverplumber/charpy/internal/revision"
)

// CaseSchema renders the case-manifest format as a JSON Schema document,
// generated from the same registries the loader validates against, so the
// two cannot drift. The committed copy at cases/case.schema.json is what the
// `#:schema` directive in each manifest points Taplo at; a test fails when
// it is stale. See docs/design/policy-format.md section 6.
//
// The schema covers structure: keys, types, enums, required fields. The
// cross-file and semantic rules — id uniqueness, applies_to resolution,
// MUST-requires-schema-source — remain the loader's job.
func CaseSchema() ([]byte, error) {
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://raw.githubusercontent.com/serverplumber/charpy/main/cases/case.schema.json",
		"title":   "charpy case manifest",
		"description": "Generated from charpy's mechanism, matcher and invariant registries " +
			"by `just case-schema`. Do not edit by hand.",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"schema_version"},
		"properties": map[string]any{
			"schema_version": map[string]any{
				"const":       1,
				"description": "Manifest format version.",
			},
			"case": map[string]any{
				"type":        "array",
				"description": "The case entries. One [[case]] per case.",
				"items":       caseSchema(),
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("caseschema: %w", err)
	}
	return append(b, '\n'), nil
}

func caseSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"id", "applies_to", "subject", "verdict", "derives_from", "summary", "fault",
		},
		"properties": map[string]any{
			"id": map[string]any{
				"type":    "string",
				"pattern": "^[a-z0-9]+/[a-z0-9]+(-[a-z0-9]+)*$",
				"description": "Permanent case ID, family/name. Never contains a date and never " +
					"changes once released; revision and seed qualify it at citation time. " +
					"See docs/design/case-identity.md.",
			},
			"applies_to": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Revision range this case applies to, resolved against "+
					"the ordered revision list (%s). Forms: \"*\", \"=R\", \">=R\", \">=R,<R\".",
					revisionList()),
			},
			"subject": map[string]any{
				"type":        "array",
				"minItems":    1,
				"items":       map[string]any{"enum": []string{"server", "client", "gateway"}},
				"description": "Subject classes the case applies to.",
			},
			"transport": map[string]any{
				"type":        "array",
				"items":       map[string]any{"enum": []string{"stdio", "http"}},
				"description": "Transports the case applies to. Defaults to both.",
			},
			"observed_by": map[string]any{
				"type":  "array",
				"items": map[string]any{"enum": Observers},
				"description": "What can see the subject's answer to this case. Defaults to the wire. " +
					"Selection drops a case no available observer can judge, rather than arming a fault " +
					"whose answer nobody can see.",
			},
			"verdict": map[string]any{
				"anyOf": []any{
					constWithDoc("MUST", "Schema-mechanical only: requires a schema: source, "+
						"enforced at load. charpy never issues a behavioural MUST."),
					constWithDoc("OBSERVED", "Everything else, with the divergence table attached "+
						"where one exists."),
				},
				"description": "Which verdict bucket a finding lands in. Two buckets, nothing between.",
			},
			"derives_from": map[string]any{
				"type":    "string",
				"pattern": "^(spec:.+|sep:.+|schema:.+|none)$",
				"description": "Where the expectation comes from: spec:, sep:, schema:, or the " +
					"deliberately ugly \"none\" for behaviour with no normative source.",
			},
			"summary": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "One sentence; appears in the report and in JUnit.",
			},
			"status": map[string]any{
				"enum":        []string{"active", "withdrawn"},
				"description": "Withdrawn cases still parse, so old transcripts keep resolving.",
			},
			"withdrawn_reason": map[string]any{
				"type":        "string",
				"description": "Required when status = \"withdrawn\".",
			},
			"match":  matchSchema(),
			"fault":  faultSchema(),
			"expect": expectSchema(),
		},
	}
}

func matchSchema() map[string]any {
	props := map[string]any{}
	for _, k := range interpose.MatchKeys() {
		props[k.Name] = keySchema(k.Summary, k.Type, k.Values)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"description": "Selects which frame the fault attaches to. All present keys must " +
			"match; absent keys match anything. Empty matches the first frame on any face, " +
			"which is almost never intended.",
	}
}

func faultSchema() map[string]any {
	var branches []any
	for _, m := range fault.Mechanisms() {
		props := map[string]any{
			"kind": constWithDoc(m.Kind, m.Summary),
		}
		for _, p := range m.Params {
			summary := p.Summary
			if p.Default != "" {
				summary += " Default: " + p.Default + "."
			}
			if p.Values != nil {
				props[p.Name] = enumWithDocs(summary, p.Values)
			} else {
				props[p.Name] = keySchema(summary, p.Type, nil)
			}
		}
		branches = append(branches, map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"kind"},
			"properties":           props,
		})
	}
	return map[string]any{
		"oneOf": branches,
		"description": "The mechanism and its parameters. kind selects the mechanism; the " +
			"remaining keys are that mechanism's parameters. See docs/design/faults-and-cases.md.",
	}
}

func expectSchema() map[string]any {
	var invariants []any
	for _, i := range invariant.All() {
		invariants = append(invariants, constWithDoc(i.Name, i.Summary))
	}
	props := map[string]any{}
	for _, k := range expectKeys {
		if k.typ == "array" {
			props[k.name] = map[string]any{
				"type":        "array",
				"items":       map[string]any{"anyOf": invariants},
				"description": k.summary,
			}
			continue
		}
		props[k.name] = keySchema(k.summary, k.typ, nil)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"description": "What the oracle should check for this case. Optional; the universal " +
			"invariants run regardless.",
	}
}

// keySchema renders a plain typed or enumerated key.
func keySchema(summary, typ string, values []string) map[string]any {
	if values != nil {
		return map[string]any{"enum": values, "description": summary}
	}
	return map[string]any{"type": typ, "description": summary}
}

// enumWithDocs renders an enumerated key whose values carry their own
// documentation, as anyOf-of-consts so editors show it on hover.
func enumWithDocs(summary string, values []fault.Value) map[string]any {
	var branches []any
	for _, v := range values {
		branches = append(branches, constWithDoc(v.Name, v.Summary))
	}
	return map[string]any{"anyOf": branches, "description": summary}
}

func constWithDoc(value, doc string) map[string]any {
	return map[string]any{"const": value, "description": doc}
}

func revisionList() string {
	s := ""
	for i, r := range revision.All() {
		if i > 0 {
			s += ", "
		}
		s += string(r)
	}
	return s
}
