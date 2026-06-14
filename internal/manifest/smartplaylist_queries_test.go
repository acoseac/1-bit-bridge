package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// --- local helpers (uniquely named to avoid collisions with sibling tests) ---

func newSPStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func spFloat(v float64) *float64 { return &v }
func spInt(v int) *int           { return &v }

func mustUpsertTrack(t *testing.T, s *Store, tr *Track) {
	t.Helper()
	if tr.Size == 0 {
		tr.Size = 1
	}
	if tr.ModTime.IsZero() {
		tr.ModTime = time.Unix(1, 0)
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack(%s): %v", tr.Path, err)
	}
}

func mustUpsertAnalysis(t *testing.T, s *Store, a AnalysisRow) {
	t.Helper()
	a.CreatedAt = 1
	a.SourceMTimeNS = 1
	a.SourceSize = 1
	if err := s.UpsertAnalysis(context.Background(), a); err != nil {
		t.Fatalf("UpsertAnalysis(%s): %v", a.SourcePath, err)
	}
}

func mustInsertHistory(t *testing.T, s *Store, rows ...PlaybackHistoryRow) {
	t.Helper()
	for i := range rows {
		if rows[i].DeviceToken == "" {
			rows[i].DeviceToken = "dev1"
		}
	}
	if err := s.InsertHistoryBatch(context.Background(), rows); err != nil {
		t.Fatalf("InsertHistoryBatch: %v", err)
	}
}

func utcNS(y int, mo time.Month, d, h, mi int) int64 {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC).UnixNano()
}

// --- cache round-trip ---

func TestSmartPlaylistCacheRoundTrip(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	if err := s.ReplaceSmartPlaylists(ctx, []StoredSmartPlaylist{
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Subtitle: "most played", Position: 0, RefreshedAt: 100, ItemsJSON: []byte(`[{"position":0,"path":"/a.flac"}]`)},
		{Slug: "auto-mix", Kind: "autoMix", Title: "Auto Mix", Position: 1, RefreshedAt: 100, ItemsJSON: []byte(`[]`)},
	}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists: %v", err)
	}

	got, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].Slug != "heavy-rotation" || got[1].Slug != "auto-mix" {
		t.Errorf("position ordering wrong: %s, %s", got[0].Slug, got[1].Slug)
	}
	if string(got[0].ItemsJSON) != `[{"position":0,"path":"/a.flac"}]` {
		t.Errorf("items_json round-trip mismatch: %s", got[0].ItemsJSON)
	}

	// Wholesale replace drops stale families.
	if err := s.ReplaceSmartPlaylists(ctx, []StoredSmartPlaylist{
		{Slug: "recently-played", Kind: "recentlyPlayed", Title: "Recently Played", Position: 0, RefreshedAt: 200, ItemsJSON: []byte(`[]`)},
	}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists (replace): %v", err)
	}
	got, _ = s.LoadSmartPlaylists(ctx)
	if len(got) != 1 || got[0].Slug != "recently-played" {
		t.Fatalf("wholesale replace failed: %+v", got)
	}

	// Empty snapshot clears the cache.
	if err := s.ReplaceSmartPlaylists(ctx, nil); err != nil {
		t.Fatalf("ReplaceSmartPlaylists (clear): %v", err)
	}
	got, _ = s.LoadSmartPlaylists(ctx)
	if len(got) != 0 {
		t.Fatalf("empty replace should clear cache, got %d rows", len(got))
	}

	// Empty slug is rejected.
	if err := s.ReplaceSmartPlaylists(ctx, []StoredSmartPlaylist{{Slug: ""}}); err == nil {
		t.Errorf("expected error on empty slug")
	}
}

// --- 30s rule + windowing ---

func TestPlayStats_30sRuleAndWindow(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/a.flac", Title: "A"})
	mustUpsertTrack(t, s, &Track{Path: "/b.flac", Title: "B"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	mustInsertHistory(t, s,
		// /a.flac: 3 qualifying plays in the last 5 days + 1 skip (5s, excluded)
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 1*day, DurationUsed: 120},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 2*day, DurationUsed: 120},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 3*day, DurationUsed: 120},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 1*day, DurationUsed: 5}, // skip, excluded
		// /b.flac: 1 qualifying play, 20 days ago (outside a 14d window)
		PlaybackHistoryRow{Path: "/b.flac", StartedAt: now - 20*day, DurationUsed: 200},
	)

	// 14-day window: only /a.flac, count 3 (skip excluded).
	rows, err := s.PlayStatsInWindow(ctx, now-14*day, 0, 30.0, 50)
	if err != nil {
		t.Fatalf("PlayStatsInWindow: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "/a.flac" || rows[0].Plays != 3 {
		t.Fatalf("14d window: want [/a.flac x3], got %+v", rows)
	}

	// 30-day window: both, /a.flac first (3 > 1).
	rows, _ = s.PlayStatsInWindow(ctx, now-30*day, 0, 30.0, 50)
	if len(rows) != 2 || rows[0].Path != "/a.flac" || rows[1].Path != "/b.flac" {
		t.Fatalf("30d window ordering: %+v", rows)
	}
}

func TestPlayStatsForgotten(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/old.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/recent.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/oneoff.flac"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	mustInsertHistory(t, s,
		// loved long ago (3 plays, all > 60d ago) → forgotten
		PlaybackHistoryRow{Path: "/old.flac", StartedAt: now - 120*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/old.flac", StartedAt: now - 110*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/old.flac", StartedAt: now - 100*day, DurationUsed: 200},
		// played recently → NOT forgotten
		PlaybackHistoryRow{Path: "/recent.flac", StartedAt: now - 100*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/recent.flac", StartedAt: now - 1*day, DurationUsed: 200},
		// only one historic play → below minPlays
		PlaybackHistoryRow{Path: "/oneoff.flac", StartedAt: now - 100*day, DurationUsed: 200},
	)

	rows, err := s.PlayStatsForgotten(ctx, 30.0, now-30*day, 2, 50)
	if err != nil {
		t.Fatalf("PlayStatsForgotten: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "/old.flac" || rows[0].Plays != 3 {
		t.Fatalf("forgotten: want [/old.flac x3], got %+v", rows)
	}
}

func TestRecentDistinctPlays(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/a.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/b.flac"})

	now := utcNS(2026, 1, 20, 12, 0)
	hr := int64(time.Hour)
	mustInsertHistory(t, s,
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 3*hr, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/b.flac", StartedAt: now - 1*hr, DurationUsed: 200}, // most recent
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: now - 10*hr, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/b.flac", StartedAt: now - 30, DurationUsed: 5}, // skip, excluded
	)

	rows, err := s.RecentDistinctPlays(ctx, 30.0, 50)
	if err != nil {
		t.Fatalf("RecentDistinctPlays: %v", err)
	}
	if len(rows) != 2 || rows[0].Path != "/b.flac" || rows[1].Path != "/a.flac" {
		t.Fatalf("recent ordering: want [/b.flac, /a.flac], got %+v", rows)
	}
}

// --- UTC hour bucketing (the /1e9 unixepoch fix) + iface filter ---

func TestPlayCountsByHourPath(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/a.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/b.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/c.flac"})

	mustInsertHistory(t, s,
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: utcNS(2026, 1, 1, 8, 30), DurationUsed: 60, IfaceType: "CarPlay"},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: utcNS(2026, 1, 2, 8, 45), DurationUsed: 60, IfaceType: "CarPlay"},
		PlaybackHistoryRow{Path: "/b.flac", StartedAt: utcNS(2026, 1, 1, 9, 15), DurationUsed: 60, IfaceType: "BuiltInSpeakers"},
		PlaybackHistoryRow{Path: "/c.flac", StartedAt: utcNS(2026, 1, 1, 8, 10), DurationUsed: 5, IfaceType: "CarPlay"}, // skip
	)

	// All routes: hour 8 → /a.flac x2; hour 9 → /b.flac x1; /c.flac excluded by 30s.
	all, err := s.PlayCountsByHourPath(ctx, 0, 30.0, nil)
	if err != nil {
		t.Fatalf("PlayCountsByHourPath: %v", err)
	}
	got := map[int]map[string]int{}
	for _, r := range all {
		if got[r.Hour] == nil {
			got[r.Hour] = map[string]int{}
		}
		got[r.Hour][r.Path] = r.Plays
	}
	if got[8]["/a.flac"] != 2 {
		t.Errorf("hour 8 /a.flac: want 2, got %d (UTC bucket / 1e9 fix?)", got[8]["/a.flac"])
	}
	if got[9]["/b.flac"] != 1 {
		t.Errorf("hour 9 /b.flac: want 1, got %d", got[9]["/b.flac"])
	}
	if _, ok := got[8]["/c.flac"]; ok {
		t.Errorf("/c.flac (5s skip) should be excluded")
	}

	// CarPlay-only: /b.flac (BuiltInSpeakers) excluded.
	car, _ := s.PlayCountsByHourPath(ctx, 0, 30.0, []string{"CarPlay"})
	for _, r := range car {
		if r.Path == "/b.flac" {
			t.Errorf("CarPlay filter leaked BuiltInSpeakers track: %+v", r)
		}
	}
	if len(car) != 1 || car[0].Hour != 8 || car[0].Path != "/a.flac" {
		t.Fatalf("CarPlay filter: want [{8,/a.flac,2}], got %+v", car)
	}
}

func TestOrderedPlayEvents(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/a.flac"})

	mustInsertHistory(t, s,
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: 300, DurationUsed: 60},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: 100, DurationUsed: 60},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: 200, DurationUsed: 60},
		PlaybackHistoryRow{Path: "/a.flac", StartedAt: 150, DurationUsed: 5}, // skip
	)
	ev, err := s.OrderedPlayEvents(ctx, 0, 30.0, 100)
	if err != nil {
		t.Fatalf("OrderedPlayEvents: %v", err)
	}
	if len(ev) != 3 {
		t.Fatalf("want 3 qualifying events, got %d", len(ev))
	}
	if ev[0].StartedAt != 100 || ev[1].StartedAt != 200 || ev[2].StartedAt != 300 {
		t.Errorf("not chronological: %+v", ev)
	}
}

// --- feature hydration: effective values + tag-wins-over-analysis ---

func TestTrackFeatures_EffectiveValuesAndTagWins(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	// /tagged: curated BPM + RG present; analysis disagrees → tag wins. Key
	// is analysis-only.
	mustUpsertTrack(t, s, &Track{Path: "/tagged.flac", Title: "Tagged", Artist: "Art", Album: "Alb", Genre: "Jazz",
		Duration: spFloat(180.5), BPM: spInt(120), ReplayGainTrackDB: spFloat(-6.0)})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/tagged.flac",
		BPM: spInt(130), ReplayGainTrackDB: spFloat(-9.0), KeyRoot: spInt(9), KeyMode: "minor"})

	// /analysisonly: no curated bpm/rg → analysis fills both.
	mustUpsertTrack(t, s, &Track{Path: "/analysisonly.flac", Title: "AO", Genre: "Rock"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/analysisonly.flac",
		BPM: spInt(100), ReplayGainTrackDB: spFloat(-3.0), KeyRoot: spInt(0), KeyMode: "major"})

	// /noanalysis: metadata only, no analysis row (LEFT JOIN still returns it).
	mustUpsertTrack(t, s, &Track{Path: "/noanalysis.flac", Title: "NA"})

	feats, err := s.TrackFeaturesForPaths(ctx, []string{"/tagged.flac", "/analysisonly.flac", "/noanalysis.flac", "/missing.flac"})
	if err != nil {
		t.Fatalf("TrackFeaturesForPaths: %v", err)
	}
	byPath := map[string]TrackFeatureRow{}
	for _, f := range feats {
		byPath[f.Path] = f
	}
	if len(byPath) != 3 {
		t.Fatalf("want 3 features (missing path dropped), got %d", len(byPath))
	}

	tg := byPath["/tagged.flac"]
	if tg.BPM == nil || *tg.BPM != 120 {
		t.Errorf("tagged BPM: want 120 (tag wins), got %v", tg.BPM)
	}
	if tg.ReplayGainTrackDB == nil || *tg.ReplayGainTrackDB != -6.0 {
		t.Errorf("tagged RG: want -6.0 (tag wins), got %v", tg.ReplayGainTrackDB)
	}
	if tg.KeyRoot == nil || *tg.KeyRoot != 9 || tg.KeyMode != "minor" {
		t.Errorf("tagged key: want 9/minor, got %v/%q", tg.KeyRoot, tg.KeyMode)
	}
	if tg.Duration == nil || *tg.Duration != 180.5 {
		t.Errorf("tagged duration: want 180.5, got %v", tg.Duration)
	}
	if tg.Artist != "Art" || tg.Album != "Alb" || tg.Genre != "Jazz" {
		t.Errorf("tagged metadata mismatch: %+v", tg)
	}

	ao := byPath["/analysisonly.flac"]
	if ao.BPM == nil || *ao.BPM != 100 {
		t.Errorf("analysisonly BPM: want 100 (analysis fallback), got %v", ao.BPM)
	}
	if ao.ReplayGainTrackDB == nil || *ao.ReplayGainTrackDB != -3.0 {
		t.Errorf("analysisonly RG: want -3.0, got %v", ao.ReplayGainTrackDB)
	}

	na := byPath["/noanalysis.flac"]
	if na.KeyRoot != nil || na.BPM != nil || na.ReplayGainTrackDB != nil {
		t.Errorf("noanalysis should have nil scalars, got %+v", na)
	}
}

func TestAnalyzedTrackFeatures_PoolAndGenreFilter(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	mustUpsertTrack(t, s, &Track{Path: "/jazz1.flac", Genre: "Jazz"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/jazz1.flac", KeyRoot: spInt(9), KeyMode: "minor", BPM: spInt(120)})
	mustUpsertTrack(t, s, &Track{Path: "/rock1.flac", Genre: "Rock"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/rock1.flac", KeyRoot: spInt(0), KeyMode: "major", BPM: spInt(140)})
	// analyzed but no key (key_root NULL) → excluded from the pool.
	mustUpsertTrack(t, s, &Track{Path: "/nokey.flac", Genre: "Jazz"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/nokey.flac", ReplayGainTrackDB: spFloat(-5.0)})

	all, err := s.AnalyzedTrackFeatures(ctx, "", 100)
	if err != nil {
		t.Fatalf("AnalyzedTrackFeatures: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("pool should exclude key-less rows: want 2, got %d (%+v)", len(all), all)
	}

	jazz, _ := s.AnalyzedTrackFeatures(ctx, "Jazz", 100)
	if len(jazz) != 1 || jazz[0].Path != "/jazz1.flac" {
		t.Fatalf("genre filter: want [/jazz1.flac], got %+v", jazz)
	}
}
