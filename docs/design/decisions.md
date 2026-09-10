# Decisions

Short ADR entries for the design questions that had to be settled before the code that depends on
them could be written. Each records what was decided, why, and what would have to change for the
decision to be revisited.

| | Decision | Topic |
|---|---|---|
| ADR-001 | Determinism belongs to the oracle, not the run | Clock injection |
| ADR-002 | The in-process driver stays internal | Package boundaries |
| ADR-003 | No embedded OTLP receiver in v0 | Telemetry |
| ADR-004 | Pin reference SDKs *and* float them, in separate jobs | Cross-SDK differential |
| ADR-005 | Trace context per SEP-414; charpy's own keys under `dev.charpy/` | Correlation |
| ADR-006 | TOML for policy and case manifests | Control plane |
| ADR-007 | Case identity: stable slug, revision at citation | Case identity |
| ADR-008 | Truncation variants are case parameters, not separate faults | Fault taxonomy |
| ADR-009 | Registries live with the code they describe | Package boundaries |
| ADR-010 | One reference peer and an interposer; no feral peer | Architecture |

---

## ADR-001 — Determinism belongs to the oracle, not the run

**Question.** Can charpy legitimately freeze time for a stdio subject it launched, or only
for its own scheduling?

**Decision.** Only for its own scheduling. The injected clock governs charpy's scheduling and the
transcript's logical timeline. Subject deadlines remain real-time.

**Why.** Freezing a subprocess's clock requires `LD_PRELOAD`/`libfaketime`, ptrace, or a seccomp
shim. All three forfeit the property that makes charpy adoptable at all — that it is a binary you
point at code in any language with no cooperation from the subject. A tool that needs `libfaketime`
installed and working for a Python subject on macOS is not language-agnostic, it is a research
prototype. The adoption tax charpy exists to remove — no Node toolchain, no venv, no cooperation
from the subject — would simply reappear in a different place.

**The consequence, which is the useful part.** A flaky resilience suite is worth nothing, because
every failure becomes "probably the harness". That anxiety is well founded, but it is usually aimed
at the wrong target. Runs cannot be made bit-reproducible against a live subject — the subject
is loaded differently, garbage-collects at a different moment, or was redeployed. What can be made
exactly reproducible is the **judgement**:

> Runs are best-effort reproducible. Verdicts are exactly reproducible, forever, given a transcript.

This is achievable precisely because the oracle is offline (`oracle.md` §1). It yields three rules:

1. Cases assert on **ordering**, never on wall-clock durations.
2. Cases needing a real-time bound declare a generous budget and assert that recovery happened, not
   how fast.
3. **Nothing in the oracle may read `t_wall`.** It exists only to join against the subject's own
   telemetry. Enforced by a lint over `internal/oracle/`.

And one shipped guarantee: `charpy replay run.jsonl` twice produces byte-identical verdicts. It is a
CI gate, not an aspiration.

### The clock is a property of the run

Which clock is in force is declared per run, not baked into charpy:

| `clock` | Used by | Meaning |
|---|---|---|
| `injected` | v0 case runs | charpy's scheduling is driven by the injected clock. The default. |
| `real` | soak runs (`soak.md`) | charpy's scheduling follows wall time. |

Soak runs are inherently real-time — you cannot compress a six-hour leak test, because the leak is a
function of the subject's real elapsed time and its real reconnect backoff. Declaring
`clock = "real"` is honest about that, rather than pretending an injected clock is doing something
it is not.

The determinism guarantee survives the switch untouched, which is the point of putting determinism
in the oracle. `t_mono_ns` is monotonic-from-run-start under both modes; only its *source* differs.
The oracle reads `t_mono_ns` and never `t_wall`, so it cannot tell which mode produced a transcript
and replays both identically. The mode is recorded in the transcript header so a reader knows what
the numbers mean.

**Revisit if.** charpy ever grows an in-process-only mode where the subject links charpy's clock
directly — the `inproc` driver already could, and a Go fixture is the one case where freezing time is
honest. That would be a per-driver capability, never a default.

---

## ADR-002 — The in-process driver stays internal

**Question.** Public Go package or internal?

**Decision.** `internal/driver/inproc` for v0.

**Why.** Exposing it creates an API surface that must be maintained across every refactor of the
interposer, in exchange for a benefit charpy cannot yet size — nobody has asked for it. Promotion later
is a one-line move; demotion after someone depends on it is a breaking change and an apology. The
asymmetry is the whole argument.

The driver exists for what the wire cannot see: goroutine leaks per failed upstream, wedged mutexes,
unbounded channel growth, session maps that never evict, data races. Those need `-race` and
`runtime.NumGoroutine` around a fixture linked directly. That is served fine from `internal/` by
charpy's own fixture gateway.

**Revisit if.** Someone testing a Go gateway asks for it, in which case the request itself tells us
what shape the API should be — which is better information than we could invent now.

---

## ADR-003 — No embedded OTLP receiver in v0

**Question.** Embed a minimal OTLP receiver, or only document the join?

**Decision.** Document the join. No receiver in v0.

**Why.** Everyone who cares about their subject's traces already runs a collector — Jaeger, Tempo,
whatever. Writing a second one into charpy competes with tools that do it better, for the sake of
saving a configuration step. charpy's job is to emit a correlation key good enough to join on, and
`link.trace_id` plus `t_wall` is that key.

**Revisit if.** Documenting the join proves insufficient in practice — specifically, if people
repeatedly cannot join because their collector does not retain the subject's spans long enough to
match a charpy run. Then an optional receiver writing subject spans as JSONL next to the transcript
becomes attractive, because it makes the join a local DuckDB query and keeps the single binary.

---

## ADR-004 — Pin reference SDKs *and* float them

**Question.** Pin the cross-SDK reference peers, or let them float? Pinning is reproducible;
floating finds regressions.

**Decision.** Both. They were never in conflict.

- The default build pins Go, TypeScript and Python SDK versions in `go.mod` and lockfiles. A
  divergence table published today reproduces a year from now.
- A separate scheduled CI job floats every peer to latest, reruns the differential, and diffs the
  divergence *set* against the pinned baseline. A change in that set is the signal — either an SDK
  fixed something, or an SDK regressed, and both are worth a look.

**Why.** Reproducibility and regression detection are different jobs wanting different inputs. Asking
one configuration to serve both produces a suite that is either untrustworthy or blind. Two jobs cost
one CI file.

The scheduled job's output is a diff, not a verdict. An SDK changing behaviour is not a failure of
charpy and must not fail charpy's build.

---

## ADR-005 — Trace context per SEP-414

**Question.** Does the spec or any SDK define a `_meta` key for W3C trace context? If not,
pick a namespaced key and propose it upstream.

**Decision.** It does. Use `traceparent`, `tracestate` and `baggage` in `_meta`, unprefixed, per
SEP-414. Nothing to propose.

**Why.** SEP-414 documents these three keys as an explicit exception to the reverse-DNS prefix
convention, for compatibility with OpenTelemetry semantic conventions. `_meta` rather than headers is
also the right carrier for charpy's purpose: it works on stdio, and over Streamable HTTP a
long-lived stream can carry many calls, so header-based tracing captures only the connection while
`_meta` gives each call its own parentage.

charpy stamps `traceparent` on every request it originates on both transports, and additionally sets
the HTTP header. Its own non-trace markers use `dev.charpy/`, which is permitted because the reserved
prefixes are those whose second label is `modelcontextprotocol` or `mcp`.

This settles the question with no upstream work at all, which is a better outcome than expected when
it was raised.

---

## ADR-006 — TOML for policy and case manifests

**Question.** What format should the fault policy and case manifests use?

**Decision.** TOML, via `pelletier/go-toml/v2`, validated by strict decode plus semantic checks.
Details in `policy-format.md`.

**Why.** TOML has no type-inference ambiguity to defend against, and array-of-tables carries a case
catalogue cleanly. `go-toml/v2` reports decode errors with line and column, which is most of the felt
quality of a control plane. The control plane is the component most likely to overrun, and confusing
config errors are how that happens.

The cost is that TOML has no JSON Schema equivalent, so validation is Go code rather than a schema
file. Mitigated by `DisallowUnknownFields` and a `charpy policy validate` subcommand: a typo in a
matcher must fail loudly at load, never silently match nothing.

**Considered.** YAML would have matched `modelcontextprotocol/conformance`, which uses
`requirements/*.yaml`, and that consistency has some value for a project positioning itself as a
sibling. Rejected for YAML's type coercion surprises in a file where a mistyped matcher silently
disables a test.

**Amended 2026-09-09.** "No JSON Schema equivalent" overstated the cost. Taplo validates TOML
against a JSON Schema via a `#:schema` directive, and the schema is generated from the registries
(ADR-009) rather than written by hand, so authors get completion and hover documentation without a
second source of truth. JSON as a format was reconsidered and rejected: it would surrender comments
in the one file that doubles as documentation, and it does not solve the two problems it appears to —
registry single-sourcing is format-independent, and JSON Schema validators report pointer paths, not
line numbers, so the semantic-error position gap is identical in either format. See
`policy-format.md` §6; the position gap itself is scoped in `../open-problems.md`.

---

## ADR-007 — Case identity

Decided in full in `case-identity.md`. Summary: permanent kebab slug, revision and seed applied at
citation time, applicability declared as a range in the manifest.

The one contested point is that revision is not literally inside the permanent ID. The requirement
satisfied is that a *citation* be unambiguous, not that the ID string contain a date. The rationale —
that the divergence table and cross-revision regression questions both need one identity per
behaviour — is recorded in `case-identity.md` §2.

---

## ADR-008 — Truncation variants are case parameters

**Question.** SSE truncation: cut at byte, at event boundary, or after `event:` but before
`data:`? All three are different faults.

**Decision.** All three are different **cases** over one **mechanism**, distinguished by a
`cut_at` parameter. Seven values are defined in `faults-and-cases.md` §2.

**Why.** They differ in what you would claim in a bug report, which makes them separate cases. They
do not differ in what the code does — write N bytes, then stop — which makes them one mechanism.
Implementing three mechanisms would triple the code for one behaviour and make the fourth variant a
fourth code path rather than a fourth line of TOML.

This generalises into the rule the whole catalogue follows: **if it changes bytes, it is a mechanism
parameter; if it changes what you would claim in a bug report, it is a case.**

---

## ADR-009 — Registries live with the code they describe

**Question.** The loader validates case manifests against three tables: fault mechanisms and their
parameters, matcher keys and scopes, and invariant names. Where do those tables live?

**Decision.** With the code they describe, from v0:

| Registry | Package | Why there |
|---|---|---|
| Mechanisms, parameters, values | `internal/fault` | The mechanism implementations are there |
| Match keys and scopes | `internal/interpose` | The interposer evaluates `[case.match]` |
| Invariant names | `internal/oracle/invariant` | The invariant implementations are there |

The catalogue imports all three and validates manifests against them. `[case.expect]`'s key list
stays in the catalogue — it names things owned elsewhere but is itself manifest surface.

**Why.** A parameter table maintained apart from the code it describes drifts, and here drift
inverts the format's best property: a value added to the fault code but not the table makes a valid
case fail to load — strict validation gone wrong-way. One registry, defined where the behaviour is
implemented, consumed by the loader and by the schema generator (`policy-format.md` §6), means the
loader, the editor tooling and the implementation cannot disagree.

The registries carry doc strings — mechanism, parameter, value and invariant summaries — because
they are the single place that knowledge lives; the generated schema turns them into editor hover
documentation for free. The indirection costs a few files and some structs. v0 has no hot path, and
correctness of the catalogue is worth more than compactness.

**Dependency direction, fixed here.** The catalogue is the outer layer: it parses manifests and
compiles them into the domain packages' terms. `fault`, `interpose` and `invariant` never import
the catalogue — the interposer consumes compiled policy as its own types and knows nothing about
TOML. This is what keeps the future interposer ⇄ catalogue import cycle structurally impossible
rather than merely avoided.

**Incidental fix.** Moving the mechanism registry surfaced a collision: `malformed_json`'s parameter
was named `kind`, which the `[case.fault]` table already uses to select the mechanism, so the
parameter could never be set. Renamed to `how`; a registry test now rejects any parameter named
`kind`.

**Revisit if.** A registry needs to describe something with no owning package — that would be a
sign the package layout, not the registry placement, is wrong.

---

## ADR-010 — One reference peer and an interposer; no feral peer

**Question.** The pre-design brief decided "two peers, one message model": a reference-correct
peer on the official SDK and a feral peer on raw wire, because "the SDK cannot be the hostile
side." Is the hostile side really a second peer?

**Decision.** No. There is one reference peer — the official SDK, era selected at construction,
never coerced into misbehaving — and one **interposer** that owns the wire and applies faults
with three verbs: rewrite, withhold, synthesize. The reference peer plugs into the interposer as
its `Transport`; charpy man-in-the-middles its own SDK. What remains of the feral layer is not a
peer: the raw wire framing (`internal/wire`) and `json.RawMessage` frame templates for the
synthesize verb. `internal/engine` is renamed `internal/interpose` — "engine" described a bag of
behaviour; "interposer" names the position that behaviour occupies. Full design in
`interposer.md`.

**Why.** The brief's premise is true — a correct SDK will not emit a duplicate id or a truncated
frame — but the conclusion doesn't follow, because hostility doesn't have to live in the peer.
With the corruptor positioned as the SDK's transport, the SDK emits correct frames and the wire
carries corrupted ones; no coercion anywhere. Walking the v0 catalogue: every mechanism reduces
to the three verbs plus legitimate SDK reconfiguration (`manifest_mutate` is a real manifest
change with the notification optionally swallowed; `capability_flip` is a reconfigured peer at a
reconnect boundary). The apparent exception, era-lying, decomposes instead of requiring a
translator: a *lie* is a one-frame rewrite whose incoherence is the fault, coherent *speech* in
another era is SDK configuration, and the hybrids are splices (`interposer.md` §4).

Two consequences fall out and both were already wanted:

- **The double-entry ledger stops being optional.** Self-MITM forces intent-versus-wire
  bookkeeping — the interposer must un-rewrite the subject's responses or wedge its own SDK —
  and that same ledger is what the fault-poisoning cases and cross-face correlation needed
  anyway. One structure, three consumers.
- **The `dev.charpy/` argument marker is deleted.** The `inferred` correlation regime needed it
  only while correlation was imagined as a post-hoc guess over found traffic. The scenario
  player originates the traffic, so seeded distinctness inside schema-legitimate argument values
  gives the ledger a content join with zero perturbation of the subject (`interposer.md` §6,
  `transcript.md` §4).

**Cost.** The interposer must be scriptable around its own faults — the reference peer responds
in its real configuration to a subject that believed a lie, and the ledger has to tag those
frames as consequences, not subject behaviour. That attribution logic is new. It is also exactly
the "honest ledger of a falsified conversation" problem in its simplest form, so the cost is
front-loaded design, not accident-prone sprawl.

**Supersedes.** The "two peers, one message model" and "two drivers, one fault engine" framings.
`envelope` remains the shared message model — now shared by the reference peer, the interposer
and the templates rather than by two peers.

**Revisit if.** A case genuinely requires charpy to *coherently* speak one era over a connection
whose configuration is another — sustained live translation. None is known, and §4 of
`interposer.md` is the argument none should exist.
