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
// surface isn't worth ~120 LOC of generics. Reads on empty buckets
// short-circuit cheaply.
//
// **Zero `interface{}` boxing on the hot path** (PR-F audit follow-up).
// The prior implementation used `container/list`, whose `Element.Value`
// is `any` — every Get/Set required a type assertion to `*entry[K, V]`,
// and every PushFront allocated a `list.Element` value-box. This
// package replaces that with a struct-based generic doubly-linked
// list. Public API is unchanged: `Get`/`Set`/`Has`/`Len`.
package lrucache

import (
	"sync"
)

// node is one entry in the internal doubly-linked list. Generic on
// (K, V) so the prev/next pointers carry the concrete entry type —
// no interface boxing on access. `key` is carried alongside `value`
// so the eviction path (`evictOldestLocked`) can delete the map
// entry without an extra lookup.
//
// The struct is unexported and lives inside the package; consumers
// see only `Cache[K, V]` via the documented public methods.
type node[K comparable, V any] struct {
	key   K
	value V
	prev  *node[K, V]
	next  *node[K, V]
}

// Cache is a bounded, mutex-protected LRU. Zero value is NOT usable —
// always construct via New. The internal doubly-linked list is
// circular-with-sentinel: `root.next` is the MRU element, `root.prev`
// is the LRU tail. This idiom eliminates per-method nil checks for
// "empty list" vs "single element" cases.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	// root is the sentinel head/tail. `root.next` is the
	// most-recently-used; `root.prev` is the least-recently-used
	// (the eviction target). When the list is empty, both point
	// back at root.
	root  node[K, V]
	items map[K]*node[K, V]
	len   int
}

// New returns a Cache with the given capacity. Capacity <= 0 is
// silently treated as 1 — a zero-cap cache would never store
// anything, which is almost certainly a configuration bug we'd
// rather surface as "very small" than "silently broken".
func New[K comparable, V any](capacity int) *Cache[K, V] {
	if capacity <= 0 {
		capacity = 1
	}
	c := &Cache[K, V]{
		capacity: capacity,
		items:    make(map[K]*node[K, V], capacity),
	}
	c.root.prev = &c.root
	c.root.next = &c.root
	return c
}

// Get returns the cached value and true on hit; the zero value and
// false on miss. A hit moves the entry to the front (most-recently-
// used).
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.items[key]; ok {
		c.moveToFrontLocked(n)
		return n.value, true
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
	if n, ok := c.items[key]; ok {
		n.value = value
		c.moveToFrontLocked(n)
		return
	}
	n := &node[K, V]{key: key, value: value}
	c.pushFrontLocked(n)
	c.items[key] = n
	c.len++
	if c.len > c.capacity {
		c.evictOldestLocked()
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
	return c.len
}

// pushFrontLocked links n at the head of the list (root.next slot).
// Caller MUST hold c.mu.
func (c *Cache[K, V]) pushFrontLocked(n *node[K, V]) {
	n.prev = &c.root
	n.next = c.root.next
	c.root.next.prev = n
	c.root.next = n
}

// removeLocked unlinks n from the list. Caller MUST hold c.mu.
// Does NOT touch `items` or `len` — the caller's eviction /
// update path owns that.
func (c *Cache[K, V]) removeLocked(n *node[K, V]) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev = nil
	n.next = nil
}

// moveToFrontLocked promotes n to MRU position. No-op if already
// at the head. Caller MUST hold c.mu.
func (c *Cache[K, V]) moveToFrontLocked(n *node[K, V]) {
	if c.root.next == n {
		return
	}
	c.removeLocked(n)
	c.pushFrontLocked(n)
}

// evictOldestLocked unlinks the LRU tail node and drops its map
// entry. Caller MUST hold c.mu AND have just incremented `len`
// past `capacity` (the eviction restores the invariant).
func (c *Cache[K, V]) evictOldestLocked() {
	oldest := c.root.prev
	if oldest == &c.root {
		return
	}
	c.removeLocked(oldest)
	delete(c.items, oldest.key)
	c.len--
}
