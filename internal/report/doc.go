// Package report renders a finished run.
//
// Outputs are JSONL verdicts, one finding per line and queryable like the
// transcript, and plain text for a person. The build gate is not a format but
// replay's exit code, which fails on a MUST and nothing else. JUnit was
// dropped: its vocabulary cannot say what an OBSERVED finding is (see
// docs/open-problems.md). Designed, not built: a divergence table in Markdown and CSV, and a
// static HTML directory served from embed.FS with no web server, whose one
// essential affordance for gateway runs is clicking an id and seeing both
// faces side by side.
//
// SKIPPED, INCONCLUSIVE and UNTRIGGERED are rendered distinctly from passes.
// A suite that reports skips as passes acquires false confidence, which is
// how test suites become worthless.
package report
