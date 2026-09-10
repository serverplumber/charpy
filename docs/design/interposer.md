# The interposer

Status: **Decided** (ADR-010). Supersedes the "two peers, one fault engine" framing used before
2026-09-10; where older prose says *engine* or *feral peer*, this document is authoritative.

The interposer is the state machine between an origination source and the wire, and the only
component of charpy that is ever hostile. Everything the earlier framing split across a "feral
peer" and a "fault engine" is one component here, because it turned out to be one job: hold the
state of a conversation while deliberately falsifying it.

---

## 1. The shape

```
  origination source          interposer                    wire
  ┌──────────────────┐   ┌──────────────────────┐   ┌──────────────────┐
  │ scenario player  │   │  matcher             │   │ stdio pipes      │
  │ (SDK, seeded)    ├──▶│  scheduler           ├──▶│ HTTP + SSE       │──▶ subject
  │ — or —           │   │  ledger              │   │                  │
  │ proxy relay      │◀──┤  corruptor           │◀──┤ (internal/wire)  │◀── subject
  │ (external client)│   │  (3 verbs)           │   │                  │
  └──────────────────┘   └──────────────────────┘   └──────────────────┘
```

The interposer's input is a stream of `(face, direction, frame-or-event)`. Its outputs are
actions on the wire (deliver, rewrite, withhold, close, inject), transcript lines, and the `link`
objects correlation is computed from. It **originates nothing** and is indifferent to where a
frame came from. Sending traffic is external to the interposer — but not to charpy; see §5.

**One peer, not two.** The reference peer is the official Go SDK, always correct, never coerced
into misbehaving. It plugs into the interposer as its `Transport`: charpy man-in-the-middles its
own SDK. The SDK emits a correct frame; the interposer owns the socket and decides what the wire
sees. Hostility lives entirely in the interposition. What remains of the "feral peer" is not a
peer: a wire layer that can stop mid-byte (`internal/wire`) and a handful of `json.RawMessage`
frame templates for synthesis (§2, verb 3).

**The reference peer's era is a constructor parameter.** A case that needs a coherent 2026-07-28
peer gets one by instantiating the SDK in its stateless configuration, not by translating a
sessioned conversation on the fly. Coherence comes from configuration; incoherence comes from
corruption; nothing comes from translation (§4).

---

## 2. Three verbs

Every mechanism in `faults-and-cases.md` §2 reduces to three interposer verbs:

| Verb | What it does | Mechanisms |
|---|---|---|
| **rewrite** | Change the bytes of a frame in flight | `truncate`, `malformed_json`, `schema_violation`, `duplicate_id` (id rewrite), `capability_flip` (result rewrite) |
| **withhold** | Delay, reorder, or never deliver a frame; close or stall a stream | `hang`, `truncate` with `then = "stall"` |
| **synthesize** | Inject a frame no source emitted, swallow a frame no destination sees, or splice frames across configurations | `unsolicited_response`, `duplicate_id` (`double_response` replays), `manifest_mutate` with `notify = "silent"` (mutation is legitimate SDK reconfiguration; the *suppressed notification* is a swallow) |

Synthesis is the small print on "everything is the SDK filtered by a corruptor": an unsolicited
response has no correct frame behind it, so the interposer authors one from a template. That makes
the corruptor an author in a bounded way — templates, not a protocol implementation.

---

## 3. The ledger

The interposer's state is a double-entry ledger: **intent versus wire**, kept per face, per
connection. Every corruption is a pair of entries — what the origination source believes happened,
and what the wire actually carried. It exists because charpy deliberately corrupts the id space it
is itself tracking; a single-entry view is wedged by the first `duplicate_id` case.

The ledger's jobs, in order of necessity:

1. **Keep the reference peer sane.** If the interposer rewrites an outgoing id from `7` to `9`,
   the subject answers `9` — and the interposer must translate back to `7` before the SDK sees
   it, or the SDK's own pending map wedges and the scenario dies of charpy's own fault. Every
   rewrite registers its inverse.
2. **Resolve responses to methods.** A JSON-RPC response does not carry its method; the matcher's
   `method` key on responses (and the transcript's `method` echo) is a ledger lookup from the id
   — using the *wire* column, since the subject answers what crossed the wire.
3. **Count occurrences.** Counters keyed by `(scope, dimension value)` from v0
   (`policy-format.md` §2), so soak-mode population rates are an addition rather than a rework.
4. **Attribute consequences.** After a lie, the reference peer responds *in its real
   configuration* to whatever the subject does next. If the subject believed the lie, those
   responses are consequences of charpy's fault, not subject behaviour, and the ledger tags them
   so the transcript keeps attribution straight.
5. **Emit `link`.** Correlation is a ledger output, not an oracle-side guess — see §6.

## 3.1 Two clocks, two handles

The ADR-001 boundary runs through the middle of the interposer: fault scheduling
(`withdraw_after_ms`, `after_mono_ms`) is **injected** time; liveness budgets
(`liveness_probe_within_ms`) are **real** time, because the subject's deadlines are real. The
interposer's API therefore takes two named clock handles — `sched` (injected) and `wall`
(real) — rather than one clock and a convention. Reading the wrong one should be a compile-time
impossibility, not a review comment.

---

## 4. Era-lying decomposes; nothing translates

"The hostile server must be able to lie about which era it is" sounds like it requires speaking
one protocol while claiming another. It decomposes into three cheaper things:

| Need | Technique | Cost |
|---|---|---|
| **Lie** about the era | Rewrite the version claim in one frame, then let the real configuration keep talking. The incoherence *is* the fault; the subject's mishandling of it is the finding. | One rewrite |
| **Speak** an era coherently | Instantiate the reference peer in that era (§1). | Configuration |
| **Hybrids** no configuration produces — era switch mid-connection, stateless `_meta` claims on sessioned wire behaviour | Splice: synthesis verb, frame templates, or a differently-configured peer swapped in at a reconnect boundary (which is what `capability_flip` already describes). | Templates |

A live cross-era translator — rewriting a sessioned conversation into a coherent stateless one —
is never needed, because a fault does not owe the subject coherence, and coherence is available
from configuration. This is the observation that collapsed the "feral peer" to templates.

---

## 5. Origination sources

External to the interposer; internal to charpy. Which source feeds the interposer is what the
run modes actually differ in:

| Mode | Downstream source | Upstream source |
|---|---|---|
| Proxy in front of a server/gateway | External client, relayed | The subject's own upstreams, relayed |
| Hostile client (server under test) | **Scenario player** | — |
| Hostile server (client under test) | — | Reference peer as server, reconfigurable between connections |
| Gateway under test | Scenario player | Reference peer as server(s) |

The **scenario player** is the reference SDK driven by a seeded script. It exists because the
properties that distinguish charpy from a chaos proxy all require owned stimulus: case identity
reproduces from ID + seed only if the seed drives the traffic; the liveness probe must fire at
the `fault_withdrawn` instant; the cross-SDK differential needs the same scripted scenario on the
same wire three times. A pure man-in-the-middle with external traffic is Toxiproxy — useful, and
not this project.

The scenario player declares intent to the ledger as it originates ("request 7, `tools/call`,
argument digest X"). For relayed traffic, intent *is* the original bytes. Same ledger, two ways
of filling the intent column.

### 5.1 Relaying traffic charpy does not originate

The relay source is not limited to proxy-in-front-of-a-server. Because the interposer is
origination-agnostic, *anybody's* client or server can be on either side, and interposing a real
client is the cheapest way to test one: the brief ranked clients priority 3 because they are hard
to drive, but under relay **nobody drives the client — its user does**. A developer using a real
client generates realistic stimulus; charpy corrupts responses in flight; the control plane's hot
swap flips faults live.

Entry points:

- **stdio shim.** The client's configuration names charpy as the server command; charpy spawns
  the real server and sits on both pipes. One config edit, no client cooperation.
- **HTTP.** A URL swap. The caveat is TLS: a real client speaking TLS to a real server means
  charpy terminates TLS, with the certificate-trust hassle that implies. stdio and
  localhost-HTTP dev setups dodge it, and they are where this mode belongs.

What survives relay, and why, is a direct consequence of earlier decisions:

| Property | Under relay | Because |
|---|---|---|
| Verdict determinism | **Intact** | ADR-001 put determinism in the oracle. The run is unreproducible; its transcript judges identically forever. |
| Invariants, schema layer | **Intact** | Observational — they never cared who originated the traffic. |
| Occurrence reproducibility | Lost | "The third `tools/call`" depends on traffic charpy does not control. |
| A case's fault firing at all | Not guaranteed | The matched frame may simply never arrive. The outcome is `UNTRIGGERED` (`oracle.md` §2), which is neither a skip nor a pass. |
| Liveness probes | Gated | The interposer *can* synthesize a probe into a relayed session and swallow the response so the real client never sees it — the ledger already knows how — but that perturbs the subject's session state. Off by default, enabled per case, by the same reasoning that deleted the argument marker (§6). |
| Cross-SDK differential | Needs owned stimulus | The same script on the same wire three times is the whole method. |

Rate-shaped selection (`occurrence_every`, and v1's `[case.select]` population rates) fits relayed
traffic better than ordinals do; that is the same matcher shape soak mode needs, arriving early.

**Production.** People will want to run this against production traffic — chaos engineering is
exactly interposition with a fault policy, and nothing here forbids it *mechanically*: the
pass-through path is already asynchronous and non-blocking by design (§7), so throughput is not
the barrier. The barrier is failure containment: a test instrument in the request path is an
availability liability, and charpy v0 makes no fail-open, blast-radius, or abort guarantees
beyond `POST /control/withdraw`. v0's posture stands — a test instrument for dev and staging —
and what production-grade interposition would require is scoped honestly in
`../open-problems.md` rather than pretended away.

---

## 6. Correlation is a ledger output

`transcript.md` §4 defines the three join regimes; this section is where the `inferred` regime's
key comes from. The earlier design had charpy plant a `dev.charpy/` marker key inside tool
arguments so a forwarded copy could be recognised. **That marker is deleted**, for two reasons:

- **It perturbs the subject.** Tools declare an `inputSchema`, and generated schemas commonly say
  `additionalProperties: false`; a gateway or server that validates arguments rejects the call —
  a `-32602` that exists only because charpy was watching. Subtler: argument-hash cache keys
  never hit, argument-signing gateways diverge. The population that triggers the fallback —
  gateways that strip `_meta` — is exactly the population most likely to touch arguments.
- **It ignores who originated the traffic.** The scenario player *chose every argument byte*. It
  does not need to smuggle a marker in; it makes each generated call's arguments naturally
  distinct **inside values the tool's schema legitimately accepts** — a seeded query string, a
  unique URI. Traffic any real client could have sent, distinct by construction.

So the `inferred` join is a recall, not a guess: the ledger holds the content digest of every
originated frame; content arriving on the other face is matched against it, with ordering and
timing as tie-breakers. Digesting requires a canonicalisation decision — a gateway may re-order
JSON keys without changing meaning — and that policy (canonical JSON, key-sorted, whitespace-
normalised) lives here, in the ledger, not in the oracle.

Two things do not change: a content-matched join is still `inferred` with `confidence < 1.0` —
only propagated trace context is authoritative for a subject-forwarded frame — and the
credibility rule stands: no gateway verdict from an `inferred` join, ever.

---

## 7. What the interposer is not

- **Not an oracle.** Nothing in it evaluates an invariant; it records and falsifies, offline
  judgement stays offline.
- **Not in the way.** Nothing in the frame path blocks: transcript writes are buffered and
  asynchronous, schema validation and the oracle are offline. Microseconds against the subject's
  milliseconds.
- **Not an originator.** The scenario player is a client of the interposer, not a part of it.
- **Not language-aware.** It sees frames, faces, directions and bytes. Everything it knows about
  MCP semantics arrives as compiled policy from the catalogue.
