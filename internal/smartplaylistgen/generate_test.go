package smartplaylistgen

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
)

func ip(v int) *int         { return &v }
func fp(v float64) *float64 { return &v }

func openGenStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func track(t *testing.T, s *manifest.Store, path, genre string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path: path, Size: 1, ModTime: time.Unix(1, 0),
		Title: "T" + path, Artist: "Art", Genre: genre, Duration: fp(200),
	}); err != nil {
		t.Fatalf("UpsertTrack(%s): %v", path, err)
	}
}

func analysis(t *testing.T, s *manifest.Store, path string, keyRoot int, mode string, bpm int) {
	t.Helper()
	if err := s.UpsertAnalysis(context.Background(), manifest.AnalysisRow{
		SourcePath: path, CreatedAt: 1, SourceMTimeNS: 1, SourceSize: 1,
		KeyRoot: ip(keyRoot), KeyMode: mode, BPM: ip(bpm),
	}); err != nil {
		t.Fatalf("UpsertAnalysis(%s): %v", path, err)
	}
}

// seedDailyPlays inserts one listening session per day at UTC hour 8, d days
// before `now` for each d in daysAgo, every session playing each path in
// order (5-minute spacing, 200 s per play) on the given interface.
func seedDailyPlays(t *testing.T, s *manifest.Store, now time.Time, iface string, daysAgo []int, paths ...string) {
	t.Helper()
	var hist []manifest.PlaybackHistoryRow
	for _, d := range daysAgo {
		base := time.Date(now.Year(), now.Month(), now.Day()-d, 8, 0, 0, 0, time.UTC)
		for i, p := range paths {
			hist = append(hist, manifest.PlaybackHistoryRow{
				DeviceToken: "d", Path: p,
				StartedAt:    base.Add(time.Duration(i*5) * time.Minute).UnixNano(),
				DurationUsed: 200, IfaceType: iface,
			})
		}
	}
	if err := s.InsertHistoryBatch(context.Background(), hist); err != nil {
		t.Fatalf("InsertHistoryBatch: %v", err)
	}
}

// regenTestOptions returns DefaultOptions with the listening-family minimums
// lowered so a handful of seeded plays populates them (the production floors
// — MinHeavyRotation 10, MinSessions 20, … — would hide every family in a
// 3-track fixture). Analysis-gated + new-family floors stay at production
// values; tests override those explicitly when they exercise them.
func regenTestOptions(nowNS int64, analysisOn bool) Options {
	opts := DefaultOptions(nowNS, analysisOn)
	opts.Engine.MaxItems = 10
	opts.Engine.MinHeavyRotation = 2
	opts.Engine.MinRecentlyPlayed = 2
	opts.Engine.MinForgotten = 2
	opts.Engine.MinAutoMixPool = 3
	opts.Engine.MinTimeOfDayPlays = 2
	opts.Engine.MinDailyFamiliar = 2
	opts.Engine.MinSessions = 2
	return opts
}

// loadKinds returns the number of cached families and the set of their kinds.
func loadKinds(t *testing.T, s *manifest.Store) (int, map[string]bool) {
	t.Helper()
	rows, err := s.LoadSmartPlaylists(context.Background())
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	kinds := map[string]bool{}
	for _, r := range rows {
		kinds[r.Kind] = true
	}
	return len(rows), kinds
}

// TestRegenerate_PipelineEndToEnd seeds a store via the public API, runs the
// full regeneration, and verifies the cache contents + blob shapes.
func TestRegenerate_PipelineEndToEnd(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()

	// played + analyzed favourites (jazz)
	for _, p := range []string{"/a.flac", "/b.flac", "/c.flac"} {
		track(t, s, p, "Jazz")
	}
	analysis(t, s, "/a.flac", 0, "major", 120)
	analysis(t, s, "/b.flac", 7, "major", 122)
	analysis(t, s, "/c.flac", 9, "minor", 121)
	// unplayed analyzed discovery candidates
	track(t, s, "/f.flac", "Jazz")
	track(t, s, "/g.flac", "Jazz")
	analysis(t, s, "/f.flac", 2, "major", 119)
	analysis(t, s, "/g.flac", 4, "minor", 123)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowNS := now.UnixNano()
	// 3 listening sessions (one per day) at UTC hour 8, each playing a,b,c.
	seedDailyPlays(t, s, now, "CarPlay", []int{3, 2, 1}, "/a.flac", "/b.flac", "/c.flac")

	opts := DefaultOptions(nowNS, true)
	opts.ForgottenMinPlays = 2
	opts.Engine = smartplaylist.Options{
		AnalysisEnabled: true, MaxItems: 10,
		MinHeavyRotation: 2, MinRecentlyPlayed: 2, MinForgotten: 2,
		MinAutoMixPool: 3, MinTimeOfDayPlays: 2, MinDailyFamiliar: 2, MinSessions: 2,
		SessionGapSeconds: 3600, DailyDiscoveryRatio: 0.30,
	}

	n, err := Regenerate(ctx, s, opts)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if n == 0 {
		t.Fatal("expected populated families, got 0")
	}

	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("LoadSmartPlaylists returned %d, Regenerate wrote %d", len(rows), n)
	}

	byKind := map[string]manifest.StoredSmartPlaylist{}
	for i, r := range rows {
		if r.Position != i {
			t.Errorf("row %d has Position %d (not dense/ascending)", i, r.Position)
		}
		if r.RefreshedAt != nowNS {
			t.Errorf("%s RefreshedAt = %d want %d", r.Kind, r.RefreshedAt, nowNS)
		}
		byKind[r.Kind] = r
	}

	// Heavy Rotation present, flat blob decodes to items.
	hr, ok := byKind["heavyRotation"]
	if !ok {
		t.Fatalf("heavyRotation missing; got kinds %v", keysOf(byKind))
	}
	var items []manifest.SmartPlaylistItem
	if err := json.Unmarshal(hr.ItemsJSON, &items); err != nil {
		t.Fatalf("heavyRotation blob decode: %v", err)
	}
	if len(items) < 2 {
		t.Errorf("heavyRotation items = %d want >= 2", len(items))
	}

	// Time of Day present, hour-keyed blob decodes with hour 8 populated.
	tod, ok := byKind["timeOfDay"]
	if !ok {
		t.Fatalf("timeOfDay missing; got kinds %v", keysOf(byKind))
	}
	var blob manifest.SmartPlaylistHourlyBlob
	if err := json.Unmarshal(tod.ItemsJSON, &blob); err != nil {
		t.Fatalf("timeOfDay blob decode: %v", err)
	}
	if len(blob.Hourly[8]) == 0 {
		t.Errorf("timeOfDay hour-8 pool empty: %+v", blob.Hourly)
	}

	// Auto Mix present (analysis on, pool >= 3).
	if _, ok := byKind["autoMix"]; !ok {
		t.Errorf("autoMix missing; got kinds %v", keysOf(byKind))
	}

	// Re-running over unchanged data is stable (same count, wholesale replace).
	n2, err := Regenerate(ctx, s, opts)
	if err != nil || n2 != n {
		t.Fatalf("re-run: n2=%d err=%v (want %d, nil)", n2, err, n)
	}
}

func TestToStored_BlobShapes(t *testing.T) {
	gen := []smartplaylist.GeneratedPlaylist{
		{Slug: "heavy-rotation", Kind: smartplaylist.KindHeavyRotation, Title: "Heavy Rotation",
			Items:  []smartplaylist.Item{{Position: 0, Path: "/a.flac", Title: "A", Artist: "X"}},
			Energy: []float64{0.1, 0.9}, ModalRateHz: 96000},
		{Slug: "time-of-day", Kind: smartplaylist.KindTimeOfDay, Title: "For Right Now",
			HourlyItems: map[int][]smartplaylist.Item{8: {{Position: 0, Path: "/b.flac"}}}},
	}
	rows, err := toStored(gen, 1234)
	if err != nil {
		t.Fatalf("toStored: %v", err)
	}
	if len(rows) != 2 || rows[0].Position != 0 || rows[1].Position != 1 {
		t.Fatalf("positions wrong: %+v", rows)
	}

	var flat []manifest.SmartPlaylistItem
	if err := json.Unmarshal(rows[0].ItemsJSON, &flat); err != nil || len(flat) != 1 || flat[0].Path != "/a.flac" {
		t.Fatalf("flat blob: %v / %+v", err, flat)
	}

	// Energy serializes to energy_json; modal rate copies through.
	var energy []float64
	if err := json.Unmarshal(rows[0].EnergyJSON, &energy); err != nil || len(energy) != 2 || energy[1] != 0.9 {
		t.Fatalf("energy blob: %v / %+v", err, energy)
	}
	if rows[0].ModalRateHz != 96000 {
		t.Errorf("modal rate not copied: %d", rows[0].ModalRateHz)
	}
	// A family with no Energy leaves energy_json nil (iOS seeded fallback).
	if rows[1].EnergyJSON != nil {
		t.Errorf("absent energy should serialize to nil, got %s", rows[1].EnergyJSON)
	}

	var hb manifest.SmartPlaylistHourlyBlob
	if err := json.Unmarshal(rows[1].ItemsJSON, &hb); err != nil || len(hb.Hourly[8]) != 1 {
		t.Fatalf("hourly blob: %v / %+v", err, hb)
	}
}

func keysOf(m map[string]manifest.StoredSmartPlaylist) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// analysisRG is analysis() plus an explicit ReplayGain value, for the
// mood-band pools that filter on loudness.
func analysisRG(t *testing.T, s *manifest.Store, path string, bpm int, rgDB float64) {
	t.Helper()
	if err := s.UpsertAnalysis(context.Background(), manifest.AnalysisRow{
		SourcePath: path, CreatedAt: 1, SourceMTimeNS: 1, SourceSize: 1,
		KeyRoot: ip(0), KeyMode: "major", BPM: ip(bpm),
		ReplayGainTrackDB: fp(rgDB),
	}); err != nil {
		t.Fatalf("UpsertAnalysis(%s): %v", path, err)
	}
}

// TestRegenerate_EmptyLibraryClearsCache covers the no-eligible-tracks
// gating path: every candidate query returns empty, no family builder
// populates, and Regenerate commits an EMPTY snapshot (n == 0) — a stale
// cache row must be cleared, never left behind for iOS to render.
func TestRegenerate_EmptyLibraryClearsCache(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()

	// Seed a stale cache row directly so the test proves the empty
	// snapshot replaces (clears) rather than skips the write.
	stale := []manifest.StoredSmartPlaylist{{
		Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation",
		RefreshedAt: 1, ItemsJSON: []byte(`[]`),
	}}
	if err := s.ReplaceSmartPlaylists(ctx, stale); err != nil {
		t.Fatalf("seed stale cache: %v", err)
	}

	n, err := Regenerate(ctx, s, DefaultOptions(time.Now().UnixNano(), true))
	if err != nil {
		t.Fatalf("Regenerate on empty library: %v", err)
	}
	if n != 0 {
		t.Errorf("Regenerate wrote %d families on an empty library, want 0", n)
	}
	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("stale cache rows survived empty regen: %+v", rows)
	}
}

// TestRegenerate_AnalysisDisabledSkipsMoodBands pins the AnalysisEnabled
// gate in assembleInputs: with analysis off the mood-band / analyzed-pool
// queries must not run, so the analysis-gated families (Wind Down, Lift
// Off, Auto Mix) stay hidden EVEN THOUGH qualifying track_analysis rows
// exist — while the listening families keep populating.
func TestRegenerate_AnalysisDisabledSkipsMoodBands(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()

	// Two quiet/slow + two loud/fast tracks — with analysis ON both mood
	// bands qualify (MinMoodBand 2); the OFF run must hide them.
	for _, p := range []string{"/q1.flac", "/q2.flac"} {
		track(t, s, p, "Ambient")
	}
	for _, p := range []string{"/l1.flac", "/l2.flac"} {
		track(t, s, p, "Techno")
	}
	analysisRG(t, s, "/q1.flac", 80, -5.0)
	analysisRG(t, s, "/q2.flac", 82, -4.5)
	analysisRG(t, s, "/l1.flac", 130, -10.0)
	analysisRG(t, s, "/l2.flac", 128, -9.0)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowNS := now.UnixNano()
	// A few recent plays per track so the listening families populate in
	// both runs.
	seedDailyPlays(t, s, now, "", []int{3, 2, 1}, "/q1.flac", "/q2.flac", "/l1.flac", "/l2.flac")

	regenOpts := func(analysisOn bool) Options {
		opts := regenTestOptions(nowNS, analysisOn)
		opts.Engine.MinMoodBand = 2
		return opts
	}

	// Control: analysis ON — both mood bands populate from the seeded rows.
	if _, err := Regenerate(ctx, s, regenOpts(true)); err != nil {
		t.Fatalf("Regenerate (analysis on): %v", err)
	}
	_, kinds := loadKinds(t, s)
	for _, k := range []string{"windDown", "liftOff"} {
		if !kinds[k] {
			t.Fatalf("control run: %s missing with analysis ON; got %v", k, kinds)
		}
	}

	// Gated: analysis OFF — mood bands + Auto Mix must stay hidden while
	// the listening families keep populating.
	n, err := Regenerate(ctx, s, regenOpts(false))
	if err != nil {
		t.Fatalf("Regenerate (analysis off): %v", err)
	}
	cached, kinds := loadKinds(t, s)
	if cached != n {
		t.Fatalf("LoadSmartPlaylists returned %d, Regenerate wrote %d", cached, n)
	}
	for _, k := range []string{"windDown", "liftOff", "autoMix"} {
		if kinds[k] {
			t.Errorf("%s present with analysis OFF; the AnalysisEnabled gate leaked the pool", k)
		}
	}
	if !kinds["heavyRotation"] {
		t.Errorf("heavyRotation missing with analysis OFF; listening families must keep populating; got %v", kinds)
	}
}

// TestRegenerate_StoreFailureLeavesPriorCacheIntact covers the error path:
// a failing store surfaces the error (n == 0) BEFORE any cache write, so
// the last good snapshot survives — never a partial or empty commit.
func TestRegenerate_StoreFailureLeavesPriorCacheIntact(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Safety net for the Fatalf paths before the intentional Close below;
	// a second Close on an already-closed store just returns an error.
	t.Cleanup(func() { _ = s.Close() })

	for _, p := range []string{"/a.flac", "/b.flac", "/c.flac"} {
		track(t, s, p, "Jazz")
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	seedDailyPlays(t, s, now, "", []int{3, 2, 1}, "/a.flac", "/b.flac", "/c.flac")
	opts := regenTestOptions(now.UnixNano(), false)
	n1, err := Regenerate(ctx, s, opts)
	if err != nil || n1 == 0 {
		t.Fatalf("seed Regenerate: n=%d err=%v", n1, err)
	}
	before, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}

	// Kill the store underneath the regenerator: every query fails.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	n, err := Regenerate(ctx, s, opts)
	if err == nil {
		t.Fatal("Regenerate against a closed store: got nil error, want failure")
	}
	if n != 0 {
		t.Errorf("failed Regenerate reported %d written families, want 0", n)
	}

	// The pre-failure snapshot must be intact on reopen.
	s2, err := manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	after, err := s2.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists after reopen: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("cache rows changed across failed regen: before %d after %d", len(before), len(after))
	}
	for i := range before {
		if after[i].Slug != before[i].Slug || after[i].RefreshedAt != before[i].RefreshedAt {
			t.Errorf("row %d changed across failed regen: %+v -> %+v", i, before[i], after[i])
		}
	}
}

// TestRegenerate_HeavyRotationFloorDegrades pins the floor-degradation
// loop in assembleInputs: when no track reaches the configured play-count
// floor (3), the query must retry at 2 then 1 so a quiet week still
// populates Heavy Rotation instead of hiding the family.
func TestRegenerate_HeavyRotationFloorDegrades(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()

	// One play per track — below the default floor of 3, so only the
	// degrade-to-1 retry can find them.
	for _, p := range []string{"/a.flac", "/b.flac", "/c.flac"} {
		track(t, s, p, "Jazz")
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowNS := now.UnixNano()
	seedDailyPlays(t, s, now, "", []int{1}, "/a.flac", "/b.flac", "/c.flac")

	// Production engine defaults with ONLY the heavy-rotation minimum
	// lowered: every other family's production floor (MinRecentlyPlayed 5,
	// MinSessions 20, MinDriveMix 10, …) is out of reach of a 3-track
	// single-play fixture, so heavyRotation is the only populated family
	// and the count pin isolates the degraded-floor path.
	opts := DefaultOptions(nowNS, false) // HeavyRotationMinPlays: 3
	opts.Engine.MinHeavyRotation = 2

	n, err := Regenerate(ctx, s, opts)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if n != 1 || len(rows) != 1 || rows[0].Kind != "heavyRotation" {
		t.Fatalf("n=%d rows=%+v — want exactly one heavyRotation family (floor degraded 3→1)", n, rows)
	}
	var items []manifest.SmartPlaylistItem
	if err := json.Unmarshal(rows[0].ItemsJSON, &items); err != nil {
		t.Fatalf("heavyRotation blob decode: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("heavyRotation items = %d, want 3 (single plays surfaced by floor 1)", len(items))
	}
}
