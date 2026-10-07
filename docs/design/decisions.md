# Decisions

Short ADR entries for the design questions that had to be settled before
the code that depends on them could be written. Each records what was
decided, why, and what would have to change for the decision to be
revisited.

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
| ADR-011 | The reference peer is the Go SDK, pinned to a pre-release, seamed at bytes | Reference peer |
| ADR-012 | One case armed per run under owned stimulus | Scenario player |
| ADR-013 | A fault is put to its recipient, and judged by the recipient's reaction | Oracle |
| ADR-014 | charpy stamps its notifications as well as its requests | Correlation |
| ADR-015 | A gateway is judged downstream: the call a fault carried, then three questions | Oracle |
| ADR-016 | Whether a run can reach a case's frame is derived from the matcher | Scenario player |
| ADR-017 | A frame that never ends is a finding, and charpy stops reading it | Interposer |

---

## ADR-001 — Determinism belongs to the oracle, not the run

**Question.** Can charpy legitimately freeze time for a stdio subject it
launched, or only for its own scheduling?

**Decision.** Only for its own scheduling. The injected clock governs
charpy's scheduling and the transcript's logical timeline. Subject
deadlines remain real-time.

**Why.** Freezing a subprocess's clock requires `LD_PRELOAD`/`libfaketime`,
ptrace, or a seccomp shim. All three forfeit the property that makes charpy
adoptable at all — that it is a binary you point at code in any language
with no cooperation from the subject. A tool that needs `libfaketime`
installed and working for a Python subject on macOS is not
language-agnostic, it is a research prototype. The adoption tax charpy
exists to remove — no Node toolchain, no venv, no cooperation from the
subject — would simply reappear in a different place.

**The consequence, which is the useful part.** A flaky resilience suite is
worth nothing, because every failure becomes "probably the harness". That
anxiety is well founded, but it is usually aimed at the wrong target. Runs
cannot be made bit-reproducible against a live subject — the subject is
loaded differently, garbage-collects at a different moment, or was
redeployed. What can be made exactly reproducible is the **judgement**:

> Runs are best-effort reproducible. Verdicts are exactly reproducible,
> forever, given a transcript.

This is achievable precisely because the oracle is offline (`oracle.md`
§1). It yields three rules:

1. Cases assert on **ordering**, never on wall-clock durations.
2. Cases needing a real-time bound declare a generous budget and assert
   that recovery happened, not how fast.
3. **Nothing in the oracle may read `t_wall`.** It exists only to join
   against the subject's own telemetry. Enforced by a lint over
   `internal/oracle/`.

And one shipped guarantee: `charpy replay run.jsonl` twice produces
byte-identical verdicts. It is a CI gate, not an aspiration.

### The clock is a property of the run

Which clock is in force is declared per run, not baked into charpy:

| `clock` | Used by | Meaning |
|---|---|---|
| `injected` | v0 case runs | charpy's scheduling is driven by the injected clock. The default. |
| `real` | soak runs (`soak.md`) | charpy's scheduling follows wall time. |

Soak runs are inherently real-time — you cannot compress a six-hour leak
test, because the leak is a function of the subject's real elapsed time and
its real reconnect backoff. Declaring `clock = "real"` is honest about
that, rather than pretending an injected clock is doing something it is
not.

The determinism guarantee survives the switch untouched, which is the point
of putting determinism in the oracle. `t_mono_ns` is
monotonic-from-run-start under both modes; only its *source* differs. The
oracle reads `t_mono_ns` and never `t_wall`, so it cannot tell which mode
produced a transcript and replays both identically. The mode is recorded in
the transcript header so a reader knows what the numbers mean.

**Revisit if.** charpy ever grows an in-process-only mode where the subject
links charpy's clock directly — the `inproc` driver already could, and a Go
fixture is the one case where freezing time is honest. That would be a
per-driver capability, never a default.

---

## ADR-002 — The in-process driver stays internal

**Question.** Public Go package or internal?

**Decision.** `internal/driver/inproc` for v0.

**Why.** Exposing it creates an API surface that must be maintained across
every refactor of the interposer, in exchange for a benefit charpy cannot
yet size — nobody has asked for it. Promotion later is a one-line move;
demotion after someone depends on it is a breaking change and an apology.
The asymmetry is the whole argument.

The driver exists for what the wire cannot see: goroutine leaks per failed
upstream, wedged mutexes, unbounded channel growth, session maps that never
evict, data races. Those need `-race` and `runtime.NumGoroutine` around a
fixture linked directly. That is served fine from `internal/` by charpy's
own fixture gateway.

**Revisit if.** Someone testing a Go gateway asks for it, in which case the
request itself tells us what shape the API should be — which is better
information than we could invent now.

---

## ADR-003 — No embedded OTLP receiver in v0

**Question.** Embed a minimal OTLP receiver, or only document the join?

**Decision.** Document the join. No receiver in v0.

**Why.** Everyone who cares about their subject's traces already runs a
collector — Jaeger, Tempo, whatever. Writing a second one into charpy
competes with tools that do it better, for the sake of saving a
configuration step. charpy's job is to emit a correlation key good enough
to join on, and `link.trace_id` plus `t_wall` is that key.

**Revisit if.** Documenting the join proves insufficient in practice —
specifically, if people repeatedly cannot join because their collector does
not retain the subject's spans long enough to match a charpy run. Then an
optional receiver writing subject spans as JSONL next to the transcript
becomes attractive, because it makes the join a local DuckDB query and
keeps the single binary.

---

## ADR-004 — Pin reference SDKs *and* float them

**Question.** Pin the cross-SDK reference peers, or let them float? Pinning
is reproducible; floating finds regressions.

**Decision.** Both. They were never in conflict.

- The default build pins Go, TypeScript and Python SDK versions in `go.mod`
  and lockfiles. A divergence table published today reproduces a year from
  now.
- A separate scheduled CI job floats every peer to latest, reruns the
  differential, and diffs the divergence *set* against the pinned baseline.
  A change in that set is the signal — either an SDK fixed something, or an
  SDK regressed, and both are worth a look.

**Why.** Reproducibility and regression detection are different jobs
wanting different inputs. Asking one configuration to serve both produces a
suite that is either untrustworthy or blind. Two jobs cost one CI file.

The scheduled job's output is a diff, not a verdict. An SDK changing
behaviour is not a failure of charpy and must not fail charpy's build.

---

## ADR-005 — Trace context per SEP-414

**Question.** Does the spec or any SDK define a `_meta` key for W3C trace
context? If not, pick a namespaced key and propose it upstream.

**Decision.** It does. Use `traceparent`, `tracestate` and `baggage` in
`_meta`, unprefixed, per SEP-414. Nothing to propose.

**Why.** SEP-414 documents these three keys as an explicit exception to the
reverse-DNS prefix convention, for compatibility with OpenTelemetry
semantic conventions. `_meta` rather than headers is also the right carrier
for charpy's purpose: it works on stdio, and over Streamable HTTP a
long-lived stream can carry many calls, so header-based tracing captures
only the connection while `_meta` gives each call its own parentage.

charpy stamps `traceparent` on every request it originates on both
transports, and additionally sets the HTTP header. Its own non-trace
markers use `dev.charpy/`, which is permitted because the reserved prefixes
are those whose second label is `modelcontextprotocol` or `mcp`.

This settles the question with no upstream work at all, which is a better
outcome than expected when it was raised.

---

## ADR-006 — TOML for policy and case manifests

**Question.** What format should the fault policy and case manifests use?

**Decision.** TOML, via `pelletier/go-toml/v2`, validated by strict decode
plus semantic checks. Details in `policy-format.md`.

**Why.** TOML has no type-inference ambiguity to defend against, and
array-of-tables carries a case catalogue cleanly. `go-toml/v2` reports
decode errors with line and column, which is most of the felt quality of a
control plane. The control plane is the component most likely to overrun,
and confusing config errors are how that happens.

The cost is that TOML has no JSON Schema equivalent, so validation is Go
code rather than a schema file. Mitigated by `DisallowUnknownFields` and a
`charpy policy validate` subcommand: a typo in a matcher must fail loudly
at load, never silently match nothing.

**Considered.** YAML would have matched `modelcontextprotocol/conformance`,
which uses `requirements/*.yaml`, and that consistency has some value for a
project positioning itself as a sibling. Rejected for YAML's type coercion
surprises in a file where a mistyped matcher silently disables a test.

**Amended 2026-09-09.** "No JSON Schema equivalent" overstated the cost.
Taplo validates TOML against a JSON Schema via a `#:schema` directive, and
the schema is generated from the registries (ADR-009) rather than written
by hand, so authors get completion and hover documentation without a second
source of truth. JSON as a format was reconsidered and rejected: it would
surrender comments in the one file that doubles as documentation, and it
does not solve the two problems it appears to — registry single-sourcing is
format-independent, and JSON Schema validators report pointer paths, not
line numbers, so the semantic-error position gap is identical in either
format. See `policy-format.md` §6; the position gap itself is scoped in
`../open-problems.md`.

---

## ADR-007 — Case identity

Decided in full in `case-identity.md`. Summary: permanent kebab slug,
revision and seed applied at citation time, applicability declared as a
range in the manifest.

The one contested point is that revision is not literally inside the
permanent ID. The requirement satisfied is that a *citation* be
unambiguous, not that the ID string contain a date. The rationale — that
the divergence table and cross-revision regression questions both need one
identity per behaviour — is recorded in `case-identity.md` §2.

---

## ADR-008 — Truncation variants are case parameters

**Question.** SSE truncation: cut at byte, at event boundary, or after
`event:` but before `data:`? All three are different faults.

**Decision.** All three are different **cases** over one **mechanism**,
distinguished by a `cut_at` parameter. Seven values are defined in
`faults-and-cases.md` §2.

**Why.** They differ in what you would claim in a bug report, which makes
them separate cases. They do not differ in what the code does — write N
bytes, then stop — which makes them one mechanism. Implementing three
mechanisms would triple the code for one behaviour and make the fourth
variant a fourth code path rather than a fourth line of TOML.

This generalises into the rule the whole catalogue follows: **if it changes
bytes, it is a mechanism parameter; if it changes what you would claim in a
bug report, it is a case.**

---

## ADR-009 — Registries live with the code they describe

**Question.** The loader validates case manifests against three tables:
fault mechanisms and their parameters, matcher keys and scopes, and
invariant names. Where do those tables live?

**Decision.** With the code they describe, from v0:

| Registry | Package | Why there |
|---|---|---|
| Mechanisms, parameters, values | `internal/fault` | The mechanism implementations are there |
| Match keys and scopes | `internal/interpose` | The interposer evaluates `[case.match]` |
| Invariant names | `internal/oracle/invariant` | The invariant implementations are there |

The catalogue imports all three and validates manifests against them.
`[case.expect]`'s key list stays in the catalogue — it names things owned
elsewhere but is itself manifest surface.

**Why.** A parameter table maintained apart from the code it describes
drifts, and here drift inverts the format's best property: a value added to
the fault code but not the table makes a valid case fail to load — strict
validation gone wrong-way. One registry, defined where the behaviour is
implemented, consumed by the loader and by the schema generator
(`policy-format.md` §6), means the loader, the editor tooling and the
implementation cannot disagree.

The registries carry doc strings — mechanism, parameter, value and
invariant summaries — because they are the single place that knowledge
lives; the generated schema turns them into editor hover documentation for
free. The indirection costs a few files and some structs. v0 has no hot
path, and correctness of the catalogue is worth more than compactness.

**Dependency direction, fixed here.** The catalogue is the outer layer: it
parses manifests and compiles them into the domain packages' terms. `fault`
may import `interpose` — a mechanism is expressed as applications of the
three verbs, so it has to name them — and that is not a cycle, because
`interpose` does not import `fault`. What is fixed is that `fault`,
`interpose` and `invariant` never import the catalogue — the interposer
consumes compiled policy as its own types and knows nothing about TOML.
This is what keeps the future interposer ⇄ catalogue import cycle
structurally impossible rather than merely avoided.

**Incidental fix.** Moving the mechanism registry surfaced a collision:
`malformed_json`'s parameter was named `kind`, which the `[case.fault]`
table already uses to select the mechanism, so the parameter could never be
set. Renamed to `how`; a registry test now rejects any parameter named
`kind`.

**Revisit if.** A registry needs to describe something with no owning
package — that would be a sign the package layout, not the registry
placement, is wrong.

---

## ADR-010 — One reference peer and an interposer; no feral peer

**Question.** The pre-design brief decided "two peers, one message model":
a reference-correct peer on the official SDK and a feral peer on raw wire,
because "the SDK cannot be the hostile side." Is the hostile side really a
second peer?

**Decision.** No. There is one reference peer — the official SDK, era
selected at construction, never coerced into misbehaving — and one
**interposer** that owns the wire and applies faults with three verbs:
rewrite, withhold, synthesize. The reference peer plugs into the interposer
as its `Transport`; charpy man-in-the-middles its own SDK. What remains of
the feral layer is not a peer: the raw wire framing (`internal/wire`) and
`json.RawMessage` frame templates for the synthesize verb.
`internal/engine` is renamed `internal/interpose` — "engine" described a
bag of behaviour; "interposer" names the position that behaviour occupies.
Full design in `interposer.md`.

**Why.** The brief's premise is true — a correct SDK will not emit a
duplicate id or a truncated frame — but the conclusion doesn't follow,
because hostility doesn't have to live in the peer. With the corruptor
positioned as the SDK's transport, the SDK emits correct frames and the
wire carries corrupted ones; no coercion anywhere. Walking the v0
catalogue: every mechanism reduces to the three verbs plus legitimate SDK
reconfiguration (`manifest_mutate` is a real manifest change with the
notification optionally swallowed; `capability_flip` is a reconfigured peer
at a reconnect boundary). The apparent exception, era-lying, decomposes
instead of requiring a translator: a *lie* is a one-frame rewrite whose
incoherence is the fault, coherent *speech* in another era is SDK
configuration, and the hybrids are splices (`interposer.md` §4).

Two consequences fall out and both were already wanted:

- **The double-entry ledger stops being optional.** Self-MITM forces
  intent-versus-wire bookkeeping — the interposer must un-rewrite the
  subject's responses or wedge its own SDK — and that same ledger is what
  the fault-poisoning cases and cross-face correlation needed anyway. One
  structure, three consumers.
- **The `dev.charpy/` argument marker is deleted.** The `inferred`
  correlation regime needed it only while correlation was imagined as a
  post-hoc guess over found traffic. The scenario player originates the
  traffic, so seeded distinctness inside schema-legitimate argument values
  gives the ledger a content join with zero perturbation of the subject
  (`interposer.md` §6, `transcript.md` §4).

**Cost.** The interposer must be scriptable around its own faults — the
reference peer responds in its real configuration to a subject that
believed a lie, and the ledger has to tag those frames as consequences, not
subject behaviour. That attribution logic is new. It is also exactly the
"honest ledger of a falsified conversation" problem in its simplest form,
so the cost is front-loaded design, not accident-prone sprawl.

**Supersedes.** The "two peers, one message model" and "two drivers, one
fault engine" framings. `envelope` remains the shared message model — now
shared by the reference peer, the interposer and the templates rather than
by two peers.

**Revisit if.** A case genuinely requires charpy to *coherently* speak one
era over a connection whose configuration is another — sustained live
translation. None is known, and §4 of `interposer.md` is the argument none
should exist.

---

## ADR-011 — The reference peer is the Go SDK, pinned to a pre-release, seamed at bytes

**Question.** `interposer.md` §1 said "the official Go SDK" and no file
named a module or a version. Which module, at which version, and where
exactly does it attach to the interposer?

**Decision.** `github.com/modelcontextprotocol/go-sdk`, pinned to
**v1.8.0-pre.2**, attached at the SDK's **byte layer** rather than at the
`mcp.Transport` interface. Move to v1.8.0 when it is tagged. Only the Go
SDK is pinned now; the TypeScript and Python peers ADR-004 also names
arrive with the cross-SDK differential, because a lockfile nothing builds
against is a pin nothing verifies.

**Why a pre-release.** §1 promises the peer's era is a constructor
parameter, and the whole era-lying decomposition in §4 rests on it:
coherent speech in an era is *configuration*, which is what makes a live
translator unnecessary. On the latest stable, v1.7.0, that promise is false
on both faces. `ClientSessionOptions.protocolVersion` is unexported and
marked "for testing", so a client peer always opens at 2026-07-28 and, when
`server/discover` fails, hardcodes the legacy `initialize` at 2025-11-25.
There is no server-side control at all: the legacy handler calls
`negotiatedVersion(clientVersion)`, which does not consult the transport's
`ProtocolVersionSupporter` set.

The obvious workaround — the interposer rewrites the outbound `initialize`
params — is not free, and its cost disqualifies it. `Client.capabilities`
is version-sensitive: elicitation `form` is gated at `>= 2025-11-25`.
Rewriting the wire's `protocolVersion` from 2025-11-25 down to 2025-06-18
therefore ships a 2025-06-18 `initialize` carrying a 2025-11-25 capability,
on every legacy-era run, silently. charpy would be injecting an undeclared
capability fault underneath every case it declares, which is the one thing
a suite that reports faults cannot do. v1.8.0-pre.2 exports
`ClientSessionOptions.ProtocolVersion` and
`ServerOptions.SupportedProtocolVersions`, and teaches `negotiatedVersion`
to take the server's set, so the promise becomes true rather than
approximated. A pre-release pin at v0 is the cheaper honesty.

**Why bytes rather than `Transport`.** `mcp.Transport` yields a
`Connection` that reads and writes `jsonrpc.Message` — already parsed.
charpy's interposer surface is `envelope.Message`, which carries the raw
bytes, and `wire.Encoded.Cut` cuts at byte offsets. Implementing
`Transport` would mean re-encoding through `jsonrpc.EncodeMessage`, so the
ledger's *intent* column would hold bytes charpy authored rather than bytes
the SDK authored, and §6's content join would digest charpy's encoding of
the SDK's frame instead of the frame. Two seams instead, both below
`Transport`: `mcp.IOTransport` over an `io.Pipe` pair for stdio-shaped
traffic, and `mcp.StreamableClientTransport` with a charpy
`http.RoundTripper` for HTTP. Still self-MITM, one level lower; `envelope`
and `wire` remain the only parsers charpy has. §1's "plugs into the
interposer as its `Transport`" is amended to say so.

Two knobs come with the seam. `IOTransport.MaxLineLength` defaults to 16
MiB and is set negative on the reference peer: a size cap inside charpy's
own peer would turn a subject's oversized frame into a peer-side error
instead of an observation. And a subject frame the SDK cannot decode is
handed on unrepaired — repairing it for the peer's benefit would hide the
finding — so the peer dying of a subject's response is a recorded outcome,
not a crash.

**Consequences.**

- **The transcript names its peer.** `charpy_version` says what judged the
  run, not what spoke in it, so an owned-stimulus transcript was not
  reproducible from its own contents. The header grows a `peer` object —
  module, version, era requested — which guarantee 6 makes additive within
  `schema_version: 1`. Absent under relay, where there is no peer to name.
- **`draft` is unreachable under owned stimulus.** The SDK tracks dated
  revisions only, and `revisions.md` §1 deliberately puts draft last so
  every open-ended `applies_to` includes it. A draft-only case against the
  reference peer is `SKIPPED` with that reason, not `UNTRIGGERED`: the
  fault never had a peer that could carry it. `internal/peer` guards both
  directions of the skew.
- **Eight transitive modules.** `mcp` plus `jsonrpc` pull jsonschema-go,
  segmentio/encoding and asm, uritemplate, and four `golang.org/x` modules;
  go-cmp and x/tools are test-only and drop. charpy had three direct
  dependencies. `go.sum` pins all of it, so hermeticity holds, but it is a
  step change worth stating rather than discovering.

**Revisit if.** v1.8.0 ships without the exported era options, or ships
them with different semantics — then the pin stays at the pre-release and
this ADR records why. Or the byte seam proves unable to express something
`Transport` could: nothing known needs it, since every verb operates on
bytes by construction.

---

## ADR-012 — One case armed per run under owned stimulus

**Question.** The matcher takes a set — `NewMatcher(ledger, cases...)`,
`Select(f) []Case` — so a run can arm the whole applicable catalogue
against one session. Under owned stimulus, should it?

**Decision.** No. One case per run: the scenario player runs one script per
case, serially, and each produces its own transcript. Relayed runs keep
arming many at once.

**Why.** The first reason is what the output is for. A transcript carrying
eleven injected faults is not a finding, it is a puzzle, and the person
opening it has most likely never run a hostile fixture against anything.
One fault, one transcript, one thing to fix is the unit that can be acted
on, and a suite whose output cannot be acted on does not get used twice.
charpy's whole premise is that its findings are citable; a citation nobody
can isolate is not one.

The reproducibility argument arrives second and independently.
`case-identity.md` §5 promises `case-id + revision + seed` fully determines
a run. §5 already repaired the seed-stream version of that promise with
per-purpose domain separation. Owned stimulus exposes a second version it
does not cover: case A's fault changes what the peer does next — `truncate`
with `then = "close"` ends the stream, so the call case B was waiting for
never happens — which changes what frames case B ever sees. Arm A and B
together and B's traffic differs from B alone, so
`charpy run --case B --seed S` reproduces B only when B ran first. One case
per run makes the promise literally true rather than qualified, because
there is no other armed fault for the traffic to depend on.

The cost is a subject session per case. That is already the model
`internal/driver/stdio` documents for stdio subjects, which spawn per case
and may run concurrently, so this generalises an existing shape rather than
introducing one. In CI it is wall-clock and nothing else.

**Why relay is different.** Nothing there controls the traffic, so
isolating a case buys no determinism — the frames arrive when someone
else's client sends them, and `UNTRIGGERED` is the expected outcome for
most cases (`oracle.md` §2). Arming one case per relayed session would
multiply sessions without making any of them reproducible. The asymmetry is
real and is stated where it bites rather than smoothed over.

The output stays legible either way because the report is already keyed by
citation, one `<testcase>` per finding (`internal/report`), so a relayed
transcript that does fire three faults still reports three separate
findings.

**Consequence.** `charpy run --case 'stream/*'` under owned stimulus is a
loop over matching cases, not one session with a glob armed. The CLI
surface does not change; what it does per case does.

**Revisit if.** Soak mode (v1) wants sustained concurrent faults against
one long-lived session, which is a different product — `soak.md` finds
leaks rather than bugs, and a leak needs pressure rather than isolation.
That is an argument for soak arming many, not for v0 doing so.

---

## ADR-013 — A fault is put to its recipient, and judged by the recipient's reaction

**Question.** Every oracle layer judges the *author* of a frame: layer 1
validates what the subject wrote, and I1–I3 ask whether the subject's ids
resolve, and `oracle.SubjectOriginated` excludes anything charpy touched.
But a damaged frame is a question put to whoever *receives* it, and the
answer is never inside the damaged frame -- it is in what the recipient
does next. Who does the oracle judge a fault by, and what makes sure the
recipient is the subject at all?

**Decision.** Five parts.

1. **A fault's subject is its recipient.** A case's `direction` and `face`
   must deliver its fault to every subject class it lists: `c2s` at the
   downstream face for a server, `s2c` at the upstream face for a client,
   either on the face it receives on for a gateway. The loader rejects a
   case that breaks this, so `charpy policy validate` reports it as an
   authoring error.
2. **Layer 4 is widened from liveness to reaction.** It is anchored at
   `fault_applied` and judges only subject-originated frames after it, on
   the same connection. The fresh-session recovery probe becomes one of its
   checks rather than the layer's whole meaning.
3. **The generic tier comes first.** For every fault that reached the
   subject, the layer reports whether the subject answered a request issued
   after the fault, stopped answering, or exited. The case-specific tier --
   expectations declared in `[case.expect]`, such as "answered `-32700`" or
   "called a removed tool" -- follows on the same anchor.
4. **Observability is applicability.** A case whose answer the wire cannot
   show -- a pending-map leak, a client silently accepting `7` then
   `"7"` -- declares what can observe it, and selection drops it where
   nothing can, as it drops an out-of-revision case.
5. **The server column is re-authored as `c2s`**, each case judged by what
   the server writes next.

**Why.** Running the catalogue against real subjects showed that every
shipped case was `s2c`, so every case listed for `server` delivered its
fault to charpy's own reference peer. The run reported nothing, and nothing
rendered exactly like a server that passed. No shipped case tested a
server. Three forms of the same error were in the catalogue at once: a
malformed frame whose summary says "must not wedge the parser", when the
parser was charpy's; a stdio truncation the subject never saw; and a schema
violation declared `MUST` that could never produce one, because layer 1
rightly excludes the only frame that violated the schema -- the one charpy
wrote.

The oracle was not wrong about any of them. It had nothing of the subject's
to judge, because nothing judged the recipient. The ledger already tags the
*reference peer's* frames after a lie as consequences, so they are not
blamed on the subject (`interposer.md` §3, job 4). The subject's frames
after a fault are the other half of the same window, and they are the
evidence.

Widening liveness rather than adding a fifth layer, because recovery is one
reaction among several -- answered again, stopped answering, exited,
recovered on a fresh session -- and a layer for each would split one
question across four places. Generic before case-specific, because it gives
every fault an answer with no per-case authoring; a case with no
expectations still says what happened. Before the gateway driver, because
the gateway is the one subject where the shipped `s2c` cases already reach
the subject, and a gateway run judged by author-only layers would carry the
same blind spot onto both faces.

**Considered.** A fourth non-verdict for "the fault reached charpy, not the
subject". Rejected: the verdict vocabulary is a public contract -- the
JSONL report, exit codes, archived transcripts -- and a misdirected case is
an authoring error, which belongs at load time rather than in every report.
The reaction layer still reports such a fault as `SKIPPED` with a reason,
for transcripts that predate the rule and for relayed runs whose traffic
the loader cannot see.

Dropping misdirected cases at selection rather than rejecting them at load.
Rejected: selection drops cases that are correct but inapplicable to *this*
run. A case whose direction can never reach a subject it names is wrong for
every run.

**Cost.** Every fault_applied event must record the direction of the frame
it acted on, which is additive within `schema_version: 1`. The scenario
needs a follow-up exchange after the fault, and a `c2s` fault that destroys
the peer's own request must still leave the peer able to ask the next
question. And the catalogue loses its server cases until `c2s` ones replace
them.

**Supersedes.** `oracle.md` §6's definition of layer 4 as liveness alone.
`faults-and-cases.md` §2's claim that injecting into
`declared_output_schema` is MUST-eligible: *detecting* a server that breaks
its own declared schema is layer 1 over any run and needs no fault, while
*injecting* a break asks the recipient whether it validates, which is
OBSERVED at most. The one-faced entry in `open-problems.md`, which part 1
closes at its source.

**Revisit if.** A case turns out to need a fault delivered to charpy's own
peer on purpose -- a fault whose point is what charpy's peer then sends the
subject. That is a consequence, which job 4 already attributes; it would be
a new kind of case rather than an exception to part 1.

---

## ADR-014 — charpy stamps its notifications as well as its requests

**Question.** ADR-005 has charpy stamp `traceparent` on every request it
originates. What about its notifications?

**Decision.** Stamp them too. Every message charpy's peers originate
carries a trace in `_meta`, and over HTTP in the header; where the SDK
leaves a message's params nil, the peer supplies the empty params type
for that method so there is somewhere to put it.

**Why.** The first gateway run showed what requests-only costs. charpy's
`notifications/initialized` went out unstamped, so on the downstream face
it was a frame of charpy's with no join key at all. And the only join
left for it was by content -- which for a notification with no params is
the same bytes in every session, so the fixture gateway's *own*
`initialized` upstream was joined to charpy's as `inferred`: a false join,
one-sided at that. Stamping gives each of charpy's notifications its own
key, and a gateway that forwards one carries the trace across exactly as
it does a request.

The second half is in the ledger, not here: a message whose content is
empty once `_meta` is set aside is never content-joined
(`interpose.ContentDigest`). Stamping alone would still let a gateway's
contentless message, sent with no trace, match a contentless one of
charpy's.

**Cost.** Every notification charpy's peers send grows a `_meta`, which
SEP-414 allows; nothing in the protocol keys on its absence.

**Supersedes.** ADR-005's "every request it originates", which now reads
every message.

**Revisit if.** A subject is found that rejects `_meta` on a notification
-- which would be a conformance failure, and excluded by the precondition.

---

## ADR-015 — A gateway is judged downstream: the call a fault carried, then three questions

**Question.** ADR-013 part 2 anchors the reaction layer at `fault_applied`
and judges the subject's frames after it *on the same connection*. A
gateway receives a fault on one face and is asked about it on the other:
a cut on an upstream stream lands on `u0-c-1`, and the only questions
charpy can put are on `d-c-1`. Asking `ping` alone, as charpy does of a
server, does not reach the fault either -- `ping` is hop-by-hop, and a
gateway answers it itself. What does charpy ask a gateway, and where?

**Decision.** First, the call the fault was carrying. A fault on the
upstream face lands on an exchange the gateway opened to serve a call of
charpy's, asked downstream *before* the fault -- and what the gateway tells
its client about that call is its handling of the fault itself: an error
when its deadline fires, or nothing. `fault_applied` records the faulted
frame's join in its `link`, read from the ledger rather than made, and the
layer follows a traced join to charpy's downstream request and judges the
gateway's answer to it. An inferred join is `INCONCLUSIVE`
(`carried-call-join-inferred`): no verdict rests on one. No join means the
exchange served none of charpy's calls -- a handshake, a listing the
gateway made for itself -- and there is nothing carried to judge.

Then, after a fault on either face, charpy's downstream client asks three
questions, each with its own deadline and kept out of the
matcher:

1. `ping` -- is the gateway itself alive?
2. a call through the upstream the fault reached -- does that path still
   work: answered, an error, or a hang?
3. a call through another upstream -- did the damage stay where it was
   put?

A downstream fault reached no upstream in particular, and the calls go
through `u0` and `u1`. For a gateway, the layer judges charpy's downstream
requests after the fault and the gateway's downstream answers to them, by
id on their own connection, whatever face the fault was on. Each question's
role is read off what it is: `ping`, or a call whose tool prefix (`uN_`)
is, or is not, the fault's connection prefix (`uN-`). No new event kind
records it. One `OBSERVED` finding per question; one still open when the
run ends is a hang.

Only a downstream connection open at the fault carries a question. The
questions are asked on the script's own session, so a connection first
seen after the fault is the liveness probe's, and its ping is the recovery
check's to judge. Counting it here once let the probe stand in for a ping
the questions never sent.

A question not asked is `INCONCLUSIVE`, `question-not-asked`, one finding
per role, whether or not a carried call was judged: a report silent about
a call reads the same as a gateway nobody needed to ask. A gateway that
passes a broken upstream stream through ends charpy's client's session,
and the questions then fail in charpy's client without crossing. The
driver notes each such failure with the question and the error as keys of
their own (`transcript.QuestionDetail`), and the finding cites it -- in its
detail, never in its verdict.

The questions are asked even when the script ran out of time, which a
server driver does not do: they run under the run's own context, and a
gateway that hung its client's call is exactly what they exist to tell
apart from a dead one.

**Why.** Downstream, because that is the face the gateway's client sees,
and the reaction worth judging is what the gateway tells its client. The
answer is joined to its question by id on one connection, a fact, so no
verdict rests on a join across the gateway. Three questions, because each
separates a failure the others cannot: `cascade` (fixture plant) fails all
three, a gateway that wedged one upstream fails only the second, and a
gateway that crashed fails the first.

No exemption is needed upstream. A case fires once (ADR-012), so the
gateway's forwarded copy of a question cannot be faulted a second time.

**Considered.** Recording each question's role in the transcript.
Rejected: the role is derivable from what the question is, and a recorded
role would be one more thing the transcript says that the frames could
contradict.

The carried call is what the questions cannot reach. `nodeadline`
(fixture plant) shows only there: the carried call gets no answer until
charpy's client gives up and cancels it, and the cancellation frees the
gateway, so all three questions are answered exactly as for the control.
A finding on a carried call that went unanswered says whether charpy's
client cancelled it, and when.

**Cost.** The roles lean on charpy's per-upstream tool prefixes, an open
problem; replacing those replaces this. The carried call leans on the
gateway forwarding charpy's trace: one that drops `_meta` gets an
`INCONCLUSIVE` there, which is the honest result for a join that is only a
guess. `fault_applied` grows a `link`, which event lines already carried
as an optional field.

**Supersedes.** ADR-013 part 2's "on the same connection", for gateways.

**Revisit if.** A gateway forwards `ping` upstream, which would make the
first question a second path question.

## ADR-016 — Whether a run can reach a case's frame is derived from the matcher

**Question.** A case can apply to a run by revision, transport, subject
class and observer, and still select a frame nothing in the run sends.
Behind a gateway, `manifest/mutate-silent` waits for an upstream
`list_changed` that charpy's reference servers never send, and
`capability-narrowed-on-reconnect` for a second upstream handshake that
nothing provokes. Both passed selection and were refused when the driver
was built, mid-run: the run exited 2 and left an empty transcript. Where
is that decided, and from what?

**Decision.** At selection, from the case's `[case.match]`.
`scenario.Reaches` states who sends what for each scripted subject class --
charpy's script and the subject's answers downstream; behind a gateway,
also the gateway's forwards and charpy's upstreams' answers -- and a case
whose frame no sender reaches is dropped like an out-of-revision one. A
glob that names it says why. Drivers call the same function when built, so
nothing passes one check and fails the other. A client subject drives its
own traffic, so nothing is ruled out for it.

**Why.** The matcher already says what traffic a case needs, and the script
is derived from it for the same reason (ADR-012): a second statement could
drift from the first. A refusal at build is the wrong place for an
applicability fact -- it costs a spawned subject and a transcript, and
reads as a harness failure.

**Considered.** A declared field on the case (`needs = [...]`). Rejected:
it repeats `[case.match]`, and the one need the matcher cannot show --
whether a tool samples -- is the run's `--tool`, not the case's. Whether a
pair is reachable depends on the setup as much as the case: the same
`capability-narrowed-on-reconnect` is reachable against a client over
HTTP, where the reconnect is the client's own.

**Revisit if.** A setup gains a sender the table does not know -- an
upstream script, a reconnecting upstream -- which adds rows rather than
changing the rule.

## ADR-017 — A frame that never ends is a finding, and charpy stops reading it

**Question.** The proxy read request and response bodies whole with
`io.ReadAll`, and the SSE scanner read lines with `bufio`'s `ReadBytes`
and buffered units until a blank line. All three are unbounded. A subject
that streams a body forever took charpy down -- killed by the bug class it
hunts. Where does charpy stop, and what does the run say when it has to?

**Decision.** Stop reading at 4 MiB a frame (`wire.MaxFrame`), record it,
and go on. A frame past the cap gets a `frame_capped` event -- direction,
bytes read, the cap, the first 64 KiB -- and no frame line. The proxy
closes the subject's body and breaks the client's HTTP exchange without
answering it (`http.ErrAbortHandler`). An event stream also gets a
`stream_close` with reason `frame_cap`. The run ends as it would have; the
reaction layer reports the frame as `OBSERVED`.

**Why.** A sender that streams forever is a finding about that sender, not
charpy failing, so exit 2 is wrong: it would discard the very reaction a
fault provoked. Under the conformance precondition a conformant subject
does not do this under a correct sequence, so a capped frame almost always
follows a fault, and failing the run would throw that result away. Exit 2
stays for charpy's own failures -- an unwritable transcript, say.

The cap is per unit, never per stream: a stream of well-formed events that
never ends is doing its job, and the deadline bounds it. Reads stop at
cap+1 and report the overrun rather than truncating at the cap, because a
body silently cut there looks exactly like a subject that cut it.

**Considered.** A frame line with `raw_truncated`. Rejected: `raw_len` is
the true length, and nobody measured one; the frame never crossed whole, so
a frame line would say it did. A charpy-authored 502, or an error frame,
toward the client. Rejected: it is charpy's answer posing as the
subject's, and the client's next move would be a reaction to charpy. A
bare `charpy_close`. Rejected: the cancellation invariant would credit a
case with a close no fault made. `MUST`. Rejected: MCP sets no frame size,
and nothing generated rejects one.

**Cost.** A correct subject answering with a frame over 4 MiB -- a large
`resources/read` blob -- is reported as past the cap and its exchange
broken. The cap is generous for that reason, and the finding names the cap
as charpy's. The 64 KiB transcript cap and the stdio shim's and hostile
driver's own 4 MiB scanner limits are separate numbers.

**Revisit if.** A real subject's correct traffic reaches the cap, which
makes it a run option rather than a constant.
