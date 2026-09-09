// Package inproc wraps the official Go SDK's Transport interface, roughly
// fifty lines on top of the shared engine.
//
// It exists for what the wire cannot see: goroutine leaks per failed upstream,
// wedged mutexes, unbounded channel growth, session maps that never evict, and
// data races. Those need -race and runtime.NumGoroutine around a fixture
// linked directly.
//
// Deliberately internal for v0. Promotion later is a one-line move; demotion
// after someone depends on it is a breaking change. See
// docs/design/decisions.md ADR-002.
package inproc
