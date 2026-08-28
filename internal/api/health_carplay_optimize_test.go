package api

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fetchHealthFeatures builds a minimal /v1/health-only server with
// the requested upscale + carPlayOptimize wiring and returns the
// decoded Features slice. Pulled out of the three carPlayOptimize
// tests below so the fixture isn't repeated three times (SonarCloud
// duplication finding on PR #270).
func fetchHealthFeatures(t *testing.T, upscaleWired, carPlayOptimizeOn bool) []string {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, store, nil, "fp")
	if upscaleWired {
		srv = srv.WithUpscale(true, newStubVariantStore())
	}
	srv = srv.WithCarPlayOptimize(func() bool { return carPlayOptimizeOn })
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.Features
}

// TestHealthAdvertisesCarPlayOptimizeFeature — iOS gates the Tier 0
// runtime CarPlay-routing path on this flag. Bridges with sox + the
// optimize config opted in (default-on) advertise it; iOS then
// surfaces "Mobile-optimized" routing on CarPlay routes. Locked
// alongside the alpha-sort invariant — clients compare /v1/health
// fingerprints byte-for-byte for content-equality short-circuit
// caches.
func TestHealthAdvertisesCarPlayOptimizeFeature(t *testing.T) {
	features := fetchHealthFeatures(t, true /* upscale */, true /* carPlayOptimize */)
	found := false
	for _, f := range features {
		if f == "carPlayOptimize" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Features did not contain \"carPlayOptimize\"; got %v", features)
	}
	for i := 1; i < len(features); i++ {
		if features[i-1] > features[i] {
			t.Errorf("Features not alpha-sorted at index %d: %q > %q (got %v)",
				i, features[i-1], features[i], features)
		}
	}
}

// TestHealthOmitsCarPlayOptimizeWhenUpscaleDisabled — optimize
// shares the sox infrastructure with upscale and has no meaning
// without it. Even if `WithCarPlayOptimize(true)` was called, a
// disabled-upscale bridge must NOT advertise the capability.
func TestHealthOmitsCarPlayOptimizeWhenUpscaleDisabled(t *testing.T) {
	features := fetchHealthFeatures(t, false /* upscale */, true /* carPlayOptimize */)
	for _, f := range features {
		if f == "carPlayOptimize" {
			t.Errorf("upscale disabled but Features advertised carPlayOptimize; got %v", features)
		}
	}
}

// TestHealthOmitsCarPlayOptimizeWhenOptimizeDisabled — operator
// opted out of optimize via cfg.Upscale.OptimizeEnabled=false while
// keeping upscale on. The wiring layer passes that gate to
// WithCarPlayOptimize. The Features list reflects the operator choice.
func TestHealthOmitsCarPlayOptimizeWhenOptimizeDisabled(t *testing.T) {
	features := fetchHealthFeatures(t, true /* upscale */, false /* carPlayOptimize */)
	for _, f := range features {
		if f == "carPlayOptimize" {
			t.Errorf("optimize disabled but Features advertised carPlayOptimize; got %v", features)
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
