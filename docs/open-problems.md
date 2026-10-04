# Open problems

Known gaps, deliberately left open. Each entry records what the gap is, why
it is not closed, what closing it would take, and what would trigger doing
so. The design docs in `design/` state what charpy promises; this file is
where a promise's known shortfall is scoped rather than papered over.

An entry leaves this file by being solved or by being promoted to an ADR
that declares it permanent. It does not leave by being forgotten.

---

## Positions for registry and semantic loader errors

**Gap.** `charpy policy validate` reports file, line and column for decode
errors only. Registry and semantic errors — unknown fault parameter,
out-of-range enum value, MUST without a `schema:` source — report file and
case ID, no position (`design/policy-format.md` §4).

**Why it exists.** Those checks run after strict decode, over plain Go
values (`map[string]any`), where position information no longer exists.
`go-toml/v2` attaches positions to errors *it* raises during decode; it
does not hand back a position index for keys that decoded successfully.

**Why it is not closed for v0.** Two paths, both real work for marginal
return:

- Walk `go-toml`'s `unstable.Parser` AST alongside the decode and build a
  key-path → position index to consult when a later check fails. Correct,
  but couples the loader to an API that is named `unstable` and doubles the
  parsing code for an error path.
- Re-scan the raw file for the offending key when a check fails. Cheap, but
  heuristic — a key appearing in several tables needs disambiguation, which
  is the same problem again.

Meanwhile the return is small: a case ID plus the offending key finds the
line in one editor search, the accepted-alternatives list is what actually
fixes the mistake, and Taplo already flags structural mistakes at their
exact position while the file is being written (`design/policy-format.md`
§6) — earlier than any loader message.

**Trigger to revisit.** Evidence that people hit these errors outside an
editor with the schema wired — in CI logs, or via `POST /control/policy`,
where Taplo isn't standing in front of the loader — and lose time locating
the key. Or `go-toml` stabilising a positioned-decode API, which would
collapse the cost side.

---

## `no-credential-leak` (I5) is verbatim-only

**Gap.** I5 compares redaction digests, and a digest is of the exact bytes
of a whole matched value. It catches a credential the gateway forwarded
unchanged. It misses one the gateway re-encoded — base64-wrapped, re-signed
into a new JWT, hex-dumped into a log field — and one embedded inside a
larger value, because the containing value digests differently. A gateway
can leak every secret it holds and pass I5, provided it never leaks one
byte-for-byte.

**Why it exists.** Digest equality is the only comparison the oracle can
make without holding the secret, and the transcript must not become a
secret (`design/transcript.md` §5). Recognising an *arbitrary*
transformation of a secret from the middle would require inverting the
transformation, which is not computable in general.

**Why it is not closed for v0.** The general case never closes. The
tractable narrowing rests on an asymmetry charpy already has: in a gateway
run charpy *plays the upstream*, so it planted the upstream credentials and
knows their plaintext. It can therefore precompute digests of known
transformations — base64, base64url, hex, the value behind a `Bearer`
prefix, JSON-string-escaped — and match downstream values against that set.
Each added transformation is one more digest to compare, not a redesign.
Not built for v0 because the transformation list is speculation until a
real gateway shows which re-encodings actually occur, and a list built on
speculation invites false confidence one layer deeper.

**Reporting rule, in force now.** A pass is worded
*"no verbatim credential propagation observed"*, never
*"no credential leak"* (`design/oracle.md` §4). The limitation being open
is acceptable; a pass being citable as proof of the untested stronger
property is not.

**Trigger to revisit.** The first observed gateway that re-encodes
credentials across the seam — at which point the known-plaintext probe
above is an afternoon, with the transformation list grounded in evidence.

---

## I5's digest set does not survive soak scale

**Gap.** I5's evidence is that a redaction digest seen on the upstream face
also appeared on the downstream one. That is set membership, and at v0
scale it needs no design at all: the oracle is offline over a finished
file, and an exact `map[string]struct{}` over a run's distinct redacted
values is correct and small. At soak scale it stops being free — a fleet
running for hours produces a transcript that need not fit in memory
alongside its digest set — and the structure anyone reaches for next is
approximate, which introduces the one failure this invariant cannot afford:
charpy reporting that a credential propagated when it did not, the
confident, specific, wrong accusation that `design/oracle.md` §4 and
`design/transcript.md` §4 both name as the credibility loss you can spend
exactly once.

**Why the digest is carried at full width today.** `transcript.Digest`
emits the whole SHA-256, untruncated. Truncating trades collision
probability for transcript size, and charpy has no use for that trade yet:
without soak runs the space is there, and at full width collisions stop
being a consideration rather than being made unlikely. Truncation is
something a membership filter buys back — when the set is the expensive
part, a shorter fingerprint is what makes the filter compact — so it is a
payment to make once there is something to buy, not in advance. Recording
it here so that a later change to the digest width is read as what it is:
the filter arriving, not a tidy-up.

**The shape the filter has to take.** A bare approximate-membership
structure has a false-positive rate *by construction*, which converts the
risk full-width digests removed into a designed-in one, permanently. The
only admissible arrangement is the filter as a **pre-filter**: absence is
definitive and cheap, presence is confirmed by exact comparison before any
verdict. A verdict must never rest on the probabilistic step alone.

**The structure this wants is an adaptive quotient filter**, and three of
its properties are load- bearing rather than incidental:

- **Adaptivity.** A static filter charges its false-positive tax on every
  line carrying the offending value, so one collision recurs the whole way
  down a transcript. An adaptive filter corrects a false positive once it
  is detected, and the exact-confirmation step above is exactly the
  detector — so the pre-filter converges over a sweep instead of staying
  wrong. The confirmation step is not a compromise the adaptive design
  tolerates; it is what the adaptive design runs on.
- **Quotienting is reversible.** Quotient plus remainder reconstructs the
  hash, so a quotient filter resizes and merges
  *without the original items*. That is the property soak mode needs: a
  run's distinct-credential cardinality is not known before the run, so a
  filter sized up front is sized wrong, and a Bloom filter cannot be
  resized without the set it no longer has. Merging also suits a fleet —
  per-client or per-segment filters combined at the end.
- **Deletion.** Credentials rotate over a soak run. A structure that can
  only accumulate is a structure whose false-positive rate only degrades.

**Library situation.** No suitable Go implementation is known — not a heavy
dependency to weigh, an absent one. This is code to write, which is itself
part of why v0 stays with the exact set: writing a filter to solve a
problem no v0 run has is the wrong order. `github.com/creachadair/mds` was
raised; its `distinct` package is a CVM distinct-elements *counter*, which
is cardinality estimation rather than membership, so it does not do the
filtering job. What it does answer, cheaply and today, is the sizing
question this entry turns on — how many distinct redacted values a run
actually faced, and therefore whether the exact set was ever in danger of
not fitting. That is the measurement that tells you the filter is needed,
rather than assuming it.

**Trigger to revisit.** Soak mode (v1), where the exact set stops being
free. Nothing before it: at v0 scale the exact set over full-width digests
is both the cheapest and the most correct implementation available, which
is a pleasant place for an open problem to sit.

---

## A held frame cannot be delivered after an HTTP response ends

**Gap.** The `hang` mechanism withholds a frame and, when the fault
withdraws, does something with it: `then = "deliver"` sends it late,
`then = "error"` sends a `-32603` in its place. Over stdio the shim does
exactly that -- the pipe is still open, so `release` delivers or errors on
it. Over HTTP the proxy cannot: once charpy's handler has returned, the
client's response is closed, and a frame withdrawn afterwards has no open
stream to arrive on. `releaseHold` records the withdrawal and drops the
action, noting it in the transcript.

**Why it exists.** "Hold, then deliver later" assumes a persistent
bidirectional channel. stdio is one; a Streamable HTTP POST response is a
one-shot the server closes when it is done answering. The asymmetry is the
transport's, not charpy's -- the same reason `interposer.md` §5.1 lists
what survives relay differently per transport.

**Why it is not closed for v0.** Nothing exercises it. The one `hang` case
that runs today, `lifecycle/server-request-unanswered`, is stdio-only, and
the other is a gateway case, which needs the gateway driver. The HTTP cases
the proxy runs cut or corrupt rather than hold. So the gap is real but
currently unreachable, and closing it speculatively would be inventing a
delivery channel for a case that cannot yet arrive.

**What closing it would take.** A held-then-deliver over HTTP has to keep
the response stream open past the point the subject stopped writing --
charpy holds the SSE stream itself, delivers the withdrawn frame onto it
when the timer fires, then closes. That is `wire.SSE` staying open under
charpy's control rather than the handler returning, which is close to what
`wire.Stall` already does; the withdrawal would flush onto the stalled
stream instead of ending it. It becomes worth building when a `hang` case
is first made HTTP-runnable, which is a gateway run (item 14) or an HTTP
variant of a response-hang case.

**Trigger to revisit.** The first `hang` case that applies to an HTTP
subject.

---

## Emitting a malformed HTTP response

**Gap.** `net/http` will not send a `Content-Length` that disagrees with
the body, emit invalid chunked framing, reset a connection mid-body, or let
a handler hijack an HTTP/2 connection. Those are correctness guarantees of
a good HTTP implementation and precisely the guarantees a hostile peer
exists to violate. The library is an obstacle here for the same reason it
is excellent everywhere else.

**Most of this is reachable without forking anything**, which is the part
worth knowing before anyone starts:

- **All seven `cut_at` values**, through `http.ResponseWriter` and
  `http.NewResponseController`. `Flush` is the whole requirement;
  `SetWriteDeadline` covers holding a response past the server's own
  limits.
- **HTTP/1.1 below the response** — a reset, a lying `Content-Length` —
  through `ResponseController.Hijack` and hand-written bytes on the
  returned `net.Conn`.
- **HTTP/2 framing faults**, through `golang.org/x/net/http2`. Its `Framer`
  carries `AllowIllegalWrites`, documented upstream as permitting frames
  "that do not conform to the HTTP/2 spec […] to test other HTTP/2
  implementations' conformance" — charpy's use case, named as such, in a
  module the Go team maintains. `AllowIllegalReads` is its counterpart, and
  `WriteRawFrame`, `WriteRSTStream`, `WriteGoAway`, `WriteContinuation` and
  `WritePushPromise` cover the family. `http2/hpack` is exposed for header
  encoding.

**What actually remains is code, not questions — but two very different
amounts of it**, and the split falls exactly along the interposer's three
verbs (`design/interposer.md` §2).

`http2.Server` takes a public `NewWriteScheduler` hook, and a
`WriteScheduler` sits in the frame-write path with `Push` and `Pop`. A
scheduler of charpy's own can therefore delay, reorder or drop frames the
server has queued. It cannot author one: `FrameWriteRequest`'s fields are
unexported, so a scheduler may only pass through what it was handed.

| Verb | HTTP/2 on a stock `http2.Server` | Owns the connection |
|---|---|---|
| **withhold** | A custom `WriteScheduler`. No fork, no own server. | no |
| **rewrite** | Unreachable — the frame writer is unexported. | yes |
| **synthesize** | Unreachable. | yes |

So withhold-class HTTP/2 faults — a response that never arrives, frames
that arrive out of order — are a small piece of work against a supported
hook. Rewrite and synthesize mean driving the connection with a `Framer`,
and since charpy's peer must be *correct* everywhere it is not deliberately
hostile, that means owning the settings exchange, stream state and HPACK
context around the faults. `serverConn.Framer()` exists upstream but on an
unexported type, so there is no way to be served correctly and inject a raw
frame on the same connection.

That is the real boundary: not fork versus no fork, but "a scheduler"
versus "enough of an HTTP/2 server to be correct between faults". Days
against weeks, and worth knowing which a case needs before promising it.

HTTP/1.1 has no equivalent split. Withhold is just declining to write,
which `Stall` already does, and everything below the response is a hijacked
connection and hand-written bytes.

**If it does come to a fork**, the comparison is not fork-versus-nothing.
Writing an HTTP server is strictly worse on every axis: a fork inherits
conformance, TLS, connection management and years of accumulated edge
cases, and carries a diff; a fresh implementation re-earns all of it and
will be wrong in ways the fork is not. Maintaining a hostile fork is the
cheaper path, not the expensive one, and the burden is the rebase, not the
authorship.

**The security framing, honestly scoped.** A forked HTTP server must be
tracked against upstream fixes, but the peer on the other side is the
subject under test on a developer's machine, not untrusted internet traffic
— v0's posture is a dev and staging instrument (`design/interposer.md`
§5.1). The exposure that would matter belongs to production interposition,
which is the entry below and has its own reasons to wait.

**Why it is not closed for v0.** The v0 catalogue is frame-shaped and
reaches the wire through `ResponseWriter`. Everything needing more is
already deferred in `design/faults-and-cases.md` §3, and the first two
bullets above cover it when the time comes.

**Trigger to revisit.** The first case family that needs HTTP/2 framing
faults — most plausibly a gateway one, since intermediaries are where
HTTP/2 gets terminated and re-originated. At that point the question is not
whether to fork but whether `x/net/http2` alone suffices, and the answer is
probably yes.

## Production interposition (chaos mode)

**Gap.** Chaos engineering *is* interposition with a fault policy, so
people will point charpy at production traffic whether or not the docs
bless it. The relay source makes it mechanically trivial
(`design/interposer.md` §5.1), and performance is not the barrier — the
pass-through path is asynchronous and non-blocking by design. What is
missing is everything that makes an in-request-path instrument survivable
in production.

**Why it is not closed for v0.** A test instrument in the request path is
an availability liability, and closing that is real engineering with its
own design space:

- **Fail-open.** A charpy crash or stall must not take the path down. That
  means a bypass that outlives the process — socket handoff, a supervising
  shim, or an LB health-check contract — none of which a test instrument
  needs.
- **Blast radius and abort.** Chaos tooling convention is an explicit scope
  ("1% of sessions, these fault kinds, this window") and a kill switch
  honoured in bounded time. `POST /control/withdraw` is the seed of the
  kill switch; rate-and-population selection is the seed of scoping — and
  both seeds are the *soak-mode* machinery: `[case.select]` population
  rates, `clock = "real"`, and the sampling transcript writer
  (`design/soak.md` §§3–6).
- **Control-plane hardening.** Localhost-only binding is the correct v0
  default and an inadequate production posture; an endpoint that injects
  faults into production traffic needs authentication, audit, and a story
  better than "don't expose it".
- **TLS.** Interposing real HTTPS means terminating TLS with the
  certificate-trust burden that implies.

**The honest framing.** Production chaos mode is not a new subsystem; it is
soak mode's machinery plus the relay source plus fail-open hardening.
Building it before soak mode exists would mean building soak mode's hardest
parts out of order, without the leak-detection payoff. The brief's "not a
proxy for production use" stands for v0 as a statement about guarantees,
not about demand.

**Trigger to revisit.** Soak mode (v1) shipping — at that point the
marginal cost is the fail-open and control-plane work alone — or a credible
external user asking for it, which would tell us which guarantees they
actually need rather than which ones we imagine.

---

## Findings have no CI-facing format but the exit code

**Gap.** JUnit has four outcomes -- pass, failure, error, skipped -- and
charpy has two verdicts and three non-verdicts, one of which, `OBSERVED`,
covers both good news ("recovered") and bad ("exited after the fault"). The
JUnit report rendered only `MUST` as a failure, carried each `OBSERVED`
finding as a passing test with the finding in `<system-out>`, and the three
non-verdicts as skips with distinguishing messages. That was honest, but it
was a mapping onto a poorer vocabulary: a CI system reading it could not
tell an `OBSERVED` finding worth a look from a pass without reading the
text, nor `INCONCLUSIVE` from `SKIPPED` without parsing the message. A
dashboard built on it showed a run as mostly green with the findings
hidden, which misleads more than it informs.

**Decision: JUnit is dropped.** The mismatch is too wide for the format to
be useful. The gate is the exit code, which is `1` on a `MUST` and nothing
else; the record is the JSONL verdicts, which keep all five outcomes
intact; people read the text report. What remains open is a format a CI
dashboard can read.

**Why it exists.** JUnit is what every CI system already reads, which is
why it was the first format (`cebbf5a`) and why dropping it leaves a gap
rather than closing one. Its vocabulary was designed for tests with an
author who decided in advance what passing means. charpy's findings are not
that: most are facts for a reader to weigh -- fixed, or accepted as a
cost -- and very few must be fixed.

**Why SARIF, and why it is not closed for v0.** SARIF (OASIS, 2.1.0) is the
format whose vocabulary fits. A result's `kind` is one of `notApplicable`,
`pass`, `fail`, `review`, `open`, `informational` or `unavailable`, and its
`level` -- `error`, `warning`, `note` -- applies only to a `fail`; every
other kind has level `none`. That separates "what happened" from "how bad
is it" in exactly the place JUnit merges them. Its consumers fit the
reading model too: a GitHub code-scanning alert stays open until it is
fixed or dismissed, "won't fix" is a dismissal reason, and
`partialFingerprints` carries an alert's identity across runs -- the case
citation without its seed, as case identity already defines it. An accepted
cost stays accepted; a new finding shows as new. But three things stand in
the way:

- **It is built around source code.** A SARIF location is a region of an
  artifact, and the tools that consume it -- GitHub code scanning chief
  among them -- show results against files in the repository. charpy's
  findings are about a running subject, and the natural location, a line of
  the transcript (`region.startLine` = seq + 1, since seq is dense from
  zero with the header first), is a file that is not in anyone's
  repository. `logicalLocations` exist for this, but whether GitHub shows a
  result with no physical location, and what it does with any `kind` but
  `fail`, needs a real upload to find out before the format is committed
  to.
- **The mapping has open choices.** `MUST` is `fail` with level `error`,
  and `SKIPPED` is `notApplicable`. But `OBSERVED` could be `review` --
  "requires review", which is charpy's stance -- or `informational`, and
  splitting it that way is the INFORMATIONAL / NON-STRICT grading question,
  deferred until an ADR decides whether charpy may grade a behaviour at
  all. `INCONCLUSIVE` reads as `unavailable` ("could not be evaluated");
  `UNTRIGGERED` is either that or `notApplicable`, and neither says "the
  fault never fired" as plainly as charpy does.
- **Nobody has asked for it.** The exit code covers the CI gate, JSONL
  covers anything a person wants to query, and a new format is a new
  contract to keep stable.

**What closing it would take.** A `sarif` format beside `text` and `jsonl`
in `internal/report`, one result per finding: `ruleId` from the check (and,
where there is one, the case id), rules metadata with a `helpUri` into
`docs/design/`, the citation and seq in the result's properties,
`partialFingerprints` from the seedless citation, and a location in the
transcript. The code is small; the decisions above are not. Settling the
`OBSERVED` split first would make the SARIF mapping follow from it rather
than force it.

**Trigger to revisit.** Someone wanting charpy's findings in a
code-scanning dashboard or any other SARIF consumer, or the INFORMATIONAL /
NON-STRICT ADR landing, at which point SARIF is the standard carrier for
the grade it introduces.

---

## A gateway's upstreams are separate hosts, never paths under one

**Gap.** The gateway driver gives each of charpy's upstreams its own
reference server, its own proxy and its own listener: the gateway is
handed `http://127.0.0.1:A/` and `http://127.0.0.1:B/`. A gateway that
addresses its upstreams as paths under one host -- `/u0/mcp`, `/u1/mcp`
behind one URL -- never meets that shape under charpy, and a gateway whose
configuration only accepts one host cannot be pointed at charpy at all.

**Why it exists.** Separate listeners needed nothing new: a proxy forwards
to one URL, and two proxies on one run share a ledger once their
connection ids are prefixed. One host would need one upstream proxy that
routes by path to two servers and rewrites the path on the way, and the
transcript records no request path, so which upstream a frame went to would
have to be added to the frame rather than read off its connection.

**Why it is not closed for v0.** No gateway yet run under charpy needs it,
and the fixture takes one URL per upstream.

**What closing it would take.** A path-routing upstream proxy -- or a
router in front of the existing ones -- and the path recorded on the
frame's HTTP detail, so the upstream a frame reached is still a fact on the
line. The connection prefix that names an upstream today would become a
path prefix.

**Trigger to revisit.** The first gateway whose upstream configuration
cannot take two hosts.

---

## Upstream tools carry a per-upstream prefix

**Gap.** charpy's two reference upstreams serve the same tools under
different names, `u0_add_numbers` and `u1_add_numbers`, so which upstream
received a call is read off the call's name. Real upstreams do not prefix
their tools. A gateway that rewrites or namespaces tool names sees charpy's
naming rather than its own convention, and a gateway case that depends on
two upstreams declaring the same name cannot be expressed.

**Why it exists.** It is the cheapest way to make routing a fact rather
than an inference: with disjoint names, an upstream receiving a tool it
never declared is wrong on its face (I7). The alternative -- inferring the
route from the gateway's own naming scheme -- trades stated structure for
runtime guessing.

**Why it is not closed for v0.** Nothing needs overlapping names yet, and
I7 is not built.

**What closing it would take.** Identifying the upstream a call reached by
the connection it arrived on, which the connection prefix already does,
and letting the tool names be whatever a case says they are -- including
the same name on both upstreams, for the collision cases a real gateway
has to handle.

**Trigger to revisit.** The first case that needs two upstreams to declare
the same name, or a gateway that rewrites tool names.
