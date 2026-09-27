// Package proxy is the reverse-proxy driver: charpy presents an HTTP endpoint,
// forwards to a real server, and corrupts what crosses in between.
//
// Because charpy forwards the bytes itself, it stamps its own join id on both
// copies of a frame, so correlation here is authoritative (link.via =
// "forwarded"). Contrast the gateway-under-test case, where the subject
// forwards and correlation must fall back to trace context.
//
// One face: charpy sits in front of one server, and its stimulus is the
// scenario player -- charpy's own peer, connecting to charpy's listener,
// which forwards to the subject. Two real HTTP hops, so wire's SSE half
// carries real traffic. Faults go both ways: a request is faulted on its way
// to the server, which is where a server subject is asked its questions, and
// a response on its way back. The hostile HTTP mode reuses this proxy in
// front of charpy's own reference server, with the client as the subject. In
// front of a *gateway* is the gateway driver, which is not built.
//
// The bookkeeping this driver and the stdio shim did identically lives in
// driver/exchange. What stays here is the verb dance, which is where the two
// genuinely differ: the shim writes to one persistent bidirectional pipe,
// while the proxy writes to a per-request response that is application/json
// or text/event-stream and then ends, where "then close" means ending an HTTP
// response rather than closing a socket.
package proxy
