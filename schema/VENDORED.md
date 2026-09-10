# Vendored MCP schemas

`schema.json` for every protocol revision charpy supports, copied from
`modelcontextprotocol/modelcontextprotocol` and embedded into the binary.

Vendored rather than fetched, because charpy must produce identical verdicts offline, in an
air-gapped CI, and in five years when a URL has moved. A verdict must never change without a commit.

## Provenance

| Revision | Bytes | Upstream path |
|---|---|---|
| `2024-11-05` | 87877 | `schema/2024-11-05/schema.json` |
| `2025-03-26` | 89428 | `schema/2025-03-26/schema.json` |
| `2025-06-18` | 108234 | `schema/2025-06-18/schema.json` |
| `2025-11-25` | 174323 | `schema/2025-11-25/schema.json` |
| `2026-07-28` | 181474 | `schema/2026-07-28/schema.json` |
| `draft` | 181474 | `schema/draft/schema.json` |

- Source: <https://github.com/modelcontextprotocol/modelcontextprotocol>
- Commit: `aa8ce049f089f92618340190d4ece141f663310d`
- Pulled: 2026-09-08

The specification prose is vendored separately, from the same commit, under `spec/` — see
`spec/VENDORED.md`. Refresh both together by bumping `mcp_commit` in the justfile.

`schema.ts` is upstream's source of truth and `schema.json` is generated from it. charpy vendors the
generated JSON because that is the artifact a validator can consume, and because a generated
normative artifact rejecting a frame is what earns a `MUST` verdict (`docs/design/oracle.md` §3).

## Note on `draft`

As of this pull, `draft` is byte-identical to `2026-07-28` — expected immediately after a release,
before the next revision's changes begin landing. It will diverge. charpy keeps both because
open-ended `applies_to` ranges include `draft` by design, so the suite breaks loudly when the draft
moves rather than silently ceasing to test it (`docs/design/revisions.md` §1).

## Refreshing

```
just vendor-schemas
```

Deliberately a manual step. An automatic refresh would let a verdict change without a commit, which
is the one thing a citable suite must never do. After refreshing, update the table above and run the
suite: the failures are the changelog.
