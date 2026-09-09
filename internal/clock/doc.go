// Package clock provides charpy's injected clock.
//
// It governs charpy's own scheduling and the transcript's logical timeline
// only. Subject deadlines stay real-time: charpy cannot freeze a subprocess's
// clock without LD_PRELOAD or ptrace, and doing so would forfeit the
// language-agnosticism that makes charpy adoptable.
//
// The consequence is that determinism belongs to the oracle rather than the
// run: runs are best-effort reproducible, verdicts are exactly reproducible
// given a transcript. See docs/design/decisions.md ADR-001.
package clock
