package scenario_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/scenario"
	"github.com/serverplumber/charpy/internal/transcript"
)

// The shipped catalogue, every case against every class it names: exactly the
// cases whose frame nothing charpy stands up sends are unreachable, and a case
// a client drives is never ruled out.
func TestReachesTheShippedCatalogue(t *testing.T) {
	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := map[transcript.Class][]string{
		transcript.ClassGateway: {
			// A notification upstream: charpy's upstreams only answer.
			"manifest/mutate-silent",
			// A second upstream handshake: nothing asks one to reconnect.
			"lifecycle/capability-narrowed-on-later-handshake",
			// A sampling answer: charpy's upstreams never sample.
			"lifecycle/server-request-unanswered",
		},
	}

	seen := 0
	for _, r := range revision.All() {
		for _, cs := range cat.Applicable(r) {
			compiled, err := cs.Compile(r, "c4324d")
			if err != nil {
				t.Fatal(err)
			}
			for _, class := range cs.Subject {
				c := transcript.Class(class)
				err := scenario.Reaches(compiled.Match, c)
				want := slices.Contains(unreachable[c], cs.ID)
				switch {
				case want && !errors.Is(err, scenario.ErrUnreachable):
					t.Errorf("%s@%s as %s: err = %v, want ErrUnreachable", cs.ID, r, c, err)
				case !want && err != nil:
					t.Errorf("%s@%s as %s: %v", cs.ID, r, c, err)
				}
				seen++
			}
		}
	}
	if seen == 0 {
		t.Fatal("no case was checked")
	}
}

// The rules the catalogue does not exercise yet.
func TestReaches(t *testing.T) {
	m := func(face transcript.Face, dir transcript.Direction, method string) interpose.Match {
		return interpose.Match{Face: face, Direction: dir, Method: interpose.ParseGlob(method)}
	}
	tests := []struct {
		name  string
		match interpose.Match
		class transcript.Class
		ok    bool
	}{
		{"a method the script never sends", m(transcript.Downstream, transcript.C2S, "resources/subscribe"), transcript.ClassServer, false},
		{"a sampling answer from a server's client", m(transcript.Downstream, transcript.C2S, "sampling/createMessage"), transcript.ClassServer, true},
		{"the upstream face of a server", m(transcript.Upstream, transcript.S2C, "tools/call"), transcript.ClassServer, false},
		{"a forwarded call answered upstream", m(transcript.Upstream, transcript.S2C, "tools/call"), transcript.ClassGateway, true},
		{"the first upstream handshake", m(transcript.Upstream, transcript.S2C, "initialize"), transcript.ClassGateway, true},
		{"anything a client drives", m(transcript.Upstream, transcript.S2C, "resources/subscribe"), transcript.ClassClient, true},
		{"no face or direction", m("", "", "tools/call"), transcript.ClassServer, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := scenario.Reaches(tc.match, tc.class)
			if tc.ok && err != nil {
				t.Errorf("err = %v, want reachable", err)
			}
			if !tc.ok && !errors.Is(err, scenario.ErrUnreachable) {
				t.Errorf("err = %v, want ErrUnreachable", err)
			}
		})
	}

	// It names the frame, so a refusal says which.
	err := scenario.Reaches(m(transcript.Downstream, transcript.C2S, "resources/subscribe"), transcript.ClassServer)
	if err == nil || !strings.Contains(err.Error(), "resources/subscribe") {
		t.Errorf("error does not name the method: %v", err)
	}
}
