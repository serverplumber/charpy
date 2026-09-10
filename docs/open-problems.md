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
