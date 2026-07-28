package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/lrucache"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A PERSISTENT artist-search failure must be negative-cached, so N
// tracks by the same artist cost ONE upstream request rather than N.
//
// The album path has done this since the transient-retry work landed
// (`albumCache.Set(key, albumResolution{})` on its persistent branch);
// the artist path never got the mirror. The gap was measurable on a
// live Atlas-backed bridge re-enriching a 19k-track library: 106
// identical HTTP 400s for one 2-character artist name — one per track,
// ~2.5 minutes of enricher time at the 1.1s pacer — against 7 for the
// album path rejecting the same way.
//
// (The 400 itself is an Atlas-side trigram-search limitation, not a
// MusicBrainz one — public MB resolves 2-character terms fine. That is
// tracked separately; this test is only about not amplifying whatever
// the upstream rejects.)
func TestResolveArtist_PersistentErrorIsNegativeCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// The exact shape an Atlas-backed bridge sees for a short term.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"query must be at least 3 characters"}`))
	}))
	defer srv.Close()

	e := &Enricher{
		mb:            NewMusicBrainzClient(srv.URL, "test", nil),
		MBMinInterval: 0, // don't pace the test
		artistCache:   newArtistCacheForTest(),
	}

	// Five sibling tracks by the same artist — the shape that produced
	// 106 requests in production.
	for i := 0; i < 5; i++ {
		tr := &manifest.Track{Path: "LP/Album/0" + string(rune('1'+i)) + ".flac", Artist: "LP"}
		if err := e.resolveArtist(context.Background(), tr); err != nil {
			t.Fatalf("resolveArtist #%d returned %v; a persistent error must be "+
				"absorbed so the track is still stamped enriched", i, err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream saw %d requests for one artist, want 1 — the persistent "+
			"failure is not being negative-cached, so every sibling track re-queries", got)
	}
}

// A TRANSIENT failure must NOT be cached: the whole point of the
// transient path is that the track retries once the upstream recovers.
// Caching it would reintroduce the outage-poisoning this file's
// transient handling exists to prevent.
func TestResolveArtist_TransientErrorIsNotCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // 503 → transient
	}))
	defer srv.Close()

	e := &Enricher{
		mb:            NewMusicBrainzClient(srv.URL, "test", nil),
		MBMinInterval: 0,
		artistCache:   newArtistCacheForTest(),
	}

	for i := 0; i < 3; i++ {
		tr := &manifest.Track{Path: "LP/Album/x.flac", Artist: "LP"}
		if err := e.resolveArtist(context.Background(), tr); err == nil {
			t.Fatalf("resolveArtist #%d returned nil for a 503; a transient error "+
				"must propagate so enrichOne skips MarkEnriched and retries", i)
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream saw %d requests, want 3 — a transient failure was cached, "+
			"which blocks the retry the transient path exists to preserve", got)
	}
}

// A genuine "no match" (200 with no results) must also stay uncached,
// preserving the documented PR #13 behaviour: a later metadata fix
// should be able to resolve the artist without a process restart.
func TestResolveArtist_NoMatchIsNotCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[]}`))
	}))
	defer srv.Close()

	e := &Enricher{
		mb:            NewMusicBrainzClient(srv.URL, "test", nil),
		MBMinInterval: 0,
		artistCache:   newArtistCacheForTest(),
	}

	for i := 0; i < 3; i++ {
		tr := &manifest.Track{Path: "Nobody/Album/x.flac", Artist: "Nobody At All"}
		if err := e.resolveArtist(context.Background(), tr); err != nil {
			t.Fatalf("resolveArtist #%d: %v", i, err)
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream saw %d requests, want 3 — a no-match result was cached, "+
			"blocking re-resolution after a metadata fix", got)
	}
}

// newArtistCacheForTest builds the same cache shape NewEnricher wires,
// so these tests exercise the real lookup path rather than a stand-in.
func newArtistCacheForTest() *lrucache.Cache[string, string] {
	return lrucache.New[string, string](128)
}
