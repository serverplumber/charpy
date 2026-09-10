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
