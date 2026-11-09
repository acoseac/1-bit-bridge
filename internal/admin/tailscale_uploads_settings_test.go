package admin

import (
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestTheUploadConsentIsARestartSettingTheConsoleSaves: the Settings page's
// switch for tailscale.uploadDiagnosticLogs (backlog B749) saves the key into
// bridge.yaml and answers restart for a change, unchanged otherwise; GET
// /api/settings shows it; a save of another setting keeps it; and turning it
// off again leaves the file no key, the default.
func TestTheUploadConsentIsARestartSettingTheConsoleSaves(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()
	patch := func(body map[string]any) settingsPatchResponse {
		t.Helper()
		var resp settingsPatchResponse
		if code := doJSON(t, h, "PATCH", "/api/settings", body, &resp); code != 200 {
			t.Fatalf("PATCH %v: %d", body, code)
		}
		return resp
	}
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if load().Tailscale.UploadDiagnosticLogs {
		t.Fatal("the fixture's config already consents")
	}
	resp := patch(map[string]any{"tailscaleUploadDiagnosticLogs": true})
	if got := resp.Fields["tailscaleUploadDiagnosticLogs"].Status; got != applyRestart || !resp.RestartRequired {
		t.Errorf("turning the uploads on answered %q (restartRequired %v), want restart", got, resp.RestartRequired)
	}
	if !load().Tailscale.UploadDiagnosticLogs {
		t.Error("the console's save did not write the consent")
	}
	var shown map[string]any
	if code := doJSON(t, h, "GET", "/api/settings", nil, &shown); code != 200 || shown["tailscaleUploadDiagnosticLogs"] != true {
		t.Errorf("GET /api/settings shows tailscaleUploadDiagnosticLogs = %v (status %d), want true", shown["tailscaleUploadDiagnosticLogs"], code)
	}
	patch(map[string]any{"libraryName": "Renamed"})
	if cfg := load(); cfg.LibraryName != "Renamed" || !cfg.Tailscale.UploadDiagnosticLogs {
		t.Errorf("a save of another setting: name %q, consent %v; want the rename and the consent kept",
			cfg.LibraryName, cfg.Tailscale.UploadDiagnosticLogs)
	}
	if got := patch(map[string]any{"tailscaleUploadDiagnosticLogs": true}).Fields["tailscaleUploadDiagnosticLogs"].Status; got != applyUnchanged {
		t.Errorf("the same value again answered %q, want unchanged", got)
	}
	if got := patch(map[string]any{"tailscaleUploadDiagnosticLogs": false}).Fields["tailscaleUploadDiagnosticLogs"].Status; got != applyRestart {
		t.Errorf("turning the uploads off answered %q, want restart", got)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "uploadDiagnosticLogs") {
		t.Errorf("turning the uploads off left the key in the file:\n%s", raw)
	}
}
