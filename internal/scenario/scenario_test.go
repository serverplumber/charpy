package scenario_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/scenario"
)

// A conforming subject. charpy assumes its subject already passes conformance
// (README), so the fixture is the SDK's own server rather than a hand-rolled
// one: anything the scenario provokes here is the scenario's doing.
func subject(t *testing.T, calls *[]string) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			*calls = append(*calls, req.Params.Name)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil, nil
		})
	return s
}

// connect wires a client session straight to a server. The pipe seam itself is
// peer's business and is tested there; here the subject is the point.
func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	c := mcp.NewClient(&mcp.Implementation{Name: "charpy", Title: "charpy"}, nil)
	cs, err := c.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// The scenario's traffic comes off the compiled case, which is what keeps it
// from drifting away from the matcher it is meant to feed. A case wanting the
// second tools/call gets exactly two.
func TestTrafficIsDerivedFromTheMatcher(t *testing.T) {
	tests := []struct {
		name  string
		match interpose.Match
		want  int
	}{
		{"no ordinal", interpose.Match{Method: interpose.ParseGlob("tools/call")}, 1},
		{"second occurrence", interpose.Match{Occurrence: 2}, 2},
		{"fourth occurrence", interpose.Match{Occurrence: 4}, 4},
		{"every third", interpose.Match{Every: 3}, 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scenario.Repeats(tc.match); got != tc.want {
				t.Errorf("Repeats = %d, want %d", got, tc.want)
			}

			var calls []string
			sess := connect(t, subject(t, &calls))
			run, err := scenario.Basic(tc.match, scenario.Options{})
			if err != nil {
				t.Fatalf("Basic: %v", err)
			}
			if err := run(t.Context(), sess); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(calls) != tc.want {
				t.Errorf("originated %d calls, want %d", len(calls), tc.want)
			}
		})
	}
}

// Nothing pads the count. A scenario sending more than the case needs is one
// whose traffic is not derived from the case, and the surplus frames look
// exactly like a chatty subject.
func TestNothingIsSentBeyondWhatTheCaseNeeds(t *testing.T) {
	var calls []string
	sess := connect(t, subject(t, &calls))
	run, err := scenario.Basic(interpose.Match{Occurrence: 2}, scenario.Options{})
	if err != nil {
		t.Fatalf("Basic: %v", err)
	}
	if err := run(t.Context(), sess); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("originated %v, want exactly two calls", calls)
	}
}

// A case selecting on a method nothing here originates must say so at
// construction. Running anyway would report UNTRIGGERED, which is
// indistinguishable from a subject that behaved.
func TestAnUnoriginatableMethodIsRefusedUpFront(t *testing.T) {
	_, err := scenario.Basic(
		interpose.Match{Method: interpose.ParseGlob("resources/subscribe")},
		scenario.Options{},
	)
	if !errors.Is(err, scenario.ErrCannotOriginate) {
		t.Fatalf("err = %v, want ErrCannotOriginate", err)
	}
	if !strings.Contains(err.Error(), "resources/subscribe") {
		t.Errorf("error does not name the method: %v", err)
	}
}

// The scenario picks a tool rather than being told one, because a catalogue
// case says "a tools/call" and not which.
func TestTheFirstListedToolIsCalledWhenNoneIsNamed(t *testing.T) {
	var calls []string
	sess := connect(t, subject(t, &calls))
	run, err := scenario.Basic(interpose.Match{}, scenario.Options{})
	if err != nil {
		t.Fatalf("Basic: %v", err)
	}
	if err := run(t.Context(), sess); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 1 || calls[0] != "echo" {
		t.Errorf("called %v, want one call to echo", calls)
	}
}

// A tool answering with an error still produced the frame the matcher wanted,
// so the script keeps going. Only a transport failure ends it.
func TestAToolErrorDoesNotEndTheScript(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Title: "fixture"}, nil)
	n := 0
	mcp.AddTool(s, &mcp.Tool{Name: "grumpy", Description: "refuses"},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			n++
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "no"}},
			}, nil, nil
		})

	sess := connect(t, s)
	run, err := scenario.Basic(interpose.Match{Occurrence: 3}, scenario.Options{})
	if err != nil {
		t.Fatalf("Basic: %v", err)
	}
	if err := run(t.Context(), sess); err != nil {
		t.Fatalf("run stopped on a tool error: %v", err)
	}
	if n != 3 {
		t.Errorf("the subject saw %d calls, want 3", n)
	}
}
