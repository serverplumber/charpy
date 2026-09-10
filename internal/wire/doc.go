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
// SSE writing takes direct http.ResponseWriter and http.Flusher control so a
// stream can be cut mid-event, at an event boundary, between event: and data:,
// or inside a keep-alive comment. See docs/design/faults-and-cases.md.
package wire
