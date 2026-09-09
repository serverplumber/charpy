package invariant

import "testing"

func TestRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, i := range All() {
		if seen[i.Name] {
			t.Errorf("duplicate invariant %q", i.Name)
		}
		seen[i.Name] = true
		if i.Summary == "" {
			t.Errorf("invariant %q has no summary; the generated schema needs one", i.Name)
		}
		if i.SecondWave && !i.RequiresCorrelation {
			// The second wave is the stateless-era gateway set; every one of
			// them needs both faces. A non-correlated second-wave invariant
			// would be a new category and deserves a deliberate decision.
			t.Errorf("invariant %q: second wave but not correlated — is that intended?", i.Name)
		}
	}
	if n := len(All()); n != 13 {
		t.Errorf("registry has %d invariants, oracle.md defines 13", n)
	}
}
