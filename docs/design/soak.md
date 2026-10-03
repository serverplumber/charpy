# Soak mode

Status: **v1 sketch.** Not built in v0. This document exists so that v0
does not foreclose it, and records exactly which v0 decisions are
load-bearing for it.

> **v0 finds bugs. v1 finds leaks.**

A v0 run is minutes long, drives one client, and asks whether the subject
handles a specific hostile frame correctly. A soak run is hours long,
drives a fleet, and asks whether the subject is still healthy afterwards.
Those are different questions and the second is the one that matters in
production — a gateway that survives every fault in the catalogue and then
dies at 03:00 on the fourth day, holding forty thousand half-open sessions,
passed every test charpy v0 can write.

---

## 1. The load generator is a third driver

Alongside the proxy and hostile-server drivers, soak mode adds a
**synthetic client fleet**: N clients, each with its own identity, session
lifecycle, and request pattern, driven against one subject for hours.

It is honestly more work than either existing driver. A proxy relays what
it is given; a fleet has to *originate* plausible traffic, maintain
per-client state, model think time and reconnect backoff, and stay well
behaved enough that anything the subject does wrong is attributable to the
subject.

That is why it is v1 rather than a stretch goal for v0, and why it makes a
natural second release: one announcement for "charpy finds protocol bugs",
another for "charpy finds resource leaks".

---

## 2. What v0 already paid for

Three v0 decisions exist wholly or partly to make this possible. None of
them are cheap to retrofit; all of them were nearly free to include.

### `client_id` and `session_id` on every frame

`transcript.md` §3. In v0 there is exactly one of each and they are
constants. In soak they are the partition keys for every question worth
asking.

The reasoning is the same as the one that produced face tagging: **a
transcript that assumes a single session is the same category of mistake as
a transcript that assumes a single face.** Both retrofits touch every
writer, every reader, every invariant, and — worst — invalidate every
transcript already captured. Since `oracle.md` §1 promises that invariants
written later re-run against transcripts captured today, that promise is
only worth something if the dimensions were there at capture time.

### Counter scope in the matcher

`policy-format.md` §2. v0's `occurrence` counts within a declared `scope`,
which in v0 is always `run` because there is one client and one session.

Soak policies need **rates over populations** — "30% of clients reconnect
every 2s without closing" — not ordinals over a stream. That is a different
selector kind, and it lands as a sibling `[case.select]` table rather than
a rework, because the interposer's counters are already keyed by
`(scope, dimension)` rather than being a single integer.

### The clock as a run property

`decisions.md` ADR-001. Soak runs declare `clock = "real"`, because a
six-hour leak cannot be compressed: it is a function of the subject's real
elapsed time and real reconnect backoff.

Determinism is unaffected, which is the whole reason it was put in the
oracle rather than the run. `t_mono_ns` is monotonic-from-run-start under
both clocks and the oracle never reads `t_wall`, so `charpy replay` over a
soak transcript is as reproducible as over a v0 transcript.

---

## 3. Soak faults are a separate taxonomy

The eight v0 mechanisms are *frame-shaped*: they corrupt, delay, or
duplicate something on the wire. Soak faults are *lifecycle-shaped*: they
are about how connections and sessions are created and destroyed over time,
and most of them involve no malformed bytes at all.

| Fault | What it does | What it finds |
|---|---|---|
| `reconnect_without_close` | Clients abandon connections and open new ones without a clean shutdown | Session maps that never evict; fd exhaustion |
| `retry_storm` | A population retries simultaneously after an induced failure | Thundering-herd handling, backoff, admission control |
| `duplicate_session_id` | Two clients present the same session identifier | Session confusion and cross-identity leakage under contention |
| `slow_drip` | Bytes arrive continuously, a complete frame never does, for hours | Read-buffer growth; timeouts that never fire |
| `upstream_flap` | An upstream goes away and returns on a cycle | Reconnect loops that give up silently; per-failure goroutine leaks |
| `session_churn` | Sustained create/destroy at a fixed rate | Slow leaks that only appear at volume |

Note that these are largely *well-formed* traffic. That is the point: the
v0 catalogue asks whether the subject rejects bad input correctly, and soak
asks whether the subject survives good input for a long time. A fair number
of production incidents are the second kind.

`slow_drip` and `upstream_flap` already appear on the v0 "later" list in
`faults-and-cases.md` §3. They belong here instead — they are only
meaningful over hours.

---

## 4. Oracle: the invariants change shape

The v0 invariants (`oracle.md` §4) are mostly
*existential over a short run*: did this id resolve, did this credential
leak. They still apply, evaluated per session.

Soak adds a class the v0 oracle has no vocabulary for:
**trend invariants**, which are assertions about a quantity's behaviour
over time rather than about any single frame.

- Unresolved requests per client do not grow monotonically.
- Sessions created minus sessions destroyed stays bounded.
- Time-to-first-byte does not degrade beyond a factor of the run's early
  baseline.
- Recovery time after `upstream_flap` does not increase across successive
  flaps — the signal that a subject is leaking a little on each failure and
  will eventually stop reconnecting.

The last one is the most valuable thing in this document. A subject that
recovers from one upstream failure passes `oracle.md` §6 liveness. A
subject whose recovery takes 40ms, then 90ms, then 210ms across successive
failures is dying, and only a long run with a monotonic trend check will
say so.

These remain **OBSERVED**. A trend is evidence, never a spec violation, and
the divergence between a threshold and a judgement is exactly where a suite
loses credibility.

The in-process driver stays the sharpest instrument here:
`runtime.NumGoroutine` and heap deltas around a fixture gateway see leaks
directly, where the wire only sees their symptoms (`decisions.md` ADR-002).

---

## 5. What is deliberately not decided

Left open on purpose; deciding now would be guessing.

- **Traffic model.** Whether client behaviour is a fixed script, a Markov
  model, or recorded and replayed from a real transcript. Replay is
  attractive because charpy already has the format.
- **Transcript volume.** A six-hour run at moderate rate is millions of
  frames. Whether that stays one JSONL file, rotates, or samples
  steady-state traffic while keeping every faulted frame is a real question
  and the answer affects the reader, not the schema.
- **What shape a soak verdict takes.** "Did not leak over six hours" is a
  trend, not a finding anchored to a frame, and forcing it into the shape
  of one may be worse than a separate report.
- **Baseline handling.** Trend invariants need a baseline. Whether that is
  the run's own first N minutes, a stored prior run, or a declared
  threshold changes what a regression means.
