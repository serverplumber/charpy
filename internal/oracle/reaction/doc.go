// Package reaction is oracle layer 4: what the subject did after a fault
// reached it.
//
// A damaged frame is a question put to whoever receives it, and the answer is
// never inside the damaged frame -- layers 1 and 2 rightly exclude that frame,
// because charpy wrote it. The answer is in what the recipient does next. So
// this layer is anchored at each fault_applied event and judges only the
// subject's own frames after it, on the same connection
// (docs/design/decisions.md ADR-013).
//
// Two checks, both OBSERVED at most, because neither has a generated artifact
// behind it:
//
//   - reaction: did the subject answer a request asked after the fault, stop
//     answering, or exit? Every fault that reached the subject gets an answer,
//     with no per-case authoring. A fault that reached charpy's own peer is
//     SKIPPED with that reason rather than read as a pass.
//   - recovery: did the subject answer a probe on a fresh session once charpy
//     stopped interfering? This was the whole of layer 4 when it was called
//     liveness. Everyone tests that a fault does not crash the subject; almost
//     nobody tests recovery, and recovery is the production question.
//
// Both report success as well as failure. "Answered 3ms after the fault" is the
// observation worth making, not a silence.
package reaction
