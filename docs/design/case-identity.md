# Case identity

Status: **Decided.**

Autobahn's durability comes from `case 6.4.3` being citable across implementations and years. charpy
needs the same property against a protocol that shipped four revisions in eighteen months. This
document defines the scheme and, more importantly, records why it is shaped the way it is — the
constraints outlast the syntax.

## 0. Requirements

The scheme has to satisfy four things at once, and they pull against each other:

1. **A citation must be revision-unambiguous.** Otherwise the suite rots into arguments about
   whether a failure is a failure, because the behaviour a case tests may have changed under it.
2. **A case must be reproducible from its identifier plus a seed**, with no other state.
3. **Identifiers must be stable under the addition of new cases.** No renumbering, ever.
4. **The scheme must express both** a case that applies to a range of revisions and one that applies
   to exactly one.

---

## 1. The scheme

A case has a **permanent ID** and is quoted as a **citation**.

```
Permanent ID   stream/truncate-mid-event
Citation       stream/truncate-mid-event@2025-11-25#seed=8f2c1a
```

| Part | Rule |
|---|---|
| Family | Lowercase kebab, one segment, from the fixed family list in §3 |
| Name | Lowercase kebab, one or more segments separated by `-` |
| Separator | `/` between family and name, exactly once |
| Revision | `@` followed by a revision label: a spec date (`2025-11-25`) or `draft` |
| Seed | `#seed=` followed by lowercase hex, 6–16 digits |

Grammar:

```
citation    = case-id [ "@" revision ] [ "#seed=" seed ]
case-id     = family "/" name
family      = 1*( LOWER / DIGIT )
name        = segment *( "-" segment )
segment     = 1*( LOWER / DIGIT )
revision    = 4DIGIT "-" 2DIGIT "-" 2DIGIT / "draft"
seed        = 6*16HEXDIG
```

The permanent ID never changes and never contains a date. Revision and seed qualify it at the point
of citation. Applicability is data in the case manifest, never in the ID string.

### Rendering

| Context | Form |
|---|---|
| Report, bug report, prose | `stream/truncate-mid-event@2025-11-25#seed=8f2c1a` |
| Transcript `fault.case_id` | `stream/truncate-mid-event` |
| Transcript `fault.citation` | full citation |
| JUnit XML | `<testcase classname="charpy.stream" name="truncate-mid-event@2025-11-25"/>` |
| CLI selection | `charpy run --case 'stream/*'` — globs match the permanent ID only |

JUnit drops the seed. CI systems key history off `classname`+`name`, so including a per-run seed
there would make every run look like a brand-new test and destroy flake history. The seed stays in
the transcript and the report, which is where anyone reproducing the failure will look.

---

## 2. Why revision is not in the permanent ID

Requirement 1 is often stated more strongly: that the ID string itself must contain the revision.
This design satisfies the weaker and, we think, correct form — the *citation* is unambiguous —
because two of charpy's own outputs need **one identity per behaviour**:

- **The divergence table** (`oracle.md` §5) pivots the same case across Go, TypeScript and Python
  *and* across revisions. With revision-prefixed IDs, `2025-06-18/stream/truncate-mid-event` and
  `2026-07-28/stream/truncate-mid-event` are unrelated strings, and the pivot becomes prefix
  stripping — which is to say, the scheme would encode a relationship it then forces every consumer
  to decode.
- **Cross-revision regression.** "Did this behaviour change between 2025-06-18 and 2025-11-25?" is
  one of the more valuable questions charpy can answer, and it is a `GROUP BY case_id` over the
  transcript. It should not require string surgery.

A revision-prefixed scheme also duplicates the entire catalogue per revision, so a case applying to
five revisions is five entries that must be kept in sync by hand. The `applies_to` range in §4 does
that job in one line.

Against the four requirements in §0:

1. *Revision-unambiguous.* Every citation carries it; the bare ID is never quoted as a verdict.
2. *Reproducible from ID plus seed.* `case-id + revision + seed` fully determines a run — see §5.
3. *Stable under addition.* Nothing is numbered, so nothing renumbers.
4. *Ranges and exact pins.* `applies_to` accepts both.

### Rejected alternatives

**Hierarchical numbers (`6.4.3`).** Maximum citability and the strongest precedent, but the taxonomy
is baked into the identifier: reorganising families forces renumbering, which is exactly the
stability requirement 3 asks for. It also carries no revision at all, so `6.4.3 failed` against a
five-revision matrix is exactly the ambiguity requirement 1 rules out.

**Revision-prefixed IDs (`2026-07-28/stream/truncate-mid-event`).** Satisfies requirement 1 in the
strong form, at the cost of both properties above.

---

## 3. Families

Fixed list. Adding a family is a design change, not a case-authoring change, because family names
appear in JUnit `classname` and become CI history keys.

| Family | Covers |
|---|---|
| `frame` | Framing and encoding: malformed JSON, truncation at the byte level, encoding violations |
| `stream` | SSE and stdio stream behaviour: cuts, slow drip, keep-alives, premature close |
| `id` | JSON-RPC correlation: duplicate ids, unsolicited responses, never-resolved requests |
| `schema` | Valid JSON that violates a declared schema — envelope, tool result, or declared output schema |
| `lifecycle` | Version negotiation, capability changes, reconnect, initialization era mismatch |
| `manifest` | Tool/prompt/resource list mutation and `list_changed` fan-out |
| `cancel` | Cancellation semantics across transports and eras |
| `gateway` | Seam behaviour observable only with both faces correlated |
| `liveness` | Recovery after a withdrawn fault |

`gateway` is a family rather than a subject tag because its cases are structurally different: they
require a correlated two-face transcript and are skipped rather than failed when correlation is
degraded (see `transcript.md` §4).

---

## 4. The case manifest

Cases live in `cases/*.toml`. One `[[case]]` per case. See `policy-format.md` for the full schema;
the identity-bearing fields are:

```toml
[[case]]
id           = "stream/truncate-mid-event"
applies_to   = ">=2025-03-26"
subject      = ["server", "gateway"]
verdict      = "OBSERVED"
derives_from = "spec:2025-11-25/basic/transports#streamable-http"
summary      = "SSE stream cut in the middle of an event's data field."
```

### `applies_to`

A revision range over the ordered revision list in `revisions.md`. Accepted forms:

| Form | Meaning |
|---|---|
| `">=2025-03-26"` | That revision and every later one, including `draft` |
| `"=2026-07-28"` | Exactly one revision |
| `">=2025-03-26,<2026-07-28"` | Half-open range — the sessioned era |
| `"*"` | Every revision charpy knows |

Ranges resolve against the ordered list, not against date arithmetic, so `draft` sorts last and a
future revision inserted into the list is picked up by every open-ended range automatically.

A case whose `applies_to` excludes the negotiated revision is **skipped**, and the skip is recorded
with its reason. Skips are not passes; the report distinguishes them, because "we did not test this"
and "this passed" are different claims and conflating them is how suites acquire false confidence.

### `derives_from`

The durable citation — where the expectation comes from. One of:

| Prefix | Example |
|---|---|
| `spec:` | `spec:2025-11-25/basic/transports#streamable-http` |
| `sep:` | `sep:2322` |
| `schema:` | `schema:#/definitions/CallToolResult` |
| `none` | Behaviour with no normative source; only legal when `verdict = "OBSERVED"` |

A `spec:` path resolves against the vendored prose: `spec:R/path#anchor` is
`spec/R/path*.mdx` in this repository, at the commit `spec/VENDORED.md` records — the citation
stays checkable offline and after upstream reorganises its site.

`derives_from = "none"` is deliberately available and deliberately ugly. A case with no normative
source is legitimate — goroutine leaks have no clause — but it should be visible in review that the
case is asserting a judgement rather than citing an artifact.

**Constraint, enforced at load:** `verdict = "MUST"` requires a `schema:` source. charpy never issues
a behavioural MUST (`oracle.md` §2); only a generated normative artifact rejecting a frame earns one.
The loader rejects any other combination rather than trusting the author to remember.

---

## 5. Reproducibility

`case-id + revision + seed` fully determines a run, with one honest caveat.

The seed drives every random choice the case makes: byte offsets for truncation, which occurrence to
target when the matcher permits several, generated payload content, and jitter within a case's
scheduling. `charpy run --case X --revision R --seed S` reproduces the same *injected* behaviour
byte for byte.

What it cannot reproduce is the subject. A remote server may be differently loaded, may have been
redeployed, or may simply be nondeterministic. This is why the determinism guarantee lives in the
oracle rather than the run — see `decisions.md` ADR-001. The seed reproduces what charpy did; the
transcript records what happened; the oracle turns a transcript into verdicts deterministically and
forever.

A citation in a bug report should therefore carry the transcript, not just the ID. The ID says which
case; the transcript says what was observed. `charpy report --cite <citation>` emits both as a
minimal reproducer.

---

## 6. Lifecycle

Once a case ID has appeared in a tagged release it is permanent.

| Change | Handling |
|---|---|
| Case is wrong or superseded | Mark `status = "withdrawn"` with a `withdrawn_reason`. The ID is never reused and never deleted. Withdrawn cases still parse, so old transcripts and old reports keep resolving. |
| Behaviour changes in a new revision | Narrow `applies_to` on the existing case and add a new case for the new behaviour. Do not mutate a case's meaning under a stable ID. |
| Family reorganisation | Not supported. This is the cost of putting families in JUnit `classname`, and it is the cost the scheme is deliberately paying for stability. |
| Typo in an ID | Fix only before the first release that contains it. After that it is permanent and wrong, which is cheaper than a rename. |

`charpy cases --json` emits the full catalogue with IDs, ranges, sources and status, so external
tooling can track the set without parsing TOML.
