// Command fixture-gateway serves charpy's fixture gateway over Streamable
// HTTP, so a driver can spawn it and conformance can be pointed at it.
//
//	fixture-gateway -http 127.0.0.1:8080 -upstream http://127.0.0.1:3000/mcp
//
// -upstream repeats, once per upstream. A test instrument, not a deliverable:
// see package gateway.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

func main() {
	var o gateway.Options
	addr := flag.String("http", "127.0.0.1:0", "address the downstream face listens on")
	verbose := flag.Bool("v", false, "log the gateway's diagnostics to stderr")
	flag.Func("upstream", "a Streamable HTTP upstream endpoint (repeatable)", func(s string) error {
		o.Upstreams = append(o.Upstreams, s)
		return nil
	})
	flag.Parse()

	if *verbose {
		o.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	h, err := gateway.Handler(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture-gateway:", err)
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture-gateway:", err)
		os.Exit(1)
	}
	// The bound address on stdout, once, is the readiness signal: with port 0
	// it is the only way a spawning process learns where to connect.
	fmt.Printf("http://%s/\n", ln.Addr())
	if err := http.Serve(ln, h); err != nil {
		fmt.Fprintln(os.Stderr, "fixture-gateway:", err)
		os.Exit(1)
	}
}
