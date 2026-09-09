// Package stdio is the stdio-ingress driver: charpy launches the subject as a
// subprocess and sits on its pipes.
//
// stdio subjects spawn per case and may run concurrently, unlike an HTTP
// gateway which is one process with shared state. Scheduling follows the
// subject's model, not charpy's.
package stdio
