// Package proxy is the reverse-proxy driver: charpy sits in front of an HTTP
// server or gateway and relays both directions.
//
// Because charpy forwards the bytes itself, it stamps its own join id on both
// copies of a frame, so correlation here is authoritative (join.via =
// "forwarded"). Contrast the gateway-under-test case, where the subject
// forwards and correlation must fall back to trace context.
package proxy
