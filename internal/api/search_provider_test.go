package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestProviderSatisfiesTheSearchSurface is the guard whose absence let
// /v1/search ship INERT.
//
// The api reaches for search via a type assertion on its
// ManifestProvider, and what cmd/bridge actually passes is
// *manifest.Provider — a wrapper — not *manifest.Store. The assertion
// failed on every bridge: the `search` feature flag never appeared in
// /v1/health and the endpoint answered 503 to everyone.
//
// It shipped because the handler tests used a stub that implemented the
// methods DIRECTLY. A test double more capable than production proves
// nothing about production, and no amount of handler testing would have
// caught this. So this asserts against the CONCRETE type the wiring
// uses.
func TestProviderSatisfiesTheSearchSurface(t *testing.T) {
	var p any = (*manifest.Provider)(nil)

	if _, ok := p.(ServedSearcher); !ok {
		t.Error("*manifest.Provider does not implement ServedSearcher — " +
			"GET /v1/search would answer 503 on every bridge")
	}
	// The health flag's probe is a separate anonymous-interface assertion,
	// so it can regress independently of the one above.
	if _, ok := p.(interface {
		SearchAvailable(ctx context.Context) (bool, error)
	}); !ok {
		t.Error("*manifest.Provider does not implement SearchAvailable — " +
			"the `search` feature flag would never be advertised")
	}
	// And it must still be a ManifestProvider, since that is the field
	// the assertions are made against.
	if _, ok := p.(ManifestProvider); !ok {
		t.Error("*manifest.Provider no longer satisfies ManifestProvider")
	}
}

// TestSearchWorksThroughTheRealProvider drives the endpoint end-to-end
// against a real store behind a real Provider — no stub anywhere — so
// the wiring the previous test asserts structurally is also exercised.
func TestSearchWorksThroughTheRealProvider(t *testing.T) {
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if ok, _ := store.SearchAvailable(context.Background()); !ok {
		t.Skip("FTS5 not compiled into this driver build")
	}

	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Aphex Twin/SAW/01 Xtal.flac", Title: "Xtal",
		Artist: "Aphex Twin", Album: "Selected Ambient Works", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// The REAL provider, exactly as cmd/bridge constructs it.
	provider := manifest.NewProvider(store, manifest.NewScanner([]string{lib}, store, ""))
	s := newRouteRegistryTestServer(t)
	s.manifest = provider

	rr := httptest.NewRecorder()
	s.health(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	var h HealthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.Features, "search") {
		t.Fatalf("`search` not advertised through the real provider: %v", h.Features)
	}

	rr = httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=xtal", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("search through the real provider: status %d, want 200; body=%s",
			rr.Code, rr.Body.String())
	}
	var got searchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Title != "Xtal" {
		t.Errorf("tracks = %+v, want the seeded track", got.Tracks)
	}
}
