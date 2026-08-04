package acoustid

import (
	"sync"
	"testing"
)

func testDecision(artist string) Decision {
	return Decision{ArtistMBID: artist, ArtistName: "Name", AcoustID: "acid-1"}
}

func TestCacheRoundTrip(t *testing.T) {
	c := NewCache(10)
	k := Key{Path: "a.flac", Size: 100, MTimeNS: 1}

	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache must miss")
	}
	if _, ok := c.LookupPath("a.flac"); ok {
		t.Fatal("empty cache must miss by path too")
	}

	c.Set(k, Outcome{Matched: true, Decision: testDecision("artist-1")})

	if o, ok := c.Get(k); !ok || !o.Matched {
		t.Fatalf("Get = (%+v, %v), want a match", o, ok)
	}
	d, ok := c.LookupPath("a.flac")
	if !ok || d.ArtistMBID != "artist-1" {
		t.Fatalf("LookupPath = (%+v, %v)", d, ok)
	}
}

// TestCacheRecordsMissesAsValues pins why there is no separate negative cache.
//
// A no-match must be REMEMBERED, or every sweep re-decodes the same
// unmatchable files — the expensive half of the work. It is stored as
// Matched:false rather than as an absent entry, which is also what keeps a
// second cache unnecessary: a fingerprint is per-file, so a negative cache
// would hold one entry per candidate, the same cardinality as this one, while
// saving only the HTTP call.
func TestCacheRecordsMissesAsValues(t *testing.T) {
	c := NewCache(10)
	k := Key{Path: "a.flac", Size: 100, MTimeNS: 1}
	c.Set(k, Outcome{}) // a recorded miss

	// The sweeper must see it, so it does not re-decode.
	if _, ok := c.Get(k); !ok {
		t.Fatal("a recorded miss must be a cache HIT for the sweeper")
	}
	// The enricher must not, so it does not act on nothing.
	if _, ok := c.LookupPath("a.flac"); ok {
		t.Fatal("a miss must not surface to the enricher as a match")
	}
}

// TestCacheKeyIncludesFileVersion — an edited or re-encoded file must not
// inherit the previous version's verdict, because a stale hit here writes an
// MBID.
func TestCacheKeyIncludesFileVersion(t *testing.T) {
	c := NewCache(10)
	base := Key{Path: "a.flac", Size: 100, MTimeNS: 1}
	c.Set(base, Outcome{Matched: true, Decision: testDecision("artist-1")})

	for _, changed := range []Key{
		{Path: "a.flac", Size: 101, MTimeNS: 1}, // tag edit changes size
		{Path: "a.flac", Size: 100, MTimeNS: 2}, // touched or re-encoded
	} {
		if _, ok := c.Get(changed); ok {
			t.Errorf("%+v must miss — it is a different file version", changed)
		}
	}
}

// TestCacheEvictionKeepsBothMapsConsistent — eviction resets both indexes, so
// a path can never survive its key. A leftover byPath entry would let the
// enricher act on a verdict the sweeper believes it has forgotten.
func TestCacheEvictionKeepsBothMapsConsistent(t *testing.T) {
	const cap = 4
	c := NewCache(cap)
	for i := range cap {
		c.Set(Key{Path: string(rune('a' + i)), Size: 1, MTimeNS: 1},
			Outcome{Matched: true, Decision: testDecision("x")})
	}
	if c.Len() != cap {
		t.Fatalf("Len = %d, want %d", c.Len(), cap)
	}

	// The next write trips eviction.
	c.Set(Key{Path: "z", Size: 1, MTimeNS: 1}, Outcome{Matched: true, Decision: testDecision("z")})
	if c.Len() != 1 {
		t.Fatalf("Len = %d after eviction, want 1", c.Len())
	}
	for i := range cap {
		p := string(rune('a' + i))
		if _, ok := c.LookupPath(p); ok {
			t.Errorf("%q survived eviction in byPath — the two indexes must reset together", p)
		}
	}
	if _, ok := c.LookupPath("z"); !ok {
		t.Error("the entry that triggered eviction must survive")
	}
}

// TestCacheIsConcurrencySafe — the sweeper's workers write while the enricher
// goroutine reads. Run under -race, which CI does.
func TestCacheIsConcurrencySafe(t *testing.T) {
	c := NewCache(1000)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				k := Key{Path: string(rune('a' + i)), Size: int64(j), MTimeNS: int64(j)}
				c.Set(k, Outcome{Matched: true, Decision: testDecision("x")})
				c.Get(k)
				c.LookupPath(k.Path)
				c.Len()
			}
		}()
	}
	wg.Wait()
}

func TestCacheUnboundedWhenCapacityNonPositive(t *testing.T) {
	c := NewCache(0)
	for i := range 50 {
		c.Set(Key{Path: string(rune('a' + i%26)), Size: int64(i), MTimeNS: 1}, Outcome{})
	}
	if c.Len() != 50 {
		t.Fatalf("Len = %d, want 50 — a non-positive capacity disables bounding", c.Len())
	}
}
