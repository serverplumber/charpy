# Runnable examples, one per mode charpy supports. `just demo` first.
#
# Every recipe here takes no arguments and runs end to end against a subject
# the repo can build: the Go SDK's conformance everything-server, which arrives
# with the pinned reference peer (ADR-011) and needs no network, no credentials
# and no box to be safe in. Transcripts land in examples/out.
#
# These are demonstrations, not an interface. To run charpy against your own
# software, copy the charpy command line each recipe echoes when it runs -- the
# comment above each one shows that line with the parts you change -- and put it
# in your own justfile, your CI job, or your shell. charpy's arguments are the
# thing worth learning; these recipe names are not.

gocmd       := env("GO", "go")
ex_bin      := justfile_directory() / "examples/bin"
ex_out      := justfile_directory() / "examples/out"
ex_src      := justfile_directory() / "examples/src"
ex_subject  := "github.com/modelcontextprotocol/go-sdk/conformance/everything-server"
ex_client   := "github.com/modelcontextprotocol/go-sdk/conformance/everything-client"
ex_server   := ex_bin / "everything-server"

# A subject charpy does not own, pinned to a release rather than a branch: a
# finding has to stay reproducible after upstream moves. It is built from
# source in examples/src, which survives demo-clean because re-cloning it is
# minutes rather than seconds.
gh_repo     := "https://github.com/github/github-mcp-server"
gh_ref      := "v1.12.2"
gh_src      := ex_src / "github-mcp-server"
gh_server   := ex_bin / "github-mcp-server"

# A read-only tool, so a call that reaches the API asks it for nothing it can
# change -- and with no credentials it never reaches the API at all.
gh_tool     := "get_me"

[doc('Build charpy and the SDK conformance subjects into examples/bin')]
demo-build: build
    mkdir -p {{ex_bin}} {{ex_out}}
    {{gocmd}} build -o {{ex_bin}}/everything-server {{ex_subject}}
    {{gocmd}} build -o {{ex_bin}}/everything-client {{ex_client}}

#   charpy run --case <case-id> --revision <rev> --tool <tool> -- <subject cmd>
#   charpy replay <transcript>
[doc('One case against a real subject over stdio, then the verdict')]
demo: demo-build
    ./charpy run --case frame/malformed-request --revision 2025-11-25 --timeout 5s \
        --tool test_simple_text --out {{ex_out}} -- {{ex_server}}
    just demo-replay

#   charpy run --case '<glob>' --revision <rev> --tool <tool> -- <subject cmd>
#
# The glob selects cases by permanent id; `charpy cases` lists them, and
# selection drops the ones this subject class or transport cannot run. Each
# case is its own run against its own subject process, and its own transcript.
# --timeout bounds a case whose fault leaves the subject unable to answer.
# Without --tool, charpy calls whatever tool the subject lists first, which is
# fine against a fixture and not against a subject whose first tool does
# something.
[doc('charpy drives the subject over stdio, one case per run')]
demo-stdio: demo-build
    ./charpy run --case '*' --revision 2025-11-25 --timeout 5s \
        --tool test_simple_text --out {{ex_out}} -- {{ex_server}}

#   charpy run --subject-url <url> --case '<glob>' --revision <rev> --tool <tool>
#
# The subject is already running and charpy sits in front of it, so the cases
# put to a server -- a corrupted or cut-short request body, a duplicate request,
# an answer to nothing -- reach it as real HTTP requests. Here the recipe starts
# one to have something to proxy; against your own server, drop that and point
# --subject-url at it.
[doc('charpy proxies an HTTP subject it did not start')]
demo-http: demo-build
    #!/usr/bin/env bash
    set -euo pipefail
    {{ex_server}} --http 127.0.0.1:9310 --stateless=false &
    subject=$!
    trap 'kill $subject 2>/dev/null || true' EXIT
    until curl -s -o /dev/null http://127.0.0.1:9310/; do sleep 0.1; done
    ./charpy run --subject-url http://127.0.0.1:9310/ --case '*' \
        --revision 2025-11-25 --timeout 5s --tool test_simple_text --out {{ex_out}}

#   charpy run --hostile-http <addr> --case '<glob>' --revision <rev>
#
# The client under test drives, so charpy arms every applicable case and ends on
# a signal rather than on a finished script. Here the client is the SDK's
# everything-client, which picks its scenario from MCP_CONFORMANCE_SCENARIO;
# against your own client, start it however you normally do and point it at the
# address charpy is serving.
[doc('A real SDK client connects to charpy, which serves it hostile')]
demo-hostile-http: demo-build
    #!/usr/bin/env bash
    set -euo pipefail
    ./charpy run --hostile-http 127.0.0.1:8080 --case 'frame/*' \
        --revision 2025-11-25 --out {{ex_out}} &
    charpy_pid=$!
    trap 'kill $charpy_pid 2>/dev/null || true' EXIT
    until curl -s -o /dev/null http://127.0.0.1:8080/; do sleep 0.1; done
    # A client the fault broke exits non-zero, which is the fault working: the
    # transcript and the oracle own that judgment, not the client's exit code.
    MCP_CONFORMANCE_SCENARIO=initialize \
        {{ex_bin}}/everything-client http://127.0.0.1:8080/ || true

# Clone and build github/github-mcp-server, a subject nobody here maintains.
# The only recipe that needs the network; everything else builds from the
# module cache.
[doc('Clone and build github-mcp-server at its pinned release')]
demo-github-build:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p {{ex_src}} {{ex_bin}}
    if [ ! -d {{gh_src}}/.git ]; then
        git clone --depth 1 --branch {{gh_ref}} {{gh_repo}} {{gh_src}}
    else
        git -C {{gh_src}} fetch --depth 1 origin tag {{gh_ref}}
        git -C {{gh_src}} checkout -q {{gh_ref}}
    fi
    cd {{gh_src}} && {{gocmd}} build -o {{gh_server}} ./cmd/github-mcp-server

#   charpy run --case '<glob>' --revision <rev> --tool <tool> -- <subject cmd>
#
# The same stdio invocation as demo-stdio, pointed at software charpy does not
# own. Three things make it a subject rather than a hazard: --read-only, a
# token that is not a credential, and a pinned read-only tool. Every tool call
# fails at the API boundary, which still produces the response frame a fault
# needs, so the run tests the server's wire handling and touches nothing.
#
# Its CI builds, unit-tests and diffs tool schemas; it runs no conformance
# suite, and its e2e tests are gated behind a build tag and a live token. So
# whether it survives a hostile peer is, as far as its CI knows, untested.
[doc('Run the stdio cases against github-mcp-server, then judge every transcript')]
demo-github: demo-build demo-github-build
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf {{ex_out}}/github
    mkdir -p {{ex_out}}/github
    GITHUB_PERSONAL_ACCESS_TOKEN=dummy_not_a_real_token \
        ./charpy run --case '*' --revision 2025-11-25 --timeout 5s --tool {{gh_tool}} \
            --out {{ex_out}}/github -- {{gh_server}} stdio --read-only
    status=0
    for t in {{ex_out}}/github/*.jsonl; do
        echo "== $t"
        ./charpy replay "$t" || status=$?
    done
    exit $status

#   charpy replay <transcript>
#
# Offline over a finished file, so invariants written next month re-run against
# transcripts captured today. Exit 1 on a schema MUST.
[doc('Re-run the oracle over the newest transcript')]
demo-replay:
    ./charpy replay "$(ls -t {{ex_out}}/*.jsonl | head -1)"

#   charpy replay --format junit <transcript> > charpy.xml
[doc('The same verdict as JUnit XML, which is what puts charpy in CI')]
demo-junit:
    ./charpy replay --format junit "$(ls -t {{ex_out}}/*.jsonl | head -1)"

# No recipe can run these: the client under test is what starts charpy, so they
# belong in its server configuration rather than in a justfile.
[doc('The two modes a client under test spawns for itself')]
demo-client-config:
    @echo 'The client starts charpy, so these go in its server configuration.'
    @echo
    @echo 'relay: charpy passes traffic to a subject it spawns, and transcribes it'
    @echo '  {{justfile_directory()}}/charpy run --out {{ex_out}} -- {{ex_server}}'
    @echo
    @echo 'hostile: charpy is the server, and it misbehaves'
    @echo "  {{justfile_directory()}}/charpy run --hostile --case 'id/*' --revision 2025-11-25 --out {{ex_out}}"

[doc('Remove the built subjects and every transcript the examples wrote')]
demo-clean:
    rm -rf {{ex_bin}} {{ex_out}}
