// Package peer is the reference peer: the official Go MCP SDK, always
// correct, never coerced into misbehaving.
//
// It plugs into the interposer at the SDK's byte layer rather than at the
// mcp.Transport interface. Transport deals in jsonrpc.Message -- already
// parsed -- and charpy's whole interposer surface is envelope.Message, which
// carries the raw bytes the wire actually saw. Re-encoding a typed message
// would make the ledger's intent column bytes charpy authored rather than
// bytes the SDK authored, and the inferred join would digest charpy's
// encoding of the SDK's frame instead of the SDK's frame. So there are two
// seams, both below Transport:
//
//   - stdio-shaped: mcp.IOTransport over an io.Pipe pair. The SDK writes
//     newline-delimited JSON; charpy reads exactly those bytes into
//     envelope.Parse. Nothing re-encodes.
//   - HTTP-shaped: mcp.StreamableClientTransport with a charpy
//     http.RoundTripper. internal/wire's SSE half applies to the bodies.
//
// The peer's era is a constructor parameter, per docs/design/interposer.md
// section 1 -- mcp.ClientSessionOptions.ProtocolVersion for a client peer,
// mcp.ServerOptions.SupportedProtocolVersions for a server one. Both are
// exported only as of v1.8.0, which is why ADR-011 pins no earlier.
package peer
