package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	_ "modernc.org/sqlite"
)

func doReqMatch(t *testing.T, srv *Server, method, path, token, deviceToken, match, body string) *http.Response {
	t.Helper()
	resp := doReq(t, srv, method, path, token, deviceToken, body)
	if match == "" {
		return resp
	}
	resp.Body.Close()
	hs := httpTestServer(t, srv)
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, hs+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if deviceToken != "" {
		req.Header.Set("X-Device-Token", deviceToken)
	}
	req.Header.Set("If-None-Match", match)
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFavoritesConditionalGet(t *testing.T) {
	token, dt, srv := newFavoritesTestServer(t)
	first := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("get: %d", first.StatusCode)
	}
	etag := first.Header.Get("ETag")
	first.Body.Close()
	if etag == "" || !strings.Contains(etag, ".0") {
		t.Fatalf("never-stored etag %q", etag)
	}
	star := doReqMatch(t, srv, http.MethodGet, "/v1/favorites", token, dt, "*", "")
	if star.StatusCode != http.StatusOK {
		t.Fatalf("* on a never-stored bridge: %d, want 200", star.StatusCode)
	}
	star.Body.Close()
	exact := doReqMatch(t, srv, http.MethodGet, "/v1/favorites", token, dt, etag, "")
	if exact.StatusCode != http.StatusNotModified {
		t.Fatalf("exact epoch.0: %d, want 304", exact.StatusCode)
	}
	exact.Body.Close()

	put := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":1,"tracks":[{"path":"a.flac","favoritedAt":1}],"albums":[]}`)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", put.StatusCode)
	}
	put.Body.Close()
	again := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	stored := again.Header.Get("ETag")
	again.Body.Close()
	weak := doReqMatch(t, srv, http.MethodGet, "/v1/favorites", token, dt, "W/"+stored, "")
	if weak.StatusCode != http.StatusNotModified || weak.Header.Get("ETag") != stored {
		t.Fatalf("weak match: %d etag %q, want 304 %q", weak.StatusCode, weak.Header.Get("ETag"), stored)
	}
	weak.Body.Close()
	any := doReqMatch(t, srv, http.MethodGet, "/v1/favorites", token, dt, "*", "")
	if any.StatusCode != http.StatusNotModified {
		t.Fatalf("* once stored: %d, want 304", any.StatusCode)
	}
	any.Body.Close()
}

func TestPlaylistListConditionalGet(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":200,"items":[{"position":0,"path":"A/B/c.flac"}]}`
	put := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", put.StatusCode)
	}
	var stored playlistStoredResponse
	if err := json.NewDecoder(put.Body).Decode(&stored); err != nil || stored.LastModifiedAt != 200 {
		t.Fatalf("stored %+v err %v", stored, err)
	}
	put.Body.Close()

	list := doReq(t, srv, http.MethodGet, "/v1/playlists", token, dt, "")
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", list.StatusCode)
	}
	etag := list.Header.Get("ETag")
	var payload playlistsListResponse
	if err := json.NewDecoder(list.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Epoch == "" || !strings.HasPrefix(etag, `"`+payload.Epoch+".") {
		t.Fatalf("epoch %q etag %q", payload.Epoch, etag)
	}
	cached := doReqMatch(t, srv, http.MethodGet, "/v1/playlists", token, dt, etag, "")
	if cached.StatusCode != http.StatusNotModified {
		t.Fatalf("list 304: %d", cached.StatusCode)
	}
	cached.Body.Close()

	renamed := `{"id":"` + id + `","name":"Other","lastModifiedAt":300,"items":[{"position":0,"path":"A/B/c.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, renamed); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	moved := doReqMatch(t, srv, http.MethodGet, "/v1/playlists", token, dt, etag, "")
	if moved.StatusCode != http.StatusOK {
		t.Fatalf("stale list etag: %d, want 200", moved.StatusCode)
	}
	moved.Body.Close()
}

func TestPlaylistMatchingBaseOnTheWire(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "6e0b3a5d-9f32-4d4b-8c88-1a2f3e4d5c6b"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":5000,"items":[{"position":0,"path":"a.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	behind := `{"id":"` + id + `","name":"Later","lastModifiedAt":1000,"baseLastModifiedAt":5000,"items":[{"position":0,"path":"b.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, behind)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("behind-clock: %d", resp.StatusCode)
	}
	var stored playlistStoredResponse
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if stored.LastModifiedAt != 5001 {
		t.Fatalf("stamp %d, want 5001", stored.LastModifiedAt)
	}

	miss := `{"id":"` + id + `","name":"Nope","lastModifiedAt":9000,"baseLastModifiedAt":1000,"items":[{"position":0,"path":"c.flac"}]}`
	bad := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, miss)
	if bad.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch: %d", bad.StatusCode)
	}
	var stale playlistStaleResponse
	if err := json.NewDecoder(bad.Body).Decode(&stale); err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if stale.Error != "base_mismatch" || stale.Server.Name != "Later" {
		t.Fatalf("409 %+v", stale)
	}
}

func TestMatchingBaseRevivesADeletedPlaylistOnTheWire(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "7f1c4b6e-0a43-4e5c-9d99-2b3a4f5e6d7c"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":100,"items":[{"position":0,"path":"a.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, dt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	revive := `{"id":"` + id + `","name":"Changed","lastModifiedAt":100,"baseLastModifiedAt":100,"items":[{"position":0,"path":"a.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, revive)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revive: %d", resp.StatusCode)
	}
	resp.Body.Close()
	got := doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dt, "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get after revive: %d, want 200", got.StatusCode)
	}
	var dto playlistDTO
	if err := json.NewDecoder(got.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if dto.Name != "Changed" {
		t.Fatalf("name %q, want Changed", dto.Name)
	}
}

func TestFavorites304DoesNotReadTheRows(t *testing.T) {
	token, dt, srv, _, dbPath := newFavoritesHarness(t)
	put := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":1,"tracks":[{"path":"a.flac","favoritedAt":1}],"albums":[]}`)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", put.StatusCode)
	}
	put.Body.Close()
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE favorite_tracks`); err != nil {
		t.Fatal(err)
	}
	resp := doReqMatch(t, srv, http.MethodGet, "/v1/favorites", token, dt, "*", "")
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional get: %d, want 304", resp.StatusCode)
	}
	resp.Body.Close()
}

func doReqAddedValidators(t *testing.T, srv *Server, path, token, deviceToken string, validators ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, httpTestServer(t, srv)+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if deviceToken != "" {
		req.Header.Set("X-Device-Token", deviceToken)
	}
	for _, v := range validators {
		req.Header.Add("If-None-Match", v)
	}
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIfNoneMatchJoinsHeaderLines(t *testing.T) {
	token, dt, fav, _, _ := newFavoritesHarness(t)
	put := doReq(t, fav, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":1,"tracks":[{"path":"a.flac","favoritedAt":1}],"albums":[]}`)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("favorites put: %d", put.StatusCode)
	}
	put.Body.Close()
	got := doReq(t, fav, http.MethodGet, "/v1/favorites", token, dt, "")
	etag := got.Header.Get("ETag")
	got.Body.Close()
	resp := doReqAddedValidators(t, fav, "/v1/favorites", token, dt, `"nope"`, etag)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("favorites: %d, want 304", resp.StatusCode)
	}
	resp.Body.Close()

	ptoken, pdt, play := newPlaylistTestServer(t)
	list := doReq(t, play, http.MethodGet, "/v1/playlists", ptoken, pdt, "")
	petag := list.Header.Get("ETag")
	list.Body.Close()
	presp := doReqAddedValidators(t, play, "/v1/playlists", ptoken, pdt, `"nope"`, petag)
	if presp.StatusCode != http.StatusNotModified {
		t.Fatalf("playlists: %d, want 304", presp.StatusCode)
	}
	presp.Body.Close()
}

func TestATombstonedBaseMismatchCarriesDeleted(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "7f1c4b6e-0a43-4e5c-9d99-2b3a4f5e6d7c"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":100,"items":[{"position":0,"path":"a.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, dt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	mismatch := `{"id":"` + id + `","name":"Changed","lastModifiedAt":100,"baseLastModifiedAt":1,"items":[{"position":0,"path":"a.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, mismatch)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	var stale playlistStaleResponse
	if err := json.NewDecoder(resp.Body).Decode(&stale); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if stale.Error != "base_mismatch" || !stale.Server.Deleted {
		t.Fatalf("409 %+v", stale)
	}
}

func TestIdenticalPlaylistBodyWithAStaleBaseIsUnchanged(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "7f1c4b6e-0a43-4e5c-9d99-2b3a4f5e6d7c"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":1000,"items":[{"position":0,"path":"a.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	again := `{"id":"` + id + `","name":"Favs","lastModifiedAt":1000,"baseLastModifiedAt":1,"items":[{"position":0,"path":"a.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, again)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLegacyIdenticalPlaylistPutKeepsTheStampGuard(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "9b3e6d80-2c65-407e-9f11-4d5c6b708f9e"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":5000,"items":[{"position":0,"path":"a.flac"}]}`
	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	older := `{"id":"` + id + `","name":"Favs","lastModifiedAt":1000,"items":[{"position":0,"path":"a.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, older)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("older identical: %d, want 409", resp.StatusCode)
	}
	var stale playlistStaleResponse
	if err := json.NewDecoder(resp.Body).Decode(&stale); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if stale.Error != "stale" {
		t.Fatalf("409 %+v, want stale", stale)
	}
	newer := `{"id":"` + id + `","name":"Favs","lastModifiedAt":9000,"items":[{"position":0,"path":"a.flac"}]}`
	resp = doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, newer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("newer identical: %d, want 200", resp.StatusCode)
	}
	var stored playlistStoredResponse
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if stored.LastModifiedAt != 9000 {
		t.Fatalf("stored stamp %d, want 9000", stored.LastModifiedAt)
	}
}

func TestLegacyFavoritesPutOmitsTheBaseAndKeepsAnOmittedKey(t *testing.T) {
	token, dt, srv, _, _ := newFavoritesHarness(t)
	first := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":2000,"tracks":[{"path":"a.flac","favoritedAt":2000},{"path":"b.flac","favoritedAt":2000}],"albums":[]}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first: %d", first.StatusCode)
	}
	first.Body.Close()
	second := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":1000,"tracks":[{"path":"a.flac","favoritedAt":1000}],"albums":[]}`)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second: %d", second.StatusCode)
	}
	second.Body.Close()
	got := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	var doc struct {
		Tracks []favoriteTrackDTO `json:"tracks"`
	}
	if err := json.NewDecoder(got.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	have := map[string]bool{}
	for _, row := range doc.Tracks {
		have[row.Path] = true
	}
	if !have["a.flac"] || !have["b.flac"] {
		t.Fatalf("tracks %+v", doc.Tracks)
	}
}

func TestLegacyFavoritesPutWithAnOlderStampAddsTheNewKey(t *testing.T) {
	token, dt, srv, _, _ := newFavoritesHarness(t)
	first := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":5000,"tracks":[{"path":"a.flac","favoritedAt":5000}],"albums":[]}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first: %d", first.StatusCode)
	}
	first.Body.Close()
	second := doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt,
		`{"lastModifiedAt":1000,"tracks":[{"path":"a.flac","favoritedAt":1000},{"path":"b.flac","favoritedAt":1000}],"albums":[]}`)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second: %d", second.StatusCode)
	}
	second.Body.Close()
	got := doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, "")
	var doc struct {
		Tracks []favoriteTrackDTO `json:"tracks"`
	}
	if err := json.NewDecoder(got.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	have := map[string]int64{}
	for _, row := range doc.Tracks {
		have[row.Path] = row.FavoritedAt
	}
	if have["a.flac"] != 1000 || have["b.flac"] != 1000 {
		t.Fatalf("tracks %+v", doc.Tracks)
	}
}

func TestHealthAdvertisesThePhase1Keys(t *testing.T) {
	token, _, fav := newFavoritesTestServer(t)
	resp := doReq(t, fav, http.MethodGet, "/v1/health", token, "", "")
	var hr struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	seen := map[string]bool{}
	for _, f := range hr.Features {
		seen[f] = true
	}
	if !seen["favoritesRevisions"] {
		t.Fatalf("features %v", hr.Features)
	}
	if seen["playlistListRevision"] || seen["syncEvents"] || seen["firstIndexedAt"] {
		t.Fatalf("favorites-only bridge advertised %v", hr.Features)
	}
}
