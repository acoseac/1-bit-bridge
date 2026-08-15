package acoustid

import (
	"strings"
	"sync"
)

// Key identifies a file version. Size and mtime are both present because a
// tag edit changes the size and a re-encode changes both — either way the
// audio may differ, and a stale hit here would write an MBID.
//
// mtime is kept despite object-storage backends being inconsistent about it,
// because the bridge's own scanner skip-gate already relies on size+mtime
// against the same mounts and works. Dropping it to size-alone would trade a
// proven signal for a slightly higher hit rate on a cache whose misses are
// cheap and whose false hits are not.
//
// The sweeper builds these from the manifest ROW rather than from a stat, so
// the version qualifier is "the file as the scanner last recorded it" rather
// than "the file right now". The two diverge only between an on-disk change
// and the next scan — and in that window UpsertTrack has not reset enriched_at
// either, so the enricher could not consume a verdict for the row anyway. Once
// the scanner catches up, tags_json carries the new size and mtime, the key
// changes, and the next sweep re-fingerprints. Keying from the row is what
// keeps a filesystem round-trip off the candidate scan.
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

	// prevKey / prevPath are the previous generation, retained so
	// overflow demotes rather than discards. See Set.
	prevKey  map[Key]Outcome
	prevPath map[string]Outcome
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
// Checks the current generation, then the previous one (see Set).
func (c *Cache) Get(k Key) (Outcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if o, ok := c.byKey[k]; ok {
		return o, true
	}
	o, ok := c.prevKey[k]
	return o, ok
}

// Set records an outcome.
//
// Overflow DEMOTES the current generation instead of discarding it: the
// full maps become `prev` and a fresh pair starts, so the newest
// `capacity` entries always survive and reads fall through to `prev`.
// Memory is bounded at 2x capacity.
//
// This used to be a whole-map reset, justified by "each key is written
// once and read at most once by the enricher, so recency carries no
// information". The first half is true and the conclusion does not
// follow, because of WHEN the read happens. The sweeper writes every
// verdict and only then calls ResetEnrichedByPaths, whose own comment
// spells out the ordering: "Cache writes complete before the re-queue:
// the enricher must be able to find an answer for every path it is
// handed." So the enricher reads the whole batch AFTER the sweep, and
// the newest entries are exactly the ones whose consumer has not run.
//
// A reset therefore discarded the most valuable entries, not the least,
// and "starting fresh is the correct response" was backwards: each
// destroyed verdict costs an fpcalc decode plus an AcoustID lookup to
// recompute on the next cycle. Self-healing, but not free — and it
// degraded precisely under the load that caused it.
//
// A generation swap rather than the package's LRU (internal/lrucache)
// because the two maps have to evict in lockstep and that LRU exposes no
// eviction hook; keying byPath off byKey's evictions would need one.
// Demotion also fits the access pattern better than recency ordering:
// entries are written once, so there is no reuse for an LRU to learn
// from — only age, which is all this needs.
func (c *Cache) Set(k Key, o Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && len(c.byKey) >= c.capacity {
		c.prevKey, c.prevPath = c.byKey, c.byPath
		c.byKey = make(map[Key]Outcome)
		c.byPath = make(map[string]Outcome)
	}
	c.byKey[k] = o
	c.byPath[k.Path] = o
}

// Forget drops cached outcomes so the next sweep asks about those files
// again. An empty prefix drops everything; otherwise only paths at or beneath
// it go. Returns how many keyed entries were removed.
//
// This exists because the persisted no-match verdict alone does not make
// "Retry missing" mean try again. The sweeper's candidate scan consults THIS
// cache before anything else, so clearing the database rows would still leave
// every file answered during the current process suppressed until a restart —
// the button would appear to work and quietly do nothing for exactly the files
// the operator just watched fail. Both retry paths therefore clear both layers.
//
// Prefix matching is byte-exact and boundary-anchored, so "Album" does not
// reach "AlbumOther". Case-SENSITIVE deliberately: the database twin of this
// call bounds its scope with a BINARY range, and a case-folding cache would
// silently clear a different set than the rows it is paired with.
//
// Both generations are swept (see Set) — a survivor in prev would keep
// answering Get.
func (c *Cache) Forget(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prefix == "" {
		n := len(c.byKey) + len(c.prevKey)
		c.byKey, c.byPath = make(map[Key]Outcome), make(map[string]Outcome)
		c.prevKey, c.prevPath = nil, nil
		return n
	}
	n := 0
	for _, m := range []map[Key]Outcome{c.byKey, c.prevKey} {
		for k := range m {
			if pathUnderPrefix(k.Path, prefix) {
				delete(m, k)
				n++
			}
		}
	}
	for _, m := range []map[string]Outcome{c.byPath, c.prevPath} {
		for p := range m {
			if pathUnderPrefix(p, prefix) {
				delete(m, p)
			}
		}
	}
	return n
}

// pathUnderPrefix reports whether p is prefix itself or sits beneath it. The
// separator check is what stops a prefix matching a longer sibling name.
func pathUnderPrefix(p, prefix string) bool {
	trimmed := strings.TrimSuffix(prefix, "/")
	return p == trimmed || strings.HasPrefix(p, trimmed+"/")
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
	if !ok {
		// Fall through to the demoted generation — this is the read the
		// generation swap exists for. The enricher consumes a batch
		// AFTER the sweep that wrote it, so an overflow mid-sweep would
		// otherwise lose exactly the verdicts it is about to ask for.
		o, ok = c.prevPath[path]
	}
	if !ok || !o.Matched {
		return Decision{}, false
	}
	return o.Decision, true
}

// Len reports the number of cached outcomes across both generations,
// for logging and tests — what a caller can still find, not what is in
// the current map.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byKey) + len(c.prevKey)
}
