package interpose

import (
	"fmt"

	"github.com/creachadair/mds/cache"
	"github.com/creachadair/mds/ring"
)

// FIFO constructs a [cache.Config] whose store evicts strictly in insertion
// order: the oldest entry goes first, and reading an entry does not extend its
// life.
//
// mds/cache ships LRU and Sieve, and both deliberately keep an entry alive
// because it was read. That is correct for a cache, whose job is to serve
// hits, and wrong for the ledger's windows, whose job is to stop offering an
// entry once it is too old to be a plausible match. A resolved id or a content
// digest from ten minutes ago must not become matchable again merely because
// something looked at it: a join proposed against a stale entry is a
// correlation charpy cannot support, and the credibility rule is that charpy
// does not offer one. See docs/design/interposer.md section 3.
//
// Pair it with a limit, which is what bounds the window:
//
//	cache.New(FIFO[string, resolved]().WithLimit(4096))
//
// Do not reach for [cache.Cache.PutWithExpiration] to bound a window by time.
// Its deadline is a time.Time, and ledger retention is charpy's own
// scheduling, which belongs on the injected clock (decisions.md ADR-001). A
// time bound, if one is ever wanted, reads clock.Sched.
func FIFO[Key comparable, Value any]() cache.Config[Key, Value] {
	return cache.Config[Key, Value]{}.WithStore(&fifoStore[Key, Value]{
		present:  make(map[Key]handle[Key, Value]),
		sentinel: ring.New[fifoEntry[Key, Value]](1),
	})
}

type fifoEntry[Key comparable, Value any] struct {
	key   Key
	value Value
}

type handle[Key comparable, Value any] = *ring.Ring[fifoEntry[Key, Value]]

// fifoStore implements [cache.Store] with first-in-first-out eviction.
//
// The layout mirrors mds/cache's own sieveStore: a map to ring handles, so
// removing an entry is a pointer splice and leaves nothing behind. An
// array-backed FIFO would make Remove leave a tombstone for Evict to skip,
// and a workload that removes entries without ever reaching the limit would
// accumulate them indefinitely -- a leak in the one structure that exists to
// bound memory.
type fifoStore[Key comparable, Value any] struct {
	present  map[Key]handle[Key, Value]
	sentinel handle[Key, Value]
}

// Check implements part of the [cache.Store] interface.
func (s *fifoStore[Key, Value]) Check(key Key) (Value, bool) {
	if e, ok := s.present[key]; ok {
		return e.Value.value, true
	}
	var zero Value
	return zero, false
}

// Access implements part of the [cache.Store] interface.
//
// It does not reorder, and that is the whole policy: under FIFO an entry's
// life is its age, never its popularity.
func (s *fifoStore[Key, Value]) Access(key Key) (Value, bool) { return s.Check(key) }

// Store implements part of the [cache.Store] interface.
func (s *fifoStore[Key, Value]) Store(key Key, val Value) {
	if _, ok := s.present[key]; ok {
		panic(fmt.Sprintf("fifo store: unexpected key %v", key))
	}
	// Join splices the new entry in directly after the sentinel, so the
	// newest is always sentinel.Next() and the oldest sentinel.Prev().
	e := ring.Of(fifoEntry[Key, Value]{key: key, value: val})
	s.sentinel.Join(e)
	s.present[key] = e
}

// Remove implements part of the [cache.Store] interface.
func (s *fifoStore[Key, _]) Remove(key Key) {
	if e, ok := s.present[key]; ok {
		e.Pop()
		delete(s.present, key)
	}
}

// Evict implements part of the [cache.Store] interface.
func (s *fifoStore[Key, Value]) Evict() (Key, Value) {
	out := s.sentinel.Prev()
	if out == s.sentinel {
		panic("fifo evict: no entries left")
	}
	out.Pop()
	delete(s.present, out.Value.key)
	return out.Value.key, out.Value.value
}
