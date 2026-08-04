package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// fakeMissLister serves a canned projection and counts how many times it
// was walked — the counter is what pins the "facet must not join the
// cache key" contract.
type fakeMissLister struct {
	refs  []manifest.TrackMetaRef
	walks atomic.Int32
}

func (f *fakeMissLister) StreamTrackMetaRefsUnderPrefix(_ context.Context, _ string,
	fn func(manifest.TrackMetaRef) error) error {
	f.walks.Add(1)
	for _, r := range f.refs {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func missFixture() []manifest.TrackMetaRef {
	return []manifest.TrackMetaRef{
		{Path: "A/complete.flac", ArtworkMBID: "aa", ArtistMBID: "bb", ReleaseMBID: "cc"},
		{Path: "A/no-release.flac", ArtworkMBID: "local-x", ArtistMBID: "bb"},
		{Path: "A/no-artist.flac", ArtworkMBID: "aa", ReleaseMBID: "cc"},
		{Path: "A/nothing.flac"},
	}
}

func TestComputeEnrichmentMissesCountsPerFacet(t *testing.T) {
	lister := &fakeMissLister{refs: missFixture()}
	got, err := computeEnrichmentMisses(context.Background(), lister, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	if got.Scanned != 4 {
		t.Errorf("Scanned = %d, want 4", got.Scanned)
	}
	// Three rows are short of something; the complete one is not.
	if got.Missing != 3 {
		t.Errorf("Missing = %d, want 3", got.Missing)
	}
	// A track short of two facets counts in both, so these deliberately
	// sum to more than Missing.
	want := map[string]int{
		manifest.MissFacetArtwork: 1, // nothing.flac
		manifest.MissFacetArtist:  2, // no-artist, nothing
		manifest.MissFacetRelease: 2, // no-release, nothing
	}
	for facet, n := range want {
		if got.Facets[facet] != n {
			t.Errorf("Facets[%s] = %d, want %d", facet, got.Facets[facet], n)
		}
	}
	// The local- sentinel satisfies the artwork arm — the #595 shape.
	for _, row := range got.Sample[manifest.MissFacetArtwork] {
		if row.Path == "A/no-release.flac" {
			t.Error("a local- artwork sentinel must NOT count as missing artwork")
		}
	}
}

func TestComputeEnrichmentMissesCapsTheSampleAndSaysSo(t *testing.T) {
	var refs []manifest.TrackMetaRef
	for i := 0; i < enrichmentMissSampleCap+25; i++ {
		refs = append(refs, manifest.TrackMetaRef{Path: fmt.Sprintf("A/%d.flac", i)})
	}
	got, err := computeEnrichmentMisses(context.Background(), &fakeMissLister{refs: refs}, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	// Counts stay EXACT even though the sample is capped — a capped
	// sample that also shrank the count would understate the problem.
	if got.Facets[manifest.MissFacetRelease] != len(refs) {
		t.Errorf("Facets[release] = %d, want the exact %d", got.Facets[manifest.MissFacetRelease], len(refs))
	}
	if n := len(got.Sample[manifest.MissFacetRelease]); n != enrichmentMissSampleCap {
		t.Errorf("sample len = %d, want the cap %d", n, enrichmentMissSampleCap)
	}
	if len(got.Truncated) == 0 {
		t.Error("Truncated must name the capped facets — a silently-cut list reads as complete")
	}
}

func TestNarrowMissesKeepsCountsWhileTrimmingTheSample(t *testing.T) {
	full, err := computeEnrichmentMisses(context.Background(), &fakeMissLister{refs: missFixture()}, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	got := narrowMisses(full, manifest.MissFacetRelease, 1)
	if len(got.Sample) != 1 {
		t.Fatalf("narrowed sample has %d facets, want only the requested one", len(got.Sample))
	}
	if n := len(got.Sample[manifest.MissFacetRelease]); n != 1 {
		t.Errorf("sample len = %d, want 1", n)
	}
	// Narrowing the VIEW must not change the reported size of the problem.
	if got.Facets[manifest.MissFacetRelease] != full.Facets[manifest.MissFacetRelease] {
		t.Errorf("narrowing changed the release count: %d -> %d",
			full.Facets[manifest.MissFacetRelease], got.Facets[manifest.MissFacetRelease])
	}
	if got.Missing != full.Missing {
		t.Errorf("narrowing changed Missing: %d -> %d", full.Missing, got.Missing)
	}
}

// TestEnrichmentMissesFacetDoesNotJoinTheCacheKey is the cost guard, and
// it drives the real handler.
//
// The walk is a json_extract subtree scan (the AtlasMetaBreakdownCounts
// cost class). If `facet` or `limit` joined the cache key, an operator
// clicking through the three facets in the UI would pay three
// full-library walks instead of one. One cached entry across three
// differently-parameterised requests is exactly that property.
func TestEnrichmentMissesFacetDoesNotJoinTheCacheKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, q := range []string{
		"?facet=artist", "?facet=release", "?facet=artwork&limit=5", "?limit=17", "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/enrichment/misses"+q, nil)
		w := httptest.NewRecorder()
		srv.apiEnrichmentMisses(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d (%s)", q, w.Code, w.Body.String())
		}
	}
	srv.libMetaMisses.mu.Lock()
	entries := len(srv.libMetaMisses.m)
	srv.libMetaMisses.mu.Unlock()
	if entries != 1 {
		t.Fatalf("cache holds %d entries after five requests that differ only by "+
			"facet/limit, want 1 — a query parameter has joined the cache key and "+
			"each facet now re-walks the library", entries)
	}
}

// TestEnrichmentMissesNarrowsFromOneSnapshot pins the other half: one
// walk must still produce correctly narrowed per-facet views.
func TestEnrichmentMissesNarrowsFromOneSnapshot(t *testing.T) {
	lister := &fakeMissLister{refs: missFixture()}
	s := &Server{}
	view := func(facet string) enrichmentMissesResponse {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/enrichment/misses?facet="+facet, nil)
		resp, err := s.libMetaMisses.fetch(r, "", func(ctx context.Context) (enrichmentMissesResponse, error) {
			return computeEnrichmentMisses(ctx, lister, "")
		})
		if err != nil {
			t.Fatalf("fetch(%s): %v", facet, err)
		}
		return narrowMisses(resp, facet, enrichmentMissSampleCap)
	}
	for _, facet := range []string{
		manifest.MissFacetArtist, manifest.MissFacetRelease, manifest.MissFacetArtwork,
	} {
		got := view(facet)
		if len(got.Sample) != 1 {
			t.Errorf("%s view carries %d facets, want exactly its own", facet, len(got.Sample))
		}
		if _, ok := got.Sample[facet]; !ok {
			t.Errorf("%s view lost its own facet", facet)
		}
	}
	if got := lister.walks.Load(); got != 1 {
		t.Fatalf("walked %d times for three views, want 1", got)
	}
}

func TestApiEnrichmentMissesRejectsBadFacetAndLimit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct {
		name, query string
		wantStatus  int
	}{
		{"bad facet", "?facet=bogus", http.StatusBadRequest},
		{"zero limit", "?limit=0", http.StatusBadRequest},
		{"negative limit", "?limit=-3", http.StatusBadRequest},
		{"non-numeric limit", "?limit=abc", http.StatusBadRequest},
		{"valid facet", "?facet=release", http.StatusOK},
		{"no params", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/enrichment/misses"+tc.query, nil)
			w := httptest.NewRecorder()
			srv.apiEnrichmentMisses(w, r)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if w.Code == http.StatusOK {
				var resp enrichmentMissesResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
			}
		})
	}
}

// TestApiEnrichmentMissesSurfacesSkipReasons pins the wiring of the
// enricher's bounded reason tally through the nil-safe Deps hook.
func TestApiEnrichmentMissesSurfacesSkipReasons(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.EnrichSkipReasons = func() map[string]int64 {
		return map[string]int64{"no_mb_match": 7, "no_search_terms": 2}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/enrichment/misses", nil)
	w := httptest.NewRecorder()
	srv.apiEnrichmentMisses(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp enrichmentMissesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SkipReasons["no_mb_match"] != 7 {
		t.Errorf("skipReasons = %v, want no_mb_match=7", resp.SkipReasons)
	}
}

// TestApiEnrichmentMissesNilHookOmitsSkipReasons — the CLI path and any
// deployment without a running enricher must not see a bogus empty map.
func TestApiEnrichmentMissesNilHookOmitsSkipReasons(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.EnrichSkipReasons = nil
	r := httptest.NewRequest(http.MethodGet, "/api/enrichment/misses", nil)
	w := httptest.NewRecorder()
	srv.apiEnrichmentMisses(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["skipReasons"]; present {
		t.Error("skipReasons must be omitted when no enricher is wired")
	}
}
