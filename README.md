# charpy

**Resilience testing for MCP clients, servers, and gateways.**

Conformance asks whether a correct sequence produces spec-compliant behaviour.
charpy asks what happens when the peer misbehaves — truncates the stream
mid-event, holds a response open forever, answers an id it was never asked
about, or returns valid JSON that violates the schema the server itself
declared. Those are different questions, and conflating them is a mistake.
[`modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance)
answers the first and has a working group behind it. charpy answers the second.
It is additive, not a competitor.

## Conformance first, then resilience

**charpy assumes its subject already passes conformance.** That is a precondition, not
a recommendation. Resilience results for an implementation that is not correct under a
*correct* sequence are worthless: you cannot tell a fault charpy injected from a bug
that was there all along, so every finding is suspect and none of them are citable.
The order is the only one in which either result means anything.

Everything downstream leans on it. A case states the revision, subject class and
transport it needs; charpy observes to **confirm what a case asserts, or to learn what
a case deliberately left open** — never to work out the conversation from scratch. The
fingerprint ladder ([`revisions.md`](docs/design/revisions.md) §3) is the fallback for
`revision = "auto"`, not the normal shape of a run.

charpy is a hostile MCP peer. It sits in front of, behind, or on both sides of
an implementation under test, injects faults into an otherwise valid protocol
exchange, records a correlated transcript of everything that crosses the wire,
and reports what the implementation did.

Named for the [Charpy impact test](https://en.wikipedia.org/wiki/Charpy_impact_test):
it measures toughness under sudden load, not conformance to dimensions.

> **Status: pre-alpha.** The design is settled and documented in
> [`docs/design/`](docs/design/). The interposer is not built yet. Commands
> parse and exit 2.

**v0 finds bugs. v1 finds leaks.** v0 drives one client through a catalogue of hostile
frames and asks whether the subject handles each correctly. v1 adds [soak
mode](docs/design/soak.md) — a fleet of synthetic clients run for hours — and asks
whether the subject is still healthy afterwards. A gateway that survives every fault in
the catalogue and then dies at 03:00 holding forty thousand half-open sessions passed
every test v0 can write.

## Why Go

Single static binary. `go build`, drop it into any CI, no Node toolchain, no
venv. The official conformance framework is TypeScript, which means a Python
shop testing a Python server installs npm to do it. That adoption tax is the
honest reason for a second implementation to exist. Secondarily,
`net/http/httptest`, `httputil.ReverseProxy` and goroutine-per-connection make
a middleman pleasant to write — this is Go's home ground.

## Gateways are the wedge

A gateway is simultaneously a client and a server, and its failures live in the
seam: merged manifests, notification fan-out across upstreams, session identity
mapping, the credential boundary. None of these are observable from one side,
which is why charpy tags every frame with the face it crossed and correlates
the two.

As of the 2026-07-28 revision this is no longer only an engineering argument.
The specification now carries requirements aimed directly at intermediaries —
unrecognised `Mcp-Param-*` headers **MUST** be forwarded, and an intermediary
enforcing policy on mirrored headers **SHOULD** reject a request whose
`MCP-Protocol-Version` does not require header–body validation rather than
trusting it. A header/body mismatch is a spec'd error with its own code,
`-32020`. Nothing currently tests any of it.

## Verdicts

Two buckets, nothing between.

- **MUST** — schema-mechanical only. A generated normative artifact rejected
  the frame, and the report cites the `$ref` that did it.
- **OBSERVED** — everything else, with a cross-SDK divergence table attached
  where one exists.

charpy never issues a behavioural MUST. "You violate the spec" from a third
party is an opinion that requires a clause inventory rotting at every revision.
"Your gateway leaks a goroutine per failed upstream and stops reconnecting" is
a fact that needs no authority behind it.

## The transcript is the product

Every run emits JSONL: one line per frame, face-tagged, correlated, with a
versioned schema. The oracle, the report and the divergence table are all
consumers of it, and the oracle runs **offline** over a finished transcript —
so invariants written next month re-run against transcripts captured today, and
verdicts are exactly reproducible even though runs against a live subject are
not.

There is no database. JSONL *is* the database, and every invariant is a query
someone else can write with no code from charpy:

```sql
-- every request id that resolved more than once
select id, count(*) from read_json('run.jsonl', union_by_name = true)
 where type = 'frame' and kind in ('response', 'error')
 group by id having count(*) > 1;

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

Eight mechanisms in v0: hang · truncate mid-frame · malformed JSON · valid JSON
violating a declared schema · duplicate `id` · response to an `id` never
requested · tool list mutated mid-session · capability set changed across
reconnect.

A mechanism is code that produces a wire effect; a **case** is a named,
citable, revision-scoped parameterisation of one. Cases are cited as
`stream/truncate-mid-event@2025-11-25#seed=8f2c1a` — permanent id, revision,
seed. See [`docs/design/case-identity.md`](docs/design/case-identity.md).

## Outputs

| Output | Format |
|---|---|
| Transcript | JSONL, `schema_version` on every line |
| Verdicts | JSONL, referencing case citation and frame sequence numbers |
| Test results | JUnit XML, one `<testcase>` per case citation |
| Report | Static HTML directory, no server, openable locally |
| Divergence table | Markdown and CSV |
| Exit code | `0` clean · `1` MUST violation · `2` harness error · `3` subject failed to start · `4` invalid policy |

## Documentation

| | |
|---|---|
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

```
just           # list recipes
just check     # gofmt, vet, and test -race
just build
just repl      # open a transcript in duckdb, view `t` bound to it
```

A Nix shell (`shell.nix`) provides Go, plus node and python3 for the cross-SDK
reference peers and duckdb for transcript queries.

The reference peer — the always-correct side of every run charpy originates — is
`github.com/modelcontextprotocol/go-sdk`, pinned in `go.mod`. It is a pre-release
pin while v1.8.0 is unreleased, because the exported era controls charpy's design
depends on land there; [ADR-011](docs/design/decisions.md) records the reasoning
and what the alternative would have cost. The TypeScript and Python peers arrive
with the cross-SDK differential.

## License

Not yet chosen.
