# Revisions

Status: **Decided.** The protocol shipped five revisions in under two years, and another should be
assumed to land mid-build. Revision handling is therefore not a detail to be added later; every
oracle layer keys off the negotiated revision.

---

## 1. The revision list

Ordered. This ordering — not date arithmetic — is what `applies_to` ranges resolve against, so a
future revision inserted here is picked up by every open-ended range automatically.

| # | Revision | Era | charpy v0 |
|---|---|---|---|
| 0 | `2024-11-05` | sessioned, HTTP+SSE transport | schema vendored, no cases |
| 1 | `2025-03-26` | sessioned, Streamable HTTP introduced | schema vendored, few cases |
| 2 | `2025-06-18` | sessioned | **primary target** |
| 3 | `2025-11-25` | sessioned | **primary target** |
| 4 | `2026-07-28` | stateless | supported, case set second wave |
| 5 | `draft` | moving | schema vendored, sorts last |

**v0 is sessioned-first.** That is where the deployed population is, where the classic gateway seam
bugs live — session identity mapping, `Mcp-Session-Id` handling, `Last-Event-ID` resumability, the
standalone GET SSE stream — and therefore where real findings arrive fastest. 2026-07-28 transport
support and fingerprinting ship in v0 so nothing rots; its distinctive case family
(`faults-and-cases.md` §5) is the second wave.

`draft` always sorts last and is always the highest revision. Cases with `applies_to = ">=X"`
therefore include `draft`, which is intentional: charpy should break loudly when the draft moves, not
silently stop testing it.

**Nothing speaks `draft` on charpy's own side.** The reference peer (ADR-011) tracks the five dated
revisions and no more, so a case that resolves to draft alone has no peer to carry it under owned
stimulus. That outcome is `SKIPPED` with the reason, not `UNTRIGGERED`: the fault did not fail to
match, it never had a peer. Relayed stimulus is unaffected — the traffic is somebody else's and
charpy only observes it. `internal/peer` asserts the two revision lists agree, in both directions,
so the day the SDK adds a revision charpy has not vendored is the day the build fails rather than
the day the coverage quietly narrows (§7).

---

## 2. Schema vendoring

Upstream keeps `schema/<revision>/` with `schema.ts`, `schema.json` and `schema.mdx`. `schema.ts` is
the source of truth and `schema.json` is generated from it.

**Vendor `schema.json` for every revision** into `schema/<revision>/schema.json`, embedded with
`embed.FS`. Vendoring, not fetching: charpy must produce identical verdicts offline, in an air-gapped
CI, and in five years when a URL has moved.

`schema/VENDORED.md` records, per revision, the upstream commit SHA and the date pulled.
`just vendor-schemas` refreshes them and is deliberately a manual step — an automatic refresh would
let a verdict change without a commit, which is the one thing a citable suite must never do.

A test asserts every listed revision embeds, parses, and compiles as a JSON Schema. It is cheap and
it catches a truncated vendor pull immediately.

**The specification prose is vendored too**, into `spec/<revision>/**.mdx` (`just vendor-specs`),
from the same pinned commit as the schemas so the two can never disagree about which revision they
describe. The design docs quote normative clauses and `derives_from` cites `spec:` paths; a
citation that resolves only against a moving external website is a citation that rots. Every clause
these documents rely on is checkable in-repo, offline — `spec/VENDORED.md` records the provenance
and the list of claims re-verified at each pull.

---

## 3. Fingerprinting

charpy must know which revision the subject speaks before it can choose an oracle, a probe method, or
an applicable case set. Usually it is *told*: a run pins a revision, or a case declares an
`applies_to` range that only one revision in the run satisfies. Fingerprinting is the fallback for
`revision = "auto"` — and, where a revision is already stated, a way to check the subject agrees.
This is a consequence of the conformance precondition (README, "Conformance first, then
resilience"): charpy's subject has already been shown correct under a correct sequence, so charpy
observes to confirm or to fill a deliberate gap, not to discover the protocol from the traffic.

**On `auto`, nothing is armed until the revision settles.** A relayed run watches somebody else's
handshake rather than probing, and until the subject answers it charpy knows neither which cases
apply nor which revision a citation would name. So each case is compiled once per revision
beforehand, which surfaces a case that does not compile before the subject starts, and the set for
the negotiated revision is armed when the handshake answer arrives. That answer is itself matched
after settlement, so a case can still fault it. What crosses before it -- in practice the client's
`initialize` request -- is relayed untouched: a fault charpy cannot cite correctly is one it should
not inject. Arming the union up front instead cited every case against the oldest revision its
range allowed, and fired cases the negotiated revision rules out.

When it does run, the 2026-07-28 spec prescribes the detection algorithm, so implement that rather
than inventing one.

```
1. POST a modern request carrying MCP-Protocol-Version and _meta.
   200  -> modern era. Negotiated version is what the server accepted.
   404 with JSON-RPC -32601 -> modern server, method not implemented. Still modern.
   400  -> inspect the body before concluding anything.

2. On 400, parse the body:
   UnsupportedProtocolVersionError (-32022) -> modern. Retry with a version from
                                              its `supported` list.
   MissingRequiredClientCapabilityError (-32021) -> modern. Fix the request.
   HeaderMismatch (-32020)                  -> modern. Fix the headers.
   empty body, or nothing recognisable      -> legacy. Fall back to step 3.

3. Legacy: send `initialize`, take the negotiated version from the result,
   send `notifications/initialized`.

4. Still nothing: try the deprecated 2024-11-05 HTTP+SSE handshake — GET expecting
   an `endpoint` event. Only when explicitly enabled.
```

The trap this avoids: a bare `400` is **not** evidence of a legacy server. Modern servers return
`400` for three distinct modern errors, and a client that falls back to `initialize` on any `400`
will misidentify a conforming 2026-07-28 server as legacy. Inspecting the body before falling back is
the whole algorithm.

On stdio there are no headers and no status codes. `server/discover` doubles as the probe: a modern
server answers it, a legacy one returns `-32601` and gets `initialize` instead.

The outcome is recorded in the transcript header as `revision.negotiated` and `revision.how`
(`initialize` · `server-discover` · `probe` · `forced` · `unknown`). `--revision` forces a value and
sets `how = "forced"`, which is how you test a subject's behaviour on a version it would not have
chosen.

---

## 4. What changes across the era boundary

The two eras differ enough that "MCP" names two protocols. This table is the reference the case
loader and the oracle both consult.

| | `<= 2025-11-25` | `>= 2026-07-28` |
|---|---|---|
| Handshake | `initialize` + `notifications/initialized` | none; `_meta` per request |
| Version carried in | handshake, then `MCP-Protocol-Version` header | `_meta` **and** header, which MUST agree |
| Capabilities | negotiated once | `io.modelcontextprotocol/clientCapabilities` per request |
| Server identity | `initialize` result | `io.modelcontextprotocol/serverInfo` per result |
| Capability discovery | `initialize` | `server/discover` (servers MUST implement) |
| Session | `Mcp-Session-Id` header, DELETE to end | **removed** (SEP-2567) |
| Server→client requests | sampling, elicitation, roots on SSE | **removed**; MRTR `input_required` + retry |
| Long-lived notifications | GET SSE stream, `resources/subscribe` | `subscriptions/listen` response stream |
| Resumability | `Last-Event-ID`, SSE event ids, `EventStore` | **removed**; re-issue with a new id |
| Cancellation on HTTP | `notifications/cancelled` | closing the response stream |
| `ping` | present | **removed** |
| Log level | `logging/setLevel` | `io.modelcontextprotocol/logLevel` per request |
| Results | untyped | `resultType` required: `complete` \| `input_required` |
| List and read results | plain | `CacheableResult`: `ttlMs`, `cacheScope` required |
| Mirrored headers | none | `Mcp-Method`, `Mcp-Name`, `Mcp-Param-*` REQUIRED |
| Roots / Sampling / Logging | active | deprecated (SEP-2577) |

Consequences charpy must honour:

- **The hostile server must be able to lie about which era it is.** A subject that mishandles an era
  mismatch is a legitimate target, and this is `capability_flip` with `field = "protocol_version"`.
- **Streamable HTTP accepts 2026-07-28 only from a stateless handler.** That version against a
  stateful handler is rejected, so charpy must drive both models rather than assuming one.
- **I6 (`session-identity-isolation`) has no stateless analogue** and is skipped there, its role
  taken by I9 and I11 (`oracle.md` §4).

---

## 4.1 Resumability: charpy does not replay, and that is conforming

`Last-Event-ID` resumption exists only through 2025-11-25 — SEP-2567 removed it — so anything built
for it has a bounded life. The spec's obligation is weaker than it first looks:

> The server **MAY** use this header to replay messages that would have been sent after the last
> event ID […] The server **MUST NOT** replay messages that would have been delivered on a
> different stream.

Replay is **MAY**. A server that never replays is fully conforming, so charpy as the reference
server declines to, holds no event store, and stays correct. The decline is recorded in the
transcript rather than being silent, because "the client reconnected with `Last-Event-ID` and
charpy started the stream fresh" is the context for whatever the client does next.

This is also a test rather than merely a saving. A client that only works against a server that
replays is broken, because the server is permitted not to — and that client exists.

**What it would cost, when the `stream/replay-*` family is wanted.** The sharp case is the
`MUST NOT`: a server replaying another stream's messages, which is a spec'd prohibition nothing
currently tests. That one needs a store, and the sizing is settled here so the number is not
invented later.

The window is whatever one stream emits during a disconnect gap, and the gap is bounded by the SSE
`retry` field the client **MUST** respect — one to five seconds. Against 121 JSON examples in the
2025-11-25 spec the median frame is 114 bytes and the largest 850; the fat tail is `tools/list`
with full `inputSchema`s, for which `transcript.md` §5 already takes 64 KiB as the plausible
maximum.

| Stream over a 5s gap | Events | Bytes |
|---|---|---|
| POST response stream | ~5 | 4 KiB |
| Chatty notification GET | ~50 | 12 KiB |
| Same, all fat frames | ~50 | 3,200 KiB |

That last row is why the bound is **bytes and not events**: an event count generous enough for the
middle row is 270× too large in the worst one. A 64 KiB per-stream budget covers the realistic
case five times over, and totals stay small where v0 lives — 256 KiB for a CI run of one client and
one session, 512 KiB for a gateway run. Only a v1 soak fleet makes it interesting, at roughly
125 MiB for a thousand clients, which is a tuning decision rather than a design one.

The structure already exists: the FIFO store in `internal/interpose` plugged into `mds/cache` with
`WithSizeFunc`, which is what makes a byte budget free rather than a second implementation.

---

## 5. Error codes by revision

Revision-dependent, and therefore a per-revision table rather than constants.

| Code | Name | Revisions |
|---|---|---|
| `-32700`, `-32600`…`-32603` | Standard JSON-RPC | all |
| `-32002` | resource not found | `<= 2025-11-25`; clients should still accept it from older servers |
| `-32042` | URL elicitation required | `2025-11-25` only |
| `-32020` | `HeaderMismatch` | `>= 2026-07-28` (was `-32001` in draft) |
| `-32021` | `MissingRequiredClientCapability` | `>= 2026-07-28` (was `-32003` in draft) |
| `-32022` | `UnsupportedProtocolVersion` | `>= 2026-07-28` (was `-32004` in draft) |
| `-32602` | Invalid params; also resource-not-found | `>= 2026-07-28` for resource-not-found |

The allocation policy partitions the JSON-RPC server-error range: `-32000`…`-32019` stays
implementation-defined with existing SDK usage grandfathered, and `-32020`…`-32099` is reserved for
the specification. charpy's own synthesized errors, where a case needs one, use the
implementation-defined range and never the reserved one.

Note the draft renumbering. A case citing `-32001` against a 2026-07-28 subject is citing a code that
moved, which is precisely the rot `case-identity.md` §2 exists to prevent.

---

## 6. `_meta` keys

Reserved: any prefix whose second label is `modelcontextprotocol` or `mcp` — so
`io.modelcontextprotocol/`, `dev.mcp/`, `org.modelcontextprotocol.api/` and `com.mcp.tools/` are all
reserved. `com.example.mcp/`
is not, because its second label is `example`.

| Key | Use |
|---|---|
| `io.modelcontextprotocol/protocolVersion` | Version; MUST match the `MCP-Protocol-Version` header |
| `io.modelcontextprotocol/clientInfo` | Client identity, SHOULD be sent per request |
| `io.modelcontextprotocol/clientCapabilities` | Capabilities, per request |
| `io.modelcontextprotocol/serverInfo` | Server identity, SHOULD be in each result's `_meta` |
| `io.modelcontextprotocol/logLevel` | Per-request log level |
| `io.modelcontextprotocol/subscriptionId` | Tags notifications on the `subscriptions/listen` stream |
| `traceparent`, `tracestate`, `baggage` | W3C trace context — **exempt** from the prefix rule per SEP-414 |

charpy's own keys use `dev.charpy/`. The second label is `charpy`, so it is permitted, and it is
namespaced enough that a subject echoing it back is unambiguously echoing charpy's marker.

SEP-414 settles what would otherwise have been an open question: the spec documents the trace-context
keys, so there is nothing to propose upstream and stdio correlation needs no charpy-specific
invention.

---

## 7. When the next revision lands

Assume it lands mid-build. The work is bounded by design:

1. Add the revision to the ordered list in §1 and to `internal/revision`.
2. `just vendor-schemas`; update `schema/VENDORED.md`.
3. Run the existing suite against it. Every case with an open-ended `applies_to` now targets it, and
   the failures are the changelog — mechanically derived rather than read.
4. Narrow `applies_to` on cases the revision invalidated; add cases for what it introduced. Never
   mutate an existing case's meaning under a stable ID (`case-identity.md` §6).
5. Update the §4 era table and the §5 error-code table if either moved.

Step 3 is the payoff, and it is worth stating as a design goal rather than a convenience: **charpy
should be able to tell you what a new revision broke before you have finished reading its
changelog.**
