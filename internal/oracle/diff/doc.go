// Package diff is oracle layer 3: the cross-SDK differential.
//
// The same scripted scenario runs against reference peers built on the Go,
// TypeScript and Python SDKs, each a subprocess over stdio, same wire to all
// three. Agreement among three independent implementations is strong evidence
// of intended behaviour; disagreement is underspecification found
// mechanically.
//
// Frames are normalised before diffing -- ids, timestamps and serverInfo
// differ by construction. Output is a divergence table, which is a
// contribution to the spec process rather than a competing verdict.
package diff
