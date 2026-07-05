package api

import (
	"compress/gzip"
	"context"
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
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// pagePtrTo returns a pointer to its argument; helper so tests can
// declare `pagePtrTo("cursor-1")` for the *string fields on Manifest.
func pagePtrTo[T any](v T) *T { return &v }

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
	mp := &fakeManifestProvider{body: map[string]any{"echo": "ok"}}
	hs, tok := withManifest(t, mp)
	wantSince := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	u := hs.URL + "/v1/manifest?since=" + wantSince.Format(time.RFC3339Nano)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !mp.lastSince.Equal(wantSince) {
		t.Errorf("since forwarded = %v, want %v", mp.lastSince, wantSince)
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

// --- Pagination handler tests (v1.1 §8) ---

// TestManifestPaginatedRoutesToBuildManifestPage verifies that a
// `?limit=N` query routes to the paginated builder and forwards
// `limit` + `cursor` through unchanged. Locks the handler's param
// parsing so a refactor can't silently regress it.
func TestManifestPaginatedRoutesToBuildManifestPage(t *testing.T) {
	mp := &fakeManifestProvider{
		pageBody: &manifest.Manifest{
			Version: 1,
			Total:   pagePtrTo(0),
		},
	}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest?limit=250&cursor=abc/def", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if mp.lastPageCursor != "abc/def" {
		t.Errorf("cursor forwarded = %q, want abc/def", mp.lastPageCursor)
	}
	if mp.lastPageLimit != 250 {
		t.Errorf("limit forwarded = %d, want 250", mp.lastPageLimit)
	}
}

// TestManifestPaginatedCapsHugeLimit defends against a client asking
// for `?limit=10000000` which would otherwise allocate a huge JSON
// response. The 5000-page-ceiling kicks in silently (no error, just
// a smaller page) since exceeding the server's preferred ceiling
// isn't a user-facing fault.
func TestManifestPaginatedCapsHugeLimit(t *testing.T) {
	mp := &fakeManifestProvider{pageBody: &manifest.Manifest{}}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest?limit=10000000", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if mp.lastPageLimit != 5000 {
		t.Errorf("limit cap not applied: got %d, want exactly 5000", mp.lastPageLimit)
	}
}

// TestManifestRejectsZeroLimit locks in that `?limit=0` (and
// negative) return 400 — a zero-row page request would just spin the
// caller's pagination loop forever.
func TestManifestRejectsZeroLimit(t *testing.T) {
	mp := &fakeManifestProvider{}
	hs, tok := withManifest(t, mp)
	for _, bad := range []string{"0", "-5", "foo"} {
		req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest?limit="+url.QueryEscape(bad), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("limit=%q status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestManifestRejectsLimitPlusSince locks the mutual-exclusion rule:
// pagination + since-delta would need a composite cursor for
// ordering; until we need it, this combination is 400.
func TestManifestRejectsLimitPlusSince(t *testing.T) {
	mp := &fakeManifestProvider{}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET",
		hs.URL+"/v1/manifest?limit=100&since=2026-01-01T00:00:00Z", nil)
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

// TestManifestStreamFailureBeforeFirstByteReturns500 pins the
// `deferredStatusWriter` contract: when WriteManifest fails before
// writing any body bytes (e.g. a DB read error inside ListFolders /
// CountTracks), the handler must respond with a structured 5xx, not
// a `200 OK` with a truncated body that would make iOS-side decode
// fail opaquely.
func TestManifestStreamFailureBeforeFirstByteReturns500(t *testing.T) {
	mp := &fakeManifestProvider{err: io.ErrUnexpectedEOF}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	var er ErrorResponse
	if derr := json.NewDecoder(resp.Body).Decode(&er); derr != nil {
		t.Fatalf("decode error body: %v", derr)
	}
	if er.Error != "internal" {
		t.Errorf("error code = %q, want internal", er.Error)
	}
}

// TestManifestClientCancelBeforeFirstByteDoesNotReturn500 pins the
// pre-write cancellation demotion: when WriteManifest returns
// context.Canceled / DeadlineExceeded before any body byte (iOS
// backgrounded mid-sync, or the client's own deadline fired), the handler
// must demote to a debug log and bail — NOT emit a 500. A pre-fix 500
// here logged at Error and tripped the exact false-positive monitoring
// alerts PR #117 suppressed for the post-write path. A genuine pre-write
// DB fault still 500s (TestManifestStreamFailureBeforeFirstByteReturns500
// is the sibling guard).
func TestManifestClientCancelBeforeFirstByteDoesNotReturn500(t *testing.T) {
	mp := &fakeManifestProvider{err: context.Canceled}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("pre-write context.Canceled produced 500 (should demote + bail): body=%q", body)
	}
	// Nothing was written before the bail, so net/http flushes a bare 200
	// with an empty body (the client that actually cancelled never sees it).
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (empty body)", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestManifestLegacyPathUntouched verifies the no-params path still
// hits BuildManifest (not BuildManifestPage). Back-compat guard for
// v1.0 iOS clients.
func TestManifestLegacyPathUntouched(t *testing.T) {
	// Sentinel total on the paginated body; if the legacy path
	// accidentally fell through to the paginated builder, the
	// response would carry total=42 instead of just the body's
	// version=1.
	mp := &fakeManifestProvider{
		body:     map[string]any{"version": 1, "tracks": []any{}},
		pageBody: &manifest.Manifest{Total: pagePtrTo(42)},
	}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if total, hasTotal := got["total"]; hasTotal {
		t.Errorf("legacy path hit paginated builder — back-compat broken (total=%v)", total)
	}
	if mp.lastPageLimit != 0 {
		t.Errorf("paginated builder was called (limit=%d)", mp.lastPageLimit)
	}
}

// TestManifestPaginatedGzipsWhenAccepted pins that the paginated page
// path honours Accept-Encoding: gzip (the legacy single-shot path
// already did; a max 5000-track page is several MB of JSON). Setting the
// header manually stops Go's transport from transparently decompressing,
// so we can observe Content-Encoding and gunzip ourselves.
func TestManifestPaginatedGzipsWhenAccepted(t *testing.T) {
	mp := &fakeManifestProvider{
		pageBody: &manifest.Manifest{Version: 1, Total: pagePtrTo(7)},
	}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest?limit=100", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	var got map[string]any
	if err := json.NewDecoder(gz).Decode(&got); err != nil {
		t.Fatalf("decode gunzipped page: %v", err)
	}
	if int(got["version"].(float64)) != 1 {
		t.Errorf("version = %v", got["version"])
	}
	if int(got["total"].(float64)) != 7 {
		t.Errorf("total = %v, want 7", got["total"])
	}
}

// TestManifestPaginatedPlainWhenGzipRefused is the fallback guard: with
// gzip explicitly refused the page ships uncompressed (no
// Content-Encoding) and decodes directly.
func TestManifestPaginatedPlainWhenGzipRefused(t *testing.T) {
	mp := &fakeManifestProvider{pageBody: &manifest.Manifest{Version: 1, Total: pagePtrTo(0)}}
	hs, tok := withManifest(t, mp)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest?limit=100", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (identity)", ce)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode plain page: %v", err)
	}
	if int(got["version"].(float64)) != 1 {
		t.Errorf("version = %v", got["version"])
	}
}
