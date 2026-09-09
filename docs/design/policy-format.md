# Policy and case manifest format

Status: **Decided.** TOML, per `decisions.md` ADR-006. This is the control plane: the component most
likely to overrun its budget, and the one that makes charpy a tool rather than a script.

---

## 1. Two files, one schema

| File | Contains | Ships with charpy |
|---|---|---|
| `cases/*.toml` | The case catalogue — named, citable, revision-scoped | Yes, embedded |
| A run policy | Ad-hoc faults for a single run, no case IDs | No, user-supplied |

Both parse into the same structures. A case is a policy entry with an identity; a policy entry is an
anonymous case. Sharing the parser means the format cannot drift between "what charpy ships" and
"what a user can write", which is what keeps the catalogue honest — everything charpy does, a user
can express.

---

## 2. Case entries

```toml
schema_version = 1

[[case]]
id           = "stream/truncate-event-boundary"
applies_to   = ">=2026-07-28"
subject      = ["server", "gateway"]
transport    = ["http"]
verdict      = "OBSERVED"
derives_from = "spec:2026-07-28/basic/transports/streamable-http#receiving-messages"
summary      = "SSE stream stops cleanly between events; the request is never answered."
status       = "active"

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
  invariants = ["id-resolves-once", "cancel-honoured"]
```

### Top-level fields

| Field | Required | Notes |
|---|---|---|
| `id` | yes | Permanent case ID (`case-identity.md` §1) |
| `applies_to` | yes | Revision range; resolves against the ordered list in `revisions.md` §1 |
| `subject` | yes | Any of `server`, `client`, `gateway` |
| `transport` | no | Defaults to both `stdio` and `http` |
| `verdict` | yes | `MUST` or `OBSERVED` |
| `derives_from` | yes | `spec:` · `sep:` · `schema:` · `none` |
| `summary` | yes | One sentence; appears in the report and JUnit |
| `status` | no | `active` (default) or `withdrawn`, with `withdrawn_reason` |

### `[case.match]`

Selects which frame the fault attaches to. All present keys must match; absent keys match anything.

| Key | Values |
|---|---|
| `method` | Exact method name, or a `*` glob |
| `face` | `downstream` · `upstream` |
| `direction` | `c2s` · `s2c` |
| `kind` | `request` · `response` · `error` · `notification` |
| `client_id` | Exact id, or a `*` glob |
| `session_id` | Exact id, or a `*` glob |
| `occurrence` | Integer, 1-based — lets a case say "the third `tools/call`" |
| `occurrence_every` | Integer — every Nth match, mutually exclusive with `occurrence` |
| `after_mono_ms` | Integer — not before this point on the clock |
| `scope` | Which population the ordinal counts within: `run` (default) · `client` · `session` · `connection` |

An empty `[case.match]` matches the first frame on any face, which is almost never what an author
means. The loader warns on it.

### `scope`, and why it exists before it is needed

`scope` names the population an `occurrence` counts within. In v0 there is one client and one
session, so every scope collapses to `run` and the field changes nothing.

It is here because of what comes next. A soak policy needs to express *"30% of clients reconnect
every 2s without closing"* — a **rate over a population**, not an ordinal over a stream. Those are
different selector kinds, and a matcher that has only ever counted globally cannot grow one: the
engine's counters have to be keyed by `(scope, dimension value)` rather than being a single integer.
That is a data-structure decision, not a feature, and it is cheap now and invasive later.

So v0 keys its counters that way, with a single key. The v1 addition is then a new selector kind
sitting alongside `occurrence` rather than a rework of it:

```toml
  [case.match]              # v0, shipping
  method     = "tools/call"
  occurrence = 3
  scope      = "session"    # "the third tools/call of each session"

  [case.select]             # v1, not built -- the shape it grows into
  population = "client"
  rate       = 0.30         # 30% of clients, chosen by seed
  every_ms   = 2000
```

The rule is the same one that put `client_id` in the transcript: **do not build the feature, but do
not choose a structure that forbids it.** `select` is a sibling table rather than more keys on
`match`, because "which frames does this apply to" and "which members of a population, how often" are
different questions, and collapsing them is what makes matcher languages unreadable.

### `[case.fault]`

`kind` names a mechanism from `faults-and-cases.md` §2; the remaining keys are that mechanism's
parameters. Parameters are validated against the mechanism — an unknown key, or a value outside a
mechanism's enum, is a load error naming the file, line, mechanism and the accepted values.

### `[case.expect]`

What the oracle should check for this case. Optional; when absent, the universal invariants still
run.

| Key | Meaning |
|---|---|
| `invariants` | Invariant names that must hold. Unknown names are a load error. |
| `liveness_probe_within_ms` | Real-time budget for recovery after withdrawal (`oracle.md` §6) |
| `expect_error_code` | The subject is expected to answer with this JSON-RPC code |
| `expect_http_status` | HTTP only |

---

## 3. Run configuration

The non-case half: what to point at, and how.

```toml
schema_version = 1

[run]
mode      = "proxy"                       # proxy | hostile-server | stdio-ingress | inproc
seed      = "8f2c1a"                      # omit to generate and record one
revision  = "auto"                        # or a pinned revision; "auto" fingerprints
out_dir   = "./charpy-out"
redact    = true
clock     = "injected"                    # injected | real -- see decisions.md ADR-001

[subject]
class      = "gateway"
descriptor = "http://localhost:8080/mcp"

[subject.stdio]                           # stdio-ingress only
command = ["python", "-m", "my_server"]
env     = { LOG_LEVEL = "debug" }

[select]
cases   = ["stream/*", "id/duplicate-response"]
exclude = ["gateway/*"]

[parallel]
stdio_subjects = 8       # spawned per case, safe to run concurrently
http_subjects  = 1       # one process with shared state; serialise unless isolated

[fleet]
clients = 1              # v0 is always 1; soak mode drives many (soak.md)
```

`[parallel]` encodes the rule that parallelism is subject-bound, not charpy-bound. stdio subjects
spawn per case and run concurrently; an HTTP gateway is one process with shared state, and its
cases run serially unless the subject can be session-isolated. Defaulting
`http_subjects` to 1 is the safe choice, and a user raising it is asserting something about their
subject that charpy cannot verify.

---

## 4. Validation

Three layers, all at load, none at run:

1. **Strict decode.** `DisallowUnknownFields`. A typo in a key is an error, not a silently ignored
   line. This is the single most valuable property of the format: a matcher that silently matches
   nothing is a test that silently passes.
2. **Semantic checks.**
   - `verdict = "MUST"` requires a `schema:` source (`oracle.md` §2). Enforced here rather than in
     review, because "never issue a behavioural MUST" is a rule about the product, not about
     authors' memories.
   - `id` is unique across every loaded file, and well-formed per the `case-identity.md` grammar.
   - `applies_to` parses and names revisions charpy knows.
   - `fault.kind` names a real mechanism and its parameters typecheck against that mechanism.
   - `expect.invariants` names real invariants.
   - `occurrence` and `occurrence_every` are mutually exclusive.
3. **Warnings**, which do not fail the load: an empty `[case.match]`; a case whose `applies_to`
   excludes every revision charpy knows; a `withdrawn` case still selected.

```
$ charpy policy validate cases/
cases/stream.toml:24:3: unknown key "cut_after" in [case.fault]
  fault kind "truncate" accepts: cut_at, after_bytes, then
cases/gateway.toml:11:1: case "gateway/header-rederived" declares verdict = "MUST"
  but derives_from = "spec:..."; MUST requires a schema: source (see oracle.md §2)
2 errors, 0 warnings
```

Errors carry file, line and column — this is why `pelletier/go-toml/v2` was chosen over
`BurntSushi/toml` — and name the accepted alternatives. A control plane whose error messages require
reading the source is a control plane people work around.

---

## 5. Hot swap

Swapping policy without restarting the run needs an HTTP endpoint.

```
POST /control/policy      body: the TOML; validates, then swaps atomically
GET  /control/policy      current policy and its digest
GET  /control/status      run state, cases executed, frames seen
POST /control/withdraw    withdraw all active faults now
```

A swap that fails validation changes nothing and returns the same diagnostics as
`charpy policy validate`. Each swap emits a `note` event into the transcript carrying the new
digest, so a transcript spanning a swap is still self-describing about what produced each frame —
`policy_digest` in the header line is the *initial* policy, and the note events are the amendments.

`POST /control/withdraw` exists for the operator who needs the subject back immediately. It is also
the manual version of what liveness testing does automatically.

The control plane binds to localhost by default. It executes fault injection against whatever charpy
is pointed at, so exposing it on `0.0.0.0` is a decision a user must make explicitly with
`--control-addr`.
