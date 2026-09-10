// Package envelope is the shared JSON-RPC message model.
//
// It is shared by the reference peer, the interposer and the synthesis
// templates, and is the only thing they share. json.RawMessage is preserved
// throughout: charpy must be able to re-emit a frame byte for byte, and must
// represent ids and payloads that no conforming implementation would produce.
//
// The JSON-RPC id is carried as canonical JSON text plus a type tag rather
// than as a Go any, so that the transcript's id column has one type. See
// docs/design/transcript.md section 5.
package envelope
