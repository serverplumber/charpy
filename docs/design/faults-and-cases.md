# Faults and cases

Status: **Decided.** Defines the fault mechanisms, their parameters, and the factoring that separates
a mechanism from a case.

---

## 1. Mechanisms are not cases

"Eight faults, each at most a day" is a fair estimate of the **mechanism** — the code that mangles
bytes. It is not an estimate of the **cases**, which are where the design judgement lives and which
are what gets cited.

| | Mechanism | Case |
|---|---|---|
| What it is | Go code that produces a specific wire effect | A named, parameterised, revision-scoped application of a mechanism |
| Lives in | `internal/fault/` | `cases/*.toml` |
| Identified by | A `kind` string | A permanent case ID (`case-identity.md`) |
| Count in v0 | 8 | ~35 |
| Changes when | The wire effect changes | Any parameter, subject, or revision range changes |

The factoring settles a question that otherwise recurs: cutting an SSE stream at a byte, at an event
boundary, and after `event:` but before `data:` are three different *faults* in the sense that
matters to a bug report — but they are three **cases** over one **mechanism**, distinguished by a
parameter. Implementing them as three
mechanisms would triple the code for one behaviour and make the fourth variant, when it is wanted, a
fourth code path instead of a fourth line of TOML.

The rule: **if it changes bytes, it is a mechanism parameter; if it changes what you would claim in
a bug report, it is a case.**

---

## 2. The eight v0 mechanisms

Each takes parameters. Defaults are chosen so an omitted parameter yields the most common variant.

### `hang`

Hold a response open and never deliver it. The liveness half of the suite depends on this
mechanism's withdrawal behaviour.

| Param | Values | Default |
|---|---|---|
| `withdraw_after_ms` | integer, injected-clock ms; `0` = never withdraw | `0` |
| `scope` | `response` · `stream` · `connection` | `response` |
| `keepalive` | `none` · `comments` — whether SSE keep-alives keep arriving while it is held. HTTP only | `none` |
| `then` | `deliver` · `close` · `error` — what happens at withdrawal | `deliver` |

`fault_withdrawn` is emitted at the withdrawal instant and is where the liveness clock starts.

### `truncate`

Stop mid-frame. Emits what was written and then closes or stalls.

| Param | Values | Default |
|---|---|---|
| `cut_at` | see table below | `mid_frame` |
| `after_bytes` | integer, used by `byte` | seeded |
| `then` | `close` · `stall` — close the stream or hold it open | `close` |
| `keepalive` | `none` · `comments` — what arrives on a stalled stream. HTTP only | `none` |

`cut_at` values:

| Value | Transport | Effect |
|---|---|---|
| `byte` | both | Cut at exactly `after_bytes`, wherever that lands |
| `mid_frame` | both | Cut at a seeded point inside the JSON body |
| `mid_line` | stdio | Cut before the terminating newline — the frame never delimits |
| `mid_event` | http | Cut inside an SSE `data:` value |
| `field_boundary` | http | Cut after `event:` and before `data:` — a structurally incomplete SSE event |
| `event_boundary` | http | Cut cleanly between events — every event delivered was well formed |
| `mid_comment` | http | Cut inside a `:` keep-alive comment line (2026-07-28 encourages these) |

**A held stream is two faults, not one.** `then = "stall"` and `hang` with `scope = "stream"` both
keep a stream open with no result on it, and what *else* crosses the wire decides which bug is
found. Under `keepalive = "none"` nothing arrives at all, which tests dead-peer detection and
transport-level timeouts — the subject has to notice silence. Under `keepalive = "comments"` the
`:` keep-alive lines keep coming while no data ever does, which tests whether the subject has an
application-level timeout at all, because the stream looks healthy the entire time. Both happen in
production and neither is *the* stall, so the parameter exists rather than one being picked as the
default meaning. `none` is the default only because it is what this document previously described.

**`after_bytes` counts bytes written, never bytes received.** charpy flushes N bytes; it cannot
know the subject read them. Kernel buffers, the OS and any intermediary sit in between, so
`raw` in the transcript is what charpy sent (`transcript.md` §5) and nothing here is a claim about
what the subject perceived. Write cases accordingly: assert on what the subject *did*, never on
what it must have seen. `byte`, `mid_event` and `field_boundary` all read like claims about
subject-side perception and are not.

`event_boundary` deserves emphasis: nothing malformed is ever delivered, so a subject that only
validates individual events sees a clean stream that simply stops. Under 2026-07-28, where
resumability was removed, the client **MUST** re-issue as a new request with a new id, and failing to
is a common and load-bearing bug. It is the least dramatic variant and the most likely to find
something.

### `malformed_json`

Bytes a JSON parser rejects.

| Param | Values | Default |
|---|---|---|
| `how` | `unbalanced` · `trailing_garbage` · `bad_utf8` · `nul_byte` · `deep_nest` · `duplicate_key` | `unbalanced` |
| `depth` | integer, for `deep_nest` | `1000` |

The parameter is `how`, not `kind`: `kind` selects the mechanism itself in a `[case.fault]` table,
so a parameter by that name could never be set. Enforced by a registry test.

`duplicate_key` is technically valid JSON with undefined semantics, which is more interesting than
invalid JSON: implementations disagree about last-wins versus first-wins, and that disagreement is
differential-table material rather than a bug.

### `schema_violation`

Valid JSON that violates a declared schema. This is the interesting one: it passes every parser and
fails only the oracle.

| Param | Values | Default |
|---|---|---|
| `target` | `envelope` · `result` · `declared_output_schema` · `tool_input_schema` | `result` |
| `how` | `wrong_type` · `missing_required` · `extra_required_absent` · `enum_out_of_range` | `wrong_type` |

`declared_output_schema` needs the declaration before it can break it. Where charpy serves, the
schema is charpy's own and there is nothing to find out. Where charpy relays a real server, charpy
asks: it issues its own `tools/list` and caches what comes back. That is an extra request in the
transcript and it shifts occurrence counters, which is a real cost and a far smaller one than the
`dev.charpy/` marker that was deleted for the same class of reason — a `tools/list` is a request
any client legitimately makes, and it is visible in the transcript rather than hidden inside
someone else's arguments.

`declared_output_schema` is the sharpest target: the *subject's own* `outputSchema` from `tools/list`
becomes the oracle. charpy does not need to know what the tool means, only that the server declared
a shape and then broke it. That is a MUST-eligible finding under `verdict = "MUST"` because the
rejecting artifact is the subject's own declaration.

### `duplicate_id`

Reuse a JSON-RPC id.

| Param | Values | Default |
|---|---|---|
| `mode` | `concurrent_request` · `double_response` · `reuse_after_close` | `double_response` |
| `vary_type` | boolean — send `7` then `"7"` | `false` |

`vary_type` exploits the number/string ambiguity noted in `transcript.md` §5. An implementation
keying its pending map on a stringified id will treat these as the same request; one keying on the
typed value will not. Both are defensible, which makes it differential material.

### `unsolicited_response`

A response or error for an id never requested.

| Param | Values | Default |
|---|---|---|
| `id_source` | `never_used` · `already_resolved` · `reserved_null` | `never_used` |

`already_resolved` is the one that finds pending-map leaks: a late duplicate for an id that was
correctly resolved ten frames ago.

### `manifest_mutate`

Change the tool, prompt or resource list mid-session.

| Param | Values | Default |
|---|---|---|
| `list` | `tools` · `prompts` · `resources` | `tools` |
| `op` | `add` · `remove` · `rename` · `change_schema` | `remove` |
| `notify` | `list_changed` · `silent` · `subscriptions_listen` | `list_changed` |

`notify = "silent"` is not adversarial. Notification reliability is already distrusted in the field:
at least one production gateway polls its upstreams on a fixed interval rather than trusting
`tools/list_changed`. Silent mutation is the case that matches deployed reality.
`subscriptions_listen` is the 2026-07-28 path, where change notifications arrive on the
`subscriptions/listen` response stream rather than a GET stream.

### `capability_flip`

Reconnect as a peer claiming different capabilities.

| Param | Values | Default |
|---|---|---|
| `field` | `capabilities` · `protocol_version` · `server_info` · `extensions` | `capabilities` |
| `direction_of_change` | `narrow` · `widen` · `incompatible` | `narrow` |

`narrow` is the dangerous one: the subject cached a capability from the first connection and keeps
using it after the peer stopped offering it.

---

## 3. Later mechanisms

Deferred, listed so the parameter shapes above do not have to change to accommodate them: TCP reset
mid-body · chunked encoding that lies about length · HTTP 200 with a JSON-RPC error body · upstream
restart with a new session identity · `Mcp-Session-Id` echoed back wrong · notification storm ·
`header_body_desync` (see §5).

The first two of those are **HTTP/1.1 only, by construction rather than by choice.** Both need the
connection beneath the response — Go owns response framing and will neither emit a `Content-Length`
that disagrees with the body nor send a reset — and hijacking is unavailable over HTTP/2, which
`net/http` states it has no plan to support. The specification mandates no HTTP version, so a
subject may well speak HTTP/2. `internal/wire` therefore probes for the capability rather than
assuming it, and a case needing either fault is scoped to the transport that can carry it.

A second group belongs to soak mode rather than here, because they are lifecycle-shaped and only
mean anything over hours: `slow_drip`, `upstream_flap`, `reconnect_without_close`, `retry_storm`,
`duplicate_session_id`, `session_churn`. See `soak.md` §3.

---

## 4. Cases are mechanism × parameters × subject × revision range

A case binds a mechanism and its parameters to a matcher, a subject class, and a revision range:

```toml
[[case]]
id           = "stream/truncate-event-boundary"
applies_to   = ">=2026-07-28"
subject      = ["server", "gateway"]
verdict      = "OBSERVED"
derives_from = "spec:2026-07-28/basic/transports/streamable-http#receiving-messages"
summary      = "SSE stream stops cleanly between events; the request is never answered."

  [case.match]
  method     = "tools/call"
  face       = "downstream"
  direction  = "s2c"
  occurrence = 2

  [case.fault]
  kind   = "truncate"
  cut_at = "event_boundary"
  then   = "close"

  [case.expect]
  liveness_probe_within_ms = 30000
```

The v0 catalogue is roughly 35 cases: eight mechanisms, two to six parameterisations each, split
across subject classes and the two protocol eras. Case authoring is a day's work once the loader
exists; the mechanisms are three days; the oracle is the rest of the month. That ratio is the
correct one: the faults are not where the time goes.

---

## 5. The stateless-era gateway family

Not in the v0 case set — v0 is sessioned-first — but designed now, because these are the strongest
cases charpy will have and the mechanism parameters above must not need reshaping to express them.

2026-07-28 mirrors JSON-RPC body fields into HTTP headers so intermediaries can route without
parsing bodies, and then constrains intermediaries directly:

> Intermediate servers that do not recognize an `Mcp-Param-{Name}` header **MUST** forward it and
> otherwise ignore it.

> Intermediaries that enforce policy based on mirrored headers **SHOULD** verify that the
> `MCP-Protocol-Version` header indicates a version that requires header–body validation. If the
> version is older or the header is absent, the intermediary **SHOULD** reject the request rather
> than trusting unvalidated header values.

Servers **MUST** reject any header/body mismatch with `400` and JSON-RPC `-32020` `HeaderMismatch`.

This is a header/body confusion attack written into the specification, with its own error code, and
nothing tests it. It needs a ninth mechanism, `header_body_desync`:

| Param | Values |
|---|---|
| `header` | `mcp-method` · `mcp-name` · `mcp-protocol-version` · `mcp-param` |
| `mode` | `mismatch` · `omit` · `inject_unknown` · `downgrade` |

The resulting case family, all `family = gateway`:

| Case | What it asks |
|---|---|
| `gateway/header-body-mismatch-rejected` | Does the gateway reject `-32020`, or forward a request whose header and body disagree? |
| `gateway/mcp-param-unknown-forwarded` | Does it forward an `Mcp-Param-*` it does not recognise, as it MUST? |
| `gateway/header-rederived-after-rewrite` | If it rewrites the body, does it re-derive `Mcp-Method`/`Mcp-Name` — or leave stale headers a downstream LB will route on? |
| `gateway/protocol-version-downgrade` | With `MCP-Protocol-Version` absent, does it reject, or trust unvalidated headers as if validated? |

`gateway/header-rederived-after-rewrite` is only observable with both faces correlated, which is the
the gateway thesis arriving as a concrete, spec-cited, currently-untested case. It is also the
strongest content for a working-group issue: not "please add fault injection", but
"the specification added intermediary requirements and there is no suite that covers them."
