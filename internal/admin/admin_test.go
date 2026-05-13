package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// newTestServer spins up an admin Server over a temp dir with one library
// root, fresh manifest store, fresh auth store, and no real listener.
// Tests drive the Handler directly via httptest.
func newTestServer(t *testing.T) (*Server, *config.Config, string) {
	t.Helper()
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	cfg := &config.Config{
		LibraryRoots:    []string{lib},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(tmp, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "Test Library",
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mstore, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mstore.Close() })
	astore, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	scanner := manifest.NewScanner(cfg.LibraryRoots, mstore, "")
	resolver := bridgefs.New(cfg.LibraryRoots)

	// Cancel any admin-triggered scan goroutine when the test ends so it
	// doesn't race the tempdir cleanup.
	scanCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// A scan started just before cancel may still be mid-walk on the
		// tempdir. Wait for it to notice the cancellation — bounded loop
		// so a stuck scan can't hang the whole test run.
		for i := 0; i < 100 && scanner.IsScanning(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	})

	srv, err := New(Deps{
		Cfg:         cfg,
		CfgPath:     cfgPath,
		Auth:        astore,
		Manifest:    mstore,
		Scanner:     scanner,
		Resolver:    resolver,
		Fingerprint: "AB:CD:EF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC",
		StartedAt:   time.Now().UTC(),
		Restart:     func() {}, // no-op in tests
		ScanCtx:     scanCtx,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, cfg, cfgPath
}

// doJSON is a small helper: fires an HTTP request and returns status +
// JSON-decoded body. RemoteAddr is set to a loopback so the
// loopbackOnly middleware accepts the request.
func doJSON(t *testing.T, h http.Handler, method, path string, body any, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "127.0.0.1:54321"
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if out != nil && rw.Body.Len() > 0 {
		if err := json.NewDecoder(rw.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return rw.Code
}

func TestLoopbackGuardRejectsLAN(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.RemoteAddr = "192.168.1.5:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("LAN RemoteAddr: got %d, want 403", rw.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	var s statsResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/stats", nil, &s)
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if s.LibraryName != cfg.LibraryName {
		t.Errorf("libraryName = %q", s.LibraryName)
	}
	if s.ListenAddress != cfg.ListenAddress {
		t.Errorf("listenAddress = %q", s.ListenAddress)
	}
	if s.Fingerprint == "" {
		t.Error("fingerprint missing")
	}
}

// TestEndpointsHandler verifies /api/endpoints returns a JSON array
// of { url, class } entries. We can't assert specific URLs (the
// test runner's interface set varies) but we can assert: the
// response is well-formed JSON, every entry has a non-empty URL
// matching `https://...`, and every entry has one of the documented
// class strings.
//
// PR #69 — admin-side companion to iOS PR #150's per-endpoint
// visibility work.
func TestEndpointsHandler(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var entries []map[string]string
	code := doJSON(t, srv.Handler(), "GET", "/api/endpoints", nil, &entries)
	if code != 200 {
		t.Fatalf("endpoints: %d", code)
	}
	// Empty is acceptable on a runner with no advertisable
	// interfaces (rare CI sandboxes); we don't gate on length.
	validClasses := map[string]bool{
		"LAN":           true,
		"mDNS":          true,
		"Tailscale DNS": true, // ClassTailscaleDNS — magic-DNS, ATS-compatible
		"Tailscale":     true, // CGNAT IP-based
		"Public":        true,
	}
	for i, e := range entries {
		if !strings.HasPrefix(e["url"], "https://") {
			t.Errorf("entry[%d].url = %q, want https:// prefix", i, e["url"])
		}
		if !validClasses[e["class"]] {
			t.Errorf("entry[%d].class = %q, want one of LAN/mDNS/Tailscale DNS/Tailscale/Public", i, e["class"])
		}
	}
}

// Regression for Qodo PR #69 review: when the operator binds with
// `:0` (OS-pick-a-port mode used in tests / dev), the admin
// endpoint must NOT 500 — the devices-page polls every 30s and a
// 500 storm would mask the real "no external addresses" condition.
// Empty array is the honest answer.
func TestEndpointsHandlerHandlesPortZero(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.ListenAddress = ":0"
	var entries []map[string]string
	code := doJSON(t, srv.Handler(), "GET", "/api/endpoints", nil, &entries)
	if code != 200 {
		t.Fatalf("port-zero endpoints: got %d, want 200", code)
	}
	if len(entries) != 0 {
		t.Errorf("port-zero endpoints: got %v, want empty", entries)
	}
}

func TestTokensMintListRevokeFlow(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Empty list.
	var list []tokenRow
	doJSON(t, h, "GET", "/api/tokens", nil, &list)
	if len(list) != 0 {
		t.Errorf("fresh list = %v, want empty", list)
	}

	// Mint.
	var mint pairResult
	code := doJSON(t, h, "POST", "/api/tokens", map[string]string{
		"name": "iPhone Test",
		"url":  "https://127.0.0.1:7788",
	}, &mint)
	if code != http.StatusCreated {
		t.Fatalf("mint: %d", code)
	}
	if mint.RawToken == "" || mint.ID == "" || mint.PairURL == "" {
		t.Errorf("mint result incomplete: %+v", mint)
	}
	if !strings.HasPrefix(mint.PairURL, "bridge://pair?") {
		t.Errorf("PairURL = %q, want bridge://pair prefix", mint.PairURL)
	}
	if !strings.HasPrefix(mint.QRDataURL, "data:image/png;base64,") {
		t.Errorf("QRDataURL missing or malformed: %q", truncForLog(mint.QRDataURL))
	}
	// Alternates surfaces the full URL list the QR baked in via the
	// `urls=` field — the admin pair modal renders this as "Other URLs
	// the device will try" so the operator sees what the iOS app will
	// actually roam across, not just the operator-supplied primary.
	// First entry MUST be the primary URL so older clients reading
	// only `alternates[0]` see the same URL the operator typed.
	if len(mint.Alternates) == 0 {
		t.Errorf("Alternates is empty; expected at least the primary URL")
	} else if mint.Alternates[0] != mint.URL {
		t.Errorf("Alternates[0] = %q, want primary URL %q", mint.Alternates[0], mint.URL)
	}

	// Rotate must preserve the same alternates contract (CodeRabbit
	// on PR #101) — the rotate response is the same shape and
	// flows through the same `ensurePrimaryFirst` defence-in-depth.
	var rotated pairResult
	rotateCode := doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate",
		map[string]string{"url": mint.URL}, &rotated)
	if rotateCode != http.StatusOK {
		t.Fatalf("rotate: %d", rotateCode)
	}
	if len(rotated.Alternates) == 0 {
		t.Errorf("rotate Alternates is empty; expected at least the primary URL")
	} else if rotated.Alternates[0] != rotated.URL {
		t.Errorf("rotate Alternates[0] = %q, want primary URL %q", rotated.Alternates[0], rotated.URL)
	}

	// List now has 1.
	doJSON(t, h, "GET", "/api/tokens", nil, &list)
	if len(list) != 1 || list[0].Name != "iPhone Test" {
		t.Errorf("post-mint list = %+v", list)
	}

	// Revoke unknown.
	code = doJSON(t, h, "DELETE", "/api/tokens/ffffffffffff", nil, nil)
	if code != http.StatusNotFound {
		t.Errorf("revoke unknown: got %d, want 404", code)
	}

	// Revoke known.
	code = doJSON(t, h, "DELETE", "/api/tokens/"+mint.ID, nil, nil)
	if code != http.StatusNoContent {
		t.Errorf("revoke known: got %d, want 204", code)
	}
	doJSON(t, h, "GET", "/api/tokens", nil, &list)
	if len(list) != 0 {
		t.Errorf("post-revoke list = %v, want empty", list)
	}
}

func TestMintRejectsEmptyName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code := doJSON(t, srv.Handler(), "POST", "/api/tokens", map[string]string{
		"name": "  ",
		"url":  "https://127.0.0.1:7788",
	}, nil)
	if code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", code)
	}
}

func TestRootsAddAndPersist(t *testing.T) {
	srv, cfg, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Prepare a real directory to add as a second root.
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}

	var row rootRow
	code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, &row)
	if code != http.StatusCreated {
		t.Fatalf("add root: %d", code)
	}
	if row.Path != extra {
		t.Errorf("row.Path = %q, want %q", row.Path, extra)
	}

	// Scanner + resolver reflect the addition.
	got := srv.deps.Scanner.Roots()
	if len(got) != 2 {
		t.Errorf("scanner roots = %v, want 2", got)
	}
	// Config was re-saved to disk.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.LibraryRoots) != 2 {
		t.Errorf("persisted roots = %v", reloaded.LibraryRoots)
	}
}

func TestRootsAddDuplicateBasename(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()

	// Two roots both called "Music" — basename collision.
	dup := filepath.Join(filepath.Dir(cfg.DataDir), "other", "Music")
	if err := os.MkdirAll(dup, 0o755); err != nil {
		t.Fatal(err)
	}
	code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": dup}, nil)
	if code != http.StatusConflict {
		t.Errorf("dup basename: got %d, want 409", code)
	}
}

func TestRootsRemoveLastRejected(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	code := doJSON(t, srv.Handler(), "DELETE", "/api/roots",
		map[string]string{"path": cfg.LibraryRoots[0]}, nil)
	if code != http.StatusBadRequest {
		t.Errorf("remove last: got %d, want 400", code)
	}
}

// TestRootsRemoveManifestFailureDoesNotCommitRoots pins the PR #22 review
// finding: the handler must attempt the destructive manifest op BEFORE
// persisting the new root list, so a failure on the manifest side doesn't
// leave bridge.yaml with the root gone while /v1/manifest keeps advertising
// its tracks. We induce a failure by closing the underlying store mid-test;
// DeleteTracksByPrefix then returns a "database is closed" error.
func TestRootsRemoveManifestFailureDoesNotCommitRoots(t *testing.T) {
	srv, cfg, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Add a second root so the handler takes the non-collapse branch
	// (DeleteTracksByPrefix, not WipeAllTracks). Both branches share the
	// same order invariant, but the non-collapse branch is the hotter
	// one.
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add root: %d", code)
	}
	rootsBefore := append([]string(nil), srv.deps.Cfg.LibraryRoots...)

	// Force DeleteTracksByPrefix to fail by closing the sqlite handle.
	if err := srv.deps.Manifest.Close(); err != nil {
		t.Fatalf("close manifest: %v", err)
	}

	// Attempt to remove the Extra root. The handler should see the
	// manifest error, return 500, and leave Cfg.LibraryRoots untouched.
	code := doJSON(t, h, "DELETE", "/api/roots", map[string]string{"path": extra}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("manifest failure: got %d, want 500", code)
	}

	// In-memory config still holds the pre-request roots.
	if !stringSlicesEqual(srv.deps.Cfg.LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots mutated on manifest failure: got %v, want %v",
			srv.deps.Cfg.LibraryRoots, rootsBefore)
	}
	// On-disk config matches too — no phantom-state window.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload cfg: %v", err)
	}
	if !stringSlicesEqual(reloaded.LibraryRoots, rootsBefore) {
		t.Errorf("persisted LibraryRoots mutated: got %v, want %v",
			reloaded.LibraryRoots, rootsBefore)
	}
}

// TestRootsRemoveSaveFailureRollsBackInMemory pins the PR #25 review
// finding: when Cfg.Save fails, the handler must roll back the
// in-memory LibraryRoots mutation it just performed, otherwise the
// next *successful* Save (from an unrelated edit like a library-name
// change) would silently commit a removal the operator had seen fail.
//
// We induce the Save failure by chmod'ing the config file read-only
// after the initial write. This exercises the true code path
// (Cfg.Save actually tries and fails) rather than a mock.
func TestRootsRemoveSaveFailureRollsBackInMemory(t *testing.T) {
	if os.Getuid() == 0 {
		// Root ignores file-mode writability; the chmod trick we use
		// below wouldn't produce a failing Save.
		t.Skip("chmod-based permission test doesn't apply under root")
	}
	srv, cfg, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Add a second root so the non-collapse branch runs (the branch
	// that reached the reviewer's finding).
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add root: %d", code)
	}
	rootsBefore := append([]string(nil), srv.deps.Cfg.LibraryRoots...)

	// Make the config directory read-only so Cfg.Save's atomic-
	// write-then-rename pattern can't land the new file. Reverted in
	// cleanup so t.TempDir's teardown can still run.
	dir := filepath.Dir(cfgPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Attempt to remove the Extra root — the manifest delete succeeds,
	// so we reach Cfg.Save, which now fails. Handler should 500 AND
	// restore the in-memory slice.
	code := doJSON(t, h, "DELETE", "/api/roots", map[string]string{"path": extra}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("save failure: got %d, want 500", code)
	}

	if !stringSlicesEqual(srv.deps.Cfg.LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots not rolled back on Save failure: got %v, want %v",
			srv.deps.Cfg.LibraryRoots, rootsBefore)
	}
	// Mirror on-disk assertion — same rationale as the Add Save-failure
	// test. A failed Save must leave no observable state change.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload cfg: %v", err)
	}
	if !stringSlicesEqual(reloaded.LibraryRoots, rootsBefore) {
		t.Errorf("persisted LibraryRoots mutated on Save failure: got %v, want %v",
			reloaded.LibraryRoots, rootsBefore)
	}
}

// TestRootsAddSaveFailureRollsBackInMemory mirrors the Remove test:
// apiRootsAdd was missing the same rollback, so a failed Save left
// in-memory holding the new root while disk had the old. Any later
// successful Save (name edit, bind change) would silently commit the
// addition the operator had seen fail.
func TestRootsAddSaveFailureRollsBackInMemory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chmod-based permission test doesn't apply under root")
	}
	srv, cfg, cfgPath := newTestServer(t)
	h := srv.Handler()

	rootsBefore := append([]string(nil), srv.deps.Cfg.LibraryRoots...)

	// New root we're about to try to add.
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "ExtraAdd")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}

	// Make the config directory read-only so Cfg.Save's atomic-
	// write-then-rename pattern can't land the new file.
	dir := filepath.Dir(cfgPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("save failure: got %d, want 500", code)
	}

	if !stringSlicesEqual(srv.deps.Cfg.LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots not rolled back on Save failure: got %v, want %v",
			srv.deps.Cfg.LibraryRoots, rootsBefore)
	}
	// On-disk config must also match — the in-memory slice could be
	// rolled back while a half-written file still lived on disk.
	// `Cfg.Save`'s atomic write-then-rename makes this unlikely, but
	// the assertion nails down the contract: a failed Save leaves
	// zero observable state change, both in memory and on disk.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload cfg: %v", err)
	}
	if !stringSlicesEqual(reloaded.LibraryRoots, rootsBefore) {
		t.Errorf("persisted LibraryRoots mutated on Save failure: got %v, want %v",
			reloaded.LibraryRoots, rootsBefore)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSettingsPatchRestartRequired(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Change library name only — no restart.
	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"libraryName": "Renamed"}, &resp)
	if code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if resp.RestartRequired {
		t.Error("libraryName change should not require restart")
	}

	// Change listenAddress — restart required.
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"listenAddress": "127.0.0.1:9000"}, &resp)
	if code != 200 {
		t.Fatalf("patch listen: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("listenAddress change must mark restart required")
	}

	// Config file reflects both.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LibraryName != "Renamed" || reloaded.ListenAddress != "127.0.0.1:9000" {
		t.Errorf("persisted settings wrong: %+v", reloaded)
	}
}

func TestSettingsPatchValidationRollsBack(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Non-loopback admin address — must be rejected and not persisted.
	code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"adminAddress": "0.0.0.0:7789"}, nil)
	if code != http.StatusBadRequest {
		t.Errorf("bad admin addr: got %d, want 400", code)
	}
	if srv.deps.Cfg.AdminAddress != "127.0.0.1:7789" {
		t.Errorf("admin addr leaked through: %q", srv.deps.Cfg.AdminAddress)
	}
}

// TestSettingsPatchUpscaleEnabled covers the v1.2 upscale toggle:
// flipping the flag persists to bridge.yaml AND requires a restart
// (the long-lived transcode.Pool is wired at constructor time).
// Mirrors TestSettingsPatchRestartRequired's pattern.
func TestSettingsPatchUpscaleEnabled(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Flip on.
	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"upscaleEnabled": true}, &resp)
	if code != 200 {
		t.Fatalf("patch upscale on: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("upscaleEnabled change must mark restart required (Pool wired at constructor time)")
	}
	if !srv.deps.Cfg.Upscale.Enabled {
		t.Error("in-memory cfg did not reflect upscale.enabled=true")
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Upscale.Enabled {
		t.Error("upscale.enabled did not persist to disk")
	}

	// Flip off — also restart-required (Pool teardown / re-wire).
	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"upscaleEnabled": false}, &resp)
	if code != 200 {
		t.Fatalf("patch upscale off: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("upscaleEnabled flip-off must also require restart")
	}

	// Same value re-submission MUST NOT mark restart required —
	// restart only fires on actual change. Without this, an
	// operator clicking Save with the same value displayed would
	// see a misleading restart banner.
	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"upscaleEnabled": false}, &resp)
	if code != 200 {
		t.Fatalf("patch idempotent: %d", code)
	}
	if resp.RestartRequired {
		t.Error("idempotent upscaleEnabled patch must not require restart")
	}
}

// TestUpscaleStatsHandler covers the GET /api/upscale/stats
// shape across three states: feature off + no cached variants
// (fields default-zero, no Pool); feature off + history (Pool
// nil but cachedVariants populated); feature wired (Pool
// snapshot).
func TestUpscaleStatsHandler(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Default state: feature off, no manifest tracks → zero
	// counts everywhere, no Pool.
	var got upscaleStatsResponse
	code := doJSON(t, h, "GET", "/api/upscale/stats", nil, &got)
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if got.Enabled {
		t.Error("Enabled should mirror cfg.Upscale.Enabled (default false)")
	}
	if got.CachedVariants != 0 || got.CachedBytes != 0 {
		t.Errorf("default cached counters non-zero: %+v", got)
	}
	if got.Pool != nil {
		t.Errorf("Pool should be nil when feature is off; got %+v", got.Pool)
	}
	// SoxAvailable nil when no precheck wired (test harness).
	if got.SoxAvailable != nil {
		t.Errorf("SoxAvailable should be nil without UpscalePrecheck; got %v", *got.SoxAvailable)
	}

	// Wire a precheck closure that reports sox available, plus
	// a Pool stats closure to simulate the feature-on state.
	srv.deps.UpscalePrecheck = func() error { return nil }
	srv.deps.UpscaleStats = func() *UpscalePoolStats {
		return &UpscalePoolStats{
			Workers: 4, QueueCap: 5000, QueueLen: 12, Inflight: 2,
			Enqueued: 100, Done: 86, Failed: 1,
		}
	}
	srv.deps.Cfg.Upscale.Enabled = true

	got = upscaleStatsResponse{}
	code = doJSON(t, h, "GET", "/api/upscale/stats", nil, &got)
	if code != 200 {
		t.Fatalf("stats wired: %d", code)
	}
	if !got.Enabled {
		t.Error("Enabled should be true when Pool is populated")
	}
	if got.SoxAvailable == nil || !*got.SoxAvailable {
		t.Error("SoxAvailable should be true when precheck reports nil")
	}
	if got.Pool == nil {
		t.Fatal("Pool should be populated when feature is wired")
	}
	if got.Pool.Workers != 4 || got.Pool.QueueLen != 12 || got.Pool.Done != 86 {
		t.Errorf("Pool snapshot wrong: %+v", got.Pool)
	}
}

// TestUpscaleStatsDisabledWithHistory pins the off-with-history
// branch the Settings page UX depends on (CodeRabbit nit on PR
// #110): when the feature is off but cached variants exist on
// disk, the response MUST surface CachedVariants > 0 with
// Pool == nil and Enabled == false. Without coverage here, a
// regression that returns Pool zero-padded (instead of nil)
// would mis-render the card with "0 inflight, 0 done"
// instead of em-dashes for the live fields.
func TestUpscaleStatsDisabledWithHistory(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Insert a parent track + a variant row so CountVariants
	// reports a non-zero history. UpscaleStats stays nil
	// (closure not wired) → simulates "feature off with
	// historical converted files on disk".
	if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
		Path:    "Music/Album/01.flac",
		Size:    100,
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := srv.deps.Manifest.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath:    "Music/Album/01.flac",
		VariantID:     "upscaled-v2-176400-24",
		SidecarPath:   "/dev/null/sidecar.flac",
		Format:        "flac",
		SampleRate:    176400,
		BitsPerSample: 24,
		SizeBytes:     12_345_678,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	var got upscaleStatsResponse
	code := doJSON(t, h, "GET", "/api/upscale/stats", nil, &got)
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if got.Enabled {
		t.Error("Enabled must be false when Pool is nil — the off-with-history contract")
	}
	if got.Pool != nil {
		t.Errorf("Pool must be nil when feature is off; got %+v", got.Pool)
	}
	if got.CachedVariants != 1 {
		t.Errorf("CachedVariants: got %d, want 1", got.CachedVariants)
	}
	if got.CachedBytes != 12_345_678 {
		t.Errorf("CachedBytes: got %d, want 12345678", got.CachedBytes)
	}
}

// TestUpscaleSoxAvailabilityCached pins the TTL cache (CodeRabbit
// major on PR #110): the precheck closure shells out to sox and
// can wait up to 2 s, so polling at 5 s would 12×/min spend that
// time. The handler caches the result for soxAvailabilityCacheTTL
// (30 s) and reuses across calls.
func TestUpscaleSoxAvailabilityCached(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var calls int
	srv.deps.UpscalePrecheck = func() error {
		calls++
		return nil
	}

	// Three back-to-back calls within the TTL window MUST
	// invoke the precheck closure exactly once.
	for i := 0; i < 3; i++ {
		var got upscaleStatsResponse
		code := doJSON(t, h, "GET", "/api/upscale/stats", nil, &got)
		if code != 200 {
			t.Fatalf("stats[%d]: %d", i, code)
		}
		if got.SoxAvailable == nil || !*got.SoxAvailable {
			t.Errorf("stats[%d]: SoxAvailable wrong", i)
		}
	}
	if calls != 1 {
		t.Errorf("PrecheckSox invoked %d times; should have been cached after the first call", calls)
	}

	// Forcing the cache to expire (zero out the timestamp)
	// MUST trigger a fresh probe. Done via the test seam of
	// taking the lock + clearing the timestamp directly —
	// matches the project convention for `internal` test-
	// only state mutation.
	srv.soxAvailabilityMu.Lock()
	srv.soxAvailabilityAt = time.Time{}
	srv.soxAvailabilityMu.Unlock()

	var got upscaleStatsResponse
	doJSON(t, h, "GET", "/api/upscale/stats", nil, &got)
	if calls != 2 {
		t.Errorf("after expiry, PrecheckSox should have re-run; calls=%d", calls)
	}
}

func TestPagesRenderWithoutError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	for _, path := range []string{"/", "/library", "/devices", "/settings"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Errorf("%s: status %d", path, rw.Code)
		}
		ct := rw.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: content-type %q", path, ct)
		}
		if !strings.Contains(rw.Body.String(), "1-bit") {
			t.Errorf("%s: body missing brand", path)
		}
	}
}

func TestStaticAssetsEmbedded(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	for _, p := range []string{"/static/app.css", "/static/app.js"} {
		req := httptest.NewRequest("GET", p, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Errorf("%s: %d", p, rw.Code)
		}
		if rw.Body.Len() < 100 {
			t.Errorf("%s: suspiciously small body %d bytes", p, rw.Body.Len())
		}
	}
}

func truncForLog(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "..."
}

// PR #45 review (Gemini): the rotate + lifecycle handlers gated body
// decode on `r.ContentLength > 0`, which incorrectly skipped the
// decode for chunked transfer encodings (Content-Length 0 or -1
// even when a body is genuinely present). Fix swapped to a
// `decodeOptionalJSONBody` helper that decodes unconditionally and
// tolerates io.EOF for empty bodies. These tests lock in the
// regression: empty body = no error + no fields applied; chunked
// body with content = decode succeeds and fields apply.
func TestRotateAcceptsEmptyBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Mint a token to rotate.
	var mint pairResult
	doJSON(t, h, "POST", "/api/tokens",
		map[string]string{"name": "x", "url": "https://127.0.0.1:7788"}, &mint)

	// Empty body — Gemini's reported failure mode previously
	// skipped the decode entirely on `Content-Length: 0`. The
	// fixed handler treats an empty body as "no fields supplied"
	// and falls back to the default URL.
	req := httptest.NewRequest("POST", "/api/tokens/"+mint.ID+"/rotate", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("rotate (empty body): got %d, want 200; resp: %s", rw.Code, rw.Body.String())
	}
}

func TestSetLifecycleAcceptsEmptyBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var mint pairResult
	doJSON(t, h, "POST", "/api/tokens",
		map[string]string{"name": "x", "url": "https://127.0.0.1:7788"}, &mint)

	// Empty body — handler returns the unchanged row.
	req := httptest.NewRequest("PATCH", "/api/tokens/"+mint.ID, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("PATCH (empty body): got %d, want 200; resp: %s", rw.Code, rw.Body.String())
	}
	var row tokenRow
	if err := json.NewDecoder(rw.Body).Decode(&row); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if row.ID != mint.ID {
		t.Errorf("PATCH (empty body) returned wrong row: got %s, want %s", row.ID, mint.ID)
	}
	if row.ExpiresAt != nil {
		t.Errorf("PATCH (empty body) shouldn't have set expiry, got %v", row.ExpiresAt)
	}
}

func TestSetLifecycleSetsAndClearsExpiry(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var mint pairResult
	doJSON(t, h, "POST", "/api/tokens",
		map[string]string{"name": "x", "url": "https://127.0.0.1:7788"}, &mint)

	// Set expiry to a future RFC3339.
	future := "2030-01-01T00:00:00Z"
	var set tokenRow
	code := doJSON(t, h, "PATCH", "/api/tokens/"+mint.ID,
		map[string]any{"expiresAt": future}, &set)
	if code != http.StatusOK {
		t.Fatalf("PATCH set expiry: %d", code)
	}
	if set.ExpiresAt == nil || set.ExpiresAt.IsZero() {
		t.Errorf("PATCH set expiry: ExpiresAt missing on response")
	}

	// Clear via explicit JSON null.
	var cleared tokenRow
	code = doJSON(t, h, "PATCH", "/api/tokens/"+mint.ID,
		map[string]any{"expiresAt": nil}, &cleared)
	if code != http.StatusOK {
		t.Fatalf("PATCH clear expiry: %d", code)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("PATCH clear: ExpiresAt should be nil, got %v", cleared.ExpiresAt)
	}
}
