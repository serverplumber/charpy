package catalogue

import (
	"fmt"
	"slices"
	"strings"

	"github.com/serverplumber/charpy/internal/fault"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/oracle/invariant"
)

// The registries validated against here live with the code they describe:
// mechanisms in internal/fault, match keys and scopes in internal/interpose,
// invariants in internal/oracle/invariant. The catalogue is the outer layer —
// it imports all three and compiles manifests into their terms; none of them
// import the catalogue. See docs/design/decisions.md ADR-009.

// expectKey describes one key a [case.expect] table accepts. This registry
// stays in the catalogue because [case.expect] is manifest surface: it names
// things owned elsewhere (invariants, liveness budgets) but is itself owned
// by the format.
type expectKey struct {
	name    string
	typ     string // "integer" | "boolean" | "array"
	summary string
}

var expectKeys = []expectKey{
	{name: "invariants", typ: "array",
		summary: "Invariant names that must hold for this case. The universal invariants run regardless."},
	{name: "liveness_probe_within_ms", typ: "integer",
		summary: "Real-time budget for recovery after withdrawal. Generous by design: the verdict asserts recovery happened, not how fast."},
	{name: "expect_error_code", typ: "integer",
		summary: "The subject is expected to answer with this JSON-RPC error code."},
	{name: "expect_http_status", typ: "integer",
		summary: "The subject is expected to answer with this HTTP status. HTTP only."},
	{name: "expect_negotiated_only", typ: "boolean",
		summary: "After the faulted handshake, the subject is expected to send only requests that handshake's capabilities allow."},
}

func lookupExpectKey(name string) (expectKey, bool) {
	for _, k := range expectKeys {
		if k.name == name {
			return k, true
		}
	}
	return expectKey{}, false
}

func expectKeyNames() []string {
	out := make([]string, 0, len(expectKeys))
	for _, k := range expectKeys {
		out = append(out, k.name)
	}
	return out
}

// checkFault validates a [case.fault] table against the mechanism registry.
func checkFault(table map[string]any) []string {
	kindV, ok := table["kind"]
	if !ok {
		return []string{"[case.fault] needs a kind"}
	}
	kind, ok := kindV.(string)
	if !ok {
		return []string{"[case.fault] kind must be a string"}
	}

	m, ok := fault.Lookup(kind)
	if !ok {
		return []string{fmt.Sprintf("unknown fault kind %q; known mechanisms: %s",
			kind, strings.Join(fault.Kinds(), ", "))}
	}

	var errs []string
	for k, v := range table {
		if k == "kind" {
			continue
		}
		p, known := m.Param(k)
		if !known {
			errs = append(errs, fmt.Sprintf("unknown key %q in [case.fault]; kind %q accepts: %s",
				k, kind, strings.Join(m.ParamNames(), ", ")))
			continue
		}
		if p.Values == nil {
			if e := checkType(v, p.Type, "[case.fault]", k); e != "" {
				errs = append(errs, e)
			}
			continue
		}
		s, ok := v.(string)
		if !ok {
			errs = append(errs, fmt.Sprintf("[case.fault] %s must be a string", k))
			continue
		}
		if !p.Allows(s) {
			errs = append(errs, fmt.Sprintf("[case.fault] %s = %q is not valid for kind %q; accepts: %s",
				k, s, kind, strings.Join(p.ValueNames(), ", ")))
		}
	}
	return errs
}

// checkMatch validates a [case.match] table against the interposer's
// matcher-key registry.
func checkMatch(table map[string]any) []string {
	var errs []string
	for k, v := range table {
		mk, known := interpose.LookupMatchKey(k)
		if !known {
			errs = append(errs, fmt.Sprintf("unknown key %q in [case.match]; accepts: %s",
				k, strings.Join(interpose.MatchKeyNames(), ", ")))
			continue
		}
		if mk.Values == nil {
			if e := checkType(v, mk.Type, "[case.match]", k); e != "" {
				errs = append(errs, e)
			}
			continue
		}
		s, ok := v.(string)
		if !ok || !slices.Contains(mk.Values, s) {
			errs = append(errs, fmt.Sprintf("[case.match] %s = %v is not valid; accepts: %s",
				k, v, strings.Join(mk.Values, ", ")))
		}
	}
	return errs
}

// checkExpect validates a [case.expect] table, including that every named
// invariant is registered: an expectation that references nothing is worse
// than none.
func checkExpect(table map[string]any) []string {
	var errs []string
	for k, v := range table {
		ek, known := lookupExpectKey(k)
		if !known {
			errs = append(errs, fmt.Sprintf("unknown key %q in [case.expect]; accepts: %s",
				k, strings.Join(expectKeyNames(), ", ")))
			continue
		}
		if ek.typ != "array" {
			if e := checkType(v, ek.typ, "[case.expect]", k); e != "" {
				errs = append(errs, e)
			}
			continue
		}
		// invariants
		list, ok := v.([]any)
		if !ok {
			errs = append(errs, "[case.expect] invariants must be an array of strings")
			continue
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				errs = append(errs, "[case.expect] invariants must be an array of strings")
				continue
			}
			if !invariant.Known(name) {
				errs = append(errs, fmt.Sprintf("unknown invariant %q in [case.expect]", name))
			}
		}
	}
	return errs
}

// checkRecipient validates that a case's fault reaches every subject class it
// lists.
//
// A damaged frame is a question put to whoever receives it (ADR-013). A case
// whose direction delivers its fault to charpy's own peer instead arms, fires,
// and reports nothing -- and nothing renders exactly like a subject that
// passed. That is an authoring error, true of the case on every run, so it is
// refused here rather than reported in every run's output. It is why the
// direction must be stated at all: a matcher that leaves it open selects
// frames going both ways, and half of them reach charpy.
func checkRecipient(subjects []Subject, match map[string]any) []string {
	dir, _ := match["direction"].(string)
	face, _ := match["face"].(string)
	if dir == "" {
		return []string{"[case.match] needs a direction: a damaged frame is put to whoever " +
			"receives it, and without one the case also damages frames charpy's own peer receives"}
	}

	var errs []string
	for _, s := range subjects {
		var ok bool
		var want string
		switch s {
		case SubjectServer:
			ok, want = dir == "c2s" && (face == "" || face == "downstream"),
				`direction = "c2s" (face "downstream", if given)`
		case SubjectClient:
			ok, want = dir == "s2c" && (face == "" || face == "upstream"),
				`direction = "s2c" (face "upstream", if given)`
		case SubjectGateway:
			// A gateway receives on both of its faces, so the face is what
			// says which side of it the fault is put to; left open, the
			// matcher also damages what the gateway itself sends.
			ok, want = (face == "upstream" && dir == "s2c") || (face == "downstream" && dir == "c2s"),
				`direction = "s2c" on face "upstream", or "c2s" on face "downstream"`
		default:
			continue
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("its fault does not reach a %s subject, which receives "+
				"%s; this case matches direction = %q, face = %q", s, want, dir, face))
		}
	}
	return errs
}

// checkActs validates that a matcher selects only frames the mechanism can act
// on. A mechanism that refuses a frame at run time says ErrNotApplicable, and
// the case reads as one that never applied; a case built to select such frames
// is wrong on every run, so the loader says so.
func checkActs(faultTable, match map[string]any) []string {
	kind, _ := faultTable["kind"].(string)
	m, ok := fault.Lookup(kind)
	if !ok {
		return nil // checkFault has already said so
	}
	acts := m.ActsOn(faultTable)
	if acts == nil {
		return nil
	}
	names := make([]string, len(acts))
	for i, k := range acts {
		names[i] = string(k)
	}
	got, _ := match["kind"].(string)
	if got == "" {
		return []string{fmt.Sprintf("kind %q acts only on %s frames, so [case.match] must name "+
			"one with kind", kind, strings.Join(names, " or "))}
	}
	if !slices.Contains(names, got) {
		return []string{fmt.Sprintf("kind %q acts only on %s frames, and [case.match] selects %s",
			kind, strings.Join(names, " or "), got)}
	}
	return nil
}

// checkType validates a free-form value against a registry-declared type.
// go-toml decodes integers as int64 and booleans as bool.
func checkType(v any, typ, where, key string) string {
	ok := false
	switch typ {
	case "string":
		_, ok = v.(string)
	case "integer":
		_, ok = v.(int64)
	case "boolean":
		_, ok = v.(bool)
	}
	if !ok {
		return fmt.Sprintf("%s %s must be a %s", where, key, typ)
	}
	return ""
}
