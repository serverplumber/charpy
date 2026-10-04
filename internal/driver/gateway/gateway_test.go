package gateway_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/serverplumber/charpy/cases"
	"github.com/serverplumber/charpy/internal/catalogue"
	"github.com/serverplumber/charpy/internal/clock"
	"github.com/serverplumber/charpy/internal/driver/gateway"
	"github.com/serverplumber/charpy/internal/interpose"
	"github.com/serverplumber/charpy/internal/revision"
	"github.com/serverplumber/charpy/internal/transcript"
)

// fixture is the fixture gateway's binary, built once: the driver spawns a
// process, so the subject it is tested against has to be one.
var fixture string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "charpy-gateway-test")
	if err != nil {
		panic(err)
	}
	fixture = filepath.Join(dir, "fixture-gateway")
	build := exec.Command("go", "build", "-o", fixture, "github.com/serverplumber/charpy/internal/fixture/gateway/cmd/fixture-gateway")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("building the fixture gateway: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// shipped compiles one case from the shipped catalogue.
func shipped(t *testing.T, id string) interpose.Case {
	t.Helper()
	cat, _, err := catalogue.Load(cases.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range cat.Applicable(revision.V20251125) {
		if cs.ID == id {
			c, err := cs.Compile(revision.V20251125, "8f2c1a")
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	t.Fatalf("no shipped case %s at 2025-11-25", id)
	return interpose.Case{}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Logf("gateway: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// drive runs one case against a freshly spawned fixture gateway and returns
// the transcript.
func drive(t *testing.T, c interpose.Case) *transcript.Transcript {
	t.Helper()
	var buf strings.Builder
	sched := clock.RealSched()
	tr, err := transcript.New(&buf, transcript.Options{
		Run: transcript.Run{
			CharpyVersion: "test", Seed: "8f2c1a", Mode: transcript.ModeProxy,
			Subject: transcript.Subject{Class: transcript.ClassGateway}, Clock: clock.ModeReal,
		},
		Sched: sched,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	d, err := gateway.New(gateway.Options{
		Command: []string{fixture, "-http", addr,
			"-upstream", "{upstream0}", "-upstream", "{upstream1}"},
		SubjectURL: "http://" + addr + "/",
		Errs:       logWriter{t},
		Case:       c,
		Era:        revision.V20251125,
		Timeout:    10 * time.Second,
		Transcript: tr, Sched: sched, Ledger: interpose.NewLedger(sched), RunSeed: "8f2c1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := transcript.Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func conn(f *transcript.FrameLine) string {
	if f.ConnID == nil {
		return ""
	}
	return *f.ConnID
}

// What every gateway run's transcript must hold, whichever face its case
// faults: one header, at the downstream face's revision; frames on both
// faces, upstream ones named by the upstream they reached; each upstream
// stated once; and the gateway charpy spawned, killed by charpy at the end.
func checkRun(t *testing.T, tr *transcript.Transcript) {
	t.Helper()
	if tr.Header.Revision == nil || tr.Header.Revision.Negotiated != revision.V20251125 {
		t.Errorf("header revision = %+v, want 2025-11-25", tr.Header.Revision)
	}
	faces := map[transcript.Face]int{}
	for _, f := range tr.Frames() {
		faces[f.Face]++
		if f.Face == transcript.Upstream && !strings.HasPrefix(conn(f), "u0-") && !strings.HasPrefix(conn(f), "u1-") {
			t.Errorf("upstream frame %d on %q, named for no upstream", f.Seq, conn(f))
		}
		if f.Face == transcript.Downstream && !strings.HasPrefix(conn(f), "d-") {
			t.Errorf("downstream frame %d on %q", f.Seq, conn(f))
		}
	}
	if faces[transcript.Downstream] == 0 || faces[transcript.Upstream] == 0 {
		t.Errorf("frames per face = %v, want both", faces)
	}

	upstreams := map[string]bool{}
	for _, e := range tr.Events(transcript.ConnOpen) {
		if u, ok := e.Detail["upstream"].(string); ok {
			upstreams[u] = true
		}
	}
	if !upstreams["u0"] || !upstreams["u1"] {
		t.Errorf("upstreams stated = %v, want u0 and u1", upstreams)
	}

	exits := tr.Events(transcript.SubjectExit)
	if len(exits) != 1 {
		t.Fatalf("%d subject_exit events, want 1", len(exits))
	}
	if killed, _ := exits[0].Detail["killed_by_charpy"].(bool); !killed {
		t.Errorf("the gateway exited on its own: %v", exits[0].Detail)
	}
}

// appliedOn is the face each applied fault acted on.
func appliedOn(tr *transcript.Transcript) []transcript.Face {
	var out []transcript.Face
	for _, e := range tr.Events(transcript.FaultApplied) {
		if e.Face != nil {
			out = append(out, *e.Face)
		}
	}
	return out
}

// A case put to the gateway's downstream face: charpy's client sends it a
// request body cut short.
func TestADownstreamCaseRunsThroughTheGateway(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-request-body"))
	checkRun(t, tr)
	if got := appliedOn(tr); len(got) != 1 || got[0] != transcript.Downstream {
		t.Errorf("faults applied on %v, want once on the downstream face", got)
	}
}

// A case put to the gateway's upstream face: charpy's upstream cuts the event
// stream answering the gateway's second call, which the gateway made because
// charpy's client called through it.
func TestAnUpstreamCaseRunsThroughTheGateway(t *testing.T) {
	tr := drive(t, shipped(t, "stream/truncate-mid-event"))
	checkRun(t, tr)
	if got := appliedOn(tr); len(got) != 1 || got[0] != transcript.Upstream {
		t.Errorf("faults applied on %v, want once on the upstream face", got)
	}
}

func TestACommandNamingNoUpstreamIsRefused(t *testing.T) {
	for _, command := range [][]string{
		{"gw", "-http", "127.0.0.1:1"},
		{"gw", "-upstream", "{upstream1}"},
	} {
		_, err := gateway.New(gateway.Options{
			Command: command, SubjectURL: "http://127.0.0.1:1/",
			Case: shipped(t, "stream/truncate-mid-event"),
		})
		if err == nil {
			t.Errorf("%v: accepted", command)
		}
	}
}
