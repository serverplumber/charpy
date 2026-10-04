// Command metastrip is a test-only gateway: it forwards every request to one
// upstream and deletes _meta from it on the way, which drops the trace charpy
// stamped. It is the population that forces the inferred join, and nothing
// else; the fixture gateway forwards _meta and stays correct.
//
//	metastrip -http 127.0.0.1:8080 -upstream http://127.0.0.1:3000/
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("http", "127.0.0.1:0", "address to listen on")
	upstream := flag.String("upstream", "", "the one upstream")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "metastrip:", err)
		os.Exit(1)
	}
	fmt.Printf("http://%s/\n", ln.Addr())
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		out, err := http.NewRequestWithContext(r.Context(), r.Method, *upstream, bytes.NewReader(strip(body)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, vs := range r.Header {
			if k != "Content-Length" && k != "Traceparent" {
				out.Header[k] = vs
			}
		}
		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}))
}

// strip deletes params._meta from a JSON-RPC request body, leaving anything
// else -- including bodies that are not JSON -- as it was.
func strip(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m["params"] == nil {
		return body
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(m["params"], &params) != nil {
		return body
	}
	delete(params, "_meta")
	p, _ := json.Marshal(params)
	m["params"] = p
	out, _ := json.Marshal(m)
	return out
}
