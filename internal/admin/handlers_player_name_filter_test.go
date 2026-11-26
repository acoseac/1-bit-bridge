package admin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The lists' name filter (B1021 PR 3a): q on the four catalog lists and the
// playlists, folded as Catalog.Search folds a query, applied before the
// sort, the buckets and the paging.

// seedNameFilterLibrary lays down rows whose names only the fold matches: an
// article ("The Beatles", which "beatles" finds) and a diacritic ("Éric",
// which "eric" finds), on every list the filter narrows.
func seedNameFilterLibrary(t *testing.T, srv *Server) {
	t.Helper()
	ctx := t.Context()
	st := srv.deps.Manifest
	for _, r := range []struct{ path, album, artist, genre, composer string }{
		{"B/Abbey/01.flac", "Abbey Road", "The Beatles", "The Beatles Sound", "The Beatles"},
		{"B/Help/01.flac", "Help", "The Beatles", "The Beatles Sound", "The Beatles"},
		{"B/Revolver/01.flac", "Revolver", "The Beatles", "Zydeco", "Lennon"},
		{"Z/Covers/01.flac", "Beatles Forever", "Zebra Trio", "Zydeco", "Ellington"},
		{"E/Eclats/01.flac", "Éclats", "Éric Vallon", "Éric's Folk", "Éric Vallon"},
		{"M/Nocturnes/01.flac", "Nocturnes", "Mu Ensemble", "Acid Jazz", "Chopin"},
	} {
		tr := &manifest.Track{Path: r.path, Title: "T", Album: r.album, AlbumArtist: r.artist,
			Artist: r.artist, Genre: r.genre, Composer: r.composer, Codec: "FLAC", Size: 1000,
			ModTime: time.Unix(1, 0)}
		if err := st.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertDeviceRegistration(ctx, testDeviceToken, "tok-1", "Kitchen iPad"); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"The Beatles Hits", "Éric's Evening", "Road Trip"} {
		id := strings.Repeat(string(rune('a'+i)), 8) + "-2222-4222-8222-222222222222"
		row := manifest.PlaylistRow{ID: id, Name: name, LastModifiedAt: time.Now().UnixNano()}
		if err := st.UpsertPlaylist(ctx, testDeviceToken, row, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// filteredPage is one answer of a list endpoint: its status, the names of
// its rows in order, and its body's keys as they were sent.
type filteredPage struct {
	status int
	names  []string
	ids    []string
	keys   map[string]json.RawMessage
}

func (p filteredPage) total(t *testing.T) int {
	t.Helper()
	var n int
	if err := json.Unmarshal(p.keys["total"], &n); err != nil {
		t.Fatalf("total: %v", err)
	}
	return n
}

func (p filteredPage) buckets(t *testing.T) []playerBucketDTO {
	t.Helper()
	raw, ok := p.keys["buckets"]
	if !ok {
		return nil
	}
	var b []playerBucketDTO
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("buckets: %v", err)
	}
	return b
}

// getFiltered asks target and reads its rows from items, naming each by
// nameKey.
func getFiltered(t *testing.T, srv *Server, target, items, nameKey string) filteredPage {
	t.Helper()
	w, _ := playerGet(t, srv, target)
	p := filteredPage{status: w.Code, keys: map[string]json.RawMessage{}}
	if w.Code != http.StatusOK {
		return p
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p.keys); err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(p.keys[items], &rows); err != nil {
		t.Fatalf("%s: rows: %v", target, err)
	}
	for _, r := range rows {
		name, _ := r[nameKey].(string)
		id, _ := r["id"].(string)
		p.names, p.ids = append(p.names, name), append(p.ids, id)
	}
	return p
}

// TestTheListsTakeANameFilter holds q on each list: "beatles" finds "The
// Beatles" and "eric" finds "Éric …" through the fold; the total, the rows
// and the buckets are the filtered set's, page 2 is the filtered list's, and
// the buckets are absent there; a query whose fold is empty filters nothing,
// and 257 bytes are refused. The playlists, one page, answer the whole
// filtered collection.
func TestTheListsTakeANameFilter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedNameFilterLibrary(t, srv)
	for _, c := range []struct {
		path, items, name string
		rows              int
		beatles, eric     []string
	}{
		{"/api/player/albums?sort=title", "albums", "title", 6, []string{"Abbey Road", "Beatles Forever", "Help", "Revolver"}, []string{"Éclats"}},
		{"/api/player/artists?", "artists", "name", 4, []string{"The Beatles"}, []string{"Éric Vallon"}},
		{"/api/player/genres?", "entries", "name", 4, []string{"The Beatles Sound"}, []string{"Éric's Folk"}},
		{"/api/player/composers?", "entries", "name", 5, []string{"The Beatles"}, []string{"Éric Vallon"}},
	} {
		all := getFiltered(t, srv, c.path+"&limit=200", c.items, c.name)
		if all.status != http.StatusOK || all.total(t) != c.rows || len(all.ids) != c.rows {
			t.Fatalf("%s: status %d, %d rows: the case's library is not seeded as it reads", c.path, all.status, len(all.names))
		}
		for q, want := range map[string][]string{"beatles": c.beatles, "eric": c.eric} {
			got := getFiltered(t, srv, c.path+"&limit=200&q="+q, c.items, c.name)
			if got.status != http.StatusOK || !slices.Equal(got.names, want) || got.total(t) != len(want) {
				t.Errorf("%s q=%s: status %d, rows %q, total %d; want %q", c.path, q, got.status, got.names, got.total(t), want)
			}
		}
		// Every row with an A in its fold: page 2 of it is the filtered
		// list's, and the buckets are of the filtered set, on page 1 alone.
		wide := getFiltered(t, srv, c.path+"&limit=200&q=a", c.items, c.name)
		if len(wide.ids) < 2 || len(wide.ids) == len(all.ids) {
			t.Fatalf("%s q=a: %d rows of %d: the case filters nothing it can page", c.path, len(wide.ids), len(all.ids))
		}
		page2 := getFiltered(t, srv, c.path+"&limit=1&offset=1&q=a", c.items, c.name)
		if !slices.Equal(page2.ids, wide.ids[1:2]) || page2.total(t) != len(wide.ids) {
			t.Errorf("%s q=a page 2: rows %q, total %d; want %q of %d", c.path, page2.names, page2.total(t), wide.names[1:2], len(wide.ids))
		}
		if _, sent := page2.keys["buckets"]; sent {
			t.Errorf("%s q=a page 2 carries buckets: they go on the first page alone", c.path)
		}
		if c.items != "entries" || strings.Contains(c.path, "composers") {
			counted := 0
			for _, b := range wide.buckets(t) {
				counted += b.Count
			}
			if counted != len(wide.ids) {
				t.Errorf("%s q=a: buckets %v count %d rows of the %d filtered", c.path, wide.buckets(t), counted, len(wide.ids))
			}
		}
		for _, empty := range []string{"%20%20", "%21%3F"} {
			if got := getFiltered(t, srv, c.path+"&limit=200&q="+empty, c.items, c.name); !slices.Equal(got.ids, all.ids) {
				t.Errorf("%s q=%s, whose fold is empty: rows %q, want every row", c.path, empty, got.names)
			}
		}
		if got := getFiltered(t, srv, c.path+"&q="+strings.Repeat("b", 257), c.items, c.name); got.status != http.StatusBadRequest {
			t.Errorf("%s with a q of 257 bytes: status %d, want 400", c.path, got.status)
		}
		if got := getFiltered(t, srv, c.path+"&q="+strings.Repeat("b", 256), c.items, c.name); got.status != http.StatusOK {
			t.Errorf("%s with a q of 256 bytes: status %d, want 200", c.path, got.status)
		}
	}

	playlists := func(q string) filteredPage {
		return getFiltered(t, srv, "/api/player/playlists"+q, "collections", "name")
	}
	if got := playlists(""); len(got.names) != 3 {
		t.Fatalf("playlists: %q, want all three", got.names)
	}
	for q, want := range map[string][]string{"?q=beatles": {"The Beatles Hits"}, "?q=eric": {"Éric's Evening"}, "?q=%21": {"The Beatles Hits", "Éric's Evening", "Road Trip"}} {
		got := playlists(q)
		slices.Sort(got.names)
		slices.Sort(want)
		if got.status != http.StatusOK || !slices.Equal(got.names, want) {
			t.Errorf("playlists%s: status %d, %q; want %q", q, got.status, got.names, want)
		}
	}
	if got := playlists("?q=" + strings.Repeat("b", 257)); got.status != http.StatusBadRequest {
		t.Errorf("playlists with a q of 257 bytes: status %d, want 400", got.status)
	}

	// A selective filter leaves the cached catalog's own lists as they were:
	// the unfiltered lists after it are row for row the ones before it.
	for _, c := range []struct{ path, items string }{
		{"/api/player/artists?limit=200", "artists"}, {"/api/player/genres?limit=200", "entries"}, {"/api/player/composers?limit=200", "entries"},
	} {
		before := getFiltered(t, srv, c.path, c.items, "name")
		getFiltered(t, srv, c.path+"&q=zydeco", c.items, "name")
		getFiltered(t, srv, c.path+"&q=vallon", c.items, "name")
		if after := getFiltered(t, srv, c.path, c.items, "name"); !slices.Equal(after.ids, before.ids) || len(before.ids) < 3 {
			t.Errorf("%s after a filter: %q, before it %q: the filter wrote into the catalog's list", c.path, after.names, before.names)
		}
	}
}

// TestAFilteredGenreListDrawsNoRail holds the genre rail to the list before
// its name filter: genres are ordered by track count, so a filter that leaves
// one genre, which reads in letter order, still draws no rail, while the
// composers, alphabetical, keep theirs filtered.
func TestAFilteredGenreListDrawsNoRail(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedNameFilterLibrary(t, srv)
	if all := getFiltered(t, srv, "/api/player/genres", "entries", "name"); all.buckets(t) != nil || len(all.names) != 4 {
		t.Fatalf("genres: %q with buckets %v: the case's genre list is not count-ordered", all.names, all.buckets(t))
	}
	one := getFiltered(t, srv, "/api/player/genres?q=acid", "entries", "name")
	if !slices.Equal(one.names, []string{"Acid Jazz"}) {
		t.Fatalf("genres q=acid: %q", one.names)
	}
	if _, sent := one.keys["buckets"]; sent {
		t.Errorf("genres q=acid sends buckets %v: the rail was decided on the filtered list", one.buckets(t))
	}
	if b := getFiltered(t, srv, "/api/player/composers?q=beatles", "entries", "name").buckets(t); len(b) != 1 || b[0].Key != "B" {
		t.Errorf("composers q=beatles: buckets %v, want the filtered list's one letter", b)
	}
}
