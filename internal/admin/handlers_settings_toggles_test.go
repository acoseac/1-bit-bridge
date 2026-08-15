package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// Settings-console coverage for the two toggles that were YAML-only
// until the 2026-08-14 feature review (P2-37): libraryWatch.enabled and
// upscale.optimizeEnabled. Both are consumed once at `bridge serve`
// startup (watcher goroutine / optimize closures + health
// advertisement), so both are restart-required in BOTH directions —
// and both must skip the restart banner on an idempotent same-value
// submit, matching every sibling startup-wired toggle.

func TestSettingsPatchLibraryWatchRequiresRestart(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, _ := patchSettingsExpect(t, ts, `{"libraryWatchEnabled":true}`, http.StatusOK)
	if !resp.RestartRequired {
		t.Error("off→on must mark RestartRequired — the watcher goroutine is spawned at startup")
	}
	if live := srv.deps.CfgHolder.Load(); !live.LibraryWatch.Enabled {
		t.Error("libraryWatch.enabled not persisted to the live config")
	}

	// Idempotent same-value submit (the settings form always sends the
	// field) must not flag a spurious restart.
	resp, _ = patchSettingsExpect(t, ts, `{"libraryWatchEnabled":true}`, http.StatusOK)
	if resp.RestartRequired {
		t.Error("same-value submit flagged RestartRequired; want the banner skipped")
	}
}

func TestSettingsPatchOptimizeEnabledComparesResolvedDefault(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The YAML field is a nil *bool that RESOLVES to true
	// (EffectiveOptimizeEnabled). Submitting true against the unset
	// field is a same-value submit and must not flag a restart —
	// the patch compares against the resolved value, not the raw
	// pointer.
	resp, _ := patchSettingsExpect(t, ts, `{"optimizeEnabled":true}`, http.StatusOK)
	if resp.RestartRequired {
		t.Error("true against the nil-defaults-to-true field flagged RestartRequired; want the banner skipped")
	}

	resp, _ = patchSettingsExpect(t, ts, `{"optimizeEnabled":false}`, http.StatusOK)
	if !resp.RestartRequired {
		t.Error("on→off must mark RestartRequired — the optimize closures are resolved at startup")
	}
	live := srv.deps.CfgHolder.Load()
	if live.Upscale.EffectiveOptimizeEnabled() {
		t.Error("optimizeEnabled: false not persisted to the live config")
	}
}

// TestSettingsResponseCarriesToggleFields pins the shared builder both
// the JSON GET and the server-rendered page consume — omitting a field
// there is exactly how the enrich URLs once rendered blank and got
// clobbered on Save.
func TestSettingsResponseCarriesToggleFields(t *testing.T) {
	f := false
	cfg := &config.Config{
		LibraryName:  "Test",
		DataDir:      t.TempDir(),
		LibraryWatch: config.LibraryWatchConfig{Enabled: true},
		Upscale:      config.UpscaleConfig{OptimizeEnabled: &f},
	}
	resp := settingsResponseFromConfig(cfg, false)
	if !resp.LibraryWatchEnabled {
		t.Error("LibraryWatchEnabled = false, want true (enabled in config)")
	}
	if resp.OptimizeEnabled {
		t.Error("OptimizeEnabled = true, want false (explicitly disabled in config)")
	}
	// And the nil-pointer default resolves true.
	cfg.Upscale.OptimizeEnabled = nil
	if resp := settingsResponseFromConfig(cfg, false); !resp.OptimizeEnabled {
		t.Error("OptimizeEnabled = false for a nil YAML pointer; want the resolved default true")
	}
}
