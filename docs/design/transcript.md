# Transcript

Status: **Decided.** Schema version 1. The machine-readable schema lives in
the code tree at `schema/transcript/v1.json`; this document is the
rationale for it.

The transcript is the product. The oracle, the report, the divergence table
and any external tooling are consumers of it; nothing in the run path
depends on them. Retrofitting correlation into a one-sided transcript is a
rewrite, not a patch, so correlation and face tagging are present from the
first line written.

---

## 1. Shape

One JSON object per line, UTF-8, `\n`-terminated, append-only. Every line
carries `schema_version` and `type`.

Three line types. Only the first is obvious; the other two are
load-bearing, and their absence would be discovered late and expensively:

| `type` | Purpose |
|---|---|
| `header` | Exactly one, first line. Run metadata: seed, mode, subject, negotiated revision, policy digest. |
| `frame` | One protocol frame crossing one face, with its raw bytes. |
| `event` | Everything that is not a frame but changes what the oracle should conclude. |

**Why events exist.** Under 2026-07-28 Streamable HTTP,
*closing the response stream is itself the cancellation signal* — there is
no `notifications/cancelled` on the wire. An oracle that sees only frames
cannot evaluate the cancellation invariant at all, because the event it
must reason about is the absence of bytes. The same applies to fault
withdrawal — liveness is measured from the withdrawal instant (`oracle.md`
§6) — and to subprocess exit and connection teardown. Frames alone are
insufficient.

`seq` is monotonic across **all three types** in one shared sequence,
assigned under a single lock at capture time. Line order in the file
therefore equals `seq` order, and the oracle may rely on that.

---

## 2. Common fields

```jsonc
{
  "schema_version": 1,
  "type": "frame",
  "run_id": "01JBW3K9F2Q7XN4T",     // ULID, stable for the run
  "seq": 1043,                       // monotonic across all line types
  "t_mono_ns": 12345678,             // injected clock, ns since run start
  "t_wall": "2026-09-08T14:03:11.123456789Z"
}
```

Both clocks are always present. `t_mono_ns` is charpy's injected clock and
is what cases and the oracle reason about; `t_wall` exists solely to join
against the subject's own telemetry and log output. Nothing in the oracle
may read `t_wall` — see `decisions.md` ADR-001.

---

## 3. Dimensions

Every frame is tagged with four independent dimensions. Three identify
*whose* traffic it is; the fourth identifies which way it went. Getting the
first three wrong is the mistake that forces a rewrite, so all of them are
present from v0 even where v0 can only ever emit one value.

| Dimension | Answers | v0 cardinality |
|---|---|---|
| `face` | Which side of the subject | 1 for a server or client, 2 for a gateway |
| `client_id` | Which synthetic client | **always 1** |
| `session_id` | Which logical session | **always 1** |
| `direction` | Which way, in MCP role terms | 2 |

### `face`

Which side of the subject this frame crossed. Defined against the
**subject**, never against charpy.

| Value | Meaning |
|---|---|
| `downstream` | The subject's client-facing side. charpy is playing the client here. |
| `upstream` | The subject's server-facing side. charpy is playing the server here. |

A server under test has only a `downstream` face — charpy is its client. A
client under test has only an `upstream` face. A gateway has both, which is
the entire point.

### `direction`

The MCP role direction, independent of who charpy is impersonating.

| Value | Meaning |
|---|---|
| `c2s` | Client role to server role |
| `s2c` | Server role to client role |

So a hostile response charpy emits as a fake upstream server is
`face=upstream, direction=s2c`, and the gateway's forwarded copy of a
client request is `face=upstream, direction=c2s`. The pair is always
unambiguous; neither field alone is.

### `client_id` and `session_id`

`client_id` identifies which synthetic client the traffic belongs to.
`session_id` identifies which logical session — one client may open, drop
and reopen many sessions over a long run.

**In v0 there is exactly one of each**, and they are emitted as the
constant `c0` and a per-run session id. They cost one field apiece and buy
nothing today.

They are here anyway because soak mode (`soak.md`) drives a *fleet* of
synthetic clients, and **a transcript that assumes a single session is the
same category of mistake as a transcript that assumes a single face.** Both
are retrofits that touch every writer, every reader, every invariant and
every stored transcript. The one-sided-transcript mistake is the one this
design was built to avoid; making it a second time in a different dimension
would be worse, because it would be knowing.

The concrete cost of getting it wrong:
`"30% of clients reconnect every 2s without closing"` is only expressible
if `client_id` is a column you can partition on. Retrofitted, every
transcript captured before the retrofit becomes unanalysable for exactly
the questions soak mode exists to ask — and `oracle.md` §1 promises that
transcripts captured today re-run against invariants written later. That
promise is only worth something if the dimensions are already there.

### charpy's ids versus the subject's

Note the parallel with `link.charpy_id` and `link.trace_id`:

| charpy's | The subject's |
|---|---|
| `session_id` — charpy's logical session, present in every revision | `session.mcp_session_id` — the protocol session, `<= 2025-11-25` only |
| `link.charpy_id` — charpy's join key | `link.trace_id` — whoever started the trace |

They are never the same field and must never be conflated. SEP-2567 removed
the protocol session in 2026-07-28; charpy's `session_id` is unaffected,
because it denotes charpy's own notion of a client's connection lifetime
and identity binding, which exists regardless of what the protocol calls
it.

---

## 4. Correlation

It is tempting to describe this as "the correlation key charpy attached",
but that holds only when charpy is the forwarder. When the **subject**
forwards — which is exactly the gateway case charpy exists for — charpy
cannot carry its own key across, because the gateway is free to rewrite the
JSON-RPC `id`, and frequently does.
**Joining gateway faces on `id` is wrong by construction.** That is why
`link` is a distinct object rather than a reuse of `id`.

Three regimes. `link.via` records which one produced this frame's key.

| `via` | When | Authority |
|---|---|---|
| `forwarded` | Proxy and stdio-ingress modes. charpy relays the bytes, so it stamps its own id on both copies. | Authoritative, `confidence = 1.0` |
| `traced` | Gateway under test. Join on `traceparent` in `_meta` per SEP-414, which the spec blesses on both transports. | Authoritative when the subject propagates, `confidence = 1.0` |
| `inferred` | Subject drops trace context. Ledger content join: charpy originated the frame, so the interposer's ledger holds its argument digest; the other face's content is matched against it, with ordering and timing as tie-breakers (`interposer.md` §6). | `confidence < 1.0` |
| `none` | No join attempted or possible. | `confidence = 0.0` |

### The credibility rule

**Never emit a gateway verdict from an `inferred` join.** An invariant that
requires correlation is *skipped*, with the degraded join recorded as the
skip reason, rather than evaluated on a guess.

This is the same rule as the verdict policy in `oracle.md` §2, and for the
same reason: a false correlation produces a confident, specific, wrong
accusation about someone else's credential handling, and that is a
credibility loss which can be spent exactly once.

Separately, a subject that drops trace context is itself a reportable
**OBSERVED** finding — it breaks distributed tracing for everyone
downstream of it. The correlation mechanism doubles as a test, which is a
pleasant result rather than a designed one.

### Fields

```jsonc
"link": {
  "charpy_id": "c7f1a2",        // charpy's own id; equal across correlated frames
  "via": "traced",
  "confidence": 1.0,
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id":  "00f067aa0ba902b7",
  "parent_span_id": "0102030405060708"
}
```

The object is named `link` because that is exactly what it is — the key you
join the two faces on, and in DuckDB literally the join column. `charpy_id`
is charpy's own identifier, distinguished from the W3C `trace_id`/`span_id`
beside it, which belong to whoever started the trace.

charpy stamps `traceparent`, and propagates `tracestate` and `baggage`,
into `_meta` on every request it originates, on both transports. On HTTP it
additionally sets the `traceparent` header. Per SEP-414 these three keys
are an explicit exception to the reverse-DNS prefix rule and need no
namespace.

The `inferred` regime plants **no marker in the traffic**. An earlier draft
had charpy add a `dev.charpy/` key inside tool arguments so a forwarded
copy could be recognised; that is deleted, because it perturbs the subject
— a gateway validating arguments against an `inputSchema` with
`additionalProperties: false` rejects the call
*because charpy was watching*. Distinctness comes instead from the scenario
player, which controls every argument byte it originates and makes each
call unique inside values the tool legitimately accepts (`interposer.md`
§6). Where charpy does need a `_meta` key of its own it uses the
`dev.charpy/` prefix, which is permitted: the reserved prefixes are those
whose second label is `modelcontextprotocol` or `mcp`, and `charpy` is
neither.

---

## 5. Frame lines

Envelope fields are **flattened to top level** rather than nested. They are
the fields every query filters on, and the DuckDB one-liners the README
advertises should stay one-liners:

```sql
select id, count(*) from read_json('run.jsonl')
 where type = 'frame' and kind = 'response' group by id having count(*) > 1;
```

```jsonc
{
  "schema_version": 1, "type": "frame", "run_id": "...", "seq": 1043,
  "t_mono_ns": 12345678, "t_wall": "2026-09-08T14:03:11.123456789Z",

  "face": "downstream", "direction": "s2c", "transport": "http",
  "client_id": "c0", "session_id": "s-4f2a", "conn_id": "c-04", "stream_id": "s-11",

  "kind": "response",          // request | response | error | notification | malformed
  "id": "7",                   // ALWAYS a string — see below
  "id_type": "number",         // number | string | null | absent | invalid
  "method": "tools/call",      // echoed onto responses when charpy can resolve the id
  "result_type": null,         // 2026-07-28+: complete | input_required
  "error_code": null,

  "revision": "2025-11-25",
  "raw": "eyJqc29ucnBjIjoi...",  // base64 of the exact bytes
  "raw_len": 412,
  "raw_truncated": false,

  "http": { ... }, "link": { ... }, "fault": null, "session": { ... }
}
```

### `id` is always a string

JSON-RPC permits `id` to be a number, a string, or null. Emitting it with
its native type would put a type union in a column, and `read_json` over a
file where `id` is sometimes `7` and sometimes `"7"` either fails or
silently coerces. Since JSONL *is* the database here, and the queries are
written by other people without charpy's code, the column must have one
type.

So `id` carries the **canonical JSON text** of the id — `7` becomes `"7"`,
`"abc"` becomes `"abc"` — and `id_type` preserves what it actually was.
`id_type = "invalid"` covers ids charpy emitted deliberately that JSON-RPC
does not permit, such as an object or an array.

Note the consequence, and it is intended: `7` and `"7"` collide in the `id`
column. That is a real protocol ambiguity a subject may itself get wrong,
and `id_type` is how a case detects it rather than something the schema
should paper over.

### `kind = "malformed"`

A first-class value, meaning
**the bytes do not denote exactly one JSON-RPC message**. That covers bytes
no parser accepts, a frame cut mid-stream, an object with nothing to
dispatch on — and an object carrying *several* dispatchable members at
once, such as a `method` beside a `result`. charpy emits every one of those
deliberately (`faults-and-cases.md` §2, `schema_violation` with
`target = "envelope"`), so the transcript must represent them without
losing `raw`.

The last of those is the one worth stating explicitly, because the
alternative is tempting. A frame with both a `method` and a `result` could
be called a request, on the grounds that dispatchers key on the method.
That would be charpy asserting a reading of someone else's frame, in the
column I1, I2 and I3 count — and the whole point of such a frame is that
implementations *disagree* about it, which is differential-table material
rather than something to normalise away. So charpy declines: the frame is
malformed, it never enters the id-resolution bookkeeping, and what the
subject made of it is visible in what the subject did next.

The result is two accurate findings rather than one wrong pass. The schema
layer, reading `raw`, issues a MUST citing the subschema that rejected the
frame; I1 separately reports that the request never received a valid
resolution, because it did not. Both are true and neither is noise. The
alternative — picking a reading so that the id resolves — buys a quieter
report by having I1 certify a frame that was never a response.

When `kind` is `malformed`, `id`, `method`, `result_type` and `error_code`
are null and `id_type` is `absent`; `raw` still holds every byte. A member
counts toward "exactly one" when the transcript can record it: `method` a
non-empty string, `error` an object carrying an integer code, `result` any
JSON value including `null` — a result may be null, an error must be an
object, and that asymmetry is JSON-RPC's.

### `raw`

Base64 of the exact bytes as they crossed the wire, before any parsing and
after any fault was applied. Capped at 64 KiB; beyond that the value is the
first 64 KiB and `raw_truncated` is true. `raw_len` is always the true
length.

For a truncated frame, `raw` holds exactly what was sent — the truncation
is the datum.

### `http`

Present when `transport = "http"`, otherwise null.

```jsonc
"http": {
  "status": 200,
  "headers": { "content-type": "text/event-stream", "mcp-protocol-version": "2025-11-25" },
  "sse_event": "message",       // SSE event type, or "comment" for a `:` keep-alive line
  "sse_id": "42"                // <=2025-11-25 only; removed in 2026-07-28
}
```

Header names are lowercased. Values matching the redaction policy are
replaced with `"<redacted:sha256:4318c2c9…>"` — a stable digest, so the
credential-leak invariant can still prove that the *same* secret appeared
on both faces without the transcript itself becoming a secret. The digest
is the **whole SHA-256, untruncated**: truncating would trade collision
probability for transcript size, and charpy has no use for that trade until
soak runs make the digest set the expensive part (`../open-problems.md`).
The digest is also *of* the exact value, whole, so equality across faces —
and therefore I5 — is **verbatim-only**, and a re-encoded or embedded
secret produces a different digest (`oracle.md` §4). The policy is a list
of header *names*, not a heuristic over values: guessing at secrets by
shape gets it wrong in both directions and makes the file's contents
unpredictable, which is bad for something people write queries against.
`Mcp-Session-Id` is deliberately not on it — the session identifier is
already carried in the clear in `session` below, and digesting the header
while printing the field would be incoherent rather than safe. I5 compares
session identifiers by value instead, which is the stronger comparison of
the two (`oracle.md` §4).

Redaction is on by default; `--no-redact` exists for local debugging and
stamps `redaction: "off"` into the header line so a report generated from
it is visibly unsafe to share.

### `session`

Sessioned revisions only (`<= 2025-11-25`); null on 2026-07-28 and later,
where the protocol-level session was removed by SEP-2567.

```jsonc
"session": { "mcp_session_id": "...", "identity": "tenant-a" }
```

`identity` is charpy's label for the credential or tenant it presented,
which is what the session-isolation invariant partitions on.

### `fault`

Null on frames charpy did not tamper with -- and "tamper" means the bytes
that crossed differ from the bytes that were written. A frame a case
matched but delivered untouched and in full, beside what the case injected
(a duplicate's original, the real answer next to a late one, a whole event
before a close), carries no attribution: it is the subject's frame, and
every layer that judges the subject must see it as one. That the case
acted, and when, is `fault_applied`'s to record.

```jsonc
"fault": {
  "case_id":  "stream/truncate-mid-event",
  "citation": "stream/truncate-mid-event@2025-11-25#seed=8f2c1a",
  "kind":     "truncate",
  "params":   { "cut_at": "mid_event", "after_bytes": 91 },
  "replaced": { "id": "3", "id_type": "number", "kind": "response" }
}
```

`replaced` appears when what crossed no longer carries the id the original
did: a rewrite that changed or dropped it, or a cut that stopped before it
could parse. It names the id and what the frame was. A replaced *response*
means the subject answered and charpy hid the answer; a replaced *request*
means the subject was never asked, and is owed nothing under that id.
`kind` is absent in transcripts written before it was recorded, which
replaced only answers.

---

## 6. Event lines

```jsonc
{
  "schema_version": 1, "type": "event", "run_id": "...", "seq": 1044,
  "t_mono_ns": 12350000, "t_wall": "...",
  "event_kind": "stream_close",
  "face": "downstream", "client_id": "c0", "session_id": "s-4f2a",
  "conn_id": "c-04", "stream_id": "s-11",
  "detail": { "reason": "peer_close", "bytes_written": 91 },
  "fault": null, "link": { ... }
}
```

| `event_kind` | Emitted when | Consumed by |
|---|---|---|
| `conn_open` / `conn_close` | Transport connection established or torn down | Liveness, leak detection |
| `stream_open` / `stream_close` | SSE stream or stdio pipe opened or closed | **Cancellation on 2026-07-28 HTTP**, truncation cases |
| `subject_exit` | stdio subject process exited | Exit-code reporting, harness-vs-subject attribution |
| `fault_scheduled` | Interposer selected a frame or moment for a fault | Case audit |
| `fault_applied` | Fault took effect; `detail.direction` is the way its frame was travelling | The reaction layer's anchor; who the fault was put to |
| `fault_withdrawn` | Hang released, list restored, peer recovered | **Liveness clock starts here** |
| `probe` | Liveness probe sent and its outcome | The reaction layer's recovery check |
| `clock_advance` | Injected clock jumped | Replay determinism |
| `note` | Free-text harness annotation | Debugging only; oracle ignores |

`stream_close.detail.reason` distinguishes `peer_close`, `charpy_close`,
`timeout`, `error` and `subject_close`. The cancellation invariant on
2026-07-28 needs to know *who* closed, and inferring it later from a bare
close is not possible.

---

## 7. Header line

Exactly one, first.

```jsonc
{
  "schema_version": 1, "type": "header", "run_id": "01JBW3K9F2Q7XN4T", "seq": 0,
  "t_mono_ns": 0, "t_wall": "2026-09-08T14:03:10.000000000Z",
  "charpy_version": "0.1.0", "charpy_commit": "abc1234",
  "seed": "8f2c1a",
  "mode": "proxy",                  // proxy | hostile-server | stdio-ingress | inproc
  "subject": { "class": "gateway", "descriptor": "http://localhost:8080/mcp" },
  "peer": { "module": "github.com/modelcontextprotocol/go-sdk",   // absent under relay
            "version": "v1.8.0-pre.2", "era": "2025-11-25" },
  "revision": { "negotiated": "2025-11-25", "offered": ["2026-07-28", "2025-11-25"],
                "how": "initialize" },
  "policy_digest": "sha256:...",
  "cases": ["stream/truncate-mid-event@2025-11-25#seed=8f2c1a"],
  "redaction": "on",
  "clock": "injected",           // injected | real -- a property of the run (ADR-001)
  "fleet": { "clients": 1 }      // v0 is always 1; soak mode drives many
}
```

`policy_digest` makes a transcript self-describing about what produced it.
A verdict re-derived months later can state which policy was in force
without that policy having survived.

`peer` does the same for the stimulus. `charpy_version` says what judged
the run, not what spoke in it, and ADR-004 promises a divergence table
published today reproduces a year from now — which it cannot if the
transcript does not record which SDK, at which pin, said the words. It is
absent under relay, where the traffic is somebody else's and there is no
peer to name. `era` is the revision charpy *asked* the peer to speak;
`revision.negotiated` is what the conversation settled on. They differ
whenever a subject refuses the ask or a fault rewrites it in flight, which
is why they are separate columns.

---

## 8. Guarantees

These are the contract; the oracle and every external consumer may rely on
them.

1. **Ordering.** File order equals `seq` order. `seq` is dense from 0 with
   no gaps.
2. **Completeness of bytes.** Every byte charpy sent or received on a
   subject-facing connection appears in exactly one `frame.raw`, except
   beyond the 64 KiB cap where `raw_truncated` marks it.
3. **Append-only.** Lines are never rewritten. A run that crashes leaves a
   valid prefix, and a partial transcript is a legitimate oracle input — it
   simply supports fewer conclusions.
4. **Dimension totality.** Every frame carries a `face`, a `client_id` and
   a `session_id`. There is no "unknown" value for any of them. In v0 the
   latter two are constant; they are never absent.
5. **No oracle in the run path.** Nothing evaluates an invariant while the
   run is live. Writes are buffered and asynchronous so the frame path adds
   microseconds where subjects reason in milliseconds.
6. **Schema additivity within a major version.** New optional fields may
   appear in `schema_version: 1`. Consumers must ignore unknown fields.
   Removing or retyping a field requires `schema_version: 2`.

---

## 9. Versioning

`schema_version` is on every line, not only the header, so a single line
pasted into a bug report remains interpretable. The version is an integer
and increments only on a breaking change as defined in guarantee 6.

`charpy replay` accepts any `schema_version` it knows and refuses newer
ones with exit code 2 rather than guessing.

---

## 10. Loading

The advertised path is DuckDB with no charpy code involved:

```sql
-- every request id that resolved more than once
select id, count(*) from read_json('run.jsonl', union_by_name = true)
 where type = 'frame' and kind in ('response', 'error')
 group by id having count(*) > 1;

-- frames that carried an injected fault, in order
select seq, face, direction, method, fault.kind, fault.citation
  from read_json('run.jsonl', union_by_name = true)
 where fault is not null order by seq;

-- both faces of one correlated exchange, side by side
select seq, face, direction, kind, method
  from read_json('run.jsonl', union_by_name = true)
 where link.charpy_id = 'c7f1a2' order by seq;

-- soak: unresolved requests per client, which is where a leak shows up first
select client_id, count(*) filter (where kind = 'request') as sent,
       count(*) filter (where kind in ('response', 'error')) as answered
  from read_json('run.jsonl', union_by_name = true)
 where type = 'frame' group by client_id order by sent - answered desc;
```

`union_by_name = true` is required because the three line types have
different shapes; it fills the absent columns with nulls. These three are
shipped in the README, and the flag is noted there — a reader who omits it
gets a confusing error, and that is the sort of friction that stops people
using the format.
