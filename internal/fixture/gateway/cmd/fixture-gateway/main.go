// Command fixture-gateway serves charpy's fixture gateway over Streamable
// HTTP, so a driver can spawn it and conformance can be pointed at it.
//
//	fixture-gateway -http 127.0.0.1:8080 -upstream http://127.0.0.1:3000/mcp
//
// -upstream repeats, once per upstream, and -upstream-header once per header
// sent to them. -plant takes a comma-separated list of the bugs to plant
// (leak, nodeadline, cascade); none by default. A test instrument, not a
// deliverable: see package gateway.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/serverplumber/charpy/internal/fixture/gateway"
)

func main() {
	var o gateway.Options
	addr := flag.String("http", "127.0.0.1:0", "address the downstream face listens on")
	verbose := flag.Bool("v", false, "log the gateway's diagnostics to stderr")
	flag.DurationVar(&o.UpstreamIdle, "idle", gateway.DefaultUpstreamIdle,
		"how long a call may hear nothing from its upstream before it fails")
	flag.Func("upstream", "a Streamable HTTP upstream endpoint (repeatable)", func(s string) error {
		o.Upstreams = append(o.Upstreams, s)
		return nil
	})
	flag.Func("upstream-header", "a `Name: value` header sent upstream (repeatable)", func(s string) error {
		name, value, ok := strings.Cut(s, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("want Name: value, got %q", s)
		}
		if o.UpstreamHeader == nil {
			o.UpstreamHeader = http.Header{}
		}
		o.UpstreamHeader.Add(strings.TrimSpace(name), strings.TrimSpace(value))
		return nil
	})
	flag.Func("plant", "comma-separated bugs to plant: leak, nodeadline, cascade", func(s string) error {
		ps, err := gateway.ParsePlants(s)
		o.Plants = append(o.Plants, ps...)
		return err
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
