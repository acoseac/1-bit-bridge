package api

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestHealthAdvertisesCarPlayOptimizeFeature — iOS gates the Tier 0
// runtime CarPlay-routing path on this flag. Bridges with sox + the
// optimize config opted in (default-on) advertise it; iOS then
// surfaces "Mobile-optimized" routing on CarPlay routes. Locked
// alongside the alpha-sort invariant — clients compare /v1/health
// fingerprints byte-for-byte for content-equality short-circuit
// caches.
func TestHealthAdvertisesCarPlayOptimizeFeature(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, store, nil, "fp").
		WithUpscale(true, newStubVariantStore()).
		WithCarPlayOptimize(true)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, f := range got.Features {
		if f == "carPlayOptimize" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Features did not contain \"carPlayOptimize\"; got %v", got.Features)
	}
	for i := 1; i < len(got.Features); i++ {
		if got.Features[i-1] > got.Features[i] {
			t.Errorf("Features not alpha-sorted at index %d: %q > %q (got %v)",
				i, got.Features[i-1], got.Features[i], got.Features)
		}
	}
}

// TestHealthOmitsCarPlayOptimizeWhenUpscaleDisabled — optimize
// shares the sox infrastructure with upscale and has no meaning
// without it. Even if `WithCarPlayOptimize(true)` was called, a
// disabled-upscale bridge must NOT advertise the capability.
func TestHealthOmitsCarPlayOptimizeWhenUpscaleDisabled(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	// WithCarPlayOptimize(true) but no WithUpscale — the AND-gate in
	// the Features emit branch should suppress the flag entirely.
	srv := New(cfg, store, nil, "fp").
		WithCarPlayOptimize(true)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, f := range got.Features {
		if f == "carPlayOptimize" {
			t.Errorf("upscale disabled but Features advertised carPlayOptimize; got %v", got.Features)
		}
	}
}

// TestHealthOmitsCarPlayOptimizeWhenOptimizeDisabled — operator
// opted out of optimize via cfg.Upscale.OptimizeEnabled=false while
// keeping upscale on. The wiring layer passes that gate to
// WithCarPlayOptimize. The Features list reflects the operator choice.
func TestHealthOmitsCarPlayOptimizeWhenOptimizeDisabled(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, store, nil, "fp").
		WithUpscale(true, newStubVariantStore()).
		WithCarPlayOptimize(false) // operator opted out
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, f := range got.Features {
		if f == "carPlayOptimize" {
			t.Errorf("optimize disabled but Features advertised carPlayOptimize; got %v", got.Features)
		}
	}
}

// TestEffectiveOptimizeEnabled_DefaultsTrue — the YAML field is
// pointer-bool so a nil → enabled-by-default surface preserves
// the "works out of the box on upgrade" expectation. Pinned at
// config-layer to defend against an accidental flip to value-bool
// (which would default to false on existing configs).
func TestEffectiveOptimizeEnabled_DefaultsTrue(t *testing.T) {
	t.Parallel()
	var cfg config.UpscaleConfig // OptimizeEnabled is nil → enabled
	if !cfg.EffectiveOptimizeEnabled() {
		t.Errorf("EffectiveOptimizeEnabled() with nil field = false, want true (default-on contract)")
	}
	enabled := true
	cfg.OptimizeEnabled = &enabled
	if !cfg.EffectiveOptimizeEnabled() {
		t.Errorf("EffectiveOptimizeEnabled() with *true = false, want true")
	}
	disabled := false
	cfg.OptimizeEnabled = &disabled
	if cfg.EffectiveOptimizeEnabled() {
		t.Errorf("EffectiveOptimizeEnabled() with *false = true, want false (explicit opt-out)")
	}
}
