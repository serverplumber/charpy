// Package cases embeds the shipped case catalogue.
//
// Embedded rather than read from disk: charpy is a single static binary you
// drop into any CI, and a suite whose cases live beside the binary is a suite
// that runs differently depending on where it was invoked from. User-supplied
// policies are read from the filesystem; what charpy ships travels with it.
package cases

import "embed"

//go:embed *.toml
var FS embed.FS
