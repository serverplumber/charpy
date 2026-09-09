// Package transcript writes and reads the JSONL transcript.
//
// The transcript is the product; the oracle, the report and any external
// tooling are consumers of it. Three line types share one monotonic sequence:
// header, frame, and event. Events are load-bearing rather than decorative --
// under 2026-07-28 Streamable HTTP, closing a response stream is itself the
// cancellation signal, so an oracle reading only frames cannot evaluate
// cancellation at all.
//
// Writes are buffered and asynchronous so nothing in the frame path blocks on
// I/O. The file is append-only, and a run that crashes leaves a valid prefix
// that the oracle can still read.
//
// Schema in schema/transcript/v1.json; rationale in
// docs/design/transcript.md.
package transcript
