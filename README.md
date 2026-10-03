# charpy

**Resilience testing for MCP clients, servers, and gateways.**

Conformance asks whether a correct sequence produces spec-compliant
behaviour. charpy asks what happens when the peer misbehaves — truncates
the stream mid-event, holds a response open forever, answers an id it was
never asked about, or returns valid JSON that violates the schema the
server itself declared. Those are different questions, and conflating them
is a mistake.
[`modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance)
answers the first and has a working group behind it. charpy answers the
second. It is additive, not a competitor.

## Conformance first, then resilience

**charpy assumes its subject already passes conformance.** That is a
precondition, not a recommendation. Resilience results for an
implementation that is not correct under a *correct* sequence are
worthless: you cannot tell a fault charpy injected from a bug that was
there all along, so every finding is suspect and none of them are citable.
The order is the only one in which either result means anything.

Everything downstream leans on it. A case states the revision, subject
class and transport it needs; charpy observes to **confirm what a case
asserts, or to learn what a case deliberately left open** — never to work
out the conversation from scratch. The fingerprint ladder
([`revisions.md`](docs/design/revisions.md) §3) is the fallback for
`revision = "auto"`, not the normal shape of a run.

charpy is a hostile MCP peer. It sits in front of or behind an
implementation under test, injects faults into an otherwise valid protocol
exchange, records a transcript of everything that crosses the wire, and
reports what the implementation did.

Named for the
[Charpy impact test](https://en.wikipedia.org/wiki/Charpy_impact_test): it
measures toughness under sudden load, not conformance to dimensions.

## Status

**Early, and working.** charpy tests MCP servers and clients over stdio and
Streamable HTTP, and judges what they do when a fault reaches them.

- **Five ways to run:** drive a server over stdio; proxy a running HTTP
  server; relay between a client and a server it spawned; and serve a
  client, hostile, over stdio or HTTP.
- **17 cases** over eight fault mechanisms. 14 run today; the other three
  wait on the gateway driver or the cross-SDK differential.
- **An offline oracle:** schema validation (the only source of a MUST),
  invariants over the transcript, and the subject's reaction to each fault
  — did it answer the next question, stop answering, exit, or recover on a
  fresh session.
- **Reports** as text and JSONL. Replaying a transcript twice gives
  byte-identical verdicts, and CI checks that it does.

**Not built yet:** the gateway driver, which puts charpy on both sides of a
subject and correlates the two; the cross-SDK differential; soak mode; the
HTML report. [`introduction.md`](docs/introduction.md) §9 has the full
list, and [`open-problems.md`](docs/open-problems.md) the known gaps.

## Quick start

You need Go 1.26 and [`just`](https://just.systems).

```text
just demo
```

That builds charpy and the Go SDK's conformance `everything-server`, runs
one case against it over stdio, and prints the verdict. `just --list` shows
a demo for every other mode.

Against your own server:

```text
charpy run --case '*' --revision 2025-11-25 --tool <harmless-tool> -- ./your-server
charpy run --subject-url http://localhost:9000/mcp --case '*' --revision 2025-11-25
charpy replay charpy-out/<run-id>.jsonl
charpy replay --format jsonl charpy-out/<run-id>.jsonl > verdicts.jsonl
```

Each case is its own run and writes its own transcript to `charpy-out/`.
`--tool` names the tool the script calls; without it charpy calls the first
one the server lists, which is fine for a fixture and not for a server
whose first tool does something. `charpy cases --revision 2025-11-25`
prints the catalogue.

Against your own client, have it connect to charpy, which serves it
hostile:

```text
charpy run --hostile-http 127.0.0.1:8080 --case '*' --revision 2025-11-25
```

or put `charpy run --hostile --case '*' --revision 2025-11-25` in the
client's server configuration, for stdio.

## What it has found

**A malformed stdio frame kills servers built on the official Go SDK.** One
line that is not valid JSON makes the server exit instead of answering with
a `-32700` parse error. Requests it had already received can lose their
answers too. The same server over Streamable HTTP rejects the same bytes
with `400` and keeps serving. charpy saw this in the SDK's own conformance
`everything-server` and in
[`github-mcp-server`](https://github.com/github/github-mcp-server). It was
already reported as
[go-sdk#1209](https://github.com/modelcontextprotocol/go-sdk/issues/1209);
[what charpy measured](https://github.com/modelcontextprotocol/go-sdk/issues/1209#issuecomment-5856813005),
including what the proposed fix leaves open, is on that issue.

**The Go SDK's client accepts structured results that break the tool's
declared `outputSchema`.** The specification says clients SHOULD validate
them. That is a recommendation, not a requirement, and is reported here as
an observation.

## A fault is put to whoever receives it

A damaged frame is a question put to its recipient, and the answer is in
what the recipient does next — never in the damaged frame, which is
charpy's. So every case states its direction, and the loader refuses one
whose fault would not reach the subject it names: a server receives faults
on the requests it is sent, a client on the answers. A case whose answer
nothing can observe — whether a client silently takes `"7"` for `7` leaves
nothing on the wire — says so, and is not armed where it could only fire
unseen. See [ADR-013](docs/design/decisions.md).

**v0 finds bugs. v1 finds leaks.** v0 drives one client through a catalogue
of hostile frames and asks whether the subject handles each correctly. v1
adds [soak mode](docs/design/soak.md) — a fleet of synthetic clients run
for hours — and asks whether the subject is still healthy afterwards. A
gateway that survives every fault in the catalogue and then dies at 03:00
holding forty thousand half-open sessions passed every test v0 can write.

## Why Go

Single static binary. `go build`, drop it into any CI, no Node toolchain,
no venv. The official conformance framework is TypeScript, which means a
Python shop testing a Python server installs npm to do it. That adoption
tax is the honest reason for a second implementation to exist. Secondarily,
`net/http/httptest`, `httputil.ReverseProxy` and goroutine-per-connection
make a middleman pleasant to write — this is Go's home ground.

## Gateways are the wedge

A gateway is simultaneously a client and a server, and its failures live in
the seam: merged manifests, notification fan-out across upstreams, session
identity mapping, the credential boundary. None of these are observable
from one side, which is why every line of charpy's transcript records the
face it crossed. The gateway driver, which runs charpy on both sides of one
subject and correlates the two, is the next thing to build.

As of the 2026-07-28 revision this is no longer only an engineering
argument. The specification now carries requirements aimed directly at
intermediaries — unrecognised `Mcp-Param-*` headers **MUST** be forwarded,
and an intermediary enforcing policy on mirrored headers **SHOULD** reject
a request whose `MCP-Protocol-Version` does not require header–body
validation rather than trusting it. A header/body mismatch is a spec'd
error with its own code, `-32020`. Nothing currently tests any of it,
charpy included until the gateway driver lands.

## Verdicts

Two buckets, nothing between.

- **MUST** — schema-mechanical only. A generated normative artifact
  rejected the frame, and the report cites the `$ref` that did it.
- **OBSERVED** — everything else: what the subject did, stated as a fact.

charpy never issues a behavioural MUST. "You violate the spec" from a third
party is an opinion that requires a clause inventory rotting at every
revision. "Your gateway leaks a goroutine per failed upstream and stops
reconnecting" is a fact that needs no authority behind it.

A check that could not be settled says so instead of passing: **SKIPPED**
(it did not apply), **INCONCLUSIVE** (the transcript cannot decide it),
**UNTRIGGERED** (the fault never fired). Every report carries each with its
reason, and none as a pass.

Only a MUST fails the build: `charpy replay` exits `1` on a MUST and on
nothing else. Whether an OBSERVED finding is acceptable is yours to decide.
Most of what charpy finds, you will read and then either fix or accept the
cost of; very little of it must be fixed, and some of it is good news, like
a subject that recovered.

## The transcript is the product

Every run emits JSONL: one line per frame, face-tagged, with a versioned
schema. The oracle and the reports are consumers of it, and the oracle runs
**offline** over a finished transcript — so invariants written next month
re-run against transcripts captured today, and verdicts are exactly
reproducible even though runs against a live subject are not.

There is no database. JSONL *is* the database, and every invariant is a
query someone else can write with no code from charpy:

```sql
-- every exchange answered more than once: an id is only an exchange within its
-- connection and within the direction its request travelled
select conn_id, direction, id_type, id, count(*)
  from read_json('run.jsonl', union_by_name = true)
 where type = 'frame' and kind in ('response', 'error')
 group by conn_id, direction, id_type, id having count(*) > 1;

-- frames that carried an injected fault, in order
select seq, face, direction, method, fault.kind, fault.citation
  from read_json('run.jsonl', union_by_name = true)
 where fault is not null order by seq;

-- both faces of one correlated exchange, side by side
select seq, face, direction, kind, method
  from read_json('run.jsonl', union_by_name = true)
 where link.charpy_id = 'c7f1a2' order by seq;
```

## Faults

Eight mechanisms in v0: hang · truncate mid-frame · malformed JSON · valid
JSON violating a declared schema · duplicate `id` · response to an `id`
never requested · tool list mutated mid-session · capability set changed
across reconnect.

A mechanism is code that produces a wire effect; a **case** is a named,
citable, revision-scoped parameterisation of one. Cases are cited as
`stream/truncate-mid-event@2025-11-25#seed=8f2c1a` — permanent id,
revision, seed. See
[`docs/design/case-identity.md`](docs/design/case-identity.md).

## Outputs

| Output | Format |
|---|---|
| Transcript | JSONL, `schema_version` on every line |
| Verdicts | JSONL, referencing case citation and frame sequence numbers |
| Exit code | `0` clean · `1` MUST violation · `2` harness error · `3` subject failed to start · `4` invalid policy |

## Documentation

| | |
|---|---|
| [`introduction.md`](docs/introduction.md) | **Start here.** How a run becomes a verdict, and a map of the rest |
| [`case-identity.md`](docs/design/case-identity.md) | How cases are named and cited across five spec revisions |
| [`transcript.md`](docs/design/transcript.md) | The JSONL schema, face tagging, and the three correlation regimes |
| [`faults-and-cases.md`](docs/design/faults-and-cases.md) | Mechanisms, parameters, and the stateless-era gateway family |
| [`interposer.md`](docs/design/interposer.md) | The state machine between origination and the wire: three verbs, the ledger, two clocks |
| [`oracle.md`](docs/design/oracle.md) | Four layers, thirteen invariants, and why the oracle is offline |
| [`revisions.md`](docs/design/revisions.md) | The revision matrix, schema vendoring, and the fingerprint ladder |
| [`policy-format.md`](docs/design/policy-format.md) | The TOML case and policy format |
| [`decisions.md`](docs/design/decisions.md) | ADRs for every settled open question |
| [`soak.md`](docs/design/soak.md) | Soak mode: synthetic client fleet and leak detection (v1 sketch) |
| [`open-problems.md`](docs/open-problems.md) | Known gaps, deliberately open, scoped rather than papered over |

## Development

```text
just           # list recipes
just check     # gofmt, vet, go test -race, and the determinism gate
just fixture-conformance  # the conformance suite against charpy's fixture gateway
just build
just repl      # open a transcript in duckdb, view `t` bound to it
```

Go 1.26 and `just` are all a build needs. `just repl` needs
[DuckDB](https://duckdb.org); the `vendor-*` recipes need `curl`;
`fixture-conformance` needs the
[MCP conformance suite](https://github.com/modelcontextprotocol/conformance)
as `conformance` on the path. With Nix, `nix-shell` (or direnv, through
`.envrc`) provides all of them, and the Go tools, from `shell.nix`; the
suite comes from a flake, so the `flakes` feature must be enabled.

The reference peer — the always-correct side of every run charpy originates
— is `github.com/modelcontextprotocol/go-sdk`, pinned in `go.mod`. It is a
pre-release pin while v1.8.0 is unreleased, because the exported era
controls charpy's design depends on land there;
[ADR-011](docs/design/decisions.md) records the reasoning and what the
alternative would have cost. The TypeScript and Python peers arrive with
the cross-SDK differential.

## License

ISC — see [`LICENSE`](LICENSE).

`spec/` and `schema/` are vendored from
[`modelcontextprotocol/modelcontextprotocol`](https://github.com/modelcontextprotocol/modelcontextprotocol)
and remain under that project's terms; see `spec/VENDORED.md` and
`schema/VENDORED.md` for the commit they were pulled from.
