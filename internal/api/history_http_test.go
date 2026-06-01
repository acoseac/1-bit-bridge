package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
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

func TestHealthAdvertisesPlaybackHistoryAndPlaylistBackupAlphaSorted(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	mstore, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	t.Cleanup(func() { _ = mstore.Close() })
	// Wire both stores: playbackHistory + playlistBackup should both show,
	// in alpha order (playb < playl).
	srv := New(cfg, authStore, nil, "fp").WithHistoryStore(mstore).WithPlaylistStore(mstore)
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
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	srv := New(cfg, authStore, nil, "fp") // no WithHistoryStore
	resp := doReq(t, srv, http.MethodPost, "/v1/history/batch", raw, "deadbeef",
		`{"events":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("feature-off = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
