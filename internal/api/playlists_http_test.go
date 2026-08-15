package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// TestPlaylistHTTPListCarriesDeletedIds pins the sweep-propagation wire
// contract on GET /v1/playlists: a tombstoned playlist's id rides
// `deletedIds` and is absent from `playlists`; with nothing deleted the
// field is omitted entirely (`omitempty` — older clients never see it).
func TestPlaylistHTTPListCarriesDeletedIds(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	live := "aaaaaaaa-1111-0000-0000-000000000001"
	doomed := "aaaaaaaa-1111-0000-0000-000000000002"
	for _, id := range []string{live, doomed} {
		body := `{"id":"` + id + `","name":"P","lastModifiedAt":100,"items":[{"position":0,"path":"a.flac"}]}`
		resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200", id, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Nothing deleted → the deletedIds KEY is absent from the JSON
	// (omitempty). Unmarshal into a map rather than substring-scan the
	// body (Gemini + CodeRabbit on #699); the GET status is asserted so
	// a 500-with-no-body cannot pass vacuously.
	resp := doReq(t, srv, http.MethodGet, "/v1/playlists", token, dt, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("initial GET = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read initial GET body: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("decode initial GET body: %v", err)
	}
	if _, present := asMap["deletedIds"]; present {
		t.Errorf("no-tombstones list body should omit the deletedIds key: %s", raw)
	}

	// Tombstone one → it moves from playlists into deletedIds.
	resp = doReq(t, srv, http.MethodDelete, "/v1/playlists/"+doomed, token, dt, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, srv, http.MethodGet, "/v1/playlists", token, dt, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("post-tombstone GET = %d, want 200", resp.StatusCode)
	}
	var list playlistsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		resp.Body.Close()
		t.Fatalf("decode post-tombstone GET body: %v", err)
	}
	resp.Body.Close()
	if len(list.Playlists) != 1 || list.Playlists[0].ID != live {
		t.Errorf("live playlists = %+v, want only %s", list.Playlists, live)
	}
	if len(list.DeletedIds) != 1 || list.DeletedIds[0] != doomed {
		t.Errorf("deletedIds = %v, want [%s]", list.DeletedIds, doomed)
	}
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

func TestPlaylistHTTPDuplicatePosition400(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "cccccccc-0000-0000-0000-000000000003"
	body := `{"id":"` + id + `","name":"x","lastModifiedAt":1,"items":[{"position":0,"path":"a.flac"},{"position":0,"path":"b.flac"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate position = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPlaylistHTTPPartialForeign400(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "dddddddd-0000-0000-0000-000000000004"
	// Foreign item with originFingerprint but no originPath → 400.
	body := `{"id":"` + id + `","name":"x","lastModifiedAt":1,"items":[{"position":0,"originFingerprint":"AB"}]}`
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("partial-foreign item = %d, want 400", resp.StatusCode)
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

// TestPlaylistHTTPCrossDeviceRestore pins the user-wide wire contract:
// a playlist backed up by one device is listable, restorable, updatable
// and deletable from a different device (different X-Device-Token).
func TestPlaylistHTTPCrossDeviceRestore(t *testing.T) {
	token, dtA, srv := newPlaylistTestServer(t)
	dtB := "beefbeef" // a second device of the same user
	id := "eeeeeeee-0000-0000-0000-000000000005"
	body := `{"id":"` + id + `","name":"A's Favs","lastModifiedAt":100,"items":[{"position":0,"path":"A/B/c.flac"}]}`

	// Device A backs up.
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dtA, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT from devA = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Device B sees it in the list…
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists", token, dtB, "")
	var list playlistsListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Playlists) != 1 || list.Playlists[0].ID != id {
		t.Fatalf("devB list mismatch: %+v", list)
	}

	// …restores the full playlist…
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dtB, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("devB GET = %d, want 200", resp.StatusCode)
	}
	var got playlistDTO
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Name != "A's Favs" || len(got.Items) != 1 {
		t.Errorf("devB restore mismatch: %+v", got)
	}

	// …updates it with a newer clock (no playlist_conflict 409 anymore)…
	newer := `{"id":"` + id + `","name":"B's edit","lastModifiedAt":200,"items":[{"position":0,"path":"X/y.flac"}]}`
	resp = doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dtB, newer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("devB PUT = %d, want 200 (cross-device writes allowed)", resp.StatusCode)
	}
	resp.Body.Close()

	// …a STALE cross-device write still 409s (LWW is the only guard)…
	older := `{"id":"` + id + `","name":"stale","lastModifiedAt":50,"items":[]}`
	resp = doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dtA, older)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale cross-device PUT = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// …and deletes it.
	resp = doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, dtB, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("devB DELETE = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dtA, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after cross-device delete = %d, want 404", resp.StatusCode)
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

// buildPlaylistItemsJSON renders a JSON array of n minimal (`{}`) items — the
// Q22 amplification vector: parseable-but-empty items that decode into full
// structs. Each is ~3 bytes with its separator, so a modest body yields a
// huge item count.
func buildPlaylistItemsJSON(n int) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// TestPlaylistHTTPTooManyItems400 (Q22) pins the decode-time item-count cap: a
// PUT whose items array exceeds maxPlaylistItems is rejected 400 with the
// count-guard message — proving the guard fired during decode, not a generic
// parse error or a MaxBytesReader overflow.
func TestPlaylistHTTPTooManyItems400(t *testing.T) {
	token, dt, srv := newPlaylistTestServer(t)
	id := "eeeeeeee-0000-0000-0000-000000000005"
	var b bytes.Buffer
	b.WriteString(`{"id":"` + id + `","name":"big","lastModifiedAt":1,"items":`)
	b.Write(buildPlaylistItemsJSON(maxPlaylistItems + 1))
	b.WriteByte('}')
	resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, b.String())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-cap PUT = %d, want 400", resp.StatusCode)
	}
	var env ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Message != "playlist has too many items" {
		t.Errorf("over-cap 400 message = %q; want %q (the count-guard, not a generic decode error)",
			env.Message, "playlist has too many items")
	}
}

// TestCappedPlaylistItemsUnmarshalCapsBeforeMaterializing (Q22) is the
// structural proof: exactly at the cap decodes cleanly; one past it aborts
// with the sentinel from inside the streaming loop, so the whole oversized
// array is never built.
func TestCappedPlaylistItemsUnmarshalCapsBeforeMaterializing(t *testing.T) {
	var atCap cappedPlaylistItems
	if err := json.Unmarshal(buildPlaylistItemsJSON(maxPlaylistItems), &atCap); err != nil {
		t.Fatalf("decode at cap: unexpected error %v", err)
	}
	if len(atCap) != maxPlaylistItems {
		t.Fatalf("decode at cap: got %d items, want %d", len(atCap), maxPlaylistItems)
	}

	var overCap cappedPlaylistItems
	err := json.Unmarshal(buildPlaylistItemsJSON(maxPlaylistItems+1), &overCap)
	if !errors.Is(err, errPlaylistTooManyItems) {
		t.Fatalf("decode over cap: err = %v, want errPlaylistTooManyItems", err)
	}

	// null → nil slice (stdlib slice-decode parity), no error.
	var nullItems cappedPlaylistItems
	if err := json.Unmarshal([]byte("null"), &nullItems); err != nil || nullItems != nil {
		t.Fatalf("decode null: err=%v items=%+v; want (nil, nil)", err, nullItems)
	}
}

// TestCappedPlaylistItemsUnmarshalRejectsMalformed (Gemini round-1) pins that
// the stream decoder verifies its closing ']' and rejects trailing content —
// exercised by calling UnmarshalJSON directly, since the outer json.Decoder
// hands the method exactly the array bytes (the malformed forms below can only
// reach it via a direct caller). `[{}] extra` is the case the pre-round-1 code
// silently accepted.
func TestCappedPlaylistItemsUnmarshalRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		`[{}`,        // unterminated array (no closing bracket)
		`[{}}`,       // wrong closing delimiter
		`[{}] extra`, // trailing garbage after the array
		`[{},]`,      // trailing comma → dangling element
	} {
		var c cappedPlaylistItems
		if err := c.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("UnmarshalJSON(%q) = nil error; want rejection", in)
		}
	}
	// A clean array still round-trips.
	var ok cappedPlaylistItems
	if err := ok.UnmarshalJSON([]byte(`[{"position":0,"path":"a.flac"}]`)); err != nil {
		t.Errorf("UnmarshalJSON(well-formed) = %v; want nil", err)
	}
	if len(ok) != 1 || ok[0].Path != "a.flac" {
		t.Errorf("well-formed decode = %+v; want one item path a.flac", ok)
	}
}
