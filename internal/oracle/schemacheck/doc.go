// Package schemacheck is oracle layer 1: validation against the vendored JSON
// Schema for the revision actually negotiated.
//
// This is the only layer that may issue a MUST verdict, because charpy is not
// asserting a reading -- a generated normative artifact rejected the frame and
// the report cites the $ref path that did it.
//
// Frames charpy deliberately corrupted are excluded; only frames the subject
// originated are validated.
package schemacheck
