# Open problems

Known gaps, deliberately left open. Each entry records what the gap is, why it is not closed, what
closing it would take, and what would trigger doing so. The design docs in `design/` state what
charpy promises; this file is where a promise's known shortfall is scoped rather than papered over.

An entry leaves this file by being solved or by being promoted to an ADR that declares it
permanent. It does not leave by being forgotten.

---

## Positions for registry and semantic loader errors

**Gap.** `charpy policy validate` reports file, line and column for decode errors only. Registry
and semantic errors — unknown fault parameter, out-of-range enum value, MUST without a `schema:`
source — report file and case ID, no position (`design/policy-format.md` §4).

**Why it exists.** Those checks run after strict decode, over plain Go values (`map[string]any`),
where position information no longer exists. `go-toml/v2` attaches positions to errors *it* raises
during decode; it does not hand back a position index for keys that decoded successfully.

**Why it is not closed for v0.** Two paths, both real work for marginal return:

- Walk `go-toml`'s `unstable.Parser` AST alongside the decode and build a key-path → position
  index to consult when a later check fails. Correct, but couples the loader to an API that is
  named `unstable` and doubles the parsing code for an error path.
- Re-scan the raw file for the offending key when a check fails. Cheap, but heuristic — a key
  appearing in several tables needs disambiguation, which is the same problem again.

Meanwhile the return is small: a case ID plus the offending key finds the line in one editor
search, the accepted-alternatives list is what actually fixes the mistake, and Taplo already flags
structural mistakes at their exact position while the file is being written
(`design/policy-format.md` §6) — earlier than any loader message.

**Trigger to revisit.** Evidence that people hit these errors outside an editor with the schema
wired — in CI logs, or via `POST /control/policy`, where Taplo isn't standing in front of the
loader — and lose time locating the key. Or `go-toml` stabilising a positioned-decode API, which
would collapse the cost side.

---

## `no-credential-leak` (I5) is verbatim-only

**Gap.** I5 compares redaction digests, and a digest is of the exact bytes of a whole matched
value. It catches a credential the gateway forwarded unchanged. It misses one the gateway
re-encoded — base64-wrapped, re-signed into a new JWT, hex-dumped into a log field — and one
embedded inside a larger value, because the containing value digests differently. A gateway can
leak every secret it holds and pass I5, provided it never leaks one byte-for-byte.

**Why it exists.** Digest equality is the only comparison the oracle can make without holding the
secret, and the transcript must not become a secret (`design/transcript.md` §5). Recognising an
*arbitrary* transformation of a secret from the middle would require inverting the transformation,
which is not computable in general.

**Why it is not closed for v0.** The general case never closes. The tractable narrowing rests on
an asymmetry charpy already has: in a gateway run charpy *plays the upstream*, so it planted the
upstream credentials and knows their plaintext. It can therefore precompute digests of known
transformations — base64, base64url, hex, the value behind a `Bearer ` prefix, JSON-string-escaped
— and match downstream values against that set. Each added transformation is one more digest to
compare, not a redesign. Not built for v0 because the transformation list is speculation until a
real gateway shows which re-encodings actually occur, and a list built on speculation invites
false confidence one layer deeper.

**Reporting rule, in force now.** A pass is worded *"no verbatim credential propagation
observed"*, never *"no credential leak"* (`design/oracle.md` §4). The limitation being open is
acceptable; a pass being citable as proof of the untested stronger property is not.

**Trigger to revisit.** The first observed gateway that re-encodes credentials across the seam —
at which point the known-plaintext probe above is an afternoon, with the transformation list
grounded in evidence.

---

## I5's digest set does not survive soak scale

**Gap.** I5's evidence is that a redaction digest seen on the upstream face also appeared on the
downstream one. That is set membership, and at v0 scale it needs no design at all: the oracle is
offline over a finished file, and an exact `map[string]struct{}` over a run's distinct redacted
values is correct and small. At soak scale it stops being free — a fleet running for hours
produces a transcript that need not fit in memory alongside its digest set — and the structure
anyone reaches for next is approximate, which introduces the one failure this invariant cannot
afford: charpy reporting that a credential propagated when it did not, the confident, specific,
wrong accusation that `design/oracle.md` §4 and `design/transcript.md` §4 both name as the
credibility loss you can spend exactly once.

**Why the digest is carried at full width today.** `transcript.Digest` emits the whole SHA-256,
untruncated. Truncating trades collision probability for transcript size, and charpy has no use
for that trade yet: without soak runs the space is there, and at full width collisions stop being
a consideration rather than being made unlikely. Truncation is something a membership filter buys
back — when the set is the expensive part, a shorter fingerprint is what makes the filter compact
— so it is a payment to make once there is something to buy, not in advance. Recording it here so
that a later change to the digest width is read as what it is: the filter arriving, not a tidy-up.

**The shape the filter has to take.** A bare approximate-membership structure has a false-positive
rate *by construction*, which converts the risk full-width digests removed into a designed-in one,
permanently. The only admissible arrangement is the filter as a **pre-filter**: absence is
definitive and cheap, presence is confirmed by exact comparison before any verdict. A verdict must
never rest on the probabilistic step alone.

**The structure this wants is an adaptive quotient filter**, and three of its properties are load-
bearing rather than incidental:

- **Adaptivity.** A static filter charges its false-positive tax on every line carrying the
  offending value, so one collision recurs the whole way down a transcript. An adaptive filter
  corrects a false positive once it is detected, and the exact-confirmation step above is exactly
  the detector — so the pre-filter converges over a sweep instead of staying wrong. The
  confirmation step is not a compromise the adaptive design tolerates; it is what the adaptive
  design runs on.
- **Quotienting is reversible.** Quotient plus remainder reconstructs the hash, so a quotient
  filter resizes and merges *without the original items*. That is the property soak mode needs: a
  run's distinct-credential cardinality is not known before the run, so a filter sized up front is
  sized wrong, and a Bloom filter cannot be resized without the set it no longer has. Merging also
  suits a fleet — per-client or per-segment filters combined at the end.
- **Deletion.** Credentials rotate over a soak run. A structure that can only accumulate is a
  structure whose false-positive rate only degrades.

**Library situation.** No suitable Go implementation is known — not a heavy dependency to weigh,
an absent one. This is code to write, which is itself part of why v0 stays with the exact set:
writing a filter to solve a problem no v0 run has is the wrong order.
`github.com/creachadair/mds` was raised; its `distinct` package is a CVM distinct-elements
*counter*, which is cardinality estimation rather than membership, so it does not do the filtering
job. What it does answer, cheaply and today, is the sizing question this entry turns on — how many
distinct redacted values a run actually faced, and therefore whether the exact set was ever in
danger of not fitting. That is the measurement that tells you the filter is needed, rather than
assuming it.

**Trigger to revisit.** Soak mode (v1), where the exact set stops being free. Nothing before it:
at v0 scale the exact set over full-width digests is both the cheapest and the most correct
implementation available, which is a pleasant place for an open problem to sit.

## Production interposition (chaos mode)

**Gap.** Chaos engineering *is* interposition with a fault policy, so people will point charpy at
production traffic whether or not the docs bless it. The relay source makes it mechanically
trivial (`design/interposer.md` §5.1), and performance is not the barrier — the pass-through path
is asynchronous and non-blocking by design. What is missing is everything that makes an
in-request-path instrument survivable in production.

**Why it is not closed for v0.** A test instrument in the request path is an availability
liability, and closing that is real engineering with its own design space:

- **Fail-open.** A charpy crash or stall must not take the path down. That means a bypass that
  outlives the process — socket handoff, a supervising shim, or an LB health-check contract —
  none of which a test instrument needs.
- **Blast radius and abort.** Chaos tooling convention is an explicit scope ("1% of sessions,
  these fault kinds, this window") and a kill switch honoured in bounded time.
  `POST /control/withdraw` is the seed of the kill switch; rate-and-population selection is the
  seed of scoping — and both seeds are the *soak-mode* machinery: `[case.select]` population
  rates, `clock = "real"`, and the sampling transcript writer (`design/soak.md` §§3–6).
- **Control-plane hardening.** Localhost-only binding is the correct v0 default and an
  inadequate production posture; an endpoint that injects faults into production traffic needs
  authentication, audit, and a story better than "don't expose it".
- **TLS.** Interposing real HTTPS means terminating TLS with the certificate-trust burden that
  implies.

**The honest framing.** Production chaos mode is not a new subsystem; it is soak mode's machinery
plus the relay source plus fail-open hardening. Building it before soak mode exists would mean
building soak mode's hardest parts out of order, without the leak-detection payoff. The brief's
"not a proxy for production use" stands for v0 as a statement about guarantees, not about demand.

**Trigger to revisit.** Soak mode (v1) shipping — at that point the marginal cost is the
fail-open and control-plane work alone — or a credible external user asking for it, which would
tell us which guarantees they actually need rather than which ones we imagine.
