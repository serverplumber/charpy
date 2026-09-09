// Package hostile is the hostile-server driver: charpy serves an HTTP endpoint
// for a client or gateway to connect to.
//
// It must be able to lie about which protocol era it implements, because a
// subject that mishandles an era mismatch is a legitimate target. See
// docs/design/revisions.md section 4.
package hostile
