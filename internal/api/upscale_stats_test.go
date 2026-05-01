package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// stubStatsProvider is a test double for api.UpscaleStatsProvider.
// Returns a fixed snapshot so tests can assert the wire shape and
// the auth gate without standing up a transcode pool.
type stubStatsProvider struct {
	snap UpscaleStats
}

func (s *stubStatsProvider) UpscaleStatsSnapshot() UpscaleStats { return s.snap }

func upscaleStatsFixture(t *testing.T, withProvider bool, snap UpscaleStats) (string /* baseURL */, string /* token */) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{filepath.Join(tmp, "lib")},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	srv := New(cfg, store, nil, "fp")
	if withProvider {
		srv = srv.WithUpscaleStats(&stubStatsProvider{snap: snap})
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL, raw
}

// TestUpscaleStatsRequiresAuth — endpoint is bearer-token gated
// (same rule as POST /v1/upscale and every other /v1/* except
// /v1/health and the pairing routes).
func TestUpscaleStatsRequiresAuth(t *testing.T) {
	url, _ := upscaleStatsFixture(t, true, UpscaleStats{Enabled: true})
	resp, err := http.Get(url + "/v1/upscale/stats")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

// TestUpscaleStatsNoProviderReturnsZero — when WithUpscaleStats
// wasn't wired (test harness, or build without upscale support)
// the handler returns the zero-value UpscaleStats. iOS treats this
// as "feature off" without distinguishing a missing endpoint from
// a disabled feature.
func TestUpscaleStatsNoProviderReturnsZero(t *testing.T) {
	url, tok := upscaleStatsFixture(t, false, UpscaleStats{})
	got := getUpscaleStats(t, url, tok)
	if got.Enabled {
		t.Errorf("Enabled: got true, want false (no provider wired)")
	}
	if got.Pool != nil {
		t.Errorf("Pool: got %+v, want nil", got.Pool)
	}
	if got.SoxAvailable != nil {
		t.Errorf("SoxAvailable: got %+v, want nil", got.SoxAvailable)
	}
	if got.CachedVariants != 0 || got.CachedBytes != 0 {
		t.Errorf("cached counters: got (%d, %d), want (0, 0)", got.CachedVariants, got.CachedBytes)
	}
}

// TestUpscaleStatsHappyPathWireShape — golden-shape test for the
// fully-populated payload iOS decodes against. The JSON field
// names are the contract — renaming any of them is a wire-breaking
// change even though they live behind an authed endpoint.
func TestUpscaleStatsHappyPathWireShape(t *testing.T) {
	tru := true
	want := UpscaleStats{
		Enabled:      true,
		SoxAvailable: &tru,
		Pool: &UpscalePoolStats{
			Workers:  4,
			QueueCap: 5000,
			QueueLen: 12,
			Inflight: 4,
			Enqueued: 142,
			Done:     126,
			Failed:   0,
		},
		CachedVariants: 138,
		CachedBytes:    4823917568,
	}
	url, tok := upscaleStatsFixture(t, true, want)
	got := getUpscaleStats(t, url, tok)
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled: got %v, want %v", got.Enabled, want.Enabled)
	}
	if got.SoxAvailable == nil || *got.SoxAvailable != true {
		t.Errorf("SoxAvailable: got %v, want true", got.SoxAvailable)
	}
	if got.Pool == nil {
		t.Fatalf("Pool: got nil, want non-nil")
	}
	if *got.Pool != *want.Pool {
		t.Errorf("Pool: got %+v, want %+v", *got.Pool, *want.Pool)
	}
	if got.CachedVariants != want.CachedVariants || got.CachedBytes != want.CachedBytes {
		t.Errorf("cached: got (%d, %d), want (%d, %d)",
			got.CachedVariants, got.CachedBytes, want.CachedVariants, want.CachedBytes)
	}
}

// TestUpscaleStatsDisabledPoolOmitted — when the snapshot reports
// disabled, the `pool` and `soxAvailable` fields are documented as
// `omitempty`. Verify the wire JSON actually drops the keys (not
// just emits null) so the iOS decoder's lenient default path
// stays correct.
func TestUpscaleStatsDisabledPoolOmitted(t *testing.T) {
	url, tok := upscaleStatsFixture(t, true, UpscaleStats{Enabled: false, CachedVariants: 5, CachedBytes: 1024})
	req, _ := http.NewRequest("GET", url+"/v1/upscale/stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	if strings.Contains(bs, `"pool"`) {
		t.Errorf("pool field should be omitted when nil; got body: %s", bs)
	}
	if strings.Contains(bs, `"soxAvailable"`) {
		t.Errorf("soxAvailable field should be omitted when nil; got body: %s", bs)
	}
	// The numeric fields are NOT omitempty (they always appear) —
	// document that contract by asserting they're present even
	// when zero would otherwise be omitted. writeJSON pretty-
	// prints with `: ` separators, so match the field name and
	// the value via `Contains` on each side rather than asserting
	// the literal `"cachedVariants":5` form.
	if !strings.Contains(bs, `"cachedVariants"`) || !strings.Contains(bs, `5`) {
		t.Errorf("cachedVariants must always be present; got body: %s", bs)
	}
}

func getUpscaleStats(t *testing.T, baseURL, token string) UpscaleStats {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/v1/upscale/stats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body: %s)", resp.StatusCode, string(body))
	}
	var got UpscaleStats
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}
