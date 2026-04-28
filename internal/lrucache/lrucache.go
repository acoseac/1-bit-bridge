// Package lrucache implements a small bounded LRU cache safe for
// concurrent use. It exists so the enricher's per-process memoization
// caches (album → release-MBID, artist → MBID, etc.) have a hard size
// ceiling instead of growing for the lifetime of the process.
//
// The cache is keyed by `comparable` and stores any value type. Map
// capacity is pre-allocated at construction so the bulk-ingestion
// phase of a 50k-track scan doesn't trigger Go map bucket resizing
// mid-flight; the doubly-linked list grows naturally.
//
// We don't pull in `hashicorp/golang-lru` because the dependency
// surface isn't worth ~80 LOC of generics. Reads on empty buckets
// short-circuit cheaply.
package lrucache

import (
	"container/list"
	"sync"
)

// Cache is a bounded, mutex-protected LRU. Zero value is NOT usable —
// always construct via New.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[K]*list.Element
}

// entry is what the doubly-linked list nodes hold so we can recover
// the key when evicting the LRU tail.
type entry[K comparable, V any] struct {
	key   K
	value V
}

// New returns a Cache with the given capacity. Capacity <= 0 is
// silently treated as 1 — a zero-cap cache would never store
// anything, which is almost certainly a configuration bug we'd
// rather surface as "very small" than "silently broken".
func New[K comparable, V any](capacity int) *Cache[K, V] {
	if capacity <= 0 {
		capacity = 1
	}
	return &Cache[K, V]{
		capacity: capacity,
		ll:       list.New(),
		// Pre-allocate the map at full capacity so the ingestion phase
		// of a large scan doesn't rehash buckets mid-flight. The list
		// itself grows as needed; the map is the GC-pressure hot spot.
		items: make(map[K]*list.Element, capacity),
	}
}

// Get returns the cached value and true on hit; the zero value and
// false on miss. A hit moves the entry to the front (most-recently-
// used).
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*entry[K, V]).value, true
	}
	var zero V
	return zero, false
}

// Set inserts or updates the entry for key. On overflow the oldest
// entry is evicted from the tail. Updates move the entry to the
// front and overwrite the value in place — no node churn.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*entry[K, V]).value = value
		return
	}
	el := c.ll.PushFront(&entry[K, V]{key: key, value: value})
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*entry[K, V]).key)
		}
	}
}

// Has returns true if key is present, without promoting it to MRU.
// Useful for negative-cache checks where the caller doesn't want
// the lookup itself to keep the entry alive.
func (c *Cache[K, V]) Has(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[key]
	return ok
}

// Len returns the current number of entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
