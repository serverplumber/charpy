package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Plant names a bug planted in the fixture on purpose.
//
// The fixture is a control: with nothing planted, a run against it must find
// nothing, and with one planted, exactly that. So every plant is off unless
// asked for, and each one shows only under a fault -- a fixture that misbehaved
// on a correct sequence would fail conformance, and a finding against it could
// not be told apart from the bug it already had. `just fixture-conformance`
// takes a list of plants to show that.
//
// Each plant is a bug real gateways have, chosen because a shipped case
// triggers it and a layer of the oracle can judge it.
type Plant string

const (
	// PlantLeak puts the upstream's address, the SDK's account of the failure
	// -- session id included -- and the headers the gateway sends upstream into
	// the error a downstream client gets when an upstream call fails. The
	// gateway that echoes its outbound request into an error message, for
	// debugging, is common. Triggered by any upstream failure, e.g.
	// gateway/upstream-credential-downstream's hang; judged by I5,
	// no-credential-leak, which needs the credential to be one charpy planted
	// (Options.UpstreamHeader).
	PlantLeak Plant = "leak"

	// PlantNoDeadline takes away the idle deadline on upstream calls, so a hung
	// upstream hangs its downstream caller with it -- the default for a gateway
	// nobody thought about. Triggered by a hang on the upstream face, e.g.
	// gateway/upstream-credential-downstream; judged by reaction: the gateway
	// did not answer after the fault.
	PlantNoDeadline Plant = "nodeadline"

	// PlantCascade ends the downstream session when an upstream session fails,
	// rather than opening a new one on the next call: downstream and upstream
	// sessions bound one-to-one and torn down together, a common shortcut.
	// Triggered by a fault that ends the upstream session, e.g.
	// stream/truncate-mid-event on the upstream face; judged by reaction: the
	// question after the fault goes unanswered.
	PlantCascade Plant = "cascade"
)

// Plants lists every plant, for flags and diagnostics.
var Plants = []Plant{PlantLeak, PlantNoDeadline, PlantCascade}

// ParsePlants reads a comma-separated list of plant names, refusing any it does
// not know: a misspelled plant would otherwise be a control run with the bug
// silently absent.
func ParsePlants(s string) ([]Plant, error) {
	var out []Plant
	for name := range strings.SplitSeq(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(Plants, Plant(name)) {
			return nil, fmt.Errorf("gateway: unknown plant %q (known: %v)", name, Plants)
		}
		out = append(out, Plant(name))
	}
	return out, nil
}

func (s *session) planted(p Plant) bool { return slices.Contains(s.o.Plants, p) }

// cascade is PlantCascade's teardown: with an upstream session gone, the
// downstream one goes too. The close is not waited for -- the SDK waits for
// in-flight handlers before it closes, and the caller may be one -- so the
// call that failed is still answered, and nothing after it is.
func (s *session) cascade() {
	if !s.planted(PlantCascade) {
		return
	}
	if d := s.downstream(); d != nil {
		go func() { _ = d.Close() }()
	}
}

// headerTransport sends the configured headers on every upstream request: the
// gateway's credential for its upstreams, which charpy plants so I5 has a
// known secret to look for downstream.
type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, vs := range t.header {
		req.Header[k] = vs
	}
	return t.base.RoundTrip(req)
}

// leaked is the error text PlantLeak writes: everything the gateway knows
// about the failed request, as a debugging gateway would.
func (s *session) leaked(u *upstream, what string, err error) string {
	var hs []string
	for k, vs := range s.o.UpstreamHeader {
		for _, v := range vs {
			hs = append(hs, k+": "+v)
		}
	}
	slices.Sort(hs)
	return fmt.Sprintf("gateway: upstream %s %s: %v (request headers: %s)", u.endpoint, what, err, strings.Join(hs, "; "))
}
