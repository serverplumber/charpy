// Package liveness is oracle layer 4: does the subject resume serving after
// the fault is withdrawn?
//
// Everyone tests that failure doesn't crash. Almost nobody tests recovery, and
// recovery is the production question.
//
// The clock starts at the fault_withdrawn event. The probe method is
// revision-dependent: ping was removed in 2026-07-28, so server/discover is
// used there, with tools/list as the universal fallback.
package liveness
