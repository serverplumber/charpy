# Justfile (bash mode)
# Requirements: just, go. curl for vendor-schemas.
# Usage: just check
set shell := ["bash", "-eo", "pipefail", "-c"]

# Runnable invocations, one per mode charpy supports.
import 'examples/demo.just'

# -----------------------------
# Config
# -----------------------------
go  := env("GO", "go")
pkg := "./..."

# The upstream commit both vendor recipes pull from. Bump it, run vendor-specs
# and vendor-schemas together, update both VENDORED.md files.
mcp_commit  := "aa8ce049f089f92618340190d4ece141f663310d"
mcp_repo    := "https://github.com/modelcontextprotocol/modelcontextprotocol"
schema_base := "https://raw.githubusercontent.com/modelcontextprotocol/modelcontextprotocol/" + mcp_commit + "/schema"
revisions   := "2024-11-05 2025-03-26 2025-06-18 2025-11-25 2026-07-28 draft"

# fixture-conformance: the revision whose requirement set the suite scores, and
# the port the upstream it fronts listens on (the gateway takes an ephemeral
# one and says which).
conformance_revision := "2025-11-25"
conformance_port     := "9330"

version := `git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev`
commit  := `git rev-parse --short HEAD 2>/dev/null || echo unknown`

# List available recipes
default:
    @just --list

# -----------------------------
# Develop
# -----------------------------

# Everything CI runs, in the same order
check: lint vet race determinism

# Build the binary with version stamps
build:
    {{go}} build -ldflags '-X main.version={{version}} -X main.commit={{commit}}' -o charpy ./cmd/charpy

# Run the tests
test:
    {{go}} test {{pkg}}

# Not optional: the in-process driver exists to catch what the wire cannot
# see, and that needs -race.
# Run the tests under the race detector
race:
    {{go}} test -race {{pkg}}

# Run go vet
vet:
    {{go}} vet {{pkg}}

# Format the code
fmt:
    just _go-files | xargs -r gofmt -l -w

# Fail if anything is unformatted, rather than reformatting it
lint:
    #!/usr/bin/env bash
    set -eo pipefail
    out=$(just _go-files | xargs -r gofmt -l)
    if [ -n "$out" ]; then echo "unformatted:"; echo "$out"; exit 1; fi

# List every .go file that could be committed, for fmt and lint to share
_go-files:
    #!/usr/bin/env bash
    set -eo pipefail
    # Tracked or new, as long as it is not ignored -- examples/src holds
    # subjects charpy did not write and does not get to have opinions about.
    # What is on disk is what counts, so a new file is formatted before it is
    # added and a file already moved or deleted is not handed to gofmt.
    git ls-files --cached --others --exclude-standard '*.go' |
        while read -r f; do if [ -e "$f" ]; then echo "$f"; fi; done

# The determinism guarantee, as a gate rather than an aspiration: replaying one
# transcript twice must produce byte-identical verdicts (decisions.md ADR-001).
# Replay one transcript twice and require byte-identical verdicts
determinism:
    {{go}} test ./cmd/charpy -run TestReplayIsByteIdentical -v

# Validate the shipped case catalogue
cases:
    {{go}} test ./internal/catalogue/... -run TestShippedCatalogue -v

# The fixture gateway is held to the precondition every subject is: it passes
# server conformance, in front of a server that does. The upstream is run
# through the suite first, so a failure through the gateway is the gateway's.
# Not part of check: it is a precondition of the gateway runs, not of the build.
# A planted fixture must pass too -- a plant shows only under a fault -- so
# `just fixture-conformance leak,cascade` checks that.
# Run the conformance suite against the fixture gateway, optionally planted
fixture-conformance plants="":
    #!/usr/bin/env bash
    set -euo pipefail
    tmp=$(mktemp -d)
    pids=()
    trap 'kill "${pids[@]}" 2>/dev/null || true; rm -rf "$tmp"' EXIT
    {{go}} build -o "$tmp/everything-server" github.com/modelcontextprotocol/go-sdk/conformance/everything-server
    {{go}} build -o "$tmp/fixture-gateway" ./internal/fixture/gateway/cmd/fixture-gateway

    upstream=http://127.0.0.1:{{conformance_port}}/
    "$tmp/everything-server" -http 127.0.0.1:{{conformance_port}} -stateless=false 2>"$tmp/upstream.log" &
    pids+=($!)
    until curl -s -o /dev/null "$upstream"; do
      kill -0 "${pids[0]}" 2>/dev/null || { cat "$tmp/upstream.log"; exit 1; }
      sleep 0.1
    done

    # The gateway prints its URL once it is listening.
    "$tmp/fixture-gateway" -upstream "$upstream" -plant "{{plants}}" >"$tmp/gateway.url" &
    pids+=($!)
    until [ -s "$tmp/gateway.url" ]; do
      kill -0 "${pids[1]}" 2>/dev/null || exit 1
      sleep 0.1
    done
    gateway=$(head -n1 "$tmp/gateway.url")
    plants="{{plants}}"

    echo "== everything-server, directly"
    conformance server --url "$upstream" --requirements {{conformance_revision}}
    echo "== through the fixture gateway${plants:+, planted: $plants}"
    conformance server --url "$gateway" --requirements {{conformance_revision}}

# Open a transcript in duckdb with the view `t` bound to it
repl transcript="testdata/transcripts/truncate-mid-event.jsonl":
    @duckdb -cmd "create view t as select * from read_json('{{transcript}}', union_by_name=true);"

# -----------------------------
# Maintain
# -----------------------------

# Regenerate the case-manifest JSON Schema from the registries
case-schema:
    {{go}} run ./internal/catalogue/gencaseschema cases/case.schema.json

# Refresh the vendored MCP schemas (manual by design; see schema/VENDORED.md)
vendor-schemas:
    #!/usr/bin/env bash
    set -eo pipefail
    for r in {{revisions}}; do
      mkdir -p "schema/$r"
      echo "fetching $r"
      curl -fsSL --max-time 60 "{{schema_base}}/$r/schema.json" -o "schema/$r/schema.json"
    done
    echo
    echo "Now update schema/VENDORED.md with the new commit SHA and sizes,"
    echo "then run 'just test' -- the failures are the changelog."

# Refresh the vendored MCP spec prose (manual by design; see spec/VENDORED.md)
vendor-specs:
    #!/usr/bin/env bash
    set -eo pipefail
    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    echo "fetching {{mcp_commit}}"
    curl -fsSL --max-time 300 "{{mcp_repo}}/archive/{{mcp_commit}}.tar.gz" -o "$tmp/mcp.tar.gz"
    tar xzf "$tmp/mcp.tar.gz" -C "$tmp"
    src="$tmp/modelcontextprotocol-{{mcp_commit}}/docs/specification"
    for r in {{revisions}}; do
      rm -rf "spec/$r"
      (cd "$src" && find "$r" -name '*.mdx' \
        -exec install -D {} "{{justfile_directory()}}/spec/{}" \;)
    done
    echo
    echo "Now update spec/VENDORED.md (commit, date, file count) and re-check"
    echo "the claims it records -- line numbers move at every pull."

# Remove the built binary and charpy's default output
clean:
    rm -rf charpy dist charpy-out
