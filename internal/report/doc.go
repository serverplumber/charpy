// Package report renders a finished run.
//
// Outputs are the formats CI systems already consume: JUnit XML with one
// testcase per case citation, and JSONL verdicts, plus plain text for a
// person. Designed, not built: a divergence table in Markdown and CSV, and a
// static HTML directory served from embed.FS with no web server, whose one
// essential affordance for gateway runs is clicking an id and seeing both
// faces side by side.
//
// SKIPPED, INCONCLUSIVE and UNTRIGGERED are rendered distinctly from passes.
// A suite that reports skips as passes acquires false confidence, which is
// how test suites become worthless.
package report
