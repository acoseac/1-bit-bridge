package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// withManifest spins up an httptest server whose /v1/manifest is backed by
// the provided fakeManifestProvider.
func withManifest(t *testing.T, mp ManifestProvider) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	srv := New(cfg, store, mp, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw
}

func TestManifestReturns200WithProvidedBody(t *testing.T) {
	want := map[string]any{"version": 1, "tracks": []any{}, "folders": []any{}}
	hs, tok := withManifest(t, &fakeManifestProvider{body: want})

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if int(got["version"].(float64)) != 1 {
		t.Errorf("version = %v", got["version"])
	}
}

func TestManifestReturns503WhenNoProvider(t *testing.T) {
	hs, tok := withManifest(t, nil)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	var er ErrorResponse
	json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "scan_in_progress" {
		t.Errorf("error = %q", er.Error)
	}
}

func TestManifestRejectsBadSince(t *testing.T) {
	hs, tok := withManifest(t, &fakeManifestProvider{body: map[string]any{}})
	u := hs.URL + "/v1/manifest?since=" + url.QueryEscape("not-a-date")
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestManifestAcceptsRFC3339Since(t *testing.T) {
	var seenSince time.Time
	mp := &fakeManifestProvider{body: map[string]any{}}
	mp.body = map[string]any{"echo": "ok"}
	// Wrap BuildManifest to capture what we pass in.
	hs, tok := withManifest(t, captureMP{inner: mp, seen: &seenSince})
	u := hs.URL + "/v1/manifest?since=2026-01-02T03:04:05Z"
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if seenSince.IsZero() {
		t.Error("since not propagated")
	}
	if seenSince.UTC().Year() != 2026 {
		t.Errorf("since year = %d", seenSince.UTC().Year())
	}
}

func TestManifestRequiresAuth(t *testing.T) {
	hs, _ := withManifest(t, &fakeManifestProvider{body: map[string]any{}})
	resp, err := http.Get(hs.URL + "/v1/manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// ---- health reflects scan state from provider ----

func TestHealthReflectsScanState(t *testing.T) {
	last := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	mp := &fakeManifestProvider{
		isScanning:    true,
		lastFullScan:  last,
		tracksIndexed: 1234,
	}
	hs, _ := withManifest(t, mp)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HealthResponse
	json.NewDecoder(resp.Body).Decode(&got)
	if !got.ScanState.IsScanning {
		t.Error("isScanning not reflected")
	}
	if got.ScanState.TracksIndexed != 1234 {
		t.Errorf("tracksIndexed = %d", got.ScanState.TracksIndexed)
	}
	if !got.ScanState.LastFullScan.Equal(last) {
		t.Errorf("lastFullScan = %v, want %v", got.ScanState.LastFullScan, last)
	}
}

// captureMP wraps a ManifestProvider and records the since arg passed to
// BuildManifest.
type captureMP struct {
	inner ManifestProvider
	seen  *time.Time
}

func (c captureMP) BuildManifest(since time.Time) (any, error) {
	*c.seen = since
	return c.inner.BuildManifest(since)
}
func (c captureMP) IsScanning() bool        { return c.inner.IsScanning() }
func (c captureMP) LastFullScan() time.Time { return c.inner.LastFullScan() }
func (c captureMP) TracksIndexed() int      { return c.inner.TracksIndexed() }
