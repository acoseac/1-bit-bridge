package admin

import (
	"testing"
)

// TestSettingsPatchDLNAEnabled pins the DLNA toggle contract: flipping
// the value writes config and marks RestartRequired (the listener +
// SSDP advertisers are wired at startup); an idempotent same-value
// submission does NOT mark a restart.
func TestSettingsPatchDLNAEnabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Off → on: restart required + persisted.
	var resp settingsPatchResponse
	if code := doJSON(t, h, "PATCH", "/api/settings", map[string]any{"dlnaEnabled": true}, &resp); code != 200 {
		t.Fatalf("patch on: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("dlnaEnabled off→on must mark RestartRequired")
	}
	var got settingsResponse
	doJSON(t, h, "GET", "/api/settings", nil, &got)
	if !got.DLNAEnabled {
		t.Error("dlnaEnabled not persisted")
	}
	if got.DLNAListenAddress == "" {
		t.Error("dlnaListenAddress should be surfaced")
	}

	// Idempotent on → on: no restart.
	resp = settingsPatchResponse{}
	if code := doJSON(t, h, "PATCH", "/api/settings", map[string]any{"dlnaEnabled": true}, &resp); code != 200 {
		t.Fatalf("patch idempotent: %d", code)
	}
	if resp.RestartRequired {
		t.Error("idempotent dlnaEnabled submission must NOT mark RestartRequired")
	}
}
