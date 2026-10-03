# Vendored MCP specification prose

The specification text for every protocol revision charpy supports, copied
from `modelcontextprotocol/modelcontextprotocol` at
`docs/specification/<revision>/`, `.mdx` files only (two screenshot PNGs
are deliberately excluded).

Vendored for the same reason as the schemas (`../schema/VENDORED.md`), plus
one of its own: the design docs quote normative clauses and `derives_from`
cites `spec:` paths, and a citation that resolves only against a moving
external website is a citation that rots. Every clause charpy's documents
rely on is checkable in-repo, offline, at the exact commit recorded here.

## Provenance

- Source: <https://github.com/modelcontextprotocol/modelcontextprotocol>
- Commit: `aa8ce049f089f92618340190d4ece141f663310d` (same pull as the
  schemas)
- Pulled: 2026-09-09
- Contents: 142 `.mdx` files across `2024-11-05`, `2025-03-26`,
  `2025-06-18`, `2025-11-25`, `2026-07-28`, `draft`

## Refreshing

```text
just vendor-specs
```

Manual by design, pinned to the commit in the justfile: bump `mcp_commit`
there, run `just vendor-specs` and `just vendor-schemas` together so prose
and schema never come from different commits, update both VENDORED.md
files, and re-check the claims below.

## Claims checked against this pull

The design docs assert protocol facts that constrain the architecture. Each
was verified against this vendored text on 2026-09-09. Line numbers are
valid for this pull only; re-verify and update when refreshing.

| Claim (where asserted) | Verified at |
|---|---|
| SEP-2575 (stateless core) and SEP-2567 (session removal) are separate changes, both landed (`revisions.md` §4) | `2026-07-28/changelog.mdx` items 1–2 |
| `initialize`/`notifications/initialized` removed; `_meta` carries `protocolVersion` and `clientCapabilities` per request (`revisions.md` §4) | `2026-07-28/changelog.mdx` item 2 |
| `Mcp-Session-Id` removed outright (`revisions.md` §4, oracle I6 skip) | `2026-07-28/changelog.mdx` item 1 |
| `server/discover` exists and servers MUST implement it; `ping` removed (`oracle.md` §6 probes) | `2026-07-28/changelog.mdx` items 3, 5 |
| Log level per request via `_meta`; servers MUST NOT emit `notifications/message` without it (oracle I12) | `2026-07-28/changelog.mdx` item 5 |
| `subscriptions/listen` replaces the GET stream and `resources/subscribe`; notifications tagged `io.modelcontextprotocol/subscriptionId` (oracle I10, `manifest_mutate` notify param) | `2026-07-28/changelog.mdx` item 4 |
| SSE resumability and `Last-Event-ID` removed; a broken stream loses the request and the client MUST re-issue with a new id (`truncate` cases) | `2026-07-28/changelog.mdx` item 9; `basic/transports/streamable-http.mdx:157,687` |
| On 2026-07-28 HTTP, closing the response stream is itself the cancellation signal; `notifications/cancelled` is stdio-only (oracle I4, `transcript.md` event lines) | `2026-07-28/basic/transports/streamable-http.mdx:97–101` |
| MRTR: `resultType` required, `complete` \| `input_required`; `inputRequests`/`inputResponses`; `requestState` across retries (oracle I11, `revisions.md` §4) | `2026-07-28/changelog.mdx` items 7–8 (SEP-2322) |
| `CacheableResult`: `ttlMs` and `cacheScope` required on list and read results (oracle I9) | `2026-07-28/changelog.mdx` item (SEP-2549) |
| Unrecognised `Mcp-Param-*` MUST be forwarded; policy-enforcing intermediaries SHOULD verify `MCP-Protocol-Version` or reject; header/body mismatch MUST be rejected `400` + `-32020` (`faults-and-cases.md` §5, quoted verbatim) | `2026-07-28/basic/transports/streamable-http.mdx:548–560,642–646` |
| Error renumbering: `HeaderMismatch` `-32001`→`-32020`, `MissingRequiredClientCapability` `-32003`→`-32021`, `UnsupportedProtocolVersion` `-32004`→`-32022`; resource-not-found `-32002`→`-32602`; `-32020`…`-32099` reserved (`revisions.md` §5) | `2026-07-28/changelog.mdx` |
| `-32042` `URLElicitationRequiredError` exists on 2025-11-25 (`revisions.md` §5) | `2025-11-25/client/elicitation.mdx:426` |
| `traceparent`/`tracestate`/`baggage` in `_meta` are an explicit exception to the prefix rule (ADR-005) | `2026-07-28/basic/index.mdx:421` |

No contradictions found: every checked assertion in the design docs matches
the vendored text.
