package admin

import (
	"net/http"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestSettingsPatchDuplicatesFilter pins the ONE deliberately
// hot-applying feature setting: a policy change persists, never sets
// RestartRequired, and fires Deps.TriggerDuplicatesPass so the stamping
// sweeper re-evaluates immediately. Idempotent re-submits fire nothing.
func TestSettingsPatchDuplicatesFilter(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	fired := 0
	srv.deps.TriggerDuplicatesPass = func() bool { fired++; return true }
	h := srv.Handler()

	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"duplicatesFilter": "same-format"}, &resp)
	if code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if resp.RestartRequired {
		t.Error("duplicatesFilter is hot-applied and must NOT require restart")
	}
	if fired != 1 {
		t.Fatalf("policy change must nudge the stamping sweeper once, fired=%d", fired)
	}
	if got := srv.deps.CfgHolder.Load().Duplicates.Filter; got != "same-format" {
		t.Errorf("in-memory cfg = %q", got)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Duplicates.Filter != "same-format" {
		t.Errorf("persisted filter = %q", reloaded.Duplicates.Filter)
	}

	// Idempotent re-submit: no nudge, no restart.
	resp = settingsPatchResponse{}
	if code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"duplicatesFilter": "same-format"}, &resp); code != 200 {
		t.Fatalf("idempotent patch: %d", code)
	}
	if fired != 1 {
		t.Errorf("idempotent re-submit must not re-nudge, fired=%d", fired)
	}
	if resp.RestartRequired {
		t.Error("idempotent duplicatesFilter patch must not require restart")
	}

	// Case/whitespace-tolerant input normalises to the canonical value —
	// and normalising to the SAME resolved value is a no-op (no nudge).
	if code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"duplicatesFilter": "  Same-Format "}, &resp); code != 200 {
		t.Fatalf("tolerant patch: %d", code)
	}
	if fired != 1 {
		t.Errorf("case-variant of the same policy must be a no-op, fired=%d", fired)
	}

	// Unknown value → 400, nothing persisted, no nudge.
	if code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"duplicatesFilter": "bogus"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bogus filter: got %d, want 400", code)
	}
	if got := srv.deps.CfgHolder.Load().Duplicates.Filter; got != "same-format" {
		t.Errorf("rejected value leaked through: %q", got)
	}
	if fired != 1 {
		t.Errorf("rejected patch must not nudge, fired=%d", fired)
	}
}

// TestSettingsGetExposesResolvedDuplicatesFilter: the GET surface shows
// the EFFECTIVE policy (empty config resolves to the default), so the
// page's radio always has a concrete selection.
func TestSettingsGetExposesResolvedDuplicatesFilter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var got settingsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/settings", nil, &got); code != 200 {
		t.Fatalf("get: %d", code)
	}
	if got.DuplicatesFilter != config.DuplicatesFilterHighestQuality {
		t.Fatalf("default duplicatesFilter = %q, want %q (resolved default)",
			got.DuplicatesFilter, config.DuplicatesFilterHighestQuality)
	}
}
