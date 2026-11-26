package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The album list's index rail for every sort (B1021 PR 3b): buckets keyed as
// the sort orders its rows (Recently Added by the reader's calendar, Year by
// year, Title and Artist by letter), with the kind of index they are, on the
// first page alone.

// railAlbum is one album of a rail case's library: one track, its year tag
// (0 for none) and the time it was indexed (the zero time for 0).
type railAlbum struct {
	path, title, artist string
	year                int
	added               time.Time
}

// seedRailLibrary lays down albums and writes each one's indexed_at as the
// case gives it, before anything has asked for the catalog.
func seedRailLibrary(t *testing.T, srv *Server, dataDir string, albums []railAlbum) {
	t.Helper()
	for _, a := range albums {
		tr := &manifest.Track{Path: a.path, Title: "T", Album: a.title, AlbumArtist: a.artist, Artist: a.artist,
			Codec: "FLAC", Size: 1000, ModTime: time.Unix(1, 0)}
		if a.year != 0 {
			y := a.year
			tr.Year = &y
		}
		if err := srv.deps.Manifest.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, a := range albums {
		var ns int64
		if !a.added.IsZero() {
			ns = a.added.UnixNano()
		}
		if res, err := db.Exec(`UPDATE tracks SET indexed_at = ? WHERE path = ?`, ns, a.path); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("%s: stamped %d rows", a.path, n)
		}
	}
}

// railOf is a list page's rail as it was sent: its kind and its buckets, and
// whether each key was in the body at all.
func railOf(t *testing.T, srv *Server, target, items string) (kind string, buckets []playerBucketDTO, kindSent, bucketsSent bool) {
	t.Helper()
	p := getFiltered(t, srv, target, items, "id")
	if p.status != http.StatusOK {
		t.Fatalf("%s: status %d", target, p.status)
	}
	raw, kindSent := p.keys["rail"]
	if kindSent {
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatalf("%s: rail: %v", target, err)
		}
	}
	_, bucketsSent = p.keys["buckets"]
	return kind, p.buckets(t), kindSent, bucketsSent
}

// railNow is the rail cases' clock: a Thursday late in November.
var railNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// TestTheAlbumRailFollowsEverySort holds the album list's buckets to its sort:
// Recently Added by calendar period (this month, a future album with it, then
// earlier this year, each earlier year and "?" for no date), Year by year with
// "?" for the rest, Title and Artist by letter; rail names the kind beside
// them on the first page and both are absent from the second, the name filter
// narrows them, and the artists' and composers' lists say letters while a
// count-ordered genre list sends neither.
func TestTheAlbumRailFollowsEverySort(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	srv.testNow = func() time.Time { return railNow }
	seedRailLibrary(t, srv, cfg.DataDir, []railAlbum{
		{"R/Apple/01.flac", "Apple Days", "Zed Quartet", 2019, railNow.Add(-2 * time.Hour)},
		{"R/Blue/01.flac", "Blue Hour", "Mu Ensemble", 1982, railNow.Add(36 * time.Hour)},
		{"R/Cedar/01.flac", "Cedar", "The Aardvarks", 0, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)},
		{"R/Dune/01.flac", "Dune Sea", "Kilo Trio", 12345, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"R/Ember/01.flac", "Ember", "Lumen", 74, time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"R/Fable/01.flac", "Fable", "Nadir", 1982, time.Date(2023, 2, 2, 0, 0, 0, 0, time.UTC)},
		{"R/Gale/01.flac", "Gale", "Orbit", 2001, time.Time{}},
	})
	type b = playerBucketDTO
	for _, c := range []struct {
		query, kind string
		want        []b
	}{
		{"sort=recent", "dates", []b{{"this-month", 0, 2}, {"this-year", 2, 1}, {"2025", 3, 2}, {"2023", 5, 1}, {"?", 6, 1}}},
		{"", "dates", []b{{"this-month", 0, 2}, {"this-year", 2, 1}, {"2025", 3, 2}, {"2023", 5, 1}, {"?", 6, 1}}},
		{"sort=year", "years", []b{{"2019", 0, 1}, {"2001", 1, 1}, {"1982", 2, 2}, {"?", 4, 3}}},
		{"sort=title", "letters", []b{{"A", 0, 1}, {"B", 1, 1}, {"C", 2, 1}, {"D", 3, 1}, {"E", 4, 1}, {"F", 5, 1}, {"G", 6, 1}}},
		{"sort=artist", "letters", []b{{"A", 0, 1}, {"K", 1, 1}, {"L", 2, 1}, {"M", 3, 1}, {"N", 4, 1}, {"O", 5, 1}, {"Z", 6, 1}}},
		// The name filter narrows the rail with the list: "a" leaves Apple Days,
		// Cedar (by The Aardvarks), Dune Sea, Fable and Gale.
		{"sort=year&q=a", "years", []b{{"2019", 0, 1}, {"2001", 1, 1}, {"1982", 2, 1}, {"?", 3, 2}}},
		{"sort=recent&q=a", "dates", []b{{"this-month", 0, 1}, {"this-year", 1, 1}, {"2025", 2, 1}, {"2023", 3, 1}, {"?", 4, 1}}},
	} {
		kind, got, kindSent, bucketsSent := railOf(t, srv, "/api/player/albums?limit=2&"+c.query, "albums")
		if !kindSent || kind != c.kind || !bucketsSent || !slices.Equal(got, c.want) {
			t.Errorf("%q: rail %q (sent %v), buckets %v (sent %v); want %q, %v", c.query, kind, kindSent, got, bucketsSent, c.kind, c.want)
		}
		_, _, kindSent, bucketsSent = railOf(t, srv, "/api/player/albums?limit=2&offset=2&"+c.query, "albums")
		if kindSent || bucketsSent {
			t.Errorf("%q page 2 carries the rail (%v) or its buckets (%v): they go on the first page alone", c.query, kindSent, bucketsSent)
		}
	}
	if kind, got, kindSent, _ := railOf(t, srv, "/api/player/artists", "artists"); kind != "letters" || len(got) != 7 {
		t.Errorf("artists: rail %q (sent %v), buckets %v; want letters over seven", kind, kindSent, got)
	}
}

// TestTheDateRailFilesByTheReadersCalendar holds Recently Added's periods to
// the reader's calendar: tz, the minutes getTimezoneOffset gives (positive
// west of UTC), moves the month's and the year's first instant, an album at
// or after this month's (one in the future with it) is this month's, one at
// or after the year's earlier this year's, each earlier album its year's in
// that zone and one never indexed "?"; in January nothing is earlier this
// year; a tz past 14 hours, or not a whole number, is refused.
func TestTheDateRailFilesByTheReadersCalendar(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	now := railNow
	srv.testNow = func() time.Time { return now }
	seedRailLibrary(t, srv, cfg.DataDir, []railAlbum{
		{"D/Today/01.flac", "Today", "A", 0, railNow.Add(-time.Hour)},
		{"D/Ahead/01.flac", "Ahead", "B", 0, railNow.AddDate(0, 0, 30)},
		{"D/Spring/01.flac", "Spring", "C", 0, time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC)},
		// 23:30 UTC on the last day of a month and of a year: the next
		// month's and the next year's an hour east of UTC.
		{"D/Halloween/01.flac", "Halloween", "D", 0, time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)},
		{"D/Eve/01.flac", "Eve", "E", 0, time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC)},
		{"D/Summer/01.flac", "Summer", "F", 0, time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"D/Old/01.flac", "Old", "G", 0, time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"D/Never/01.flac", "Never", "H", 0, time.Time{}},
	})
	type b = playerBucketDTO
	utc := []b{{"this-month", 0, 2}, {"this-year", 2, 2}, {"2025", 4, 2}, {"2019", 6, 1}, {"?", 7, 1}}
	for _, c := range []struct {
		tz   string
		want []b
	}{
		{"", utc},
		{"&tz=0", utc},
		// UTC-1 (getTimezoneOffset 60): Halloween is 22:30 on 31 October there,
		// Eve 22:30 on 31 December, as in UTC.
		{"&tz=60", utc},
		// UTC+1 (-60): Halloween is 1 November's, this month; Eve 1 January's,
		// earlier this year.
		{"&tz=-60", []b{{"this-month", 0, 3}, {"this-year", 3, 2}, {"2025", 5, 1}, {"2019", 6, 1}, {"?", 7, 1}}},
		// The ends of the range: UTC-14 files as UTC does here, UTC+14 as UTC+1.
		{"&tz=840", utc},
		{"&tz=-840", []b{{"this-month", 0, 3}, {"this-year", 3, 2}, {"2025", 5, 1}, {"2019", 6, 1}, {"?", 7, 1}}},
	} {
		kind, got, _, _ := railOf(t, srv, "/api/player/albums?sort=recent"+c.tz, "albums")
		if kind != "dates" || !slices.Equal(got, c.want) {
			t.Errorf("tz %q: rail %q, buckets %v; want dates, %v", c.tz, kind, got, c.want)
		}
	}
	now = time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	if _, got, _, _ := railOf(t, srv, "/api/player/albums?sort=recent", "albums"); !slices.Equal(got,
		[]b{{"this-month", 0, 4}, {"2025", 4, 2}, {"2019", 6, 1}, {"?", 7, 1}}) {
		t.Errorf("in January: buckets %v; want this month's four, nothing earlier this year, then 2025, 2019 and ?", got)
	}
	for _, tz := range []string{"900", "-841", "841", "1.5", "abc", "%20"} {
		if w, _ := playerGet(t, srv, "/api/player/albums?sort=recent&tz="+tz); w.Code != http.StatusBadRequest {
			t.Errorf("tz=%s: status %d, want 400", tz, w.Code)
		}
	}
}

// TestTheYearSortFilesYearsItCannotPlaceUnderUnknown holds the Year sort to
// the phone's: a year outside 1000-9999 (12345, 74) sorts with the albums
// that have none, last, so "?" is one run of the three, while each tile keeps
// the year it is tagged with.
func TestTheYearSortFilesYearsItCannotPlaceUnderUnknown(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	seedRailLibrary(t, srv, cfg.DataDir, []railAlbum{
		{"Y/Wide/01.flac", "Wide", "A", 12345, railNow},
		{"Y/Early/01.flac", "Early", "B", 74, railNow},
		{"Y/None/01.flac", "None", "C", 0, railNow},
		{"Y/Mid/01.flac", "Mid", "D", 2001, railNow},
		{"Y/Late/01.flac", "Late", "E", 1999, railNow},
	})
	p := getFiltered(t, srv, "/api/player/albums?sort=year", "albums", "title")
	if !slices.Equal(p.names[:2], []string{"Mid", "Late"}) || !slices.Equal(p.buckets(t),
		[]playerBucketDTO{{"2001", 0, 1}, {"1999", 1, 1}, {"?", 2, 3}}) {
		t.Errorf("year sort: rows %q, buckets %v; want Mid, Late, then the three years it cannot place in one ? run", p.names, p.buckets(t))
	}
	var rows []playerAlbumDTO
	if err := json.Unmarshal(p.keys["albums"], &rows); err != nil {
		t.Fatal(err)
	}
	tagged := map[string]int{}
	for _, r := range rows {
		tagged[r.Title] = r.Year
	}
	if tagged["Wide"] != 12345 || tagged["Early"] != 74 || tagged["None"] != 0 {
		t.Errorf("the tiles' years %v: each keeps the year it is tagged with", tagged)
	}
}
