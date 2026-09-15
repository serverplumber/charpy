// Package scenario is the owned stimulus: the reference peer driven by a
// seeded script, so that a case's fault has traffic to attach to.
//
// It exists because the properties separating charpy from a chaos proxy all
// need traffic charpy chose. Case identity reproduces from ID plus seed only
// if the seed drives the traffic. The liveness probe must fire at the
// fault_withdrawn instant, which means something has to be listening for it.
// The cross-SDK differential is the same script on the same wire three times.
// A man-in-the-middle over somebody else's traffic is Toxiproxy -- useful, and
// not this.
//
// One case is armed per run here, and that is a product decision before it is
// a reproducibility one. A transcript carrying eleven faults is a puzzle, not
// a finding, and the person reading it has most likely never run a hostile
// fixture before. One fault, one transcript, one thing to fix is the unit that
// can be acted on. That case identity then reproduces exactly -- traffic
// cannot depend on which other faults were armed, because none were -- falls
// out of the same choice. Relayed runs still arm many cases at once, because
// nothing there controls the traffic and UNTRIGGERED is the expected outcome;
// the report separates them by citation either way.
//
// See docs/design/interposer.md section 5 and docs/design/case-identity.md.
package scenario
