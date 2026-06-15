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
	var hist []manifest.PlaybackHistoryRow
	for _, day := range []int{29, 30, 31} {
		base := time.Date(2026, 5, day, 8, 0, 0, 0, time.UTC)
		for i, p := range []string{"/a.flac", "/b.flac", "/c.flac"} {
			hist = append(hist, manifest.PlaybackHistoryRow{
				DeviceToken: "d", Path: p,
				StartedAt:    base.Add(time.Duration(i*5) * time.Minute).UnixNano(),
				DurationUsed: 200, IfaceType: "CarPlay",
			})
		}
	}
	if err := s.InsertHistoryBatch(ctx, hist); err != nil {
		t.Fatalf("InsertHistoryBatch: %v", err)
	}

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
			Items: []smartplaylist.Item{{Position: 0, Path: "/a.flac", Title: "A", Artist: "X"}}},
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
