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

// TestCacheEvictionKeepsBothMapsConsistent — the two indexes move
// together. A byPath entry outliving its byKey entry would let the
// enricher act on a verdict the sweeper believes it has forgotten.
//
// Overflow DEMOTES rather than discards (see Set), so "together" now
// means both are demoted, not both dropped — and Get / LookupPath both
// read through to the demoted generation, so neither side can see a
// state the other cannot.
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

	// The next write trips the generation swap.
	c.Set(Key{Path: "z", Size: 1, MTimeNS: 1}, Outcome{Matched: true, Decision: testDecision("z")})

	if _, ok := c.LookupPath("z"); !ok {
		t.Error("the entry that triggered the swap must survive")
	}
	for i := range cap {
		p := string(rune('a' + i))
		k := Key{Path: p, Size: 1, MTimeNS: 1}
		_, viaKey := c.Get(k)
		_, viaPath := c.LookupPath(p)
		if viaKey != viaPath {
			t.Errorf("%q: Get=%v but LookupPath=%v — the indexes must agree, "+
				"or the enricher acts on a verdict the sweeper thinks is gone",
				p, viaKey, viaPath)
		}
	}
}

// The generation before the swap must still be READABLE, which is the
// whole point of demoting instead of resetting.
//
// The sweeper writes every verdict and only then calls
// ResetEnrichedByPaths — its own comment states the ordering ("Cache
// writes complete before the re-queue") — so the enricher consumes the
// batch AFTER the sweep that produced it. A reset mid-sweep therefore
// destroyed exactly the entries with a pending consumer, costing an
// fpcalc decode plus an AcoustID lookup each to recompute.
func TestCacheOverflowDemotesInsteadOfDiscarding(t *testing.T) {
	const cap = 4
	c := NewCache(cap)
	for i := range cap {
		c.Set(Key{Path: string(rune('a' + i)), Size: 1, MTimeNS: 1},
			Outcome{Matched: true, Decision: testDecision("x")})
	}
	c.Set(Key{Path: "z", Size: 1, MTimeNS: 1}, Outcome{Matched: true, Decision: testDecision("z")})

	for i := range cap {
		p := string(rune('a' + i))
		if _, ok := c.LookupPath(p); !ok {
			t.Errorf("%q was discarded by the overflow; it must be demoted and "+
				"still readable — the enricher has not consumed it yet", p)
		}
		if _, ok := c.Get(Key{Path: p, Size: 1, MTimeNS: 1}); !ok {
			t.Errorf("%q missing from Get after demotion", p)
		}
	}
}

// Memory stays bounded: a second overflow drops the older generation, so
// retention is at most 2x capacity however long the process runs.
func TestCacheRetentionIsBoundedAtTwoGenerations(t *testing.T) {
	const cap = 4
	c := NewCache(cap)
	write := func(prefix string) {
		for i := range cap {
			c.Set(Key{Path: prefix + string(rune('a'+i)), Size: 1, MTimeNS: 1},
				Outcome{Matched: true, Decision: testDecision("x")})
		}
	}
	write("g1-")
	write("g2-") // demotes g1
	write("g3-") // demotes g2, drops g1

	if _, ok := c.LookupPath("g1-a"); ok {
		t.Error("generation 1 survived two swaps — retention must stay bounded")
	}
	if _, ok := c.LookupPath("g3-a"); !ok {
		t.Error("the newest generation must be present")
	}
	if c.Len() > 2*cap {
		t.Errorf("Len = %d, want <= %d (two generations)", c.Len(), 2*cap)
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
