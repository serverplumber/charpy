// Package wire is the feral framing layer: raw bytes on and off the transport,
// with no conformance opinions.
//
// stdio framing is newline delimited; HTTP framing is SSE events on a response
// stream. Both expose a writer taking []byte and the ability to stop
// mid-frame, which is what the SDK cannot be made to do and therefore why this
// package exists.
//
// SSE writing takes direct http.ResponseWriter and http.Flusher control so a
// stream can be cut mid-event, at an event boundary, between event: and data:,
// or inside a keep-alive comment. See docs/design/faults-and-cases.md.
package wire
