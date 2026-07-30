package acoustid

import "sync"

// Key identifies a file version. Size and mtime are both present because a
// tag edit changes the size and a re-encode changes both — either way the
// audio may differ, and a stale hit here would write an MBID.
//
// mtime is kept despite object-storage backends being inconsistent about it,
// because the bridge's own scanner skip-gate already relies on size+mtime
// against the same mounts and works. Dropping it to size-alone would trade a
// proven signal for a slightly higher hit rate on a cache whose misses are
// cheap and whose false hits are not.
type Key struct {
	Path    string
	Size    int64
	MTimeNS int64
}

// Outcome is what a completed fingerprint attempt concluded.
//
// A no-match is a VALUE (Matched=false), not an absent entry. That is the
// whole reason a separate negative cache is unnecessary: a fingerprint is
// per-file, so a negative cache would carry one entry per candidate — the
// same cardinality as this one — while saving only the HTTP call. The
// expensive half is the decode, and recording the miss here prevents that too.
type Outcome struct {
	Matched  bool
	Decision Decision
}

// Cache holds fingerprint outcomes for the current process.
//
// Bounded and in-memory by design. A persistent marker was considered and
// rejected: it fights the operator's "Retry missing" button, which MEANS "try
// again", and AcoustID's database grows, so a six-month-old no-match is worth
// re-checking. Persisting would need a TTL, a timestamp column and a sweeper
// for it — for a saving the per-run cap already bounds.
//
// Storing the Decision rather than the raw fingerprint keeps entries small
// (~200 B against ~2 KB for the base64), because nothing re-queries AcoustID
// with a fingerprint inside one process.
//
// Safe for concurrent use: the sweeper's workers write it while the enricher
// goroutine reads it.
type Cache struct {
	mu       sync.RWMutex
	capacity int
	byKey    map[Key]Outcome
	// byPath answers the enricher's question — it holds a client-relative
	// path and no stat — while byKey is what the sweeper dedupes on. Both
	// point at the same outcomes; byPath simply loses the version qualifier.
	byPath map[string]Outcome
}

// NewCache builds a cache bounded at capacity entries. A non-positive
// capacity disables bounding, which is only appropriate in tests.
func NewCache(capacity int) *Cache {
	return &Cache{
		capacity: capacity,
		byKey:    make(map[Key]Outcome),
		byPath:   make(map[string]Outcome),
	}
}

// Get reports a previously computed outcome for an exact file version.
func (c *Cache) Get(k Key) (Outcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.byKey[k]
	return o, ok
}

// Set records an outcome.
//
// Eviction is a whole-map reset rather than an LRU. That is deliberate: the
// access pattern is a sweep, not a working set — each key is written once and
// read at most once by the enricher — so recency carries no information and an
// LRU's bookkeeping would buy nothing. Reaching the cap at all means the
// sweeps have outrun consumption, and starting fresh is the correct response.
func (c *Cache) Set(k Key, o Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && len(c.byKey) >= c.capacity {
		c.byKey = make(map[Key]Outcome)
		c.byPath = make(map[string]Outcome)
	}
	c.byKey[k] = o
	c.byPath[k.Path] = o
}

// LookupPath returns the outcome for a client-relative path, ignoring the
// version qualifier.
//
// This is what the ENRICHER calls, and it is a pure map read: no context, no
// I/O, no stat. That is what lets the fallback sit on the enricher's single
// goroutine without giving it a filesystem dependency — an os.Stat against a
// hung network mount would otherwise block all enrichment.
//
// Ignoring size and mtime here is a deliberate, bounded imprecision: if a file
// changed between the sweep and the enricher reading this, the answer
// describes the previous version. The window is one sweep interval, the
// enricher re-reads tags for changed files anyway, and requiring a stat to
// close it would reintroduce exactly the dependency this design avoids.
func (c *Cache) LookupPath(path string) (Decision, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.byPath[path]
	if !ok || !o.Matched {
		return Decision{}, false
	}
	return o.Decision, true
}

// Len reports the number of cached outcomes, for logging and tests.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byKey)
}
