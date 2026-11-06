package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylistgen"
)

// smartFeed is GET /v1/smart-playlists as a client decodes it.
type smartFeed struct {
	RefreshedAt int64 `json:"refreshedAt"`
	Playlists   []struct {
		Slug        string `json:"slug"`
		Kind        string `json:"kind"`
		RefreshedAt int64  `json:"refreshedAt"`
		Items       []struct {
			Path string `json:"path"`
		} `json:"items"`
	} `json:"playlists"`
}

func (f smartFeed) family(kind string) (int, bool) {
	for _, p := range f.Playlists {
		if p.Kind == kind {
			return len(p.Items), true
		}
	}
	return 0, false
}

// feedServer serves the smart-playlist feed of mstore through the real
// router, at whatever instant the returned clock holds.
func feedServer(t *testing.T, mstore *manifest.Store, demo bool) (get func() smartFeed, clock *atomic.Pointer[time.Time]) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, DataDir: dir, ListenAddress: ":7788", LibraryName: "T"}
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("probe")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, tokens, nil, "fp").WithSmartPlaylistStore(mstore).WithDemoMode(demo)
	clock = &atomic.Pointer[time.Time]{}
	srv.clockNow = func() time.Time { return *clock.Load() }
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	get = func() smartFeed {
		t.Helper()
		resp := authedGET(t, hs.URL+"/v1/smart-playlists", raw)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET /v1/smart-playlists answered %d", resp.StatusCode)
		}
		var out smartFeed
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return get, clock
}

func openFeedStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The family is served from 1 December 00:00:00 to 5 January 23:59:59 UTC,
// read from the clock at the request, and left out otherwise without
// counting toward the top-level refreshedAt; the same reading of the clock
// picks the time-of-day hour. A demo bridge behaves the same.
func TestTheChristmasFamilyIsServedOnlyInItsWindow(t *testing.T) {
	plus14 := time.FixedZone("UTC+14", 14*60*60)
	for _, demo := range []bool{false, true} {
		mstore := openFeedStore(t)
		noon, err := json.Marshal(manifest.SmartPlaylistHourlyBlob{Hourly: map[int][]manifest.SmartPlaylistItem{
			12: {{Position: 0, Path: "/noon.flac"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := mstore.ReplaceSmartPlaylists(context.Background(), []manifest.StoredSmartPlaylist{
			{Slug: "daily-mix", Kind: "dailyMix", Title: "Daily Mix", Position: 0, RefreshedAt: 100,
				ItemsJSON: []byte(`[{"position":0,"path":"/a.flac"}]`)},
			{Slug: "christmas", Kind: "christmas", Title: "Christmas", Position: 1, RefreshedAt: 200,
				ItemsJSON: []byte(`[{"position":0,"path":"/carol.flac"}]`)},
			{Slug: "time-of-day", Kind: "timeOfDay", Title: "For Right Now", Position: 2, RefreshedAt: 100,
				ItemsJSON: noon},
		}); err != nil {
			t.Fatal(err)
		}
		get, clock := feedServer(t, mstore, demo)
		for _, c := range []struct {
			at                   time.Time
			christmas, timeOfDay bool
		}{
			{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), false, false},
			{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), true, false},
			{time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC), true, true},
			{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), true, false},
			{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true, false},
			{time.Date(2027, 1, 5, 23, 59, 59, 0, time.UTC), true, false},
			{time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC), false, false},
			{time.Date(2027, 1, 6, 12, 0, 0, 0, time.UTC), false, true},
			// 1 December 05:00 at UTC+14 is 30 November 15:00 UTC.
			{time.Date(2026, 12, 1, 5, 0, 0, 0, plus14), false, false},
			// 1 December 02:00 at UTC+14 is 30 November 12:00 UTC: noon.
			{time.Date(2026, 12, 1, 2, 0, 0, 0, plus14), false, true},
		} {
			at := c.at
			clock.Store(&at)
			feed := get()
			_, xmas := feed.family("christmas")
			_, tod := feed.family("timeOfDay")
			_, daily := feed.family("dailyMix")
			when := fmt.Sprintf("demo %v at %s", demo, at.Format(time.RFC3339))
			if xmas != c.christmas {
				t.Errorf("%s: christmas served %v, want %v", when, xmas, c.christmas)
			}
			if tod != c.timeOfDay {
				t.Errorf("%s: time of day served %v, want %v (the hour is the clock's)", when, tod, c.timeOfDay)
			}
			if !daily {
				t.Errorf("%s: daily mix is missing", when)
			}
			want := int64(100)
			if c.christmas {
				want = 200
			}
			if feed.RefreshedAt != want {
				t.Errorf("%s: refreshedAt %d, want %d (a family left out counts for nothing)", when, feed.RefreshedAt, want)
			}
		}
	}
}

// End to end through the regenerator and the real handler: a cache built
// on 20 November is served from 1 December 00:00 UTC and not on 6 January,
// a per-family rebuild of it later does not move the top-level refreshedAt
// once it is out of season, and a cache built inside the window on 5
// January does not keep it served on the 6th. The library has no history
// for the Christmas tracks and analysis is off.
func TestAChristmasCacheIsServedByTheClockAtTheRequest(t *testing.T) {
	ctx := context.Background()
	mstore := openFeedStore(t)
	var tracks []*manifest.Track
	for a, album := range []string{"Carols A", "Carols B", "Carols C"} {
		for i := 0; i < 5; i++ {
			tracks = append(tracks, &manifest.Track{
				Path: fmt.Sprintf("%s/%02d.flac", album, i), Size: 1, ModTime: time.Unix(1, 0),
				Title: fmt.Sprintf("Carol %d", i), Artist: fmt.Sprintf("Choir %d", a), Album: album, Genre: "Christmas",
			})
		}
	}
	for i := 0; i < 3; i++ {
		tracks = append(tracks, &manifest.Track{
			Path: fmt.Sprintf("Jazz/%02d.flac", i), Size: 1, ModTime: time.Unix(1, 0),
			Title: fmt.Sprintf("Tune %d", i), Artist: "Trio", Album: "Standards", Genre: "Jazz",
		})
	}
	if err := mstore.UpsertTrackBatch(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	built := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var hist []manifest.PlaybackHistoryRow
	for d := 1; d <= 3; d++ {
		for i := 0; i < 3; i++ {
			hist = append(hist, manifest.PlaybackHistoryRow{
				DeviceToken: "d", Path: fmt.Sprintf("Jazz/%02d.flac", i),
				StartedAt:    built.Add(-time.Duration(d)*24*time.Hour + time.Duration(i)*5*time.Minute).UnixNano(),
				DurationUsed: 200,
			})
		}
	}
	if err := mstore.InsertHistoryBatch(ctx, hist); err != nil {
		t.Fatal(err)
	}
	opts := func(at time.Time) smartplaylistgen.Options {
		o := smartplaylistgen.DefaultOptions(at.UnixNano(), false)
		o.Engine.MinHeavyRotation = 2
		return o
	}
	if _, err := smartplaylistgen.Regenerate(ctx, mstore, opts(built)); err != nil {
		t.Fatal(err)
	}
	rebuilt := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if generated, _, _, err := smartplaylistgen.RegenerateFamily(ctx, mstore, opts(rebuilt), "christmas"); err != nil || !generated {
		t.Fatalf("RegenerateFamily(christmas): generated %v, %v", generated, err)
	}

	get, clock := feedServer(t, mstore, false)
	at := func(t time.Time) smartFeed {
		clock.Store(&t)
		return get()
	}
	opening := at(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if n, ok := opening.family("christmas"); !ok || n != 15 {
		t.Fatalf("on 1 December 00:00 UTC the November cache serves christmas %v with %d items, want its 15", ok, n)
	}
	if _, ok := opening.family("heavyRotation"); !ok || opening.RefreshedAt != rebuilt.UnixNano() {
		t.Fatalf("in season: heavy rotation %v and refreshedAt %d, want the christmas rebuild's %d", ok, opening.RefreshedAt, rebuilt.UnixNano())
	}
	closed := at(time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC))
	if _, ok := closed.family("christmas"); ok {
		t.Fatal("on 6 January 00:00 UTC the cache still serves christmas")
	}
	if closed.RefreshedAt != built.UnixNano() {
		t.Fatalf("out of season refreshedAt is %d, want the served families' %d, not the christmas rebuild's %d",
			closed.RefreshedAt, built.UnixNano(), rebuilt.UnixNano())
	}

	lastDay := time.Date(2027, 1, 5, 23, 0, 0, 0, time.UTC)
	if _, err := smartplaylistgen.Regenerate(ctx, mstore, opts(lastDay)); err != nil {
		t.Fatal(err)
	}
	if _, ok := at(time.Date(2027, 1, 5, 23, 59, 59, 0, time.UTC)).family("christmas"); !ok {
		t.Fatal("a cache built on 5 January does not serve christmas that day")
	}
	if _, ok := at(time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC)).family("christmas"); ok {
		t.Fatal("a cache built inside the window on 5 January keeps christmas served on the 6th")
	}
}

// A library that is nothing but Christmas albums, never played, with
// analysis off, serves the family in season and nothing out of it.
func TestAnUnheardLibraryServesTheChristmasFamilyInSeasonAlone(t *testing.T) {
	ctx := context.Background()
	mstore := openFeedStore(t)
	var tracks []*manifest.Track
	for a, album := range []string{"Kolędy polskie", "Julsånger", "Christmas Chorales"} {
		for i := 0; i < 6; i++ {
			tracks = append(tracks, &manifest.Track{
				Path: fmt.Sprintf("%s/%02d.flac", album, i), Size: 1, ModTime: time.Unix(1, 0),
				Title: fmt.Sprintf("Track %d", i), Artist: fmt.Sprintf("Performer %d", a), Album: album, Genre: "Christmas",
			})
		}
	}
	if err := mstore.UpsertTrackBatch(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	built := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := smartplaylistgen.Regenerate(ctx, mstore, smartplaylistgen.DefaultOptions(built.UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	get, clock := feedServer(t, mstore, false)
	at := time.Date(2026, 12, 24, 18, 0, 0, 0, time.UTC)
	clock.Store(&at)
	feed := get()
	if n, ok := feed.family("christmas"); !ok || n != 18 || len(feed.Playlists) != 1 || feed.RefreshedAt != built.UnixNano() {
		t.Fatalf("on 24 December: christmas %v with %d items among %d families, refreshedAt %d; want it alone with all 18",
			ok, n, len(feed.Playlists), feed.RefreshedAt)
	}
	at = time.Date(2027, 1, 7, 9, 0, 0, 0, time.UTC)
	clock.Store(&at)
	if feed := get(); len(feed.Playlists) != 0 || feed.RefreshedAt != 0 {
		t.Fatalf("on 7 January: %d families, refreshedAt %d; want none and 0", len(feed.Playlists), feed.RefreshedAt)
	}
}

// A library with no Christmas music serves no Christmas family in season.
func TestNoChristmasMusicIsNoChristmasFamilyInSeason(t *testing.T) {
	ctx := context.Background()
	mstore := openFeedStore(t)
	var tracks []*manifest.Track
	for i := 0; i < 20; i++ {
		tracks = append(tracks, &manifest.Track{
			Path: fmt.Sprintf("Pop/%02d.flac", i), Size: 1, ModTime: time.Unix(1, 0),
			Title: "Holiday", Artist: fmt.Sprintf("Singer %d", i%4), Album: "Winter", Genre: "Holiday",
		})
	}
	if err := mstore.UpsertTrackBatch(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	if _, err := smartplaylistgen.Regenerate(ctx, mstore,
		smartplaylistgen.DefaultOptions(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC).UnixNano(), false)); err != nil {
		t.Fatal(err)
	}
	get, clock := feedServer(t, mstore, false)
	at := time.Date(2026, 12, 24, 18, 0, 0, 0, time.UTC)
	clock.Store(&at)
	if _, ok := get().family("christmas"); ok {
		t.Fatal("twenty tracks titled Holiday, on an album named Winter, in the genre Holiday made a Christmas family")
	}
}

// The API's mirror of the kind is the engine's.
func TestTheAPIsChristmasKindIsTheEngines(t *testing.T) {
	if christmasKind != string(smartplaylist.KindChristmas) {
		t.Fatalf("the API filters kind %q and the engine builds %q", christmasKind, smartplaylist.KindChristmas)
	}
}
