package catalogue

import (
	"testing"

	"github.com/serverplumber/charpy/internal/revision"
)

func TestParseID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "stream/truncate-mid-event"},
		{name: "single segment name", in: "id/duplicate"},
		{name: "digits allowed", in: "gateway/header-body-32020"},
		{name: "no family separator", in: "truncate-mid-event", wantErr: true},
		{name: "unknown family", in: "bogus/thing", wantErr: true},
		{name: "uppercase rejected", in: "stream/Truncate", wantErr: true},
		{name: "underscore rejected", in: "stream/truncate_mid", wantErr: true},
		{name: "trailing dash rejected", in: "stream/truncate-", wantErr: true},
		{name: "doubled dash rejected", in: "stream/truncate--mid", wantErr: true},
		{name: "second separator rejected", in: "stream/a/b", wantErr: true},
		{name: "empty name rejected", in: "stream/", wantErr: true},
		{name: "dashed family rejected", in: "my-family/thing", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseID(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseID(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseID(%q): %v", tc.in, err)
			}
			if got.String() != tc.in {
				t.Errorf("round trip: got %q, want %q", got.String(), tc.in)
			}
		})
	}
}

func TestParseCitation(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantErr  bool
		wantRev  revision.Revision
		wantSeed string
	}{
		{
			name:     "full citation",
			in:       "stream/truncate-mid-event@2025-11-25#seed=8f2c1a",
			wantRev:  revision.V20251125,
			wantSeed: "8f2c1a",
		},
		{
			name:    "revision only",
			in:      "id/duplicate-response@2026-07-28",
			wantRev: revision.V20260728,
		},
		{
			name:     "seed only",
			in:       "id/duplicate-response#seed=deadbeef",
			wantSeed: "deadbeef",
		},
		{name: "bare id is accepted", in: "gateway/header-rederived"},
		{name: "draft is a revision", in: "stream/cut@draft", wantRev: revision.Draft},
		{name: "unknown revision rejected", in: "stream/cut@2027-01-01", wantErr: true},
		{name: "malformed fragment rejected", in: "stream/cut#8f2c1a", wantErr: true},
		{name: "short seed rejected", in: "stream/cut#seed=abc", wantErr: true},
		{name: "non-hex seed rejected", in: "stream/cut#seed=zzzzzz", wantErr: true},
		{name: "uppercase seed rejected", in: "stream/cut#seed=8F2C1A", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCitation(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCitation(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCitation(%q): %v", tc.in, err)
			}
			if got.Revision != tc.wantRev {
				t.Errorf("Revision = %q, want %q", got.Revision, tc.wantRev)
			}
			if got.Seed != tc.wantSeed {
				t.Errorf("Seed = %q, want %q", got.Seed, tc.wantSeed)
			}
			if got.String() != tc.in {
				t.Errorf("round trip: got %q, want %q", got.String(), tc.in)
			}
		})
	}
}

func TestEveryFamilyParses(t *testing.T) {
	for _, f := range families {
		t.Run(string(f), func(t *testing.T) {
			id, err := ParseID(string(f) + "/example-case")
			if err != nil {
				t.Fatalf("family %q does not parse: %v", f, err)
			}
			if id.Family != f {
				t.Errorf("Family = %q, want %q", id.Family, f)
			}
		})
	}
}
