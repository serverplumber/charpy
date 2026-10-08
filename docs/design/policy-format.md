# Policy and case manifest format

Status: **Decided.** TOML, per `decisions.md` ADR-006. This is the control
plane: the component most likely to overrun its budget, and the one that
makes charpy a tool rather than a script.

---

## 1. Two files, one schema

| File | Contains | Ships with charpy |
|---|---|---|
| `cases/*.toml` | The case catalogue — named, citable, revision-scoped | Yes, embedded |
| A run policy | Ad-hoc faults for a single run, no case IDs | No, user-supplied |

Both parse into the same structures. A case is a policy entry with an
identity; a policy entry is an anonymous case. Sharing the parser means the
format cannot drift between "what charpy ships" and "what a user can
write", which is what keeps the catalogue honest — everything charpy does,
a user can express.

---

## 2. Case entries

```toml
schema_version = 1

[[case]]
id           = "stream/truncate-event-boundary"
applies_to   = ">=2026-07-28"
subject      = ["client", "gateway"]
transport    = ["http"]
verdict      = "OBSERVED"
derives_from = "spec:2026-07-28/basic/transports/streamable-http#receiving-messages"
summary      = "SSE stream stops cleanly between events; the request is never answered."
status       = "active"

  [case.match]
  method     = "tools/call"
  face       = "upstream"
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
| `observed_by` | no | What can see the subject's answer: `wire` (default) · `inproc` · `differential`. Selection drops a case no available observer can judge |
| `verdict` | yes | `MUST` or `OBSERVED` |
| `derives_from` | yes | `spec:` · `sep:` · `schema:` · `none` |
| `summary` | yes | One sentence; appears in the report |
| `status` | no | `active` (default) or `withdrawn`, with `withdrawn_reason` |

### `[case.match]`

Selects which frame the fault attaches to. All present keys must match;
absent keys match anything.

| Key | Values |
|---|---|
| `method` | Exact method name, or a `*` glob |
| `face` | `downstream` · `upstream` |
| `direction` | `c2s` · `s2c` |
| `kind` | `request` · `response` · `error` · `notification` |
| `client_id` | Exact id, or a `*` glob |
| `session_id` | Exact id, or a `*` glob |
| `occurrence` | Integer, 1-based — lets a case say "the third `tools/call`". Defaults to 1 |
| `occurrence_every` | Integer — every Nth match; `1` is every match. Mutually exclusive with `occurrence` |
| `after_mono_ms` | Integer — not before this point on the clock |
| `scope` | Which population the ordinal counts within: `run` (default) · `client` · `session` · `connection` |

**A case with neither `occurrence` nor `occurrence_every` fires once, on
the first matching frame.** One fault is what a case means: a transcript
carrying the same fault on every frame that fits is a puzzle rather than a
finding (ADR-012), and under a relay, where charpy does not choose the
traffic, "every" is a storm. A case that does want each frame says
`occurrence_every = 1`.

A matcher should also name the frame its summary means. A case that names
only a direction lands on the first frame going that way, which is the
handshake answer, and a case about whether a connection survives then
leaves no connection to ask about.

**`direction` is required, and it must put the fault to the subject.** A
damaged frame is a question put to whoever receives it, so the loader
refuses a case whose direction and face do not deliver its fault to every
subject class it lists: `c2s` (downstream) for a server, `s2c` (upstream)
for a client, and for a gateway an explicit face it receives on. A
mechanism that acts only on some frame kinds also requires `kind`, naming
one of them. Both are refused at load rather than reported per run, because
a case that breaks them is wrong on every run (`decisions.md` ADR-013). An
empty `[case.match]` -- which used to earn a warning -- is refused for the
same reason.

### `scope`, and why it exists before it is needed

`scope` names the population an `occurrence` counts within. In v0 there is
one client and one session, so every scope collapses to `run` and the field
changes nothing.

It is here because of what comes next. A soak policy needs to express
*"30% of clients reconnect every 2s without closing"* — a
**rate over a population**, not an ordinal over a stream. Those are
different selector kinds, and a matcher that has only ever counted globally
cannot grow one: the interposer's counters have to be keyed by
`(scope, dimension value)` rather than being a single integer. That is a
data-structure decision, not a feature, and it is cheap now and invasive
later.

So v0 keys its counters that way, with a single key. The v1 addition is
then a new selector kind sitting alongside `occurrence` rather than a
rework of it:

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

The rule is the same one that put `client_id` in the transcript: **do not
build the feature, but do not choose a structure that forbids it.**
`select` is a sibling table rather than more keys on `match`, because
"which frames does this apply to" and "which members of a population, how
often" are different questions, and collapsing them is what makes matcher
languages unreadable.

### `[case.fault]`

`kind` names a mechanism from `faults-and-cases.md` §2; the remaining keys
are that mechanism's parameters. Parameters are validated against the
mechanism — an unknown key, or a value outside a mechanism's enum, is a
load error naming the file, line, mechanism and the accepted values.

### `[case.expect]`

What the oracle should check for this case. Optional; when absent, the
universal invariants still run.

| Key | Meaning |
|---|---|
| `invariants` | Invariant names that must hold. Unknown names are a load error. Informational: the universal invariants run on every transcript regardless |
| `liveness_probe_within_ms` | Real-time budget for recovery after withdrawal (`oracle.md` §6) |
| `expect_error_code` | The subject is expected to answer the faulted request with this JSON-RPC code; a null-id error counts, since that is how JSON-RPC answers a request too broken to carry an id |
| `expect_http_status` | The subject's first HTTP answer after the fault is expected to carry this status. HTTP only |
| `expect_negotiated_only` | Boolean. After the handshake the fault rewrote, the subject is expected to send only requests that handshake's capabilities allow, on that connection |

The `expect_` keys are judged by the reaction layer's `expectation`
check (`oracle.md` §6), which reads them from the catalogue by case id at
replay: the transcript names the case, not what it expects.

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

`[parallel]` encodes the rule that parallelism is subject-bound, not
charpy-bound. stdio subjects spawn per case and run concurrently; an HTTP
gateway is one process with shared state, and its cases run serially unless
the subject can be session-isolated. Defaulting `http_subjects` to 1 is the
safe choice, and a user raising it is asserting something about their
subject that charpy cannot verify.

---

## 4. Validation

Four layers, all at load, none at run:

1. **Strict decode.** `DisallowUnknownFields`. A typo in a key is an error,
   not a silently ignored line. This is the single most valuable property
   of the format: a matcher that silently matches nothing is a test that
   silently passes.
2. **Registry checks.** The tables validated against live with the code
   they describe — mechanisms and their parameters in `internal/fault`,
   match keys and scopes in `internal/interpose`, invariant names in
   `internal/oracle/invariant` — and the loader consumes them, so
   validation cannot drift from implementation (`decisions.md` ADR-009).
   Covers:
   - `fault.kind` names a real mechanism, its parameters exist, enumerated
     values are in range, and free-form values have the declared type.
   - `[case.match]` keys exist, enumerated keys (`face`, `direction`,
     `kind`, `scope`) are in range, and ordinal keys are integers.
   - `expect.invariants` names real invariants.
3. **Semantic checks.**
   - `verdict = "MUST"` requires a `schema:` source (`oracle.md` §2).
     Enforced here rather than in review, because "never issue a
     behavioural MUST" is a rule about the product, not about authors'
     memories.
   - `id` is unique across every loaded file, and well-formed per the
     `case-identity.md` grammar.
   - `applies_to` parses and names revisions charpy knows.
   - `occurrence` and `occurrence_every` are mutually exclusive.
4. **Warnings**, which do not fail the load: an empty `[case.match]`; a
   case whose `applies_to` excludes every revision charpy knows; a
   `withdrawn` case still selected.

```text
$ charpy policy validate cases/
cases/stream.toml:24:3: strict mode: unknown key "sumary"
cases/stream.toml: case "stream/truncate-mid-event": unknown key "cut_after" in [case.fault];
  kind "truncate" accepts: cut_at, after_bytes, then
cases/gateway.toml: case "gateway/header-rederived": verdict = "MUST"
  but derives_from = "spec:..."; MUST requires a schema: source (see oracle.md §2)
3 errors, 0 warnings
```

What an error can locate depends on the layer that caught it, and the
promise is honest about the difference:

- **Decode errors carry file, line and column** — this is why
  `pelletier/go-toml/v2` was chosen over `BurntSushi/toml`.
- **Registry and semantic errors carry file and case ID, and name the
  accepted alternatives.** They run after strict decode, over plain Go
  values, where position information no longer exists. A case ID plus the
  offending key finds the line in one editor search, and the accepted-
  alternatives list is what actually fixes the mistake.

Extending positions to the later layers is a known gap, deliberately not
closed for v0 — the cost and the paths are recorded in
[`../open-problems.md`](../open-problems.md). Taplo covers the interactive
case meanwhile (§6): the generated schema flags an unknown key or
out-of-range value at its exact position while the file is being written,
which is earlier than any loader message.

Either way, an error names the accepted alternatives. A control plane whose
error messages require reading the source is a control plane people work
around.

**A failed validation exits 4**, which is a code of its own rather than a
reuse of 2. An invalid policy is not charpy breaking, not a MUST violation
and not a subject that would not start; it is input charpy declines to run
on. The code is appended to the table rather than inserted into it, so the
ordering the CI contract depends on is untouched.

Exiting 0 was considered and is wrong for the reason ADR-006 exists: the
whole point of strict decoding is that a typo fails *loudly*, and a tool
that reports findings on stdout while exiting successfully has to be
wrapped by every caller that wants a gate. charpy's own justfile already
carries that wrapper for `gofmt -l`, which is the tool that gets this
wrong.

The code is the machine half and is insufficient on its own. The
diagnostics above are the contract that matters: every broken file in one
run rather than the first, each error naming the file, the case and the
accepted alternatives.

---

## 5. Hot swap

Swapping policy without restarting the run needs an HTTP endpoint.

```text
POST /control/policy      body: the TOML; validates, then swaps atomically
GET  /control/policy      current policy and its digest
GET  /control/status      run state, cases executed, frames seen
POST /control/withdraw    withdraw all active faults now
```

A swap that fails validation changes nothing and returns the same
diagnostics as `charpy policy validate`. Each swap emits a `note` event
into the transcript carrying the new digest, so a transcript spanning a
swap is still self-describing about what produced each frame —
`policy_digest` in the header line is the *initial* policy, and the note
events are the amendments.

`POST /control/withdraw` exists for the operator who needs the subject back
immediately. It is also the manual version of what liveness testing does
automatically.

The control plane binds to localhost by default. It executes fault
injection against whatever charpy is pointed at, so exposing it on
`0.0.0.0` is a decision a user must make explicitly with `--control-addr`.

---

## 6. Authoring tooling

ADR-006 accepted that TOML has no JSON Schema equivalent, expecting
validation to be Go code only. That turned out to be half the cost it
looked like: **Taplo** — the TOML language server behind the Even Better
TOML editor extension — validates and autocompletes TOML against a JSON
Schema, wired by a directive on the file's first line:

```toml
#:schema ./case.schema.json
```

The schema is not hand-written. `just case-schema` generates
`cases/case.schema.json` from the same registries the loader validates
against (§4), so the editor tooling, the loader and the implementation
share one source of truth. The registries carry doc strings for every
mechanism, parameter, enumerated value and invariant; the generator emits
them as `description` fields, so hovering `cut_at = "event_boundary"` in an
editor shows what the value means and which transport it applies to.

Kept honest by two tests in `internal/catalogue`:

- **Staleness.** The committed schema must byte-match what the registries
  currently generate. Changing a registry without running
  `just case-schema` fails CI.
- **Agreement.** Every shipped manifest must validate against the generated
  schema, and a set of deliberately broken documents must not. The schema
  can neither lag the loader nor rot into accepting everything.

The schema covers structure — keys, types, enums, required fields. The
cross-file and semantic rules (id uniqueness, `applies_to` resolution,
MUST-requires-`schema:`) remain the loader's job, and
`charpy policy validate` remains the authority; Taplo is the fast feedback
in front of it.

`taplo` is in the Nix shell. `taplo lint cases/*.toml` checks from the CLI
what the editor checks interactively; user-supplied run policies get the
same tooling by adding the directive pointing at a vendored or URL copy of
the schema.
