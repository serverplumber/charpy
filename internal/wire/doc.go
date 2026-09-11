// Package wire is the interposer's framing layer: raw bytes on and off the
// transport, with no conformance opinions.
//
// stdio framing is newline delimited; HTTP framing is SSE events on a
// response stream. Both expose a writer taking []byte and the ability to
// stop mid-frame, which is what no SDK transport offers and therefore why
// this package exists. The reference peer never touches this layer directly:
// it speaks to the interposer, and the interposer speaks the wire. See
// docs/design/interposer.md.
//
// # The HTTP primitive is net/http, extended rather than replaced
//
// SSE writing goes through the ordinary http.ResponseWriter and
// http.NewResponseController. That is enough for every cut in the v0
// catalogue -- mid-event, at an event boundary, between event: and data:,
// inside a keep-alive comment -- because Flush puts bytes on the wire at a
// point charpy chooses. ResponseController is the extension point net/http
// already provides, so charpy adds what it needs to a stack that is tested,
// rather than hand-rolling HTTP to gain control it mostly does not need.
//
// Two deferred mechanisms do need more: a TCP reset mid-body, and chunked
// encoding that lies about its length (docs/design/faults-and-cases.md
// section 3). Both require the connection beneath the response, because
// net/http owns response framing and will neither emit a contradictory
// Content-Length nor send a reset. ResponseController.Hijack reaches it --
// over HTTP/1.1. It returns ErrNotSupported over HTTP/2, which net/http says
// it has no plan to support, and the MCP specification mandates no HTTP
// version, so a subject may well speak HTTP/2.
//
// So capability is probed, never assumed: ask the controller and handle
// ErrNotSupported. A fault charpy cannot deliver on the transport in front of
// it is a case that does not apply, which the oracle already has a verdict
// for. Silently degrading it into a weaker fault would be a test that passes
// by not running.
//
// Reaching below the response is a separate question from reaching through it.
// Over HTTP/1.1 a hijacked connection is enough; over HTTP/2 the route is
// golang.org/x/net/http2's Framer, whose AllowIllegalWrites exists upstream
// for exactly this. Both are scoped in docs/open-problems.md, neither is a
// fork, and neither is needed for the v0 catalogue.
//
// # Deadlines are per-response, not per-server
//
// A hang with withdraw_after_ms = 0 holds a response open indefinitely. An
// http.Server.WriteTimeout would cut it, and charpy's own harness bug would be
// reported as the subject's behaviour -- the failure mode ADR-001 calls out,
// where every finding becomes "probably the harness". Disabling the timeout
// server-wide would disarm it for every other request too.
//
// Instead the holding response clears its own deadline with
// ResponseController.SetWriteDeadline, so exactly one response outlives the
// server's limits and the rest keep them. Timing of the hold itself is the
// injected clock's (internal/clock), never the transport's.
//
// # No event store
//
// charpy as the reference server does not replay on Last-Event-ID, so no
// stream retains what it emitted. The spec makes replay a MAY, so declining is
// conforming -- and a client that only works against a server which replays is
// itself the finding. See docs/design/revisions.md section 4.1, which also
// sizes the store the stream/replay-* case family would need.
//
// # What "cut at N bytes" promises
//
// Bytes written, never bytes received. charpy flushes N bytes; kernel buffers,
// the OS and any intermediary sit between that and the subject's read. The
// transcript records what was sent (docs/design/transcript.md section 5) and
// nothing here is a claim about what the subject perceived.
package wire
