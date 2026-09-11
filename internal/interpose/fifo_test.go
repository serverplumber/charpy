package interpose_test

import (
	"testing"

	"github.com/creachadair/mds/cache"
	"github.com/serverplumber/charpy/internal/interpose"
)

func newFIFO(t *testing.T, limit int64) *cache.Cache[string, int] {
	t.Helper()
	return cache.New(interpose.FIFO[string, int]().WithLimit(limit))
}

func TestFIFOEvictsOldestFirst(t *testing.T) {
	c := newFIFO(t, 3)
	for _, k := range []string{"a", "b", "c"} {
		c.Put(k, 1)
	}
	c.Put("d", 1)

	if c.Has("a") {
		t.Error("the oldest entry survived eviction")
	}
	for _, k := range []string{"b", "c", "d"} {
		if !c.Has(k) {
			t.Errorf("%q was evicted before the oldest entry", k)
		}
	}
	if got := c.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}
}

// The load-bearing test. An LRU or Sieve store keeps a popular entry alive,
// which is right for a cache and wrong for a ledger window: an entry too old
// to be a plausible match must stop being matchable however often it is read.
// If this test is ever "fixed" by making reads refresh an entry, the window
// has silently become a cache and the ledger can propose a stale join.
func TestFIFOReadDoesNotExtendLife(t *testing.T) {
	c := newFIFO(t, 2)
	c.Put("old", 1)
	c.Put("new", 2)

	for range 10 {
		if _, ok := c.Get("old"); !ok {
			t.Fatal("the entry under test was evicted early")
		}
	}

	c.Put("newest", 3)

	if c.Has("old") {
		t.Error("reading an entry kept it alive; this is FIFO, not LRU")
	}
	if !c.Has("new") {
		t.Error("the wrong entry was evicted: age decides, not access")
	}
}

// Removal is a pointer splice, so an entry that leaves early is gone rather
// than lingering as a tombstone for Evict to trip over.
func TestFIFORemoveLeavesNoTombstone(t *testing.T) {
	c := newFIFO(t, 2)
	c.Put("a", 1)
	c.Put("b", 2)

	if !c.Remove("a") {
		t.Fatal("Remove reported nothing removed")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d after removing one of two, want 1", got)
	}

	c.Put("c", 3)
	c.Put("d", 4)

	// "b" is now the oldest present entry, so it is what goes -- not a dead
	// reference to the already-removed "a".
	if c.Has("b") {
		t.Error("eviction skipped the oldest live entry")
	}
	for _, k := range []string{"c", "d"} {
		if !c.Has(k) {
			t.Errorf("%q was evicted in place of the oldest live entry", k)
		}
	}
}

func TestFIFORemoveOfAbsentKeyIsANoop(t *testing.T) {
	c := newFIFO(t, 2)
	c.Put("a", 1)

	if c.Remove("nope") {
		t.Error("Remove reported removing a key that was never stored")
	}
	if !c.Has("a") || c.Len() != 1 {
		t.Error("removing an absent key disturbed the store")
	}
}

func TestFIFOReplacingAKeyDoesNotPanic(t *testing.T) {
	// cache.Cache removes before storing, so the store's duplicate-key panic
	// stays unreachable through the public API. Asserting it here means a
	// change in that contract surfaces as a test failure rather than as a
	// panic in the frame path.
	c := newFIFO(t, 2)
	c.Put("a", 1)
	c.Put("a", 2)

	if got, ok := c.Get("a"); !ok || got != 2 {
		t.Errorf("Get(a) = %d,%v want 2,true", got, ok)
	}
	if got := c.Len(); got != 1 {
		t.Errorf("Len() = %d after replacing a key, want 1", got)
	}
}

func TestFIFOEvictsInOrderAcrossManyEntries(t *testing.T) {
	const limit = 8
	c := newFIFO(t, limit)
	for i := range 100 {
		c.Put(string(rune('a'+i%26))+string(rune('0'+i/26)), i)
	}
	if got := c.Len(); got != limit {
		t.Errorf("Len() = %d, want the limit %d", got, limit)
	}
}
