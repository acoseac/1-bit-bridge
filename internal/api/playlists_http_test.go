package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// httpTestServer spins up an httptest.Server for srv and returns its base
// URL, registering teardown. One per request keeps the tests simple — the
// handler chain is stateless across requests for these endpoints.
func httpTestServer(t *testing.T, srv *Server) string {
	t.Helper()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL
}

// newPlaylistTestServer wires a real manifest.Store as the playlist store
// behind the authed() middleware, returning the test server + a valid
// bearer token.
func newPlaylistTestServer(t *testing.T) (string, string, *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{t.TempDir()},
		ListenAddress: ":7788",
		LibraryName:   "T",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	mstore, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mstore.Close() })
	srv := New(cfg, authStore, nil, "fp").WithDeviceRegistrar(mstore).WithPlaylistStore(mstore)
	return raw, "deadbeef", srv
}

func doReq(t *testing.T, srv *Server, method, path, token, deviceToken, body string) *http.Response {
	t.Helper()
	hs := httpTestServer(t, srv)
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPlaylistHTTPRoundTrip(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":200,"items":[{"position":0,"path":"A/B/c.flac"},{"position":1,"originFingerprint":"local","originPath":"x","title":"T","artist":"Ar"}]}`

	// PUT → 200 stored.
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// GET → full playlist.
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dt, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var got playlistDTO
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Name != "Favs" || len(got.Items) != 2 || got.Items[0].Path != "A/B/c.flac" {
		t.Errorf("GET body mismatch: %+v", got)
	}

	// LIST → one summary.
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists", token, dt, "")
	var list playlistsListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Playlists) != 1 || list.Playlists[0].TrackCount != 2 {
		t.Errorf("LIST mismatch: %+v", list)
	}

	// DELETE → gone.
	resp = doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, dt, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dt, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPlaylistHTTPStale409CarriesServerCopy(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "aaaaaaaa-0000-0000-0000-000000000001"
	newer := `{"id":"` + id + `","name":"v2","lastModifiedAt":300,"items":[{"position":0,"path":"x.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, newer)
	resp.Body.Close()

	older := `{"id":"` + id + `","name":"v1","lastModifiedAt":100,"items":[{"position":0,"path":"y.flac"}]}`
	resp = doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, older)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale PUT = %d, want 409", resp.StatusCode)
	}
	var stale playlistStaleResponse
	json.NewDecoder(resp.Body).Decode(&stale)
	resp.Body.Close()
	if stale.Error != "stale" || stale.Server.Name != "v2" || stale.Server.LastModifiedAt != 300 {
		t.Errorf("409 body missing/incorrect server copy: %+v", stale)
	}
}

func TestPlaylistHTTPLocalXorForeign400(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "bbbbbbbb-0000-0000-0000-000000000002"
	// Item sets BOTH path and originFingerprint → 400.
	bad := `{"id":"` + id + `","name":"x","lastModifiedAt":1,"items":[{"position":0,"path":"a.flac","originFingerprint":"AB"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both-path-and-foreign = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	// Neither set → 400.
	bad2 := `{"id":"` + id + `","name":"x","lastModifiedAt":1,"items":[{"position":0,"title":"only"}]}`
	resp = doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, bad2)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("neither = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPlaylistHTTPDeviceTokenRequired(t *testing.T) {
	token, _, srv := newPlaylistTestServer(t)
	// No X-Device-Token header → 400.
	resp := doReq(t, srv, http.MethodGet, "/v1/playlists", token, "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing device token = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPlaylistHTTPFeatureOff404(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	// No WithPlaylistStore → feature off.
	srv := New(cfg, authStore, nil, "fp")
	resp := doReq(t, srv, http.MethodGet, "/v1/playlists", raw, "deadbeef", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("feature-off = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
