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
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Subtitle: "most played", Position: 0, RefreshedAt: 100, ItemsJSON: []byte(`[{"position":0,"path":"/a.flac"}]`), EnergyJSON: []byte(`[0.25,0.75]`), ModalRateHz: 96000},
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
	// energy_json + modal_rate_hz round-trip (migration v20).
	if string(got[0].EnergyJSON) != `[0.25,0.75]` || got[0].ModalRateHz != 96000 {
		t.Errorf("energy/modal round-trip mismatch: energy=%s modal=%d", got[0].EnergyJSON, got[0].ModalRateHz)
	}
	// A row written with no energy/rate reads back nil/0 (NULL BLOB, DEFAULT 0).
	if got[1].EnergyJSON != nil || got[1].ModalRateHz != 0 {
		t.Errorf("absent energy/modal should be nil/0: energy=%v modal=%d", got[1].EnergyJSON, got[1].ModalRateHz)
	}

	// Wholesale replace drops stale families.
	if err := s.ReplaceSmartPlaylists(ctx, []StoredSmartPlaylist{
		{Slug: "recently-played", Kind: "recentlyPlayed", Title: "Recently Played", Position: 0, RefreshedAt: 200, ItemsJSON: []byte(`[]`)},
	}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists (replace): %v", err)
	}
	got, err = s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "recently-played" {
		t.Fatalf("wholesale replace failed: %+v", got)
	}

	// Empty snapshot clears the cache.
	if err := s.ReplaceSmartPlaylists(ctx, nil); err != nil {
		t.Fatalf("ReplaceSmartPlaylists (clear): %v", err)
	}
	got, err = s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
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
	rows, err = s.PlayStatsInWindow(ctx, now-30*day, 0, 30.0, 50)
	if err != nil {
		t.Fatalf("PlayStatsInWindow (30d): %v", err)
	}
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
	car, err := s.PlayCountsByHourPath(ctx, 0, 30.0, []string{"CarPlay"})
	if err != nil {
		t.Fatalf("PlayCountsByHourPath (CarPlay): %v", err)
	}
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
		Duration: spFloat(180.5), BPM: spInt(120), ReplayGainTrackDB: spFloat(-6.0), SampleRate: spFloat(96000)})
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
	if tg.SampleRate == nil || *tg.SampleRate != 96000 {
		t.Errorf("tagged sample rate: want 96000 (from tags_json), got %v", tg.SampleRate)
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

	jazz, err := s.AnalyzedTrackFeatures(ctx, "Jazz", 100)
	if err != nil {
		t.Fatalf("AnalyzedTrackFeatures (Jazz): %v", err)
	}
	if len(jazz) != 1 || jazz[0].Path != "/jazz1.flac" {
		t.Fatalf("genre filter: want [/jazz1.flac], got %+v", jazz)
	}
}

// --- Drive Mix (CarPlay-only) ---

func TestPlayStatsByInterfaceInWindow_CarPlayOnly(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/car.flac"})
	mustUpsertTrack(t, s, &Track{Path: "/home.flac"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	mustInsertHistory(t, s,
		// CarPlay plays — should be the only ones counted.
		PlaybackHistoryRow{Path: "/car.flac", StartedAt: now - 1*day, DurationUsed: 120, IfaceType: "CarPlay"},
		PlaybackHistoryRow{Path: "/car.flac", StartedAt: now - 2*day, DurationUsed: 120, IfaceType: "CarPlay"},
		PlaybackHistoryRow{Path: "/car.flac", StartedAt: now - 3*day, DurationUsed: 120, IfaceType: "CarPlay"},
		// A CarPlay skip (<30 s) — excluded.
		PlaybackHistoryRow{Path: "/car.flac", StartedAt: now - 4*day, DurationUsed: 5, IfaceType: "CarPlay"},
		// Non-CarPlay plays — must be excluded.
		PlaybackHistoryRow{Path: "/home.flac", StartedAt: now - 1*day, DurationUsed: 300, IfaceType: "USB-DAC"},
		PlaybackHistoryRow{Path: "/home.flac", StartedAt: now - 2*day, DurationUsed: 300, IfaceType: "Bluetooth"},
		PlaybackHistoryRow{Path: "/home.flac", StartedAt: now - 3*day, DurationUsed: 300, IfaceType: ""},
	)

	rows, err := s.PlayStatsByInterfaceInWindow(ctx, now-60*day, "CarPlay", 30.0, 50)
	if err != nil {
		t.Fatalf("PlayStatsByInterfaceInWindow: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "/car.flac" || rows[0].Plays != 3 {
		t.Fatalf("CarPlay filter + 30s rule: want [/car.flac x3], got %+v", rows)
	}
}

// --- On Repeat (two-level aggregation + hysteresis-callable contract) ---

func TestOnRepeatCandidates_DailyRepeatsCountedPerDevice(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/obsession.flac"})    // qualifies
	mustUpsertTrack(t, s, &Track{Path: "/once-per-day.flac"}) // never ≥2 per day → fails repeat_days
	mustUpsertTrack(t, s, &Track{Path: "/one-day-binge.flac"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	// /obsession.flac: 3 days × 2+ plays/day (passes both thresholds).
	mustInsertHistory(t, s,
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 1*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 1*day - 600*int64(time.Second), DurationUsed: 200},
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 2*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 2*day - 600*int64(time.Second), DurationUsed: 200},
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 3*day, DurationUsed: 200},
		PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 3*day - 600*int64(time.Second), DurationUsed: 200},
	)
	// /once-per-day.flac: 5 days × 1 play/day (no per-day repeat → fails repeat_days).
	for d := int64(1); d <= 5; d++ {
		mustInsertHistory(t, s, PlaybackHistoryRow{Path: "/once-per-day.flac", StartedAt: now - d*day, DurationUsed: 200})
	}
	// /one-day-binge.flac: 8 plays in one day (passes total_plays but fails repeat_days=3).
	for i := int64(0); i < 8; i++ {
		mustInsertHistory(t, s, PlaybackHistoryRow{Path: "/one-day-binge.flac", StartedAt: now - 1*day - i*600*int64(time.Second), DurationUsed: 200})
	}
	// A skip event must NOT count toward daily plays (the 30s rule).
	mustInsertHistory(t, s, PlaybackHistoryRow{Path: "/obsession.flac", StartedAt: now - 1*day, DurationUsed: 5})

	rows, err := s.OnRepeatCandidates(ctx, now-30*day, 30.0, 4, 3, 50)
	if err != nil {
		t.Fatalf("OnRepeatCandidates: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "/obsession.flac" || rows[0].Plays != 6 {
		t.Fatalf("repeat threshold: want [/obsession.flac x6], got %+v", rows)
	}
}

func TestOnRepeatCandidates_DevicesAggregateAcrossPath(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	mustUpsertTrack(t, s, &Track{Path: "/shared.flac"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	// iPhone: 3 days × 2 plays.
	for d := int64(1); d <= 3; d++ {
		mustInsertHistory(t, s,
			PlaybackHistoryRow{DeviceToken: "iphone", Path: "/shared.flac", StartedAt: now - d*day, DurationUsed: 200},
			PlaybackHistoryRow{DeviceToken: "iphone", Path: "/shared.flac", StartedAt: now - d*day - 600*int64(time.Second), DurationUsed: 200},
		)
	}
	// iPad: same days, 2 plays each — adds to total_plays + repeat_days.
	for d := int64(1); d <= 3; d++ {
		mustInsertHistory(t, s,
			PlaybackHistoryRow{DeviceToken: "ipad", Path: "/shared.flac", StartedAt: now - d*day, DurationUsed: 200},
			PlaybackHistoryRow{DeviceToken: "ipad", Path: "/shared.flac", StartedAt: now - d*day - 600*int64(time.Second), DurationUsed: 200},
		)
	}

	rows, err := s.OnRepeatCandidates(ctx, now-30*day, 30.0, 4, 3, 50)
	if err != nil {
		t.Fatalf("OnRepeatCandidates: %v", err)
	}
	// Sum-across-devices: 3+3 device-days each with 2 plays = total 12 plays, 6 repeat_days.
	if len(rows) != 1 || rows[0].Path != "/shared.flac" || rows[0].Plays != 12 {
		t.Fatalf("cross-device aggregation: want [/shared.flac x12], got %+v", rows)
	}
}

// --- From Artists You Love (artist-graph + ROW_NUMBER per-artist cap) ---

func TestLovedArtistDeepCuts_PerArtistCapAndRecentExclusion(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	// Two loved artists (≥3 plays in window) and one indifferent artist.
	mustUpsertTrack(t, s, &Track{Path: "/loved-a/played-1.flac", Artist: "LovedA"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-a/deep-1.flac", Artist: "LovedA"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-a/deep-2.flac", Artist: "LovedA"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-a/deep-3.flac", Artist: "LovedA"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-a/deep-4.flac", Artist: "LovedA"}) // 5th — exceeds per-artist cap
	mustUpsertTrack(t, s, &Track{Path: "/loved-b/deep-1.flac", Artist: "LovedB"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-b/deep-2.flac", Artist: "LovedB"})
	mustUpsertTrack(t, s, &Track{Path: "/loved-b/deep-3.flac", Artist: "LovedB"})
	mustUpsertTrack(t, s, &Track{Path: "/cold/track-1.flac", Artist: "ColdC"}) // never played by user
	mustUpsertTrack(t, s, &Track{Path: "/cold/track-2.flac", Artist: "ColdC"})

	now := utcNS(2026, 1, 20, 12, 0)
	day := int64(24 * time.Hour)
	// LovedA: 3 qualifying plays of /played-1.flac in last 30 days.
	for d := int64(1); d <= 3; d++ {
		mustInsertHistory(t, s, PlaybackHistoryRow{Path: "/loved-a/played-1.flac", StartedAt: now - d*day, DurationUsed: 200})
	}
	// LovedB: 4 qualifying plays of /loved-b/deep-1.flac — but that means it counts as "recently played" and SHOULD be excluded.
	for d := int64(1); d <= 4; d++ {
		mustInsertHistory(t, s, PlaybackHistoryRow{Path: "/loved-b/deep-1.flac", StartedAt: now - d*day, DurationUsed: 200})
	}
	// ColdC: no plays at all.

	rows, err := s.LovedArtistDeepCuts(ctx,
		now-30*day, 3, // lovedSinceNS, lovedMinPlays
		now-90*day, // deepCutCutoffNS — exclude anything played in last 90d
		30.0,
		3,  // perArtistCap
		50, // limit
	)
	if err != nil {
		t.Fatalf("LovedArtistDeepCuts: %v", err)
	}

	pathSet := map[string]bool{}
	artistCounts := map[string]int{}
	for _, r := range rows {
		pathSet[r.Path] = true
	}
	for _, p := range []string{
		"/loved-a/deep-1.flac", "/loved-a/deep-2.flac", "/loved-a/deep-3.flac",
		"/loved-b/deep-2.flac", "/loved-b/deep-3.flac",
	} {
		if !pathSet[p] {
			t.Errorf("eligible deep cut missing: %s", p)
		}
	}
	// Recently-played should be excluded.
	if pathSet["/loved-a/played-1.flac"] {
		t.Error("recently played track must be excluded")
	}
	if pathSet["/loved-b/deep-1.flac"] {
		t.Error("track recently played by loved artist must be excluded")
	}
	// Cold artist contributes nothing.
	if pathSet["/cold/track-1.flac"] || pathSet["/cold/track-2.flac"] {
		t.Error("non-loved artist must contribute zero tracks")
	}
	// Per-artist cap: LovedA had 4 deep-cut tracks but only 3 must appear.
	for _, r := range rows {
		if r.Path == "/loved-a/deep-1.flac" || r.Path == "/loved-a/deep-2.flac" ||
			r.Path == "/loved-a/deep-3.flac" || r.Path == "/loved-a/deep-4.flac" {
			artistCounts["LovedA"]++
		}
		if r.Path == "/loved-b/deep-2.flac" || r.Path == "/loved-b/deep-3.flac" {
			artistCounts["LovedB"]++
		}
	}
	if artistCounts["LovedA"] != 3 {
		t.Errorf("LovedA per-artist cap: want 3, got %d", artistCounts["LovedA"])
	}
	if artistCounts["LovedB"] != 2 {
		t.Errorf("LovedB eligible count: want 2, got %d", artistCounts["LovedB"])
	}
}

// --- Mood bands (BPM + ReplayGain, effective tag-wins-over-analysis) ---

func TestQuietSlowTrackFeatures_BoundariesAndTagWins(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	// Wind Down qualifier: bpm ≤ 90 AND RG > -6 (less-negative = quieter).
	mustUpsertTrack(t, s, &Track{Path: "/quiet-slow.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/quiet-slow.flac", BPM: spInt(80), ReplayGainTrackDB: spFloat(-4.0)})
	// Tag wins over analysis: tag bpm 60, analysis bpm 200 → effective bpm 60.
	mustUpsertTrack(t, s, &Track{
		Path: "/tag-overrides.flac", BPM: spInt(60), ReplayGainTrackDB: spFloat(-3.0),
	})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/tag-overrides.flac", BPM: spInt(200), ReplayGainTrackDB: spFloat(-20.0)})
	// Boundary EXCLUSIONS: bpm=91 (above); rg=-6 (not > -6); rg=-7 (below).
	mustUpsertTrack(t, s, &Track{Path: "/above-bpm.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/above-bpm.flac", BPM: spInt(91), ReplayGainTrackDB: spFloat(-3.0)})
	mustUpsertTrack(t, s, &Track{Path: "/equal-rg.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/equal-rg.flac", BPM: spInt(80), ReplayGainTrackDB: spFloat(-6.0)})
	mustUpsertTrack(t, s, &Track{Path: "/too-loud.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/too-loud.flac", BPM: spInt(80), ReplayGainTrackDB: spFloat(-9.0)})
	// NULL BPM and NULL RG both exclude.
	mustUpsertTrack(t, s, &Track{Path: "/null-bpm.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/null-bpm.flac", ReplayGainTrackDB: spFloat(-3.0)})
	mustUpsertTrack(t, s, &Track{Path: "/null-rg.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/null-rg.flac", BPM: spInt(80)})

	rows, err := s.QuietSlowTrackFeatures(ctx, 90, -6.0)
	if err != nil {
		t.Fatalf("QuietSlowTrackFeatures: %v", err)
	}
	gotPaths := map[string]bool{}
	for _, r := range rows {
		gotPaths[r.Path] = true
	}
	wantIn := []string{"/quiet-slow.flac", "/tag-overrides.flac"}
	wantOut := []string{"/above-bpm.flac", "/equal-rg.flac", "/too-loud.flac", "/null-bpm.flac", "/null-rg.flac"}
	for _, p := range wantIn {
		if !gotPaths[p] {
			t.Errorf("should qualify: %s", p)
		}
	}
	for _, p := range wantOut {
		if gotPaths[p] {
			t.Errorf("must NOT qualify: %s", p)
		}
	}
}

func TestLoudFastTrackFeatures_BoundariesAndTagWins(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	// Lift Off qualifier: bpm ≥ 120 AND RG ≤ -8 (more-negative = louder).
	mustUpsertTrack(t, s, &Track{Path: "/loud-fast.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/loud-fast.flac", BPM: spInt(140), ReplayGainTrackDB: spFloat(-10.0)})
	mustUpsertTrack(t, s, &Track{Path: "/tag-overrides.flac", BPM: spInt(160), ReplayGainTrackDB: spFloat(-12.0)})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/tag-overrides.flac", BPM: spInt(60), ReplayGainTrackDB: spFloat(-1.0)})
	// Boundary EXCLUSIONS.
	mustUpsertTrack(t, s, &Track{Path: "/below-bpm.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/below-bpm.flac", BPM: spInt(119), ReplayGainTrackDB: spFloat(-10.0)})
	mustUpsertTrack(t, s, &Track{Path: "/not-loud-enough.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/not-loud-enough.flac", BPM: spInt(140), ReplayGainTrackDB: spFloat(-7.9)})
	mustUpsertTrack(t, s, &Track{Path: "/equal-rg.flac"}) // -8 is INclusive (≤), so this qualifies — sanity check.
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/equal-rg.flac", BPM: spInt(140), ReplayGainTrackDB: spFloat(-8.0)})
	// NULL exclusions.
	mustUpsertTrack(t, s, &Track{Path: "/null-bpm.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/null-bpm.flac", ReplayGainTrackDB: spFloat(-10.0)})
	mustUpsertTrack(t, s, &Track{Path: "/null-rg.flac"})
	mustUpsertAnalysis(t, s, AnalysisRow{SourcePath: "/null-rg.flac", BPM: spInt(140)})

	rows, err := s.LoudFastTrackFeatures(ctx, 120, -8.0)
	if err != nil {
		t.Fatalf("LoudFastTrackFeatures: %v", err)
	}
	gotPaths := map[string]bool{}
	for _, r := range rows {
		gotPaths[r.Path] = true
	}
	wantIn := []string{"/loud-fast.flac", "/tag-overrides.flac", "/equal-rg.flac"}
	wantOut := []string{"/below-bpm.flac", "/not-loud-enough.flac", "/null-bpm.flac", "/null-rg.flac"}
	for _, p := range wantIn {
		if !gotPaths[p] {
			t.Errorf("should qualify: %s", p)
		}
	}
	for _, p := range wantOut {
		if gotPaths[p] {
			t.Errorf("must NOT qualify: %s", p)
		}
	}
}
