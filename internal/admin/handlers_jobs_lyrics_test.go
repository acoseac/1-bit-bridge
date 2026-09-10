package admin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
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
	// a/4 only. a/3 is INSTRUMENTAL — a success that correctly leaves no
	// `track_lyrics` row — so the candidate query will never offer it again;
	// counting it as outstanding floors this number at the instrumental
	// population and makes a finished tier read as a stalled job. a/4 is
	// `pending`, which is a fact about the upstream and genuinely still to do.
	if got.Lyrics.Addressable != 1 {
		t.Errorf("addressable = %d, want 1 — a/4 only; a/3 is instrumental and terminal",
			got.Lyrics.Addressable)
	}
}

// The TTL collapses concurrent polls to one query, which is the point on an
// endpoint polled every ten seconds per open tab over a measured 25.8 ms scan.
func TestJobsLyricsCountsAreCached(t *testing.T) {
	ctx := context.Background()
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	if first := srv.lyricsStats(ctx); first == nil {
		t.Fatal("the first read produced nothing")
	}
	at := srv.lyricsStatsAt
	if at.IsZero() {
		t.Fatal("nothing was stamped")
	}
	srv.lyricsStats(ctx)
	if !srv.lyricsStatsAt.Equal(at) {
		t.Error("a read inside the TTL recomputed the snapshot")
	}
}

// A caller hanging up must not synthesize a failure for everyone queued behind
// it — the PR #373 singleflight rule. The db context is detached, so a
// cancelled REQUEST still yields a real snapshot rather than poisoning the
// window for the next thirty seconds.
func TestACancelledRequestStillYieldsRealLyricsCounts(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if snap := srv.lyricsStats(ctx); snap == nil {
		t.Fatal("a cancelled request produced no snapshot — the db context is not detached")
	}
	// ...and the next healthy caller reads a real one too.
	if snap := srv.lyricsStats(context.Background()); snap == nil {
		t.Error("the cached snapshot was poisoned by the cancelled request")
	}
}

// Concurrent callers collapse to ONE query, and none of them races.
func TestConcurrentLyricsStatsReadsCollapse(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if snap := srv.lyricsStats(context.Background()); snap == nil {
				t.Error("a concurrent caller got nothing")
			}
		}()
	}
	wg.Wait()
}

// A failed read serves the LAST GOOD snapshot and still stamps the clock.
//
// Two properties, and they pull in opposite directions, which is why both are
// asserted here. Serving last-good keeps a card that was reading correctly a
// moment ago from blanking on one transient error. Stamping anyway is what
// stops the TTL from never tripping after a failure — without it every poll
// re-runs a scan that is already failing, most likely because it is slow.
func TestAFailedLyricsReadServesLastGoodAndBacksOff(t *testing.T) {
	ctx := context.Background()
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled, cfg.Atlas.HarvestEnabled, cfg.Atlas.LyricsEnabled = true, true, true
	srv.deps.CfgHolder.Store(cfg)

	// One good read, so there is a last-good to fall back to.
	good := srv.lyricsStats(ctx)
	if good == nil {
		t.Fatal("the first read produced nothing")
	}
	// Force the next read to fail, and to actually happen.
	srv.lyricsStatsMu.Lock()
	srv.lyricsStatsAt = time.Time{}
	srv.lyricsStatsMu.Unlock()
	if err := srv.deps.Manifest.Close(); err != nil {
		t.Fatal(err)
	}

	got := srv.lyricsStats(ctx)
	if got == nil {
		t.Error("a failed read blanked the card instead of serving the last good snapshot")
	}
	srv.lyricsStatsMu.Lock()
	stamped := srv.lyricsStatsAt
	srv.lyricsStatsMu.Unlock()
	if stamped.IsZero() {
		t.Error("the clock was not stamped on failure — the TTL would never trip, " +
			"so every poll re-runs a scan that is already failing")
	}
}

// TestTheLyricsCardAndTheSweepReadTheSamePredicate — the split this feature
// shipped with, asserted from the console side.
//
// `/api/jobs` always read the flag live from the config holder. The sweeper
// took it at boot, inside the `if` that decided whether to wire its sink at
// all. The two could not disagree while a restart was the only way to change
// the value — and adding a settings field is exactly what would have made them,
// with the card reporting `enabled: true` over a sweep that was never wired.
//
// Both sides now call config.AtlasConfig.LyricsTierActive. This pins the
// console half: the card follows a PATCH within the same process, with no
// restart, and it follows the WHOLE predicate rather than the one field — the
// tier rides the harvest credential, so a bridge without one must not be told
// the tier is running. (The sweeper half is
// TestTheGateIsLiveAndFailsClosed in internal/atlasharvest.)
func TestTheLyricsCardAndTheSweepReadTheSamePredicate(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled = true
	cfg.Atlas.HarvestEnabled = true
	cfg.Atlas.LyricsEnabled = false
	srv.deps.CfgHolder.Store(cfg)
	h := srv.Handler()

	enabled := func() bool {
		t.Helper()
		var got jobsSnapshotResponse
		if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
			t.Fatalf("jobs: %d", code)
		}
		return got.Lyrics.Enabled
	}
	if enabled() {
		t.Fatal("the card reports the tier on with the flag off")
	}

	var resp settingsPatchResponse
	if code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"atlasLyricsEnabled": true}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	// LIVE, not restart: the sink is wired unconditionally and the flag is the
	// gate. A `restart` here would mean the console and the sweeper had gone
	// back to disagreeing about when the change lands.
	if got := string(resp.Fields["atlasLyricsEnabled"].Status); got != string(applyLive) {
		t.Errorf("apply status = %q, want live", got)
	}
	if !enabled() {
		t.Error("the card did not follow the PATCH — the flag is not read live")
	}

	// The WHOLE predicate, not the one field: turning off the credential the
	// tier rides must take the card with it, or an operator is told a sweep is
	// running that cannot authenticate.
	cfg = config.Clone(srv.deps.CfgHolder.Load())
	cfg.Atlas.HarvestEnabled = false
	srv.deps.CfgHolder.Store(cfg)
	if enabled() {
		t.Error("the card reports the tier on without the harvest credential it rides")
	}
}

// TestDisablingTheLyricsTierIsNotAdvisedAboutPrerequisites — the
// applied-but-inert reason is for someone turning the tier ON into a bridge
// that cannot run it. Reported on the way OFF it told an operator who had just
// deliberately disabled the feature that their save needed two other settings
// turned on: advice about a thing they had asked to stop. (Gemini on #894.)
func TestDisablingTheLyricsTierIsNotAdvisedAboutPrerequisites(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Atlas.Enabled = false
	cfg.Atlas.HarvestEnabled = false
	cfg.Atlas.LyricsEnabled = true
	srv.deps.CfgHolder.Store(cfg)

	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"atlasLyricsEnabled": false}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	got := resp.Fields["atlasLyricsEnabled"]
	if string(got.Status) != string(applyLive) {
		t.Errorf("status = %q, want live", got.Status)
	}
	if got.Reason != "" {
		t.Errorf("turning the tier OFF carried the reason %q — that is advice about "+
			"enabling it, given to someone who just disabled it", got.Reason)
	}

	// NEGATIVE CONTROL: turning it ON with the prerequisites still off DOES
	// carry the reason, so the assertion above is about the direction rather
	// than about the reason having been dropped.
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"atlasLyricsEnabled": true}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if resp.Fields["atlasLyricsEnabled"].Reason == "" {
		t.Error("enabling the tier on a bridge that cannot run it reported no reason")
	}
}
