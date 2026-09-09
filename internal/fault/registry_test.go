package fault

import "testing"

func TestRegistryIsWellFormed(t *testing.T) {
	kinds := map[string]bool{}
	for _, m := range Mechanisms() {
		if kinds[m.Kind] {
			t.Errorf("duplicate mechanism kind %q", m.Kind)
		}
		kinds[m.Kind] = true
		if m.Summary == "" {
			t.Errorf("mechanism %q has no summary; the generated schema needs one", m.Kind)
		}

		params := map[string]bool{}
		for _, p := range m.Params {
			if p.Name == "kind" {
				// "kind" selects the mechanism itself in a [case.fault] table,
				// so a parameter by that name could never be set.
				t.Errorf("mechanism %q has a parameter named \"kind\"", m.Kind)
			}
			if params[p.Name] {
				t.Errorf("mechanism %q: duplicate parameter %q", m.Kind, p.Name)
			}
			params[p.Name] = true
			if p.Summary == "" {
				t.Errorf("mechanism %q: parameter %q has no summary", m.Kind, p.Name)
			}

			switch {
			case p.Values == nil:
				if p.Type != "string" && p.Type != "integer" && p.Type != "boolean" {
					t.Errorf("mechanism %q: free-form parameter %q needs a Type, got %q",
						m.Kind, p.Name, p.Type)
				}
			case p.Type != "":
				t.Errorf("mechanism %q: enumerated parameter %q must not also set Type",
					m.Kind, p.Name)
			}

			values := map[string]bool{}
			for _, v := range p.Values {
				if values[v.Name] {
					t.Errorf("mechanism %q: parameter %q: duplicate value %q", m.Kind, p.Name, v.Name)
				}
				values[v.Name] = true
				if v.Summary == "" {
					t.Errorf("mechanism %q: parameter %q: value %q has no summary", m.Kind, p.Name, v.Name)
				}
			}
			if p.Default != "" && p.Values != nil && !p.Allows(p.Default) {
				t.Errorf("mechanism %q: parameter %q: default %q is not a permitted value",
					m.Kind, p.Name, p.Default)
			}
		}
	}
}

func TestLookup(t *testing.T) {
	if _, ok := Lookup("truncate"); !ok {
		t.Error("truncate should resolve")
	}
	if _, ok := Lookup("nonsense"); ok {
		t.Error("nonsense should not resolve")
	}
	m, _ := Lookup("truncate")
	p, ok := m.Param("cut_at")
	if !ok {
		t.Fatal("truncate should have cut_at")
	}
	if !p.Allows("event_boundary") || p.Allows("nonsense") {
		t.Error("cut_at value check is wrong")
	}
}
