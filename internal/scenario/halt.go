package scenario

import (
	"context"
	"errors"
)

// Why a scenario stopped before its last step.
//
// A scenario sees these through [context.Cause], never as a bare
// cancellation. "The context was cancelled" does not distinguish a withdrawn
// fault from an operator's signal, and the step in flight owes each a
// different answer: a withdrawal is the instant the liveness clock starts and
// the peer should probe, a signal means stop talking and flush.
var (
	// ErrWithdrawn is the cause when the interposer withdrew a fault. It is
	// not a failure: it is the handover back to a subject answering for
	// itself, and the liveness budget in [case.expect] runs from here.
	ErrWithdrawn = errors.New("scenario: fault withdrawn")

	// ErrSignal is the cause when the run was asked to stop -- SIGINT,
	// SIGTERM, or a driver shutting down.
	ErrSignal = errors.New("scenario: run cancelled")

	// ErrScriptDone is the cause when the scenario reached its last step and
	// stopped itself. Nothing went wrong.
	ErrScriptDone = errors.New("scenario: script complete")

	// ErrDeadline is the cause when the script ran out of time. It usually
	// means the subject stopped answering -- a fault that killed its parser
	// leaves the script waiting on a reply that will never come -- so it is
	// an observation about the subject rather than a harness failure, and the
	// oracle reads such a run as INCONCLUSIVE rather than as a pass.
	ErrDeadline = errors.New("scenario: script deadline")
)

// Halt stops a scenario, naming the reason.
//
// It exists so that no call site anywhere writes cancel(err) with a bare
// error, and no scenario reads ctx.Err() and gets context.Canceled for three
// different situations. Both ends of the interruption are named: the driver
// calls h.Withdrawn(), and the scenario asks Why(ctx).
type Halt struct {
	cancel context.CancelCauseFunc
}

// Interruptible returns a context to run a scenario under and the named ways
// to stop it. Cancelling the parent halts with [ErrSignal].
//
// The returned context is detached from the parent's cancellation and rejoined
// through [Halt], rather than derived from it. A derived context inherits the
// parent's cause, so a parent cancelled the ordinary way would deliver
// context.Canceled and win the race against any name put on it afterwards --
// which is the bare cancellation this type exists to prevent. Detaching makes
// Halt the only way this context can end, so every ending has a reason.
// Values still pass through; a parent deadline does not, because a deadline
// is an unnamed halt.
func Interruptible(parent context.Context) (context.Context, Halt) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	context.AfterFunc(parent, func() { cancel(ErrSignal) })
	return ctx, Halt{cancel: cancel}
}

// Withdrawn halts the scenario because the interposer withdrew its fault.
// Call it from the withdrawal instant, before anything else: that instant is
// what the liveness budget is measured from.
func (h Halt) Withdrawn() { h.cancel(ErrWithdrawn) }

// Signalled halts the scenario because the run is stopping.
func (h Halt) Signalled() { h.cancel(ErrSignal) }

// Done halts the scenario because its script finished.
func (h Halt) Done() { h.cancel(ErrScriptDone) }

// Expired halts the scenario because it ran out of time.
func (h Halt) Expired() { h.cancel(ErrDeadline) }

// Why reports why a scenario's context ended, or nil if it has not.
//
// It answers with one of the named causes above. A context halted by anything
// else -- a deadline, a cancel charpy did not route through [Halt] -- answers
// with whatever cause that carried, which is the honest report rather than a
// coerced one.
func Why(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return context.Cause(ctx)
}

// Stopped reports whether ctx ended for the given reason.
func Stopped(ctx context.Context, why error) bool {
	return errors.Is(Why(ctx), why)
}
