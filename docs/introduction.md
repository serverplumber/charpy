# Introduction

Status: **Overview.** This is the map; the documents in `design/` are the
territory. Where this file and a design document disagree, the design
document is authoritative and this file is stale.

Read this first. It answers one question — what happens between
`charpy run` and a verdict — and points at the document that owns each
step.

______________________________________________________________________

## 1. Reading these documents

The documentation is split by how long each part is meant to last.

- **`design/*.md`** — one document per subsystem, stating what charpy
  promises and why. Each opens with a `Status:` line; **Decided** means it
  is settled and code may depend on it, and **sketch** means it is written
  down only so that nothing built now forecloses it. Design documents are
  amended in place when the code teaches them something.
- **`design/decisions.md`** — the **ADRs** (architecture decision records),
  numbered `ADR-001` onward and cited by number. An ADR records one
  question that had to be settled before code could depend on it:
  **Question**, **Decision**, **Why**, and where they apply **Cost**,
  **Considered** and **Supersedes**. ADRs are not edited away. A reversed
  decision gets a new ADR that supersedes the old one, so the reasoning for
  the path not taken stays on record.
- **`open-problems.md`** — known gaps, left open on purpose. Each entry
  states the **gap**, **why it exists**, **why it is not closed**,
  **what closing it would take**, and the **trigger to revisit**. An entry
  leaves by being solved, or by being promoted to an ADR that declares it
  permanent. It does not leave by being forgotten.
- **This file** — the map. It is the most likely to go stale, and the
  others win when it does.

The notation that recurs across all of them:

| Notation                 | Meaning                                                              |
|--------------------------|----------------------------------------------------------------------|
| `§n`                     | section *n* of this document, or of the one named beside it          |
| `ADR-012`                | decision record 12 in `design/decisions.md`                          |
| `SEP-414`                | an MCP Specification Enhancement Proposal, the spec's change process |
| `2025-11-25`             | an MCP protocol revision, named by date; `draft` is unreleased       |
| `I1`–`I13`               | the transcript invariants, numbered in `design/oracle.md` §4         |
| `id@rev#seed=…`          | a case citation: id, revision and seed (`design/case-identity.md`)   |
| `c2s` / `s2c`            | a frame's direction: client to server, or server to client           |
| downstream / upstream    | a frame's face: the side facing clients, or the side facing servers  |
| MUST, OBSERVED, …        | charpy's verdicts (§7); not RFC 2119 MUST in quoted spec text        |
| `spec:` `schema:` `sep:` | what a case's `derives_from` cites: spec prose, schema path, or SEP  |
| v0 / v1                  | v0 finds bugs, one fault at a time; v1 is soak mode and finds leaks  |

______________________________________________________________________

## 2. One run, one file, judged offline

charpy is split in two by a file.

```text
                         ┌──────────────────────── run ─────────────────────────┐
  cases/*.toml           │                                                      │
      │                  │   stimulus ──► interposer ──► wire ──► subject       │
      ▼                  │  (peer or      match · plan    bytes    (spawned,    │
  catalogue ─► selection ┼─► relay)       3 verbs · ledger         proxied, or  │
               revision  │                     │                   spawning us) │
               transport │                     ▼                                │
               subject   │               transcript (JSONL)                     │
                         └─────────────────────┬────────────────────────────────┘
                                               │  the only thing that crosses
                         ┌──────────────────── ▼ ─── offline ───────────────────┐
                         │   reader ─► layer 1 schemacheck   ─┐                 │
                         │             layer 2 invariant      ├─► report        │
                         │             coverage               │   text · JSONL  │
                         │             layer 4 reaction       │                 │
                         │             (layer 3 diff: stub)  ─┘                 │
                         └──────────────────────────────────────────────────────┘
```

The run side talks to a live subject and is only best-effort reproducible:
the subject's clock, scheduler and garbage collector are not charpy's. The
offline side reads a finished file and is exactly reproducible. Two replays
of one transcript produce byte-identical verdicts, and `just check`
enforces that as a gate (ADR-001, `design/decisions.md`).

Everything follows from that split. Invariants written next month re-run
against transcripts captured today. The oracle is tested against hand-built
transcripts, so it cannot silently agree with charpy about something charpy
got wrong. Nothing on the run side waits for a judgement, so the frame path
stays fast.

______________________________________________________________________

## 3. The run side, in the order a frame meets it

**Catalogue** (`internal/catalogue`, `cases/`). A case is a permanent id
plus a mechanism, its parameters, a matcher, the subject classes and
transports it applies to, and a revision range. It is cited as
`id@revision#seed=…`, and that citation reproduces charpy's injected
behaviour byte for byte. See `design/case-identity.md`,
`design/policy-format.md` and `design/faults-and-cases.md`.

**Selection** (`cmd/charpy/run.go`, `selectFor`). This drops every case the
run cannot put to its subject: out of revision, wrong transport, wrong
subject class, or an answer nothing here can observe (`observed_by`). A
dropped case never reaches the transcript, so it cannot show up later as a
silent pass.

**Stimulus.** This is where traffic comes from, and it is the main
difference between run modes:

- *Owned*: charpy's reference peer (`internal/peer`, the official Go SDK,
  pinned, ADR-011), driven by `internal/scenario`. The script is derived
  from the case's matcher, so the traffic cannot drift from the fault it
  feeds. One case is armed per run (ADR-012).
- *Relayed*: somebody else's traffic, from the client under test or the
  client that spawned charpy. Every applicable case is armed at once, and
  UNTRIGGERED is the expected outcome for most of them.

**Interposer** (`internal/interpose`, `internal/fault`). This is the only
hostile component, and it originates nothing. For each frame it:

1. Matches it against the armed cases, with occurrence counting.
1. Asks the mechanism for a **plan**: a description of what the wire should
   see, not an action.
1. Carries the plan out with three verbs: *rewrite*, *withhold*,
   *synthesize*.

Every v0 mechanism reduces to those three verbs (ADR-010), which is why
there is no separate "feral peer". The reference peer is always correct,
and charpy man-in-the-middles its own SDK. Every plan must say what becomes
of the matched frame: delivered, held or swallowed. A guard refuses one
that says none of them.

The interposer's state is the **ledger**, a double-entry record of intent
versus wire. It has five jobs (`design/interposer.md` §3):

- un-rewrite the subject's answers so the reference peer's pending map
  stays sane
- resolve responses to methods
- count occurrences
- tag the reference peer's frames that are consequences of a lie
- emit the `link` object correlation is computed from

It keeps one id space per direction a request can travel, because JSON-RPC
ids are the sender's: a client's request 1 and its server's request 1 are
two exchanges, and an answer is resolved against the requests that came the
other way to it.

**Wire** (`internal/wire`). Raw bytes on and off the transport, with no
opinions on conformance: newline-delimited stdio, and SSE on `net/http`
extended through `ResponseController` rather than replaced. It can stop
mid-frame, which no SDK transport can. It has seven cut points, and it
reads the subject's own SSE bytes so a cut lands in what the subject wrote
rather than in a re-encoding.

**Drivers** (`internal/driver/*`). A driver is an ingress shape around the
one interposer, and drivers differ only in how bytes reach the wire and how
a stream ends. The bookkeeping they share (observe, settle the revision,
record frames and events, name what was armed) lives in `driver/exchange`.
The verb dance lives in each driver, because delivery, stream close and
withdrawal are bound to the transport. §4 lists the drivers.

**Clock and seed** (`internal/clock`, `internal/seed`). charpy's own
scheduling runs on an injected clock. The subject's deadlines are real time
and stay that way, because freezing a subprocess's clock would cost
language-agnosticism. A case's randomness is ChaCha8 over SHA-256(run seed,
case id, purpose), so a case reproduces alone, regardless of which cases
ran beside it.

______________________________________________________________________

## 4. Drivers

| Mode          | Subject           | Stimulus            | Faces | Cases per run  | Status        |
|---------------|-------------------|---------------------|-------|----------------|---------------|
| stdio shim    | server            | relayed             | 1     | all applicable | built         |
| stdio script  | server            | owned               | 1     | one            | built         |
| HTTP proxy    | server            | owned               | 1     | one            | built         |
| hostile stdio | client            | relayed             | 1     | all applicable | built         |
| hostile HTTP  | client            | relayed             | 1     | all applicable | built         |
| gateway       | gateway           | owned, both faces   | 2     | —              | **not built** |
| inproc        | linked Go gateway | owned               | —     | —              | **not built** |
| fleet         | any               | N synthetic clients | —     | —              | **not built** |

| Mode          | Invocation                            |
|---------------|---------------------------------------|
| stdio shim    | `charpy run -- <cmd>`                 |
| stdio script  | `charpy run --case … -- <cmd>`        |
| HTTP proxy    | `charpy run --subject-url … --case …` |
| hostile stdio | `charpy run --hostile`                |
| hostile HTTP  | `charpy run --hostile-http <addr>`    |

- *Relayed* means somebody else's traffic: in the shim, the client that
  spawned charpy; in the hostile modes, the client under test.
- The HTTP proxy is the only mode that fires the liveness probe.
- Hostile HTTP sees real reconnects, each as its own connection.
- inproc is for the leaks and races the wire cannot see (ADR-002); fleet is
  soak mode, v1 (`design/soak.md`).

The hostile drivers run the reference peer as an in-process *server*
(`peer.NewServer`) and fault its responses toward the client. That server
peer is also what the gateway driver's upstream face needs, so the gateway
driver is not waiting on a missing part, only on being built.

______________________________________________________________________

## 5. Who a fault is put to

A case carries two separate fields, and the architecture depends on keeping
them apart:

- **`direction`** (in `[case.match]`) is which way the frame charpy damages
  is travelling.
- **`subject`** is which class of implementation is under test.

A damaged frame is a question put to whoever **receives** it. So a case
only tests its subject if its direction delivers the fault to the subject:

| Subject | Receives on         | A fault reaches it when                                     |
|---------|---------------------|-------------------------------------------------------------|
| server  | the downstream face | `direction = "c2s"`                                         |
| client  | the upstream face   | `direction = "s2c"`                                         |
| gateway | both faces          | `s2c` at its upstream face, or `c2s` at its downstream face |

A case that broke this rule would arm, fire, and put its question to
charpy's own reference peer, and the run would report nothing -- a silence
that looks exactly like a subject that passed. So the loader refuses it:
`direction` is required, and it must reach every subject class the case
lists (ADR-013). The catalogue once shipped nothing but `s2c` cases, so
none of them tested a server; the server column is `c2s` now. Where such a
fault still turns up -- in a relayed run, or a transcript older than the
rule -- the reaction layer reports it SKIPPED `fault-reached-charpy`.

______________________________________________________________________

## 6. The transcript

One JSONL file per run, `schema_version` on every line, schema in
`schema/transcript/v1.json`, rationale in `design/transcript.md`. Three
line types share one dense sequence:

- **header**: run id, seed, negotiated revision, charpy version, the
  reference peer, and the citations of every case the run armed. It is
  written once the revision settles, which in a relay is several frames in.
- **frame**: face, direction, raw bytes (base64, exactly as seen), the
  parsed envelope, the ledger's method echo, `link`, and the fault if
  charpy touched the frame. A fault attribution names the id the original
  frame carried when a rewrite did not keep it.
- **event**: `conn_open`/`conn_close`, `stream_open`/`stream_close`,
  `subject_exit`, `fault_scheduled`/`fault_applied`/`fault_withdrawn`,
  `probe`, `clock_advance`, `note`. Events are load-bearing: under
  2026-07-28, closing a stream *is* the cancellation signal.

The file is append-only, and a crashed run leaves a valid prefix. The
reader rejects rather than guesses: seq out of order, a missing or repeated
header, or an unknown `schema_version`.

**Correlation** joins the two faces of one exchange. It has four regimes,
tried in order of how much they can be trusted:

| Regime      | Basis                                            | Status                    |
|-------------|--------------------------------------------------|---------------------------|
| `forwarded` | charpy relayed the frame and stamped both copies | exercised by the proxy    |
| `traced`    | W3C trace context in `_meta` (SEP-414, ADR-005)  | needs the gateway driver  |
| `inferred`  | a content digest over method and payload         | needs the gateway driver  |
| `none`      | no join                                          | correct for a single face |

No gateway verdict may rest on an `inferred` join. It is SKIPPED instead,
because a false join produces a confident, specific, wrong accusation.

______________________________________________________________________

## 7. The offline side

**The oracle** (`internal/oracle`, `design/oracle.md`) reports in five
verdicts and nothing else:

- **MUST**: schema-mechanical only. A generated normative artifact rejected
  the frame, and the finding cites the subschema that did it. The only
  verdict that fails a build.
- **OBSERVED**: everything else. charpy says what happened and does not
  decide whether it is acceptable.
- **SKIPPED**: the check does not apply.
- **INCONCLUSIVE**: the check applied, but the transcript cannot settle it.
- **UNTRIGGERED**: the case was armed and never fired.

The last three render distinctly from a pass and from each other.

| Layer                | Package       | Judges                                | Status            |
|----------------------|---------------|---------------------------------------|-------------------|
| 1. schema-mechanical | `schemacheck` | each subject frame against its schema | built             |
| 2. invariants        | `invariant`   | properties of the whole transcript    | I1–I3 built       |
| 3. differential      | `diff`        | one script against three SDK peers    | stub              |
| 4. reaction          | `reaction`    | what the subject did after the fault  | built             |
| — coverage           | `coverage`    | armed cases that never fired          | built             |

- Layer 1 validates against the vendored schema for the negotiated
  revision.
- Layer 2's built invariants are I1 resolves-once, I2 no-unsolicited and I3
  no-duplicate-in-flight. I4 cancellation, I5–I7 gateway and I8–I13
  stateless are designed.
- Layer 3 runs the same script against Go, TypeScript and Python reference
  peers.
- Layer 4 judges the subject's own frames after each fault that reached it:
  answered again, stopped answering, or exited. A fault that reached
  charpy's own peer is SKIPPED with that reason. Its `expectation` check
  compares that with what the case declares in `[case.expect]`. Its
  recovery check reads the fresh-session probe, which only the proxy fires
  (ADR-013).
- Coverage is not a layer and asserts nothing about the subject. It reports
  UNTRIGGERED.

Layers 1 and 2 read through one filter, `oracle.SubjectOriginated`. A frame
counts against the subject only if the subject wrote it and charpy did not
touch it. That rule is why charpy does not report its own injections as
findings. It also means those layers judge frames the subject **wrote**,
and never what the subject **did in response to** a frame charpy wrote.
Layer 4 is the one that judges the response.

**Report** (`internal/report`). Output formats are text and JSONL (one
verdict per line, a sibling of the transcript). The build gate is replay's
exit code, which fails on a `MUST` and nothing else. There is no JUnit: it
cannot say what an `OBSERVED` finding is, and SARIF, which can, is an open
problem. The static HTML report and the divergence table are designed and
not built, and `charpy report` exits unimplemented.

______________________________________________________________________

## 8. Package map

| Package           | Role                                        | Owner                        |
|-------------------|---------------------------------------------|------------------------------|
| `cmd/charpy`      | CLI: `run`, `replay`, `cases`, `policy`     | README                       |
| `catalogue`       | loads cases, parses ids, compiles them      | case-identity, policy-format |
| `revision`        | the revision order; `applies_to` ranges     | revisions                    |
| `seed`            | per-(run, case, purpose) randomness         | case-identity §5             |
| `envelope`        | JSON-RPC model: raw bytes, typed ids        | transcript §5                |
| `peer`            | reference peer, client and server, at bytes | ADR-010, ADR-011             |
| `scenario`        | owned stimulus, derived from the matcher    | interposer §5, ADR-012       |
| `interpose`       | matcher, policy, ledger, three verbs, links | interposer                   |
| `fault`           | the eight mechanisms as plans; registry     | faults-and-cases             |
| `wire`            | stdio and SSE framing, cuts, stalls         | interposer                   |
| `clock`           | injected and wall clocks, kept apart        | ADR-001                      |
| `driver/exchange` | bookkeeping every driver shares             | —                            |
| `driver/stdio`    | shim relay and scripted stdio               | —                            |
| `driver/proxy`    | HTTP proxy, SSE cuts, liveness probe        | —                            |
| `driver/hostile`  | client under test, stdio and HTTP           | —                            |
| `fixture/gateway` | a gateway of charpy's own, to drive against | oracle §7                    |
| `driver/inproc`   | not built                                   | ADR-002                      |
| `driver/fleet`    | not built                                   | soak                         |
| `transcript`      | writer, reader, shared line types           | transcript                   |
| `oracle/…`        | verdict vocabulary; the §7 layers           | oracle                       |
| `report`          | text and JSONL rendering                    | oracle §2                    |

Owners are documents in `design/`; ADRs are in `design/decisions.md`.

______________________________________________________________________

## 9. Built and designed

**Built.** The five drivers marked built in §4, eight mechanisms, the
transcript in both directions, oracle layers 1, 2 (I1–I3) and 4 (reaction,
expectations, recovery), coverage, two report formats, the determinism
gate, `cases` and `policy validate`, and the fixture gateway the gateway
driver will be pointed at, conformant and with nothing planted yet.

**Designed and not built.**

- the gateway driver and two-face correlation (`traced`, `inferred`)
- invariants I4–I13
- layer 1's check of a result's `structuredContent` against its tool's
  declared `outputSchema`
- the cross-SDK differential
- the HTML report and divergence table
- the control plane (`design/policy-format.md` §5)
- the in-process driver
- soak mode

**Known gaps** are in `open-problems.md`, each with what would close it and
what would trigger doing so.
