package catalogue

import (
	"fmt"

	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// Compiling is the catalogue's half of the boundary ADR-009 draws. The
// catalogue is the outer layer: it parses manifests and turns them into the
// domain packages' own types, and those packages never import it back. The
// interposer consumes compiled policy in its own terms and knows nothing about
// TOML, which is what makes the import cycle structurally impossible rather
// than merely avoided.
//
// Registry validation has already happened by the time anything here runs --
// Load rejects an unknown match key, an out-of-range enumerated value or a
// wrong-typed ordinal before a case is ever added to the catalogue. Compile
// converts; it does not re-litigate. It fails only on what conversion itself
// can discover, which in practice means a manifest that reached here without
// going through Load.

// Compile turns a validated case into the interposer's terms, qualified by the
// revision it will run against and the seed driving the run.
//
// The citation is built here rather than carried in the manifest, because a
// case ID is permanent and a citation is per-run: the same case cited against
// two revisions is two citations and one identity, which is the whole reason
// the revision is not inside the ID. See docs/design/case-identity.md.
func (c Case) Compile(r revision.Revision, seed string) (interpose.Case, error) {
	cited := Citation{ID: c.parsedID, Revision: r, Seed: seed}

	m, err := compileMatch(c.Match)
	if err != nil {
		return interpose.Case{}, fmt.Errorf("case %q: %w", c.ID, err)
	}

	return interpose.Case{
		ID:               c.ID,
		Citation:         cited.String(),
		Match:            m,
		Fault:            interpose.Fault{Kind: faultKind(c.Fault), Params: faultParams(c.Fault)},
		LivenessWithinMS: livenessBudget(c.Expect),
	}, nil
}

// livenessBudget reads [case.expect].liveness_probe_within_ms, already
// validated as an integer by the loader. It is the one expect key a driver
// consumes; the rest stays oracle input.
func livenessBudget(expect map[string]any) int64 {
	switch v := expect["liveness_probe_within_ms"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// CompileAll compiles every active case that applies to a revision.
//
// Withdrawn cases and cases outside the revision's range are dropped rather
// than compiled and skipped later: a case that cannot run is not policy, and
// carrying it into the interposer would only give the matcher something to
// ignore on every frame.
func (cat *Catalogue) CompileAll(r revision.Revision, seed string) ([]interpose.Case, error) {
	var out []interpose.Case
	for _, cs := range cat.Applicable(r) {
		compiled, err := cs.Compile(r, seed)
		if err != nil {
			return nil, err
		}
		out = append(out, compiled)
	}
	return out, nil
}

func compileMatch(table map[string]any) (interpose.Match, error) {
	var m interpose.Match
	m.Scope = interpose.ScopeRun

	for k, v := range table {
		var err error
		switch k {
		case "method":
			m.Method, err = glob(k, v)
		case "client_id":
			m.ClientID, err = glob(k, v)
		case "session_id":
			m.SessionID, err = glob(k, v)
		case "face":
			var s string
			if s, err = str(k, v); err == nil {
				m.Face = transcript.Face(s)
			}
		case "direction":
			var s string
			if s, err = str(k, v); err == nil {
				m.Direction = transcript.Direction(s)
			}
		case "kind":
			var s string
			if s, err = str(k, v); err == nil {
				m.Kind = envelope.Kind(s)
			}
		case "scope":
			var s string
			if s, err = str(k, v); err == nil {
				m.Scope = interpose.Scope(s)
			}
		case "occurrence":
			m.Occurrence, err = integer(k, v)
		case "occurrence_every":
			m.Every, err = integer(k, v)
		case "after_mono_ms":
			var ms int64
			if ms, err = integer(k, v); err == nil {
				m.AfterMono = clock.FromMillis(ms)
			}
		default:
			// Unreachable through Load, which validates against the match-key
			// registry. Reachable if a manifest is compiled without it, and
			// silently ignoring the key would be the exact failure the strict
			// loader exists to prevent.
			err = fmt.Errorf("unknown key %q in [case.match]", k)
		}
		if err != nil {
			return interpose.Match{}, err
		}
	}
	return m, nil
}

func faultKind(table map[string]any) string {
	k, _ := table["kind"].(string)
	return k
}

// faultParams copies everything but the mechanism selector. The values stay
// untyped: internal/fault has validated them, and no mechanism exists yet to
// consume a typed form.
func faultParams(table map[string]any) map[string]any {
	if len(table) <= 1 {
		return nil
	}
	out := make(map[string]any, len(table)-1)
	for k, v := range table {
		if k != "kind" {
			out[k] = v
		}
	}
	return out
}

func glob(key string, v any) (interpose.Glob, error) {
	s, err := str(key, v)
	if err != nil {
		return interpose.Glob{}, err
	}
	return interpose.ParseGlob(s), nil
}

func str(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("[case.match] %s must be a string, got %T", key, v)
	}
	return s, nil
}

// integer reads a TOML integer, which go-toml decodes as int64.
func integer(key string, v any) (int64, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("[case.match] %s must be an integer, got %T", key, v)
	}
	return n, nil
}
