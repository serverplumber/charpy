package peer_test

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/serverplumber/charpy/internal/revision"
)

// TestSDKSpeaksEveryDatedRevision is the skew guard between the two pins.
// charpy vendors schemas and spec prose at one upstream commit (justfile
// mcp_commit) and the reference peer at another (go.mod). Nothing makes those
// agree, so this asserts the one overlap that matters: the peer can speak
// every revision charpy has a case for.
//
// The day the SDK adds a revision, this fails -- which is the point.
// revisions.md section 7 asks charpy to break loudly when the protocol moves
// rather than silently stop testing the new thing.
func TestSDKSpeaksEveryDatedRevision(t *testing.T) {
	sdk := mcp.SupportedProtocolVersions()

	for _, r := range revision.All() {
		if r == revision.Draft {
			continue
		}
		if !slices.Contains(sdk, string(r)) {
			t.Errorf("charpy knows revision %s and the reference peer cannot speak it; "+
				"either the SDK pin is behind or the revision list is ahead", r)
		}
	}

	for _, v := range sdk {
		if !revision.Known(revision.Revision(v)) {
			t.Errorf("the reference peer speaks %s and charpy does not know it; "+
				"vendor its schema and prose (just vendor-schemas vendor-specs) "+
				"and add it to revision.ordered", v)
		}
	}
}

// TestPeerCannotSpeakDraft records a gap rather than guarding against one.
// The SDK tracks dated revisions only, so draft -- which charpy sorts last and
// includes in every open-ended applies_to range -- is unreachable under owned
// stimulus. A draft-only case is SKIPPED against the reference peer, not
// UNTRIGGERED: the fault never had a peer that could carry it.
//
// If this ever fails the SDK has started tracking draft, and the skip reason
// in oracle.md section 2 can go.
func TestPeerCannotSpeakDraft(t *testing.T) {
	if slices.Contains(mcp.SupportedProtocolVersions(), string(revision.Draft)) {
		t.Errorf("the reference peer now speaks %s; the draft skip reason is obsolete",
			revision.Draft)
	}
}
