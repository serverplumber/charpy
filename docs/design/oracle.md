# Oracle

Status: **Decided.** The invariant set is derived per revision: several properties that hold in the
sessioned protocol do not survive 2026-07-28, and the stateless era introduces seams of its own.

The oracle is where the project lives or dies. Injecting a fault is trivial; deciding whether the
response was correct is not.

---

## 1. Offline, always

The oracle runs over a finished transcript, never during a run. Three reasons:

- The run path stays trivially fast, adding microseconds where subjects reason in milliseconds.
- Invariants added next month re-run against transcripts captured today, so the finding archive
  appreciates rather than decaying.
- The oracle is testable in isolation against hand-written transcripts, which is the only reason a
  suite like this can itself be trusted.

A fourth reason emerges from `decisions.md` ADR-001: because the oracle is offline and reads only
the injected clock, **verdicts are exactly reproducible even though runs are not**. That is the
determinism guarantee charpy actually ships.

```
charpy replay run.jsonl            # transcript in, verdicts out, deterministic
charpy replay run.jsonl --oracle invariant.id-resolves-once
```

---

## 2. Verdicts

Two buckets, nothing between.

| Verdict | Source | Meaning |
|---|---|---|
| `MUST` | Schema-mechanical only (§3) | A generated normative artifact rejected this frame. charpy asserts nothing. |
| `OBSERVED` | Everything else | This is what happened. Divergence table attached where one exists. |

Plus three non-verdicts that must not be confused with passing:

| | Meaning |
|---|---|
| `SKIPPED` | The case does not apply — wrong revision, wrong subject class, degraded correlation, or no reference peer that can speak the revision (`draft`, per ADR-011). Carries a reason. |
| `INCONCLUSIVE` | The case applied but the transcript cannot support a conclusion: truncated capture, subject died first, join confidence too low. |
| `UNTRIGGERED` | The case applied and the run completed, but no frame ever matched, so the fault was never injected. The normal outcome for a relayed-stimulus run (`interposer.md` §5.1) whose traffic never went where the matcher points; under owned stimulus it usually indicates a scenario bug. |

A suite that reports skips as passes acquires false confidence, which is the failure mode that makes
test suites worthless over time. The report renders all five distinctly and the JUnit output maps
`SKIPPED`, `INCONCLUSIVE` and `UNTRIGGERED` to `<skipped/>` with distinguishing messages —
never to a pass.

**Never issue a behavioural MUST.** "You violate the spec" from a third party is an opinion requiring
a clause inventory that rots at every revision. "Your gateway leaks a goroutine per failed upstream
and stops reconnecting" is a fact that needs no authority behind it. The loader enforces this: a case
declaring `verdict = "MUST"` without a `schema:` source is rejected at load, not at review.

---

## 3. Layer 1 — schema-mechanical

`schema.json` per revision, vendored and embedded (`revisions.md` §2). Every frame in the transcript
is validated against the schema for the revision actually negotiated, and a failure cites the `$ref`
path that rejected it.

This is free and it is legitimately a MUST, because charpy is not asserting a reading — a generated
artifact rejected the frame and the report says which subschema did it.

```
MUST  frame seq=1043  schema:2025-11-25#/definitions/CallToolResult/properties/content
      expected array, got string
```

Three practical notes:

- **Malformed frames are excluded.** A frame charpy deliberately corrupted cannot be held against the
  subject. Only frames the subject originated are validated.
- **Validation is offline**, so it costs nothing in the frame path.
- **`declared_output_schema` extends this layer.** When a server publishes `outputSchema` in
  `tools/list` and then returns `structuredContent` violating it, the rejecting artifact is the
  subject's own declaration. That earns a MUST by the same logic, and is the most valuable MUST
  charpy can issue because it needs no spec authority at all.

---

## 4. Layer 2 — transcript invariants

Computable from the middle with no access to the implementation, therefore language-agnostic for
free. Each is a small function over the transcript.

Below they are derived per revision, split where one statement would have covered two different
mechanisms, and extended where 2026-07-28 created new seams. v0 ships **I1–I7**; **I8–I13** are
specified now and implemented in the second wave.

### Universal — every revision, every subject

| | Invariant | Statement |
|---|---|---|
| **I1** | `id-resolves-once` | Every request id resolves exactly once — result or error, never both, never neither. |
| **I2** | `no-unsolicited-response` | No response or error carries an id that was never requested on that face. |
| **I3** | `no-duplicate-inflight-id` | No two requests share an id while both are in flight on one connection. |

I2 and I3 are split out of I1 rather than folded into it, because they fail differently and need
different reproducers: I1 is a liveness/bookkeeping failure, I2 is a pending-map leak, I3 is an
id-allocation bug.

I1's "never neither" arm is evaluated at end-of-transcript against the request's real-time budget and
against `subject_exit` — an unresolved id at the moment the subject died is `INCONCLUSIVE`, not a
failure.

### Cancellation — one invariant, two mechanisms

| | Invariant | Statement |
|---|---|---|
| **I4** | `cancel-honoured` | No result is delivered for a request after its cancellation was acknowledged. |

The **mechanism differs by transport and era**, which is why this is one invariant with two
implementations rather than two invariants:

| Context | Cancellation signal |
|---|---|
| stdio, all revisions | `notifications/cancelled` on the wire |
| HTTP, `<= 2025-11-25` | `notifications/cancelled` on the wire |
| HTTP, `>= 2026-07-28` | **Closing the response stream is itself the cancellation.** The server MUST stop work and MUST NOT send further messages for that request. |

The 2026-07-28 arm is computable only from `event` lines — specifically `stream_close` with
`detail.reason`, which is why `transcript.md` §6 requires that field. An oracle reading only frames
cannot evaluate this invariant at all, because the signal is the absence of bytes.

### Gateway, sessioned era — v0 focus

Require a correlated two-face transcript. **Skipped, never failed, when `join.via = "inferred"`**
(`transcript.md` §4).

| | Invariant | Statement |
|---|---|---|
| **I5** | `no-credential-leak` | No upstream credential, `Authorization` header, session identifier, or internal address appears **verbatim** in any downstream frame. |
| **I6** | `session-identity-isolation` | A session established under identity A never observes a frame or stream belonging to identity B. |
| **I7** | `merged-manifest-consistency` | Across a `list_changed` fan-out there is no window in which a tool name resolves to the wrong upstream. |

I5 works on redacted transcripts because redaction is a stable digest, not erasure: the invariant
proves the *same* secret appeared on both faces without the transcript itself becoming a secret.

**But I5 names four things, and the transcript does not carry them the same way, so neither does
the comparison.** "Verbatim" means something different in each arm:

| What | How the transcript carries it | How I5 compares | What *verbatim* means here |
|---|---|---|---|
| Credentials on the redaction policy's name list — `Authorization`, `Cookie`, `X-Api-Key` and the rest (`transcript.md` §5) | A stable digest of the whole value | Digest equality | The entire header value, byte for byte |
| Session identifiers | In the clear, in `session.mcp_session_id` and in the header beside it | Value comparison | The identifier appearing anywhere in a downstream frame, a larger value included |
| Internal addresses | In the clear | Value comparison against the upstream descriptor charpy dialled | As above |

Session identifiers are deliberately not digested. `session.mcp_session_id` is a first-class
transcript field, so digesting the header while printing the field beside it would be incoherent
rather than safe, and I6 needs the value legible to say *which* session observed *whose* frame — a
finding reading "session `<redacted:sha256:9f3a…>` observed a frame belonging to
`<redacted:sha256:1c8e…>`" is not one anybody can act on — and those are elided; the real markers
carry all sixty-four characters.

The asymmetry runs in charpy's favour and should not be tidied away: a value carried in the clear
can be found *inside* a larger one, which a digest structurally cannot. The clear-value arms are
the stronger test. The digest arm is the one carrying the limitation below.

**Verbatim is a limitation of the digest arm, and the reporting must carry it.** The digest is of
the exact bytes, so that arm catches a credential forwarded unchanged and misses one the gateway
re-encoded — base64-wrapped, re-signed into a new JWT, or embedded inside a larger header value. A
pass is therefore reported as *"no verbatim credential propagation observed"*, never as *"no
credential leak"*: the stronger claim was not tested, and a pass must not be citable as proof of
it. The gap and the tractable narrowing (charpy plants the upstream credentials, so it can
precompute digests of known transformations) are scoped in `../open-problems.md`. The opposite
failure direction — a collision producing a leak report where there was no leak — is not a live
concern: the digest is the whole SHA-256, untruncated, and stays that way until a membership
filter earns the trade.

**Both arms are bounded by what charpy knows to look for**, which is the limitation neither
wording above implies. The digest arm covers the headers on the redaction policy's name list plus
the credentials charpy planted as the upstream; a credential in a header nobody listed is carried
in the clear and is not a credential as far as I5 is concerned. The clear-value arms cover the
session identifiers charpy observed and the upstream descriptor it dialled, not an internal
address it never saw. I5 checks the propagation of known secrets. It does not discover unknown
ones, and a pass says nothing about secrets outside the set it was given.

I6 partitions on `session.identity`, charpy's label for the credential it presented. It exists only
where a protocol-level session does — `<= 2025-11-25`. SEP-2567 removed `Mcp-Session-Id`, so on
2026-07-28 this invariant is `SKIPPED` with reason `not-applicable-to-revision`, and its role is
taken by I9 and I11.

I7 is the sharpest v0 gateway invariant. The failure it looks for is a *window*, not a state: during
fan-out the merged manifest is briefly inconsistent and a call routed in that window reaches the
wrong upstream. Detecting it requires ordering across both faces, which is why the transcript's
single monotonic `seq` across all line types matters.

### Gateway, stateless era — designed, second wave

| | Invariant | Statement |
|---|---|---|
| **I8** | `header-body-consistency` | Mirrored headers agree with the body on both faces; a gateway that rewrites a body re-derives `Mcp-Method`/`Mcp-Name`; unrecognised `Mcp-Param-*` headers are forwarded; a request lacking `MCP-Protocol-Version` is rejected rather than trusted. |
| **I9** | `cache-scope-isolation` | A result carrying `cacheScope: "private"` is never served to a different identity, and no result is served after its `ttlMs` has elapsed. |
| **I10** | `subscription-id-remap` | An upstream `io.modelcontextprotocol/subscriptionId` never appears downstream, and a notification is delivered only to the subscriber that opted into its type. |
| **I11** | `mrtr-state-isolation` | `requestState` and `inputResponses` never cross identities across an MRTR retry. |
| **I12** | `loglevel-gating` | No `notifications/message` is emitted for a request that did not carry `io.modelcontextprotocol/logLevel`. |
| **I13** | `trace-context-propagation` | `traceparent` survives the subject. Informational — always OBSERVED, never a failure. |

I8 is the strongest thing charpy will have. The spec mirrors body fields into headers so
intermediaries can route without parsing bodies, then constrains intermediaries directly, and the
resulting failure mode is a header/body confusion attack: a load balancer routes on the header while
the server executes the body. It is spec-cited, carries its own error code `-32020`, is only
observable with both faces correlated, and is currently untested by anything. See
`faults-and-cases.md` §5.

I9 and I11 are the stateless-era descendants of I6. The protocol session is gone, but cross-identity
leakage did not go with it — it moved into the cache and into MRTR retry state, both of which are new
places for a gateway to confuse two tenants.

I13 is deliberately never a failure. It is a courtesy finding, and reporting it as a violation would
be exactly the behavioural MUST that §2 forbids.

### Trend invariants — soak, v1

Every invariant above is existential over a short run: did this id resolve, did this credential leak.
Soak mode adds a class this vocabulary cannot express — assertions about how a quantity moves over
hours, such as recovery time increasing across successive upstream failures. They are partitioned by
`client_id` and `session_id`, which is why those dimensions are in the transcript from v0. See
`soak.md` §4.

---

## 5. Layer 3 — cross-SDK differential

Run the same scripted scenario against reference peers built on the Go, TypeScript and Python SDKs,
each launched as a subprocess over stdio, same wire to all three. Diff the transcripts.

Where three independent implementations tracking the same spec agree, that is strong evidence of
intended behaviour without reading a normative clause. Where they disagree, that is
underspecification found mechanically.

**Diff normalised frames, not raw bytes.** Ids, timestamps, and implementation strings differ by
construction. The normalisation drops `id` values (preserving only resolution structure),
timestamps, `serverInfo`, and key order, then compares. Raw bytes are retained in the transcript for
the reproducer but are not the diff input.

Weight by SDK tier (SEP-1730): one tier-1 SDK diverging is a bug report; three tier-1 SDKs diverging
is a spec gap. The output is a divergence table — Markdown and CSV — which is a contribution to the
spec process rather than a competing verdict.

Reference peers are pinned in lockfiles for reproducibility, with a separate scheduled CI job that
floats to latest and diffs the divergence set (`decisions.md` ADR-004). Peers are pooled across
cases; per-case subprocess spawn costs hundreds of milliseconds each and would otherwise dominate
the run.

---

## 6. Layer 4 — liveness

After the fault is withdrawn, does the subject resume serving within N injected-clock seconds?

Everyone tests that failure doesn't crash. Almost nobody tests recovery, and recovery is the
production question — a gateway that stops reconnecting after a transient upstream failure is broken
in a way no conformance suite will ever notice.

The liveness clock starts at the **fault's end** -- `fault_withdrawn` for a hold, the
charpy-initiated `stream_close` for a truncation that closes -- not at fault application: the
question is recovery *after* charpy stops interfering. The driver fires the probe there, on a
**fresh** session, because after a truncation the client's own session may be wedged and liveness
is about the subject resuming service, not that session surviving. `probe` events carry `method`,
`outcome` and `elapsed_mono_ns`; the verdict is read from them offline, and because the probe's
deadline was the budget, the outcome already encodes within-budget (`ok`) versus not (`timeout`).

Liveness is owned-stimulus only, so in v0 it runs on the proxy: a relayed client cannot be made to
probe on cue (§5.1), and over stdio a connection-killing fault leaves no session to probe from.

**The probe method is revision-dependent**, because `ping` was removed in 2026-07-28:

| Revision | Probe |
|---|---|
| `<= 2025-11-25` | `ping` |
| `>= 2026-07-28` | `tools/list` |

`server/discover` would be the natural 2026-07-28 probe, but the SDK client issues it only inside
`Connect` and cannot re-issue it afterwards, so `tools/list` -- the universal fallback -- stands in.
It is a heavier probe, which is stated in the report so a slow recovery is not misread as a slow
probe.

A liveness verdict is `OBSERVED`, never `MUST`: recovery has no generated artifact behind it, so
the layer reports the fact -- recovered, or did not recover within the budget -- for a human to
weigh rather than failing the build. Reporting the *success* is the point; almost nobody tests
recovery, so "recovered in 1.2s" is the observation worth making, not a silence.

Liveness budgets are declared per case as `liveness_probe_within_ms` and are **real-time**, not
injected — the subject's own deadlines and reconnect backoff are real-time and charpy cannot compress
them (`decisions.md` ADR-001). Budgets are therefore generous, and the verdict asserts recovery
happened, not how fast.

---

## 7. Testing the oracle

The oracle is the component whose correctness charpy's credibility rests on, and it is the easiest
component to test properly because it is a pure function from transcript to verdicts.

- **Hand-written transcripts** in `testdata/transcripts/`, one directory per invariant, each with a
  passing case, a failing case, and the boundary cases that distinguish `SKIPPED` from
  `INCONCLUSIVE`. These are written by hand precisely so they are not co-generated with the code that
  reads them.
- **Golden verdicts** beside each, compared byte for byte.
- **Determinism gate**: `charpy replay` over every transcript in `testdata/`, twice, must produce
  byte-identical output. Enforced in CI.
- **Fuzzing** the transcript reader with `testing.F`; failing inputs land in `testdata/fuzz/` and
  become permanent regression cases.
- **The fixture gateway** — a deliberately minimal gateway charpy owns, built to be broken —
  provides transcripts with known-planted faults and exercises the in-process driver under `-race`.
  It is a test instrument, not a deliverable, and must not grow into one.
