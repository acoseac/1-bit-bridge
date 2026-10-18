package smartplaylistgen

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
)

// discoverySlugs are the shelves that record what they served and skip what
// earlier days served (smartplaylist.DiscoveryShelf).
var discoverySlugs = []string{"daily-mix", "auto-mix", "artist-deep-cuts", "lift-off", "wind-down"}

// relaxedLine is the Info line a shelf logs when it drops the exclusion to
// keep its minimum (logExclusionRelaxations).
const relaxedLine = "smart mix dropped the already-shown exclusion to keep its minimum"

// servedStore opens a store at a path the test keeps. The store has no
// reader for the served table, so the tests read it with SQL.
func servedStore(t *testing.T) (*manifest.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := manifest.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// seedDiscoveryLibrary builds a library whose first build fills all five
// discovery shelves, and whose pools, except deep cuts', can fill each shelf
// a second time without a track the first build served:
//
//   - four tracks by "Loved", played once a day at 08:00 on the three days
//     before day: Heavy Rotation, Recently Played, Daily Mix's familiar
//     slice, and the play count that makes "Loved" a loved artist;
//   - twenty unplayed keyed tracks with no ReplayGain (/disc/): the analyzed
//     pool behind Auto Mix and Daily Mix's discovery slice;
//   - ten loud fast tracks (/lift/) and ten quiet slow ones (/wind/), with no
//     key, so they are only in a mood band;
//   - five unplayed tracks by "Loved" (/loved/): one more than the deep-cut
//     shelf of four, so excluding a day's picks leaves it under its minimum.
//
// It returns the played paths, which are Daily Mix's familiar slice.
func seedDiscoveryLibrary(t *testing.T, s *manifest.Store, day time.Time) map[string]bool {
	t.Helper()
	ctx := context.Background()
	add := func(path, artist string, a *manifest.AnalysisRow) {
		t.Helper()
		if err := s.UpsertTrack(ctx, &manifest.Track{
			Path: path, Size: 1, ModTime: time.Unix(1, 0),
			Title: "T" + path, Artist: artist, Genre: "Jazz", Duration: fp(200),
		}); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", path, err)
		}
		if a == nil {
			return
		}
		a.SourcePath, a.CreatedAt, a.SourceMTimeNS, a.SourceSize = path, 1, 1, 1
		if err := s.UpsertAnalysis(ctx, *a); err != nil {
			t.Fatalf("UpsertAnalysis(%s): %v", path, err)
		}
	}
	played := map[string]bool{}
	var familiar []string
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("/fam/%d.flac", i)
		add(p, "Loved", nil)
		familiar = append(familiar, p)
		played[p] = true
	}
	seedDailyPlays(t, s, day, "", []int{1, 2, 3}, familiar...)
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("/disc/%02d.flac", i), fmt.Sprintf("Disc %02d", i),
			&manifest.AnalysisRow{KeyRoot: ip(0), KeyMode: "major", BPM: ip(100 + i)})
	}
	for i := 0; i < 10; i++ {
		add(fmt.Sprintf("/lift/%02d.flac", i), "Lift", &manifest.AnalysisRow{BPM: ip(130), ReplayGainTrackDB: fp(-10)})
		add(fmt.Sprintf("/wind/%02d.flac", i), "Wind", &manifest.AnalysisRow{BPM: ip(80), ReplayGainTrackDB: fp(-4)})
	}
	for i := 0; i < 5; i++ {
		add(fmt.Sprintf("/loved/%d.flac", i), "Loved", nil)
	}
	return played
}

// servedTestOptions is regenTestOptions with shelves small enough for
// seedDiscoveryLibrary's pools: six tracks for Auto Mix and Daily Mix (two
// of them discovery), four for each mood band and for deep cuts, and a
// minimum of three for both.
func servedTestOptions(at time.Time) Options {
	opts := regenTestOptions(at.UnixNano(), true)
	opts.Engine.MaxItems = 6
	opts.Engine.MinMoodBand = 3
	opts.Engine.MaxMoodBandItems = 4
	opts.Engine.MinArtistDeepCuts = 3
	opts.Engine.MaxArtistDeepCutsItems = 4
	opts.ArtistDeepCutsPerArtist = 10
	return opts
}

// cachedShelves returns every cached family but time-of-day as its item
// paths in order, keyed by slug.
func cachedShelves(t *testing.T, s *manifest.Store) map[string][]string {
	t.Helper()
	rows, err := s.LoadSmartPlaylists(context.Background())
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		if r.Kind == string(smartplaylist.KindTimeOfDay) {
			continue
		}
		var items []manifest.SmartPlaylistItem
		if err := json.Unmarshal(r.ItemsJSON, &items); err != nil {
			t.Fatalf("%s items: %v", r.Slug, err)
		}
		paths := make([]string, len(items))
		for i, it := range items {
			paths[i] = it.Path
		}
		out[r.Slug] = paths
	}
	return out
}

// regenerateAt runs Regenerate as a build at that instant would, and
// returns the shelves it cached. It fails unless all five discovery shelves
// are among them, so no test passes on a shelf the fixture left empty.
func regenerateAt(t *testing.T, s *manifest.Store, at time.Time) map[string][]string {
	t.Helper()
	if _, err := Regenerate(context.Background(), s, servedTestOptions(at)); err != nil {
		t.Fatalf("Regenerate at %s: %v", stamp(at.UnixNano()), err)
	}
	shelves := cachedShelves(t, s)
	for slug := range shelves {
		if smartplaylist.DiscoveryShelf(slug) != slices.Contains(discoverySlugs, slug) {
			t.Fatalf("%s: DiscoveryShelf and discoverySlugs disagree", slug)
		}
	}
	for _, slug := range discoverySlugs {
		if len(shelves[slug]) == 0 {
			t.Fatalf("the build at %s cached no %s; the fixture has to fill every discovery shelf", stamp(at.UnixNano()), slug)
		}
	}
	return shelves
}

// servedRow is one row of smart_playlist_served.
type servedRow struct {
	at   int64
	slug string
	path string
}

// readServed returns the served table ordered by slug, served_at and path.
func readServed(t *testing.T, dbPath string) []servedRow {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT served_at, slug, path FROM smart_playlist_served ORDER BY slug, served_at, path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []servedRow
	for rows.Next() {
		var r servedRow
		if err := rows.Scan(&r.at, &r.slug, &r.path); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stamp renders a UnixNano instant in UTC to the nanosecond.
func stamp(ns int64) string {
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

// sortedCopy returns paths sorted, leaving the argument as it was.
func sortedCopy(paths []string) []string {
	out := slices.Clone(paths)
	sort.Strings(out)
	return out
}

// A second build the same UTC day gets the inputs the first got: the settle
// run after a restart, "Regenerate now" and the daily tick all rebuild the
// day's shelves as they were. The engine's only seeds are the week and the
// slug, so the shelves move only if the exclusion does.
func TestRegenerate_SameDayRunsBuildTheSameDiscoveryShelves(t *testing.T) {
	s, _ := servedStore(t)
	morning := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	seedDiscoveryLibrary(t, s, morning)

	first := regenerateAt(t, s, morning)
	second := regenerateAt(t, s, morning.Add(6*time.Hour))
	for _, slug := range discoverySlugs {
		if !slices.Equal(first[slug], second[slug]) {
			t.Errorf("%s changed within the day:\n  09:00 %v\n  15:00 %v", slug, first[slug], second[slug])
		}
	}
}

// Builds on one UTC day leave one build's rows per discovery shelf, the
// last one's, whether the build ran at the day's first nanosecond, at noon
// or at its last nanosecond: each build replaces its shelves' rows for the
// day.
func TestRegenerate_SameDayRunsLeaveOneRunOfServedRows(t *testing.T) {
	s, dbPath := servedStore(t)
	midnight := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	seedDiscoveryLibrary(t, s, midnight)

	runs := []time.Time{midnight, midnight.Add(12 * time.Hour), midnight.Add(24*time.Hour - time.Nanosecond)}
	var last map[string][]string
	for _, at := range runs {
		last = regenerateAt(t, s, at)
	}
	lastNS := runs[len(runs)-1].UnixNano()

	builds := map[string]map[int64]int{}
	lastPaths := map[string][]string{}
	for _, r := range readServed(t, dbPath) {
		if builds[r.slug] == nil {
			builds[r.slug] = map[int64]int{}
		}
		builds[r.slug][r.at]++
		if r.at == lastNS {
			lastPaths[r.slug] = append(lastPaths[r.slug], r.path)
		}
	}
	for slug, byStamp := range builds {
		if !slices.Contains(discoverySlugs, slug) {
			t.Errorf("%s is not a discovery shelf, and the table holds rows for it", slug)
			continue
		}
		if len(byStamp) == 1 && byStamp[lastNS] > 0 {
			continue
		}
		var stamps []string
		for at, n := range byStamp {
			stamps = append(stamps, fmt.Sprintf("%d rows at %s", n, stamp(at)))
		}
		sort.Strings(stamps)
		t.Errorf("%s holds rows from %d builds (%s); want only the last build's", slug, len(byStamp), strings.Join(stamps, ", "))
	}
	for _, slug := range discoverySlugs {
		if have, want := sortedCopy(lastPaths[slug]), sortedCopy(last[slug]); !slices.Equal(have, want) {
			t.Errorf("%s: the last build's rows are %v, want its shelf %v", slug, have, want)
		}
	}
}

// A build on the next UTC day skips every track the day before served, and a
// shelf that would fall under its minimum without those tracks is built
// without the skip, with one line naming it. Here that shelf is deep cuts:
// five tracks in its pool for a shelf of four with a minimum of three. The
// two days are in one ISO week, so the week seed does not move the shelves.
func TestRegenerate_NextDayExcludesThePreviousDaysPicks(t *testing.T) {
	s, dbPath := servedStore(t)
	wednesday := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	thursday := wednesday.Add(24 * time.Hour)
	played := seedDiscoveryLibrary(t, s, wednesday)

	first := regenerateAt(t, s, wednesday)
	servedWednesday := map[string]bool{}
	wednesdayRows := 0
	for _, slug := range discoverySlugs {
		for _, p := range first[slug] {
			servedWednesday[p] = true
			wednesdayRows++
		}
	}

	rec := loggingtest.Record(t)
	second := regenerateAt(t, s, thursday)

	for _, slug := range []string{"auto-mix", "lift-off", "wind-down"} {
		for _, p := range second[slug] {
			if servedWednesday[p] {
				t.Errorf("%s on Thursday has %s, which Wednesday served", slug, p)
			}
		}
	}
	for _, p := range second["daily-mix"] {
		if !played[p] && servedWednesday[p] {
			t.Errorf("Daily Mix's discovery slice on Thursday has %s, which Wednesday served", p)
		}
	}
	if !slices.Equal(second["artist-deep-cuts"], first["artist-deep-cuts"]) {
		t.Errorf("deep cuts should be rebuilt without the exclusion, as on Wednesday:\n  Wednesday %v\n  Thursday  %v",
			first["artist-deep-cuts"], second["artist-deep-cuts"])
	}
	if lines := rec.Lines(relaxedLine); len(lines) != 1 || !strings.Contains(lines[0], "slug=artist-deep-cuts") {
		t.Errorf("relaxation lines = %q, want one, naming artist-deep-cuts", lines)
	}

	thursdayRows := 0
	for _, slug := range discoverySlugs {
		thursdayRows += len(second[slug])
	}
	counts := map[int64]int{}
	for _, r := range readServed(t, dbPath) {
		counts[r.at]++
	}
	if len(counts) != 2 || counts[wednesday.UnixNano()] != wednesdayRows || counts[thursday.UnixNano()] != thursdayRows {
		t.Errorf("served rows by build = %v, want Wednesday's %d kept beside Thursday's %d", counts, wednesdayRows, thursdayRows)
	}
}

// A per-family regeneration replaces its own shelf's rows for the day and
// nothing else: the other shelves' rows and its own from the day before stay
// as they were. It rebuilds the shelf the morning's build cached, since the
// morning's rows are the same day's and exclude nothing.
func TestRegenerateFamily_ReplacesOnlyItsOwnSlugsRowsForToday(t *testing.T) {
	s, dbPath := servedStore(t)
	tuesday := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	morning := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	afternoon := morning.Add(6 * time.Hour)
	dayStart := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).UnixNano()
	dayEnd := dayStart + int64(24*time.Hour)
	seedDiscoveryLibrary(t, s, morning)

	regenerateAt(t, s, tuesday)
	shelves := regenerateAt(t, s, morning)
	before := readServed(t, dbPath)

	generated, existed, n, err := RegenerateFamily(context.Background(), s, servedTestOptions(afternoon), "lift-off")
	if err != nil {
		t.Fatalf("RegenerateFamily: %v", err)
	}
	if !generated || !existed || n == 0 {
		t.Fatalf("generated=%v existed=%v n=%d, want true/true/>0", generated, existed, n)
	}
	rebuilt := cachedShelves(t, s)["lift-off"]
	if !slices.Equal(rebuilt, shelves["lift-off"]) {
		t.Errorf("lift-off changed within the day:\n  09:00 %v\n  15:00 %v", shelves["lift-off"], rebuilt)
	}

	todaysLiftOff := func(r servedRow) bool {
		return r.slug == "lift-off" && r.at >= dayStart && r.at < dayEnd
	}
	after := readServed(t, dbPath)
	keptBefore := slices.DeleteFunc(slices.Clone(before), todaysLiftOff)
	keptAfter := slices.DeleteFunc(slices.Clone(after), todaysLiftOff)
	if !slices.Equal(keptAfter, keptBefore) {
		t.Errorf("rows other than lift-off's for today changed:\n  before %v\n  after  %v", keptBefore, keptAfter)
	}
	var today []string
	for _, r := range after {
		if !todaysLiftOff(r) {
			continue
		}
		if r.at != afternoon.UnixNano() {
			t.Errorf("lift-off row %s is stamped %s; the afternoon's run should have replaced it", r.path, stamp(r.at))
		}
		today = append(today, r.path)
	}
	if have, want := sortedCopy(today), sortedCopy(rebuilt); !slices.Equal(have, want) {
		t.Errorf("lift-off's rows for today are %v, want its rebuilt shelf %v", have, want)
	}
}
