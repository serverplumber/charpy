// Package schema embeds the vendored MCP JSON Schemas, one per protocol
// revision.
//
// Vendored rather than fetched: charpy must produce identical verdicts
// offline and years from now. See VENDORED.md.
package schema

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/serverplumber/charpy/internal/revision"
)

//go:embed */schema.json
var files embed.FS

// For returns the raw JSON Schema document for a revision.
func For(r revision.Revision) ([]byte, error) {
	if !revision.Known(r) {
		return nil, fmt.Errorf("schema: unknown revision %q", r)
	}
	b, err := fs.ReadFile(files, string(r)+"/schema.json")
	if err != nil {
		return nil, fmt.Errorf("schema: %q is not vendored: %w", r, err)
	}
	return b, nil
}

// Available reports which revisions have a vendored schema. Every revision in
// revision.All() should appear; a gap means a truncated vendor pull.
func Available() []revision.Revision {
	var out []revision.Revision
	for _, r := range revision.All() {
		if _, err := fs.Stat(files, string(r)+"/schema.json"); err == nil {
			out = append(out, r)
		}
	}
	return out
}
