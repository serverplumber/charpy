// Package report renders a finished run.
//
// Outputs are the formats CI systems already consume: JUnit XML with one
// testcase per case citation, JSONL verdicts, a divergence table in Markdown
// and CSV, and a static HTML directory served from embed.FS with no web
// server. For gateway runs the HTML's one essential affordance is clicking an
// id and seeing both faces side by side.
//
// SKIPPED and INCONCLUSIVE are rendered distinctly from passes. A suite that
// reports skips as passes acquires false confidence, which is how test suites
// become worthless.
package report
