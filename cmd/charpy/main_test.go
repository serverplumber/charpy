package main

import "testing"

func TestExitCodesAreTheCIContract(t *testing.T) {
	// These values are documented in the README and consumed by every CI
	// system charpy targets. Changing one is a breaking change.
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"clean", exitClean, 0},
		{"must violation", exitMustViolate, 1},
		{"harness error", exitHarness, 2},
		{"subject failed to start", exitSubject, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("exit code = %d, want %d", tc.got, tc.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "version", args: []string{"version"}, want: exitClean},
		{name: "help", args: []string{"help"}, want: exitClean},
		{name: "no args", args: nil, want: exitHarness},
		{name: "unknown command", args: []string{"frobnicate"}, want: exitHarness},
		{name: "known but unimplemented", args: []string{"replay"}, want: exitHarness},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
