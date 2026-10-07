package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
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
