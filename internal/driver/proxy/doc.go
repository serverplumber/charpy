// Package proxy is the reverse-proxy driver: charpy presents an HTTP endpoint,
// forwards to a real server, and corrupts what crosses in between.
//
// Because charpy forwards the bytes itself, it stamps its own join id on both
// copies of a frame, so correlation here is authoritative (link.via =
// "forwarded"). Contrast the gateway-under-test case, where the subject
// forwards and correlation must fall back to trace context.
//
// One face in v0: charpy sits in front of one server, and its stimulus is the
// scenario player -- charpy's own peer, connecting to charpy's listener,
// which forwards to the subject. Two real HTTP hops, so wire's SSE half
// carries real traffic and the correlation ledger sees a real frame for the
// first time. In front of a *gateway* would need charpy in front of the
// gateway's upstreams too, with a server-side reference peer that does not yet
// exist; that is a later item.
//
// The fault-application core here is deliberately a sibling of the stdio
// shim's rather than a shared one. The two look alike -- match a frame, run
// the verb dance, record it -- but the shim writes to one persistent
// bidirectional pipe while the proxy writes to a per-request response that is
// application/json or text/event-stream and then ends, where "then close"
// means ending an HTTP response rather than closing a socket. Whether they
// are one job is a question two working drivers can answer and one plus a
// guess cannot; the duplication is intentional and scoped to be lifted once
// both exist. See notes/checklist.md.
package proxy
