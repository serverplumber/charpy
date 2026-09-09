# Justfile (bash mode)
# Requirements: just, go. curl for vendor-schemas.
# Usage: just check
set shell := ["bash", "-eo", "pipefail", "-c"]

# -----------------------------
# Config
# -----------------------------
go  := env("GO", "go")
pkg := "./..."

schema_base := "https://raw.githubusercontent.com/modelcontextprotocol/modelcontextprotocol/main/schema"
revisions   := "2024-11-05 2025-03-26 2025-06-18 2025-11-25 2026-07-28 draft"

version := `git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev`
commit  := `git rev-parse --short HEAD 2>/dev/null || echo unknown`

# List available recipes
default:
    @just --list

# -----------------------------
# Develop
# -----------------------------

# Everything CI runs, in the same order
check: lint vet race

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

vet:
    {{go}} vet {{pkg}}

# Format the code
fmt:
    gofmt -l -w .

# Fail if anything is unformatted, rather than reformatting it
lint:
    #!/usr/bin/env bash
    set -eo pipefail
    out=$(gofmt -l .)
    if [ -n "$out" ]; then echo "unformatted:"; echo "$out"; exit 1; fi

# Validate the shipped case catalogue
cases:
    {{go}} test ./internal/catalogue/... -run TestShippedCatalogue -v

# Open a transcript in duckdb with the view `t` bound to it
repl transcript="testdata/transcripts/truncate-mid-event.jsonl":
    @duckdb -cmd "create view t as select * from read_json('{{transcript}}', union_by_name=true);"

# -----------------------------
# Maintain
# -----------------------------

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

clean:
    rm -rf charpy dist charpy-out
