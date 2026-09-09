// Package engine evaluates fault policy: matching frames, counting
// occurrences, scheduling injections, and deciding what the drivers do.
//
// It is shared verbatim by every driver. The drivers are thin; this is where
// the behaviour lives.
//
// Nothing here may block the frame path. Transcript writes are buffered and
// asynchronous, schema validation is offline, and the oracle is offline, so
// the proxy adds microseconds where subjects reason in milliseconds.
package engine
