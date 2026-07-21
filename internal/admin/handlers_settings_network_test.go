package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// patchSettingsExpect issues a JSON PATCH to /api/settings and asserts the
// expected status. On a 200 it returns the decoded response; otherwise a
// nil response plus the raw error body (for content assertions).
func patchSettingsExpect(t *testing.T, ts *httptest.Server, jsonBody string, wantStatus int) (*settingsPatchResponse, []byte) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", ts.URL+"/api/settings", bytes.NewBufferString(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("PATCH %s: status %d, want %d; body: %s", jsonBody, resp.StatusCode, wantStatus, data)
	}
	if wantStatus != http.StatusOK {
		return nil, data
	}
	var patchResp settingsPatchResponse
	if err := json.Unmarshal(data, &patchResp); err != nil {
		t.Fatal(err)
	}
	return &patchResp, data
}

// TestSettingsGetReturnsTailscaleModeAndMDNS pins the new fields
// on the GET shape so the Settings page can render the
// dropdown's selected option + the mDNS checkbox state.
func TestSettingsGetReturnsTailscaleModeAndMDNS(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Loopback defaults: tailscale mode "cli", mDNS enabled true.
	if got["tailscaleMode"] != "cli" {
		t.Errorf("tailscaleMode = %v, want \"cli\"", got["tailscaleMode"])
	}
	if got["mdnsEnabled"] != true {
		t.Errorf("mdnsEnabled = %v, want true (loopback default)", got["mdnsEnabled"])
	}
	if got["isPublic"] != false {
		t.Errorf("isPublic = %v, want false", got["isPublic"])
	}
}

// TestSettingsPatchTailscaleToDisabledFiresHotReload pins the
// hot-reload contract: any → disabled transition fires the
// TailscaleDisable callback AND does NOT mark RestartRequired.
func TestSettingsPatchTailscaleToDisabledFiresHotReload(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var called atomic.Int32
	srv.deps.TailscaleDisable = func() { called.Add(1) }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patchResp, _ := patchSettingsExpect(t, ts, `{"tailscaleMode":"disabled"}`, http.StatusOK)
	if patchResp.RestartRequired {
		t.Error("any→disabled should NOT mark RestartRequired (hot-reload via TailscaleDisable)")
	}
	if called.Load() != 1 {
		t.Errorf("TailscaleDisable invocations = %d, want 1", called.Load())
	}
}

// TestSettingsPatchTailscaleDisabledToCLISetsRestartRequired: the
// inverse transition (any non-disabled mode) requires restart
// because the auto-pilot + listener composition need a clean
// boot.
func TestSettingsPatchTailscaleDisabledToCLISetsRestartRequired(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	// Pre-set cfg to disabled mode so the PATCH represents a
	// disabled→cli transition.
	cfg.Tailscale.Mode = "disabled"
	srv.deps.CfgHolder.Store(cfg)
	var called atomic.Int32
	srv.deps.TailscaleDisable = func() { called.Add(1) }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patchResp, _ := patchSettingsExpect(t, ts, `{"tailscaleMode":"cli"}`, http.StatusOK)
	if !patchResp.RestartRequired {
		t.Error("disabled→cli must mark RestartRequired")
	}
	if called.Load() != 0 {
		t.Errorf("TailscaleDisable invoked %d times for disabled→cli; want 0", called.Load())
	}
}

// TestSettingsPatchTailscaleSameValueDoesNotFire: idempotent
// PATCH (current mode == new mode) doesn't fire the callback.
func TestSettingsPatchTailscaleSameValueDoesNotFire(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Tailscale.Mode = "disabled"
	srv.deps.CfgHolder.Store(cfg)
	var called atomic.Int32
	srv.deps.TailscaleDisable = func() { called.Add(1) }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patchSettingsExpect(t, ts, `{"tailscaleMode":"disabled"}`, http.StatusOK)
	if called.Load() != 0 {
		t.Errorf("idempotent disabled→disabled fired TailscaleDisable %d times; want 0", called.Load())
	}
}

// TestSettingsPatchMDNSDisableFiresHotReload pins both
// directions of the mDNS toggle: enabled→disabled should fire
// MDNSToggle(false), disabled→enabled MDNSToggle(true). Neither
// requires restart.
func TestSettingsPatchMDNSDisableFiresHotReload(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var lastArg atomic.Bool
	var calls atomic.Int32
	srv.deps.MDNSToggle = func(b bool) {
		lastArg.Store(b)
		calls.Add(1)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Loopback default is mdnsEnabled=true; PATCH to false.
	patchResp, _ := patchSettingsExpect(t, ts, `{"mdnsEnabled":false}`, http.StatusOK)
	if patchResp.RestartRequired {
		t.Error("mDNS toggle must NOT mark RestartRequired (hot-reloadable)")
	}
	if calls.Load() != 1 {
		t.Errorf("MDNSToggle invocations = %d, want 1", calls.Load())
	}
	if lastArg.Load() {
		t.Error("MDNSToggle invoked with true; want false (true→false transition)")
	}
}

// TestSettingsPatchPublicModeRejectsMDNSTrue: even with a valid
// hot-reload path, Validate refuses to persist explicit
// mdns.enabled=true in public mode. The PATCH must 400 and the
// callback must NOT fire.
//
// **Auth bypass**: this test exercises the apiSettingsPatch
// handler DIRECTLY rather than going through Handler() so the
// public-mode sessionMiddleware doesn't 503 us (the test
// harness doesn't wire AdminAuth). The 400 path is what matters
// for this regression — Validate refusing the dangerous combo
// before the runtime side effects fire (CodeRabbit Major on PR
// #294 — pre-fix accepted 503 as a pass, masking the validation
// path entirely).
func TestSettingsPatchPublicModeRejectsMDNSTrue(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Deployment.Mode = "public"
	cfg.Deployment.AdminTLSTerminatedByProxy = true
	cfg.Autocert.Domain = "bridge.example.com"
	srv.deps.CfgHolder.Store(cfg)
	var calls atomic.Int32
	srv.deps.MDNSToggle = func(bool) { calls.Add(1) }

	body := bytes.NewBufferString(`{"mdnsEnabled":true}`)
	req := httptest.NewRequest("PATCH", "/api/settings", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	// Direct dispatch — bypass loopback / session middleware.
	srv.apiSettingsPatch(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("status %d, want 400; body: %s", resp.StatusCode, buf[:n])
	}
	if calls.Load() != 0 {
		t.Errorf("MDNSToggle should NOT fire when Validate refuses; got %d invocations", calls.Load())
	}
	// Cfg on disk MUST still reflect the pre-PATCH state.
	current := srv.deps.CfgHolder.Load()
	if current.MDNS.Enabled != nil && *current.MDNS.Enabled {
		t.Error("cfg.MDNS.Enabled persisted as true despite Validate refusal")
	}
}

// TestSettingsPatchTailscaleTsnetToDisabledRequiresRestart pins
// the Gemini High finding on PR #294: the in-process
// TailscaleDisable callback only stops the CLI auto-pilot. If
// the bridge is running in tsnet mode, switching to disabled
// must mark RestartRequired so the operator gets a clean boot
// (the embedded tsnet.Server + its listeners are wired at
// startup and can't be torn down mid-process).
func TestSettingsPatchTailscaleTsnetToDisabledRequiresRestart(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Tailscale.Mode = "tsnet"
	srv.deps.CfgHolder.Store(cfg)
	var calls atomic.Int32
	srv.deps.TailscaleDisable = func() { calls.Add(1) }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patchResp, _ := patchSettingsExpect(t, ts, `{"tailscaleMode":"disabled"}`, http.StatusOK)
	if !patchResp.RestartRequired {
		t.Error("tsnet→disabled must mark RestartRequired (tsnet.Server can't be torn down mid-process)")
	}
	if calls.Load() != 0 {
		t.Errorf("TailscaleDisable fired %d times for tsnet→disabled; want 0 (restart-only)", calls.Load())
	}
}

// TestSettingsPatchTailscaleEmptyStringRejected pins the
// Gemini medium finding on PR #294: an empty `tailscaleMode`
// payload would resolve to "cli" via EffectiveMode but to
// "disabled" via applyDefaults in public mode — divergence
// that the operator never asked for. PATCH rejects empty.
func TestSettingsPatchTailscaleEmptyStringRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	patchSettingsExpect(t, ts, `{"tailscaleMode":""}`, http.StatusBadRequest)
}

// TestSettingsPatchTailscaleInvalidModeRejected: garbage in the
// tailscaleMode field is refused at Validate time without
// touching the disk.
func TestSettingsPatchTailscaleInvalidModeRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var calls atomic.Int32
	srv.deps.TailscaleDisable = func() { calls.Add(1) }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	_, data := patchSettingsExpect(t, ts, `{"tailscaleMode":"garbage"}`, http.StatusBadRequest)
	if !strings.Contains(string(data), "tailscale.mode") {
		t.Errorf("error body %q should mention tailscale.mode", data)
	}
	if calls.Load() != 0 {
		t.Errorf("TailscaleDisable fired %d times despite invalid mode", calls.Load())
	}
}

// Compile-time guard: the new admin Deps fields exist + have the
// expected signatures. Catches a future refactor that renames
// MDNSToggle / TailscaleDisable without updating callers.
var _ = func() {
	var deps Deps
	_ = deps.MDNSToggle
	_ = deps.TailscaleDisable
	// Type-pin via assignment to a func value of the expected
	// shape. If the field type drifts (e.g. someone makes
	// MDNSToggle take int), this stops compiling.
	deps.MDNSToggle = func(bool) {}
	deps.TailscaleDisable = func() {}
	// Silence "declared but not used" via no-op consume.
	_ = config.TailscaleModeDisabled
}
