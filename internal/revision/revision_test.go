package revision

import "testing"

func TestOrderedListIsCoherent(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("no revisions declared")
	}
	if got := all[len(all)-1]; got != Draft {
		t.Errorf("draft must sort last so open-ended ranges include it; last is %q", got)
	}
	seen := map[Revision]bool{}
	for _, r := range all {
		if seen[r] {
			t.Errorf("duplicate revision %q", r)
		}
		seen[r] = true
	}
}

func TestEra(t *testing.T) {
	tests := []struct {
		rev  Revision
		want Era
	}{
		{V20241105, Sessioned},
		{V20250326, Sessioned},
		{V20250618, Sessioned},
		{V20251125, Sessioned},
		{V20260728, Stateless},
		{Draft, Stateless},
	}
	for _, tc := range tests {
		t.Run(tc.rev.String(), func(t *testing.T) {
			if got := tc.rev.Era(); got != tc.want {
				t.Errorf("Era() = %v, want %v", got, tc.want)
			}
			if got := tc.rev.Stateless(); got != (tc.want == Stateless) {
				t.Errorf("Stateless() = %v, want %v", got, tc.want == Stateless)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
		include []Revision
		exclude []Revision
	}{
		{
			name:    "wildcard matches everything",
			expr:    "*",
			include: []Revision{V20241105, V20251125, V20260728, Draft},
		},
		{
			name:    "open ended includes draft",
			expr:    ">=2025-03-26",
			include: []Revision{V20250326, V20250618, V20251125, V20260728, Draft},
			exclude: []Revision{V20241105},
		},
		{
			name:    "exact pin",
			expr:    "=2026-07-28",
			include: []Revision{V20260728},
			exclude: []Revision{V20251125, Draft},
		},
		{
			name:    "sessioned era as a half open range",
			expr:    ">=2025-03-26,<2026-07-28",
			include: []Revision{V20250326, V20250618, V20251125},
			exclude: []Revision{V20241105, V20260728, Draft},
		},
		{
			name:    "negation",
			expr:    "!=2026-07-28",
			include: []Revision{V20251125, Draft},
			exclude: []Revision{V20260728},
		},
		{name: "empty is an error", expr: "", wantErr: true},
		{name: "bare revision without an operator is an error", expr: "2025-11-25", wantErr: true},
		{name: "unknown revision is an error", expr: ">=2027-01-01", wantErr: true},
		{name: "operator with no revision is an error", expr: ">=", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rg, err := ParseRange(tc.expr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRange(%q) = nil error, want error", tc.expr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRange(%q): %v", tc.expr, err)
			}
			for _, r := range tc.include {
				if !rg.Includes(r) {
					t.Errorf("%q should include %q", tc.expr, r)
				}
			}
			for _, r := range tc.exclude {
				if rg.Includes(r) {
					t.Errorf("%q should exclude %q", tc.expr, r)
				}
			}
		})
	}
}

func TestRangeExcludesUnknownRevisions(t *testing.T) {
	rg, err := ParseRange("*")
	if err != nil {
		t.Fatal(err)
	}
	if rg.Includes(Revision("2099-01-01")) {
		t.Error("an unknown revision must never be included, even by the wildcard")
	}
}

func TestRangeRevisionsAreOrdered(t *testing.T) {
	rg, err := ParseRange(">=2025-06-18,<2026-07-28")
	if err != nil {
		t.Fatal(err)
	}
	got := rg.Revisions()
	want := []Revision{V20250618, V20251125}
	if len(got) != len(want) {
		t.Fatalf("Revisions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Revisions() = %v, want %v", got, want)
		}
	}
}
