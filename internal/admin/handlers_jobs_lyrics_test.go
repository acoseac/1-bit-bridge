package admin

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The card is OFF unless all three flags are on, and while it is off the
// counts are never read — they cost a full scan with a JSON extraction per
// row, and a bridge that never enabled this should not pay for it on a poll.
func TestJobsLyricsCardIsOffUntilTheFeatureIs(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		atlas, harvest, lyricsOn, wantOn bool
	}{
		{"all off", false, false, false, false},
		{"atlas only", true, false, false, false},
		{"atlas + harvest, lyrics off", true, true, false, false},
		{"lyrics on but harvest off", true, false, true, false},
		{"all on", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg, _ := newTestServer(t)
			cfg.Atlas.Enabled = tc.atlas
			cfg.Atlas.HarvestEnabled = tc.harvest
			cfg.Atlas.LyricsEnabled = tc.lyricsOn
			srv.deps.CfgHolder.Store(cfg)

			var got jobsSnapshotResponse
			if code := doJSON(t, srv.Handler(), "GET", "/api/jobs", nil, &got); code != 200 {
				t.Fatalf("jobs: %d", code)
			}
			if got.Lyrics.Enabled != tc.wantOn {
				t.Errorf("enabled = %v, want %v", got.Lyrics.Enabled, tc.wantOn)
			}
			if !tc.wantOn && got.Lyrics.Available {
				t.Error("the counts were read for a disabled feature")
			}
		})
	}
}

// With the feature on, the card reports what the tier actually did — driven
// through the REAL handler and a REAL store, because a rollup asserted against
// a stub proves nothing about the query that serves it.
func TestJobsLyricsCardReportsTheRealCounts(t *testing.T) {
	ctx := context.Background()
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	st := srv.deps.Manifest
	seed := func(path, album string, n int) {
		num := n
		dur := 100.0
		if err := st.UpsertTrack(ctx, &manifest.Track{
			Path: path, Size: 1, ModTime: time.Unix(0, 0).UTC(),
			Title: path, MusicBrainzAlbumID: album, TrackNumber: &num, Duration: &dur,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("a/1.flac", "alb", 1)
	seed("a/2.flac", "alb", 2)
	seed("a/3.flac", "alb", 3)
	seed("a/4.flac", "alb", 4)

	if _, err := st.UpsertAtlasLyrics(ctx, "a/1.flac",
		lyrics.Doc{Format: lyrics.FormatLRC, Synced: true, Body: "[00:01.00] x"},
		lyrics.SourceAtlasLRC); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAtlasLyrics(ctx, "a/2.flac",
		lyrics.Doc{Format: lyrics.FormatText, Body: "plain"}, lyrics.SourceAtlas); err != nil {
		t.Fatal(err)
	}
	for p, status := range map[string]string{
		"a/1.flac": manifest.AtlasLyricsAvailable,
		"a/2.flac": manifest.AtlasLyricsAvailable,
		"a/3.flac": manifest.AtlasLyricsInstrumental,
		"a/4.flac": manifest.AtlasLyricsPending,
	} {
		if err := st.MarkAtlasLyricsAttempt(ctx, p, "alb", "", "rec", status, 0); err != nil {
			t.Fatal(err)
		}
	}

	var got jobsSnapshotResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs: %d", code)
	}
	if !got.Lyrics.Available {
		t.Fatal("the counts read as unavailable")
	}
	if got.Lyrics.SyncedRows != 1 || got.Lyrics.PlainRows != 1 {
		t.Errorf("rows: synced=%d plain=%d, want 1/1", got.Lyrics.SyncedRows, got.Lyrics.PlainRows)
	}
	if got.Lyrics.Instrumental != 1 || got.Lyrics.Pending != 1 {
		t.Errorf("statuses: instrumental=%d pending=%d, want 1/1",
			got.Lyrics.Instrumental, got.Lyrics.Pending)
	}
	// a/3 and a/4 still have no row and carry an album MBID.
	if got.Lyrics.Addressable != 2 {
		t.Errorf("addressable = %d, want 2", got.Lyrics.Addressable)
	}
}

// The TTL is a single-flight cache over a measured 25.8 ms full scan, so a
// second read inside the window must not re-issue the query.
func TestJobsLyricsCountsAreCached(t *testing.T) {
	ctx := context.Background()
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	first := srv.lyricsStats(ctx)
	if !first.ok {
		t.Fatal("the first read failed")
	}
	at := srv.lyricsStatsAt
	if srv.lyricsStatsSnap == nil {
		t.Fatal("nothing was cached")
	}
	srv.lyricsStats(ctx)
	if !srv.lyricsStatsAt.Equal(at) {
		t.Error("a read inside the TTL recomputed the snapshot")
	}
}

// A cancelled REQUEST must not poison the cache for everyone else. This is the
// distinction the database block was bitten by: a failure about the request is
// not a failure about the database, and caching the former answers
// "unavailable" to the next window of perfectly good requests.
func TestACancelledRequestDoesNotPoisonTheLyricsCache(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap := srv.lyricsStats(ctx)
	if snap.ok {
		t.Skip("the cancelled read still succeeded; nothing to prove here")
	}
	if srv.lyricsStatsSnap != nil {
		t.Fatal("a cancelled request cached its own failure")
	}
	// ...and a healthy request straight afterwards gets a real answer.
	if got := srv.lyricsStats(context.Background()); !got.ok {
		t.Error("the next healthy request inherited the cancelled read's failure")
	}
}
