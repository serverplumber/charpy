package schema_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/schema"
)

// Every revision charpy knows must have a vendored schema that embeds, parses
// and compiles. This is cheap and it catches a truncated vendor pull the
// moment it happens rather than at the first verdict.
func TestEveryRevisionCompiles(t *testing.T) {
	for _, r := range revision.All() {
		t.Run(r.String(), func(t *testing.T) {
			raw, err := schema.For(r)
			if err != nil {
				t.Fatalf("schema.For(%q): %v", r, err)
			}
			if len(raw) < 1024 {
				t.Fatalf("schema for %q is only %d bytes; truncated pull?", r, len(raw))
			}

			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("schema for %q is not valid JSON: %v", r, err)
			}

			c := jsonschema.NewCompiler()
			url := "https://charpy.test/mcp/" + r.String() + ".json"
			if err := c.AddResource(url, doc); err != nil {
				t.Fatalf("add resource for %q: %v", r, err)
			}
			if _, err := c.Compile(url); err != nil {
				t.Fatalf("schema for %q does not compile: %v", r, err)
			}
		})
	}
}

func TestAvailableCoversEveryKnownRevision(t *testing.T) {
	got := schema.Available()
	want := revision.All()
	if len(got) != len(want) {
		t.Fatalf("vendored %d schemas, know %d revisions: %v vs %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Available()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUnknownRevisionIsRejected(t *testing.T) {
	if _, err := schema.For(revision.Revision("2099-01-01")); err == nil {
		t.Error("schema.For accepted an unknown revision")
	}
}

// Definitions the case set and the oracle key off, per era. If a vendor
// refresh silently swapped a file, this is what notices.
func TestEraDefinitionsPresent(t *testing.T) {
	tests := []struct {
		rev  revision.Revision
		want []string
	}{
		{
			rev: revision.V20251125,
			want: []string{
				"InitializeRequest", // the handshake, removed in 2026-07-28
				"InitializeResult",
			},
		},
		{
			rev: revision.V20260728,
			want: []string{
				"HeaderMismatchError",             // -32020, the gateway family
				"UnsupportedProtocolVersionError", // -32022, the fingerprint ladder
				"InputRequiredResult",             // MRTR
				"CacheableResult",                 // ttlMs / cacheScope
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.rev.String(), func(t *testing.T) {
			raw, err := schema.For(tc.rev)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("%s schema is missing %q", tc.rev, want)
				}
			}
		})
	}
}

// The handshake really is gone in the stateless era. This asserts the era
// boundary the whole revision matrix rests on.
func TestStatelessEraDroppedTheHandshake(t *testing.T) {
	raw, err := schema.For(revision.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"InitializeRequest"`) {
		t.Error("2026-07-28 schema still defines InitializeRequest; " +
			"the era table in docs/design/revisions.md assumes it was removed")
	}
}
