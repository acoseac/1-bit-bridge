package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func newHistoryTestServer(t *testing.T) (token, deviceToken string, srv *Server, store *manifest.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
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
	srv = New(cfg, authStore, nil, "fp").WithDeviceRegistrar(mstore).WithHistoryStore(mstore)
	return raw, "deadbeef", srv, mstore
}

func TestHistoryBatchAcceptedAndDropped(t *testing.T) {
	token, dt, srv, store := newHistoryTestServer(t)
	// 2 valid, 3 malformed (empty path, zero startedAt, negative duration).
	body := `{"events":[
		{"path":"A/1.flac","startedAt":100,"durationUsed":30.5,"codec":"FLAC","outputTarget":{"interfaceType":"USB-DAC","outputRate":176400,"isDoP":true}},
		{"path":"A/2.dsf","startedAt":200,"durationUsed":12.0},
		{"path":"","startedAt":300,"durationUsed":1},
		{"path":"bad/zero.flac","startedAt":0,"durationUsed":1},
		{"path":"bad/neg.flac","startedAt":400,"durationUsed":-5}
	]}`
	resp := doReq(t, srv, http.MethodPost, "/v1/history/batch", token, dt, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out historyBatchResponse
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Accepted != 2 || out.Dropped != 3 {
		t.Errorf("accepted=%d dropped=%d, want 2/3", out.Accepted, out.Dropped)
	}
	// The 2 clean rows landed, scoped to the device token.
	list, _ := store.ListHistory(context.Background(), dt, 100, 0)
	if len(list) != 2 {
		t.Errorf("stored %d events, want 2", len(list))
	}
}

func TestHistoryBatchDeviceTokenRequired(t *testing.T) {
	token, _, srv, _ := newHistoryTestServer(t)
	resp := doReq(t, srv, http.MethodPost, "/v1/history/batch", token, "",
		`{"events":[{"path":"a.flac","startedAt":1,"durationUsed":1}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing device token = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// newBothStoresTestServer wires history + playlist stores behind a
// real manifest.Store — the shape every feature-flag health test needs.
func newBothStoresTestServer(t *testing.T) (token string, srv *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	mstore, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	t.Cleanup(func() { _ = mstore.Close() })
	return raw, New(cfg, authStore, nil, "fp").WithHistoryStore(mstore).WithPlaylistStore(mstore)
}

// newBareTestServer wires NO optional stores — the feature-off shape.
func newBareTestServer(t *testing.T) (token string, srv *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	return raw, New(cfg, authStore, nil, "fp")
}

func TestHealthAdvertisesPlaybackHistoryAndPlaylistBackupAlphaSorted(t *testing.T) {
	// Both stores wired: playbackHistory + playlistBackup should both
	// show, in alpha order (playb < playl).
	raw, srv := newBothStoresTestServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/health", raw, "deadbeef", "")
	var hr HealthResponse
	json.NewDecoder(resp.Body).Decode(&hr)
	resp.Body.Close()

	idxHist, idxList := -1, -1
	for i, f := range hr.Features {
		if f == "playbackHistory" {
			idxHist = i
		}
		if f == "playlistBackup" {
			idxList = i
		}
	}
	if idxHist < 0 || idxList < 0 {
		t.Fatalf("missing flags: %v", hr.Features)
	}
	if idxHist > idxList {
		t.Errorf("playbackHistory must sort before playlistBackup: %v", hr.Features)
	}
	// Full alpha-sort invariant.
	for i := 1; i < len(hr.Features); i++ {
		if hr.Features[i-1] > hr.Features[i] {
			t.Errorf("features not alpha-sorted at %d: %v", i, hr.Features)
		}
	}
}

func TestHistoryBatchFeatureOff404(t *testing.T) {
	raw, srv := newBareTestServer(t) // no WithHistoryStore
	resp := doReq(t, srv, http.MethodPost, "/v1/history/batch", raw, "deadbeef",
		`{"events":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("feature-off = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestHealthAdvertisesCrossDeviceFlags pins the two user-wide-state
// flags: playbackHistoryRead (GET /v1/history) and playlistsCrossDevice
// (id-scoped playlist routes), each gated on the same store wiring as
// its sibling base flag.
func TestHealthAdvertisesCrossDeviceFlags(t *testing.T) {
	raw, srv := newBothStoresTestServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/health", raw, "deadbeef", "")
	var hr HealthResponse
	json.NewDecoder(resp.Body).Decode(&hr)
	resp.Body.Close()

	want := map[string]bool{"playbackHistoryRead": false, "playlistsCrossDevice": false}
	for _, f := range hr.Features {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for flag, seen := range want {
		if !seen {
			t.Errorf("feature %q not advertised: %v", flag, hr.Features)
		}
	}

	// Without the stores, neither flag appears.
	bareRaw, bare := newBareTestServer(t)
	resp = doReq(t, bare, http.MethodGet, "/v1/health", bareRaw, "deadbeef", "")
	var hrBare HealthResponse
	json.NewDecoder(resp.Body).Decode(&hrBare)
	resp.Body.Close()
	for _, f := range hrBare.Features {
		if f == "playbackHistoryRead" || f == "playlistsCrossDevice" {
			t.Errorf("flag %q advertised without store wiring", f)
		}
	}
}

// seedHistoryForTwoDevices uploads one event from each of two devices
// (distinct X-Device-Token headers) and registers display names for both.
func seedHistoryForTwoDevices(t *testing.T, srv *Server, store *manifest.Store, token string) {
	t.Helper()
	resp := doReq(t, srv, http.MethodPost, "/v1/history/batch", token, "aaaa1111",
		`{"events":[{"path":"A/first.flac","startedAt":100,"durationUsed":10}]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed devA = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(t, srv, http.MethodPost, "/v1/history/batch", token, "bbbb2222",
		`{"events":[{"path":"B/second.flac","startedAt":200,"durationUsed":20}]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed devB = %d", resp.StatusCode)
	}
	resp.Body.Close()
	ctx := context.Background()
	if err := store.UpsertDeviceRegistration(ctx, "aaaa1111", "tok-1", "Arseni's iPhone"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDeviceRegistration(ctx, "bbbb2222", "tok-2", "Arseni's iPad"); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryReadFeedAllDevices pins the user-wide read contract: the
// feed returns every device's events (newest first) with device-name
// attribution, and never exposes the raw recovery token.
func TestHistoryReadFeedAllDevices(t *testing.T) {
	token, _, srv, store := newHistoryTestServer(t)
	seedHistoryForTwoDevices(t, srv, store, token)

	// Read WITHOUT an X-Device-Token header — the feed is user-wide and
	// bearer-auth only.
	resp := doReq(t, srv, http.MethodGet, "/v1/history", token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/history = %d, want 200", resp.StatusCode)
	}
	var out historyListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Events) != 2 {
		t.Fatalf("events = %d, want 2 (both devices)", len(out.Events))
	}
	// Newest insert first (id DESC).
	if out.Events[0].Path != "B/second.flac" || out.Events[1].Path != "A/first.flac" {
		t.Errorf("feed order wrong: %+v", out.Events)
	}
	if out.Events[0].DeviceName != "Arseni's iPad" || out.Events[1].DeviceName != "Arseni's iPhone" {
		t.Errorf("device attribution missing: %+v", out.Events)
	}
	// deviceId is the SHA-256-derived display id, NEVER the raw token.
	for _, e := range out.Events {
		if e.DeviceID == "aaaa1111" || e.DeviceID == "bbbb2222" {
			t.Errorf("raw device token leaked as deviceId: %q", e.DeviceID)
		}
		if len(e.DeviceID) != historyDeviceIDLen {
			t.Errorf("deviceId length = %d, want %d", len(e.DeviceID), historyDeviceIDLen)
		}
	}
	if out.Events[0].DeviceID == out.Events[1].DeviceID {
		t.Error("distinct devices produced the same deviceId")
	}
}

func TestHistoryReadCursorPaging(t *testing.T) {
	token, _, srv, store := newHistoryTestServer(t)
	seedHistoryForTwoDevices(t, srv, store, token)

	resp := doReq(t, srv, http.MethodGet, "/v1/history?limit=1", token, "", "")
	var page1 historyListResponse
	json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if len(page1.Events) != 1 || page1.NextCursor == 0 {
		t.Fatalf("page1: events=%d cursor=%d", len(page1.Events), page1.NextCursor)
	}
	resp = doReq(t, srv, http.MethodGet,
		"/v1/history?limit=1&after="+strconv.FormatInt(page1.NextCursor, 10), token, "", "")
	var page2 historyListResponse
	json.NewDecoder(resp.Body).Decode(&page2)
	resp.Body.Close()
	if len(page2.Events) != 1 {
		t.Fatalf("page2: events=%d", len(page2.Events))
	}
	if page1.Events[0].Path == page2.Events[0].Path {
		t.Errorf("paging returned the same event twice: %q", page1.Events[0].Path)
	}
}

func TestHistoryReadBadParams400(t *testing.T) {
	token, _, srv, _ := newHistoryTestServer(t)
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=x", "?after=-2", "?after=x"} {
		resp := doReq(t, srv, http.MethodGet, "/v1/history"+q, token, "", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /v1/history%s = %d, want 400", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestHistoryReadFeatureOff404(t *testing.T) {
	raw, srv := newBareTestServer(t) // no WithHistoryStore
	resp := doReq(t, srv, http.MethodGet, "/v1/history", raw, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("feature-off = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
