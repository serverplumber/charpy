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
