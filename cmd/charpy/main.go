// Command charpy is a hostile MCP peer: it injects faults into an otherwise
// valid protocol exchange, records a correlated transcript of everything that
// crosses the wire, and reports what the implementation did.
//
// It is not a conformance suite. Conformance asks whether a correct sequence
// produces spec-compliant behaviour; charpy asks what happens when the peer
// misbehaves.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Exit codes. These are the CI contract and must not be reordered.
const (
	exitClean       = 0 // no MUST violation
	exitMustViolate = 1 // a schema-mechanical MUST was violated
	exitHarness     = 2 // charpy itself failed
	exitSubject     = 3 // the subject failed to start
	exitBadPolicy   = 4 // a policy or case manifest did not validate
)

// version is stamped at build time with -ldflags.
var (
	version = "0.1.0-dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitHarness
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Printf("charpy %s (%s)\n", version, commit)
		return exitClean
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitClean
	case "cases":
		return cmdCases(rest, os.Stdout)
	case "policy":
		return cmdPolicy(rest, os.Stdout)
	case "run":
		return cmdRun(rest, os.Stdout)
	case "replay", "report":
		return notImplemented(cmd, rest)
	default:
		fmt.Fprintf(os.Stderr, "charpy: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return exitHarness
	}
}

// notImplemented keeps the command surface honest while the interposer is built:
// the commands parse and exit 2, rather than existing only in documentation.
func notImplemented(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitHarness
	}
	fmt.Fprintf(os.Stderr, "charpy %s: not implemented yet\n", cmd)
	return exitHarness
}

func usage(w io.Writer) {
	fmt.Fprint(w, `charpy - resilience testing for MCP clients, servers, and gateways

usage: charpy <command> [flags]

commands:
  run       run a case set against a subject
  cases     list the case catalogue
  policy    validate a policy or case manifest
  replay    re-run the oracle over a finished transcript
  report    render a transcript as JUnit XML and static HTML
  version   print the version

exit codes:
  0  clean          2  harness error     4  invalid policy
  1  MUST violation 3  subject failed to start

docs: docs/design/
`)
}
