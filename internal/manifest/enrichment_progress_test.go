package manifest

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// TestStoreEnrichmentProgressEmpty pins the zero-state contract:
// no tracks → all counters zero, lastEnrichedAt is the zero time.
// iOS gates its "enrichment in progress" UI on
// `tracksEnriched < tracksTotal`, so a fresh store reporting
// `0 < 0 == false` correctly suppresses the footer.
func TestStoreEnrichmentProgressEmpty(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	total, enriched, last, err := s.EnrichmentProgress()
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || enriched != 0 || last != nil {
		t.Errorf("empty store: got total=%d enriched=%d last=%v, want all zero/nil",
			total, enriched, last)
	}
}

// TestStoreEnrichmentProgressMixed exercises the common case: some
// tracks enriched, some still queued. Counters must report the
// split correctly so iOS can drive its progress UI off the values.
func TestStoreEnrichmentProgressMixed(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 10; i++ {
		s.UpsertTrack(&Track{
			Path: filepath.Join("Music", "x", "t.flac") + string(rune('0'+i)),
			Size: 1, ModTime: now,
		})
	}
	// MarkEnriched 3 of them. The mark stamps `enriched_at` to now.
	all, _ := s.ListTracks(nil)
	for i := 0; i < 3; i++ {
		t2 := all[i]
		if err := s.MarkEnriched(&t2); err != nil {
			t.Fatal(err)
		}
	}
	total, enriched, last, err := s.EnrichmentProgress()
	if err != nil {
		t.Fatal(err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if enriched != 3 {
		t.Errorf("enriched = %d, want 3", enriched)
	}
	if last == nil {
		t.Errorf("lastEnrichedAt should be set after MarkEnriched calls, got nil")
	}
}

// TestListTracksPopulatesEnriched locks the column-spliced
// `Track.Enriched` flag for the non-paginated path. Pre-MarkEnriched
// rows surface as `*false`; post-MarkEnriched rows as `*true`.
// Pointer type is the disambiguation: nil means "field absent on the
// wire" (older bridges); non-nil with value gives the explicit answer.
func TestListTracksPopulatesEnriched(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	s.UpsertTrack(&Track{Path: "a.flac", Size: 1, ModTime: now})
	s.UpsertTrack(&Track{Path: "b.flac", Size: 1, ModTime: now})

	pre, _ := s.ListTracks(nil)
	for _, tr := range pre {
		if tr.Enriched == nil {
			t.Fatalf("Enriched should never be nil on read from this server, got nil for %q", tr.Path)
		}
		if *tr.Enriched {
			t.Errorf("pre-MarkEnriched %q: Enriched = true, want false", tr.Path)
		}
	}

	bRow := pre[1]
	if err := s.MarkEnriched(&bRow); err != nil {
		t.Fatal(err)
	}
	post, _ := s.ListTracks(nil)
	for _, tr := range post {
		if tr.Path == "b.flac" {
			if tr.Enriched == nil || !*tr.Enriched {
				t.Errorf("b.flac should be enriched after MarkEnriched, got %v", tr.Enriched)
			}
		} else {
			if tr.Enriched == nil || *tr.Enriched {
				t.Errorf("a.flac should still NOT be enriched, got %v", tr.Enriched)
			}
		}
	}
}

// TestListTracksPagePopulatesEnriched is the paginated mirror. Same
// contract — every row carries an explicit Enriched pointer regardless
// of the page index.
func TestListTracksPagePopulatesEnriched(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		s.UpsertTrack(&Track{
			Path: filepath.Join("Music", string(rune('a'+i))+".flac"),
			Size: 1, ModTime: now,
		})
	}
	page, _ := s.ListTracksPage("", 100)
	for _, tr := range page {
		if tr.Enriched == nil {
			t.Errorf("page row %q should have non-nil Enriched", tr.Path)
		}
	}
}

// TestBuildManifestSetsEnrichmentProgress checks that the
// non-paginated full-manifest path includes the EnrichmentProgress
// block on every response — counters reflect the store snapshot at
// build time.
func TestBuildManifestSetsEnrichmentProgress(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 4; i++ {
		s.UpsertTrack(&Track{Path: string(rune('a'+i)) + ".flac", Size: 1, ModTime: now})
	}
	first, _ := s.ListTracks(nil)
	s.MarkEnriched(&first[0])
	s.MarkEnriched(&first[1])

	m, err := BuildManifest(s, []string{"/tmp/nope/Music"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if m.EnrichmentProgress == nil {
		t.Fatal("EnrichmentProgress should be non-nil on full-manifest response")
	}
	if m.EnrichmentProgress.TracksTotal != 4 {
		t.Errorf("TracksTotal = %d, want 4", m.EnrichmentProgress.TracksTotal)
	}
	if m.EnrichmentProgress.TracksEnriched != 2 {
		t.Errorf("TracksEnriched = %d, want 2", m.EnrichmentProgress.TracksEnriched)
	}
}

// TestBuildManifestPageEnrichmentProgressOnFirstPageOnly mirrors the
// existing folders/total contract: progress lands on the first page
// (cursor=="") and is absent on subsequent pages, so a 50-page
// pagination run pays the EnrichmentProgress aggregate query exactly
// once instead of 50 times.
func TestBuildManifestPageEnrichmentProgressOnFirstPageOnly(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 1; i <= 5; i++ {
		s.UpsertTrack(&Track{
			Path: "Music/" + string(rune('0'+i)) + ".flac",
			Size: int64(i), ModTime: now,
		})
	}

	first, err := BuildManifestPage(s, []string{"/tmp/nope/Music"}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.EnrichmentProgress == nil {
		t.Fatal("first page must carry EnrichmentProgress")
	}
	if first.EnrichmentProgress.TracksTotal != 5 {
		t.Errorf("first page TracksTotal = %d, want 5", first.EnrichmentProgress.TracksTotal)
	}

	mid, err := BuildManifestPage(s, []string{"/tmp/nope/Music"}, "Music/2.flac", 2)
	if err != nil {
		t.Fatal(err)
	}
	if mid.EnrichmentProgress != nil {
		t.Errorf("mid-run page must NOT carry EnrichmentProgress, got %+v", mid.EnrichmentProgress)
	}
}

// TestManifestJSONShape locks the on-the-wire JSON contract for the
// new fields (omitempty, snake/camel case, pointer-vs-value semantics)
// so the iOS decoder doesn't get a surprise rename. iOS reads
// `enrichmentProgress` (camelCase) and `enriched` (lowercase per-track).
func TestManifestJSONShape(t *testing.T) {
	enriched := true
	tr := Track{
		Path:     "a.flac",
		Size:     1,
		ModTime:  time.Now().UTC(),
		Enriched: &enriched,
	}
	raw, _ := json.Marshal(tr)
	var got map[string]any
	json.Unmarshal(raw, &got)
	v, ok := got["enriched"]
	if !ok || v != true {
		t.Errorf("track JSON missing 'enriched' field or wrong value, got %v", got)
	}

	// Marshal a Manifest with EnrichmentProgress populated.
	last := time.Date(2026, 4, 23, 11, 41, 1, 0, time.UTC)
	m := Manifest{
		Version: 1,
		EnrichmentProgress: &EnrichmentProgress{
			TracksTotal:    100,
			TracksEnriched: 75,
			LastEnrichedAt: &last,
		},
	}
	raw2, _ := json.Marshal(m)
	var gotM map[string]any
	json.Unmarshal(raw2, &gotM)
	ep, ok := gotM["enrichmentProgress"].(map[string]any)
	if !ok {
		t.Fatalf("manifest missing 'enrichmentProgress' object, got %v", gotM)
	}
	if ep["tracksTotal"].(float64) != 100 {
		t.Errorf("tracksTotal = %v, want 100", ep["tracksTotal"])
	}
	if ep["tracksEnriched"].(float64) != 75 {
		t.Errorf("tracksEnriched = %v, want 75", ep["tracksEnriched"])
	}
	if _, ok := ep["lastEnrichedAt"]; !ok {
		t.Errorf("lastEnrichedAt missing")
	}
}

// TestManifestOmitsEnrichmentProgressWhenNil locks the back-compat
// shape: `omitempty` actually omits the field when no
// EnrichmentProgress is set, so older iOS decoders that don't know
// about the key see a manifest indistinguishable from a v1.0 one.
func TestManifestOmitsEnrichmentProgressWhenNil(t *testing.T) {
	m := Manifest{Version: 1}
	raw, _ := json.Marshal(m)
	var got map[string]any
	json.Unmarshal(raw, &got)
	if _, ok := got["enrichmentProgress"]; ok {
		t.Errorf("nil EnrichmentProgress should be omitted from JSON, got %v", got)
	}
}

// TestEnrichmentProgressOmitsLastEnrichedAtWhenNeverEnriched is the
// regression test for the bug Gemini caught on PR review: a non-pointer
// `LastEnrichedAt time.Time` slips past `omitempty` because Go's
// `encoding/json` doesn't treat a zero `time.Time` as "empty", so the
// wire shape would emit `"0001-01-01T00:00:00Z"` and the iOS decoder
// would parse that as a real, very-old date — breaking both the
// "never enriched" sentinel AND the 24 h freshness gate the iOS UI
// uses to decide whether to show the "Enrichment in progress" footer.
//
// The pointer change in `EnrichmentProgress.LastEnrichedAt` lets
// `omitempty` correctly drop the field. This test pins the wire-shape
// contract so a future "let's simplify away the pointer" refactor
// reintroduces the bug visibly instead of silently.
func TestEnrichmentProgressOmitsLastEnrichedAtWhenNeverEnriched(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	// A track that's been upserted but NOT enriched — the realistic
	// "fresh-pair, scan ran but enricher hasn't finished a single
	// row yet" state.
	s.UpsertTrack(&Track{Path: "a.flac", Size: 1, ModTime: now})

	m, err := BuildManifest(s, []string{"/tmp/nope/Music"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	ep, ok := got["enrichmentProgress"].(map[string]any)
	if !ok {
		t.Fatalf("manifest missing enrichmentProgress: %v", got)
	}
	if _, present := ep["lastEnrichedAt"]; present {
		t.Errorf("lastEnrichedAt must be ABSENT when no track has ever been enriched, got %v",
			ep["lastEnrichedAt"])
	}
}
