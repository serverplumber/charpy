package scenario_test

import (
	"context"
	"testing"

	"github.com/serverplumber/charpy/internal/scenario"
)

// The whole reason halts are named. Three situations reach a scenario as a
// cancelled context, and the step in flight owes each a different answer: a
// withdrawal starts the liveness clock, a signal means flush and stop, a
// finished script means nothing at all went wrong. context.Canceled says none
// of that.
func TestEveryHaltSaysWhy(t *testing.T) {
	tests := []struct {
		name string
		halt func(scenario.Halt)
		want error
	}{
		{"withdrawn", func(h scenario.Halt) { h.Withdrawn() }, scenario.ErrWithdrawn},
		{"signalled", func(h scenario.Halt) { h.Signalled() }, scenario.ErrSignal},
		{"script done", func(h scenario.Halt) { h.Done() }, scenario.ErrScriptDone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, halt := scenario.Interruptible(t.Context())
			if scenario.Why(ctx) != nil {
				t.Fatal("a live scenario already has a reason to stop")
			}

			tc.halt(halt)
			<-ctx.Done()

			if got := scenario.Why(ctx); got != tc.want {
				t.Errorf("Why = %v, want %v", got, tc.want)
			}
			if !scenario.Stopped(ctx, tc.want) {
				t.Errorf("Stopped(%v) is false", tc.want)
			}
			for _, other := range []error{scenario.ErrWithdrawn, scenario.ErrSignal, scenario.ErrScriptDone} {
				if other != tc.want && scenario.Stopped(ctx, other) {
					t.Errorf("also reports stopping for %v", other)
				}
			}
		})
	}
}

// A withdrawal must be distinguishable from a shutdown at the moment the
// scenario notices, not reconstructed afterwards from what else was going on.
func TestWithdrawalIsNotAShutdown(t *testing.T) {
	ctx, halt := scenario.Interruptible(t.Context())
	halt.Withdrawn()
	<-ctx.Done()

	if scenario.Stopped(ctx, scenario.ErrSignal) {
		t.Error("a withdrawn fault reads as an operator signal")
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("ctx.Err() = %v; the context should still be a plain cancellation", ctx.Err())
	}
}

// Cancelling the parent is a signal, so a driver shutting down does not have
// to remember to call Signalled as well.
func TestCancellingTheParentIsASignal(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, _ := scenario.Interruptible(parent)

	cancel()
	<-ctx.Done()

	if !scenario.Stopped(ctx, scenario.ErrSignal) {
		t.Errorf("Why = %v, want ErrSignal", scenario.Why(ctx))
	}
}

// The first halt wins. A withdrawal followed by a shutdown still reads as a
// withdrawal, because that is what stopped the step that was in flight.
func TestTheFirstReasonIsTheOneKept(t *testing.T) {
	ctx, halt := scenario.Interruptible(t.Context())
	halt.Withdrawn()
	halt.Signalled()
	<-ctx.Done()

	if !scenario.Stopped(ctx, scenario.ErrWithdrawn) {
		t.Errorf("Why = %v, want the first reason", scenario.Why(ctx))
	}
}
