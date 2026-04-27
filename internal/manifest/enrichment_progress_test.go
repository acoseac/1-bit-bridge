package manifest

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore is a small helper that calls `OpenStore` with a
// temp-dir-backed path AND properly fails the test on open error.
// Without this, a `s, _ := OpenStore(...)` followed by the usual
// `defer s.Close()` would panic on a nil receiver if open ever failed
// — masking the real migration / open error with a confusing
// nil-pointer panic. CodeRabbit flagged the broader pattern on PR
// #68; routing every test through this helper closes the gap once.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStoreEnrichmentCountsEmpty pins the zero-state contract:
// no tracks → enriched is zero, lastEnrichedAt is nil. iOS gates its
// "enrichment in progress" UI on `tracksEnriched < tracksTotal`,
// so a fresh store reporting `0 < 0 == false` correctly suppresses
// the footer. Total is sourced separately from `CountTracks` per
// Qodo's review (avoids divergence with the manifest's top-level
// `total`).
func TestStoreEnrichmentCountsEmpty(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	enriched, last, err := s.EnrichmentCounts()
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 0 || last != nil {
		t.Errorf("empty store: got enriched=%d last=%v, want zero/nil",
			enriched, last)
	}
}

// TestStoreEnrichmentCountsMixed exercises the common case: some
// tracks enriched, some still queued. Counters must report the
// split correctly so iOS can drive its progress UI off the values.
func TestStoreEnrichmentCountsMixed(t *testing.T) {
	s := openTestStore(t)
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
	enriched, last, err := s.EnrichmentCounts()
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 3 {
		t.Errorf("enriched = %d, want 3", enriched)
	}
	if last == nil {
		t.Errorf("lastEnrichedAt should be set after MarkEnriched calls, got nil")
	}
}

// TestEnrichmentProgressTotalMatchesManifestTotal is the regression
// test for Qodo's #2 concern: `manifest.total` and
// `enrichmentProgress.tracksTotal` MUST be the same value in a single
// paginated response. Previously a separate `COUNT(*)` query inside
// the old `EnrichmentProgress()` could disagree with the
// `CountTracks()` call in the manifest builder under concurrent
// writes. Refactoring `EnrichmentCounts()` to NOT return total + having
// the builder feed both fields from one count call closes that gap.
func TestEnrichmentProgressTotalMatchesManifestTotal(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 7; i++ {
		s.UpsertTrack(&Track{
			Path: "Music/" + string(rune('0'+i)) + ".flac",
			Size: 1, ModTime: now,
		})
	}
	m, err := BuildManifestPage(s, []string{"/tmp/nope/Music"}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total == nil {
		t.Fatal("manifest.total must be set on first page")
	}
	if m.EnrichmentProgress == nil {
		t.Fatal("EnrichmentProgress must be set on first page")
	}
	if *m.Total != m.EnrichmentProgress.TracksTotal {
		t.Errorf("manifest.total (%d) and EnrichmentProgress.TracksTotal (%d) must agree",
			*m.Total, m.EnrichmentProgress.TracksTotal)
	}
}

// TestListTracksPopulatesEnriched locks the column-spliced
// `Track.Enriched` flag for the non-paginated path. Pre-MarkEnriched
// rows surface as `*false`; post-MarkEnriched rows as `*true`.
// Pointer type is the disambiguation: nil means "field absent on the
// wire" (older bridges); non-nil with value gives the explicit answer.
func TestListTracksPopulatesEnriched(t *testing.T) {
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
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
	raw2, err2 := json.Marshal(m)
	if err2 != nil {
		t.Fatal(err2)
	}
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
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["enrichmentProgress"]; ok {
		t.Errorf("nil EnrichmentProgress should be omitted from JSON, got %v", got)
	}
}

// TestUpsertTrackDoesNotPersistEnrichedField is the regression test
// for CodeRabbit's column-only-invariant concern (PR #68 follow-up).
// `Track.Enriched` is column-derived at READ time (`ListTracks` /
// `ListTracksPage` splice it from `enriched_at`); it MUST NOT be
// persisted into `tags_json` because the JSON-only readers
// (`GetTrack` / `UnenrichedTracks`) don't overwrite it from the column,
// so a stale value would silently drift from the column truth.
//
// Concretely: feed a Track with `Enriched: &true` through `UpsertTrack`
// (which resets `enriched_at = 0`), then `GetTrack` reads back ONLY the
// JSON. The column says "not enriched"; the JSON should NOT carry an
// `enriched: true` that contradicts it. `marshalForStorage` strips the
// field defensively so this stays true regardless of caller hygiene.
func TestUpsertTrackDoesNotPersistEnrichedField(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)

	// Construct a Track that already has Enriched=true (the realistic
	// "caller fed a row from ListTracks back into UpsertTrack" path).
	yes := true
	tr := &Track{Path: "x.flac", Size: 1, ModTime: now, Enriched: &yes}
	if err := s.UpsertTrack(tr); err != nil {
		t.Fatal(err)
	}

	// GetTrack reads only `tags_json`. If the JSON contains
	// `"enriched": true`, the returned Track will surface
	// `Enriched != nil` — that's the bug.
	got, err := s.GetTrack("x.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v / %v", err, got)
	}
	if got.Enriched != nil {
		t.Errorf("UpsertTrack must not persist Enriched into tags_json, got Enriched=%v", *got.Enriched)
	}
}

// TestMarkEnrichedDoesNotPersistEnrichedField mirrors the above for the
// `MarkEnriched` write path. Same column-only-invariant rationale.
func TestMarkEnrichedDoesNotPersistEnrichedField(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.UpsertTrack(&Track{Path: "y.flac", Size: 1, ModTime: now}); err != nil {
		t.Fatal(err)
	}

	yes := true
	tr := &Track{Path: "y.flac", Size: 1, ModTime: now, Enriched: &yes}
	if err := s.MarkEnriched(tr); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTrack("y.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v / %v", err, got)
	}
	if got.Enriched != nil {
		t.Errorf("MarkEnriched must not persist Enriched into tags_json, got Enriched=%v", *got.Enriched)
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
	s := openTestStore(t)
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
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
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
