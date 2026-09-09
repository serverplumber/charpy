// Command gencaseschema writes the case-manifest JSON Schema, generated from
// the mechanism, matcher and invariant registries. Run via `just case-schema`;
// a test fails when the committed copy is stale.
package main

import (
	"fmt"
	"os"

	"github.com/serverplumber/charpy/internal/catalogue"
)

func main() {
	b, err := catalogue.CaseSchema()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		os.Stdout.Write(b)
		return
	}
	if err := os.WriteFile(os.Args[1], b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
