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
	"runtime"
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
		CfgHolder:   config.NewRuntimeConfig(cfg),
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

// TestRootsRemoveRefusesCaseTwinBasename pins the guard on the destructive
// branch of remove-root.
//
// Track paths are keyed by library-root BASENAME, and this handler prunes
// them with DeleteTracksByPrefix(basename + "/") — which also enumerates
// and UNLINKS the matching variant and waveform sidecars from disk. When
// two roots' basenames differ only by case the removal target is genuinely
// ambiguous, and the delete predicate used to resolve that ambiguity by
// folding case: it took both roots' rows and both roots' files.
//
// ValidateRoots now refuses that configuration up front, so the pair can
// only arrive via a bridge.yaml written before that landed or hand-edited
// since — which is exactly when a destructive path should refuse rather
// than guess. The CLI's offline `library remove` has carried an equivalent
// guard since PR #82; this handler never did.
//
// The two colliding roots live under DIFFERENT parents so both exist as
// real, distinct directories even on a case-insensitive filesystem — the
// collision under test is between the BASENAMES, not the volumes.
//
// THREE roots, deliberately. With exactly two, removing either one takes
// the collapse branch — multi-root → single-root flips the stored path
// form, so the handler runs WipeFilesystemTracks and rescans, which
// doesn't select by basename and so can't be ambiguous. The prefix delete
// this guard protects is only reachable when the removal leaves two or
// more roots behind.
func TestRootsRemoveRefusesCaseTwinBasename(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()

	// Inject the case-twin pair directly: apiRootsAdd would (correctly)
	// reject it now, and the scenario being defended is a config that
	// bypassed the API in the first place.
	base := filepath.Dir(cfg.DataDir)
	twin := filepath.Join(base, "other", "music")   // collides with the fixture's "Music"
	third := filepath.Join(base, "third", "Spoken") // keeps the removal off the collapse branch
	for _, d := range []string{twin, third} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots := append([]string(nil), srv.deps.Scanner.Roots()...)
	roots = append(roots, twin, third)
	srv.deps.Scanner.SetRoots(roots)
	if err := srv.deps.CfgHolder.Update(cfg.DataDir+"/../bridge.yaml", func(next *config.Config) error {
		next.LibraryRoots = roots
		return nil
	}); err != nil {
		t.Fatalf("inject roots: %v", err)
	}

	// Removing either of the pair must be refused, not guessed at.
	code := doJSON(t, h, "DELETE", "/api/roots", map[string]string{"path": twin}, nil)
	if code != http.StatusConflict {
		t.Errorf("remove case-twin root: got %d, want 409 — an ambiguous destructive "+
			"removal must be refused, not resolved by folding case", code)
	}
	// And nothing was removed.
	if got := len(srv.deps.Scanner.Roots()); got != len(roots) {
		t.Errorf("roots changed despite the refusal: %d, want %d", got, len(roots))
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
	rootsBefore := append([]string(nil), srv.deps.CfgHolder.Load().LibraryRoots...)

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
	if !stringSlicesEqual(srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots mutated on manifest failure: got %v, want %v",
			srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore)
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
	rootsBefore := append([]string(nil), srv.deps.CfgHolder.Load().LibraryRoots...)

	// Make the config directory read-only so Cfg.Save's atomic-
	// write-then-rename pattern can't land the new file.
	//
	// This injection is POSIX-only: a directory's mode bits do not gate
	// creation on Windows — that is an NTFS ACL's job — so os.Chmod(dir,
	// 0o500) leaves the Save perfectly able to succeed and the test then
	// fails asserting a rollback that correctly never happened. Skipped
	// rather than reworked: the rollback logic under test is
	// platform-independent, and reproducing "Save fails" on Windows would
	// need an ACL manipulation that tests nothing extra about it.
	if runtime.GOOS == "windows" {
		t.Skip("read-only-directory injection has no effect on Windows (ACLs, not mode bits)")
	}
	// Reverted in cleanup so t.TempDir's teardown can still run.
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

	if !stringSlicesEqual(srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots not rolled back on Save failure: got %v, want %v",
			srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore)
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

	rootsBefore := append([]string(nil), srv.deps.CfgHolder.Load().LibraryRoots...)

	// New root we're about to try to add.
	extra := filepath.Join(filepath.Dir(cfg.DataDir), "ExtraAdd")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}

	// Make the config directory read-only so Cfg.Save's atomic-
	// write-then-rename pattern can't land the new file.
	//
	// This injection is POSIX-only: a directory's mode bits do not gate
	// creation on Windows — that is an NTFS ACL's job — so os.Chmod(dir,
	// 0o500) leaves the Save perfectly able to succeed and the test then
	// fails asserting a rollback that correctly never happened. Skipped
	// rather than reworked: the rollback logic under test is
	// platform-independent, and reproducing "Save fails" on Windows would
	// need an ACL manipulation that tests nothing extra about it.
	if runtime.GOOS == "windows" {
		t.Skip("read-only-directory injection has no effect on Windows (ACLs, not mode bits)")
	}
	dir := filepath.Dir(cfgPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code := doJSON(t, h, "POST", "/api/roots", map[string]string{"path": extra}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("save failure: got %d, want 500", code)
	}

	if !stringSlicesEqual(srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore) {
		t.Errorf("Cfg.LibraryRoots not rolled back on Save failure: got %v, want %v",
			srv.deps.CfgHolder.Load().LibraryRoots, rootsBefore)
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
	if srv.deps.CfgHolder.Load().AdminAddress != "127.0.0.1:7789" {
		t.Errorf("admin addr leaked through: %q", srv.deps.CfgHolder.Load().AdminAddress)
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
	if !srv.deps.CfgHolder.Load().Upscale.Enabled {
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

// TestSettingsPatchAnalysisEnabled mirrors the upscale toggle: flip
// on/off marks restart-required + persists; idempotent re-submit does
// not. Analysis serve wiring is decided once at startup.
func TestSettingsPatchAnalysisEnabled(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()

	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"analysisEnabled": true}, &resp)
	if code != 200 {
		t.Fatalf("patch analysis on: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("analysisEnabled change must mark restart required")
	}
	if !srv.deps.CfgHolder.Load().Analysis.Enabled {
		t.Error("in-memory cfg did not reflect analysis.enabled=true")
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Analysis.Enabled {
		t.Error("analysis.enabled did not persist to disk")
	}

	// Idempotent re-submit → no restart banner.
	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"analysisEnabled": true}, &resp)
	if code != 200 {
		t.Fatalf("patch idempotent: %d", code)
	}
	if resp.RestartRequired {
		t.Error("idempotent analysisEnabled patch must not require restart")
	}
}

// TestSettingsPatchFingerprint covers the acoustic-fingerprint settings
// surface: the enable toggle (restart-required on change, idempotent
// re-submits skip the banner) and the set-only AcoustID key field —
// blank keeps the stored key (the settings form always submits the
// field, so blank-as-clear would wipe the key on every unrelated save),
// and the GET response reports presence without ever echoing the value.
// Mirrors TestSettingsPatchUpscaleEnabled's pattern.
func TestSettingsPatchFingerprint(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()

	// Enable + set the key (whitespace-padded — must be trimmed) in one PATCH.
	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"fingerprintEnabled": true, "fingerprintApiKey": "  testkey123  "}, &resp)
	if code != 200 {
		t.Fatalf("patch fingerprint on: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("fingerprint enable+key must mark restart required (sweeper wired at startup)")
	}
	live := srv.deps.CfgHolder.Load()
	if !live.Fingerprint.Enabled {
		t.Error("in-memory cfg did not reflect fingerprint.enabled=true")
	}
	if live.Fingerprint.APIKey != "testkey123" {
		t.Errorf("key must be trimmed + stored, got %q", live.Fingerprint.APIKey)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Fingerprint.Enabled || reloaded.Fingerprint.APIKey != "testkey123" {
		t.Errorf("fingerprint config did not persist: enabled=%v key=%q",
			reloaded.Fingerprint.Enabled, reloaded.Fingerprint.APIKey)
	}

	// Same-value toggle + BLANK key (the form's every-save shape): no
	// restart banner, stored key untouched.
	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"fingerprintEnabled": true, "fingerprintApiKey": ""}, &resp)
	if code != 200 {
		t.Fatalf("idempotent re-submit: %d", code)
	}
	if resp.RestartRequired {
		t.Error("same-value + blank-key re-submit must not mark restart required")
	}
	if got := srv.deps.CfgHolder.Load().Fingerprint.APIKey; got != "testkey123" {
		t.Errorf("blank key input must keep the stored key, got %q", got)
	}

	// GET reports presence (fingerprintKeySet) but never the key itself —
	// assert over the raw re-marshalled body so a future field addition
	// can't sneak the credential out under a new name.
	var got map[string]any
	if code := doJSON(t, h, "GET", "/api/settings", nil, &got); code != 200 {
		t.Fatalf("settings get: %d", code)
	}
	if got["fingerprintEnabled"] != true || got["fingerprintKeySet"] != true {
		t.Errorf("GET flags wrong: enabled=%v keySet=%v",
			got["fingerprintEnabled"], got["fingerprintKeySet"])
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "testkey123") {
		t.Error("settings GET must never echo the AcoustID key")
	}

	// Implausibly long key → 400, nothing persisted.
	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"fingerprintApiKey": strings.Repeat("k", 200)}, nil)
	if code != 400 {
		t.Fatalf("over-long key must 400, got %d", code)
	}
	if got := srv.deps.CfgHolder.Load().Fingerprint.APIKey; got != "testkey123" {
		t.Errorf("rejected key must not persist, got %q", got)
	}
}

// TestAnalysisStatsHandler covers GET /api/analysis/stats. Without the
// serve-side closures wired (feature off / test harness), the response
// carries no Pool or Sweep — just enabled / sox / cached counts /
// storage path.
func TestAnalysisStatsHandler(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Default: feature off, no precheck wired.
	var got analysisStatsResponse
	code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &got)
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if got.Enabled {
		t.Error("Enabled should be false by default")
	}
	if got.SoxAvailable != nil {
		t.Errorf("SoxAvailable should be nil without precheck; got %v", *got.SoxAvailable)
	}
	if got.CachedWaveforms != 0 || got.CachedBytes != 0 {
		t.Errorf("default cached counters non-zero: %+v", got)
	}
	if got.StoragePath == "" {
		t.Error("StoragePath should always be populated")
	}

	// Wire sox precheck (available) + enable the flag → enabled true.
	srv.deps.UpscalePrecheck = func() error { return nil }
	next := config.Clone(srv.deps.CfgHolder.Load())
	next.Analysis.Enabled = true
	srv.deps.CfgHolder.Store(next)

	got = analysisStatsResponse{}
	code = doJSON(t, h, "GET", "/api/analysis/stats", nil, &got)
	if code != 200 {
		t.Fatalf("stats wired: %d", code)
	}
	if !got.Enabled {
		t.Error("Enabled should be true when flag on AND sox available")
	}
	if got.SoxAvailable == nil || !*got.SoxAvailable {
		t.Error("SoxAvailable should be true when precheck reports nil")
	}
	// Pool/Sweep stay absent while the closures aren't wired — absent
	// must read as "feature machinery off", never as zero-padded idle.
	if got.Pool != nil || got.Sweep != nil {
		t.Errorf("Pool/Sweep should be nil without closures; got pool=%+v sweep=%+v", got.Pool, got.Sweep)
	}

	// Wire the pool + sweep closures → both surface on the snapshot.
	srv.deps.AnalysisPoolStats = func() *UpscalePoolStats {
		return &UpscalePoolStats{Workers: 2, QueueCap: 5000, QueueLen: 3, Inflight: 1, Done: 7}
	}
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	srv.deps.AnalysisSweep = func() *AnalysisSweepState {
		return &AnalysisSweepState{
			LastStartedAt: &start,
			Last:          &AnalysisSweepCounts{Total: 10, UpToDate: 6, DSDExcluded: 2, ZeroByte: 1, Enqueued: 1},
		}
	}
	got = analysisStatsResponse{}
	if code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &got); code != 200 {
		t.Fatalf("stats with pool: %d", code)
	}
	if got.Pool == nil || got.Pool.Workers != 2 || got.Pool.Inflight != 1 {
		t.Errorf("Pool not surfaced: %+v", got.Pool)
	}
	if got.Sweep == nil || got.Sweep.Last == nil || got.Sweep.Last.DSDExcluded != 2 {
		t.Errorf("Sweep not surfaced: %+v", got.Sweep)
	}
}

// TestApiAnalysisSweep covers POST /api/analysis/sweep: 503 when the
// trigger closure isn't wired (analysis inactive), 202 "queued" when it
// is — the endpoint only nudges the serve-side sweeper's buffered-1
// channel, so there is nothing to track or await.
func TestApiAnalysisSweep(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var out map[string]any
	if code := doJSON(t, h, "POST", "/api/analysis/sweep", nil, &out); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired trigger: code = %d, want 503", code)
	}

	triggered := 0
	srv.deps.TriggerAnalysisSweep = func() bool { triggered++; return true }
	out = nil
	if code := doJSON(t, h, "POST", "/api/analysis/sweep", nil, &out); code != http.StatusAccepted {
		t.Fatalf("wired trigger: code = %d, want 202", code)
	}
	if triggered != 1 {
		t.Errorf("trigger invoked %d times, want 1", triggered)
	}
	if v, ok := out["triggered"].(bool); !ok || !v {
		t.Errorf("response = %v, want {triggered: true}", out)
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
	cur := srv.deps.CfgHolder.Load()
	next := config.Clone(cur)
	next.Upscale.Enabled = true
	srv.deps.CfgHolder.Store(next)

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

// seedSplitVariantFixture seeds two source tracks: A/01.flac with TWO
// upscaled targets (so file counts diverge from distinct-track counts)
// and B/01.flac with one optimized target. Totals: upscaled 2 files /
// 3500 B across 1 track; optimized 1 file / 500 B across 1 track.
func seedSplitVariantFixture(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []string{"Music/A/01.flac", "Music/B/01.flac"} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{Path: p, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
	}
	mk := func(src, id string, bytes int64) manifest.VariantRow {
		return manifest.VariantRow{SourcePath: src, VariantID: id, SidecarPath: "/dev/null/" + id, Format: "flac", SampleRate: 96000, BitsPerSample: 24, SizeBytes: bytes, SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1}
	}
	for _, v := range []manifest.VariantRow{
		mk("Music/A/01.flac", "upscaled-v2-192000-24", 2000),
		mk("Music/A/01.flac", "upscaled-v2-176400-24", 1500), // 2nd target, same source
		mk("Music/B/01.flac", "optimized-v2-44100-16", 500),
	} {
		if err := srv.deps.Manifest.UpsertVariant(ctx, v); err != nil {
			t.Fatalf("UpsertVariant %q: %v", v.VariantID, err)
		}
	}
}

// TestUpscaleStatsSplitsByKind pins the honest per-kind breakdown:
// upscaled + optimized variants surface in their own fields AND sum into
// the combined CachedVariants/CachedBytes totals. Guards the regression
// where the Settings tile lumped optimize into upscale.
func TestUpscaleStatsSplitsByKind(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSplitVariantFixture(t, srv)

	var got upscaleStatsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/upscale/stats", nil, &got); code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if got.UpscaledVariants != 2 || got.UpscaledBytes != 3500 {
		t.Errorf("upscaled = (%d,%d), want (2,3500)", got.UpscaledVariants, got.UpscaledBytes)
	}
	if got.OptimizedVariants != 1 || got.OptimizedBytes != 500 {
		t.Errorf("optimized = (%d,%d), want (1,500)", got.OptimizedVariants, got.OptimizedBytes)
	}
	if got.CachedVariants != 3 || got.CachedBytes != 4000 {
		t.Errorf("combined = (%d,%d), want (3,4000)", got.CachedVariants, got.CachedBytes)
	}
}

// TestStatsCompositionFields pins the dashboard "Library composition"
// fields: 1 track with an upscaled variant (DISTINCT source, despite 2
// sidecars), 1 with optimized, 3 variant files total.
func TestStatsCompositionFields(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSplitVariantFixture(t, srv)

	var stats statsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/stats", nil, &stats); code != 200 {
		t.Fatalf("stats endpoint: %d", code)
	}
	if stats.TracksWithUpscaled != 1 || stats.TracksWithOptimized != 1 {
		t.Errorf("composition tracks = (%d,%d), want (1,1)", stats.TracksWithUpscaled, stats.TracksWithOptimized)
	}
	if stats.VariantFiles != 3 || stats.VariantBytes != 4000 {
		t.Errorf("composition variants = (%d,%d), want (3,4000)", stats.VariantFiles, stats.VariantBytes)
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
	for _, path := range []string{"/", "/library", "/library/duplicates", "/devices", "/upnp", "/settings", "/jobs"} {
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

// TestJobsPageRendersBackgroundActivity pins the reworked Jobs page:
// the background-activity card grid (scanner / enrichment / analysis /
// fingerprint / smart mixes / backups / updates / maintenance) renders
// alongside the pre-existing upscale batch table, and the Settings
// analysis copy points at the automatic pipeline instead of telling
// operators to run `bridge analyze` by hand.
func TestJobsPageRendersBackgroundActivity(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/jobs", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("/jobs: status %d", rw.Code)
	}
	body := rw.Body.String()
	for _, want := range []string{
		"Background activity",
		"jobs-page-root",
		"Library scanner",
		"enrichment-panel",
		"job-analysis-card",
		"job-fp-card",
		"Smart mixes",
		"Backups",
		"Update checks",
		"jobs-table", // the upscale batch table survives the rework
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/jobs body missing %q", want)
		}
	}

	// Settings: the stale "run `bridge analyze` to populate" operator
	// instruction is gone — analysis runs automatically now.
	req = httptest.NewRequest("GET", "/settings", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("/settings: status %d", rw.Code)
	}
	sbody := rw.Body.String()
	if !strings.Contains(sbody, "analyses the library automatically") {
		t.Error("/settings: analysis copy should describe the automatic pipeline")
	}
	if strings.Contains(sbody, "then run <code>bridge analyze</code>") {
		t.Error("/settings: stale manual bridge-analyze instruction still present")
	}
}

// TestUPnPPage_PublicModeShowsExplanationPanel pins the public-mode
// gate on the /upnp page (PR-pending follow-up to the discovery+CRUD
// feature, addressing the user's request to hide the page in public
// deployments).
//
// In public mode, `Config.Validate` refuses `upnpUpstream.enabled =
// true` (SSDP multicast is LAN-only AND the upstream's RFC1918 byte
// URLs are unreachable from a public VPS). The /upnp page MUST surface
// that explicitly via the public-mode panel rather than misleadingly
// inviting the operator to "enable it in bridge.yaml" (which would
// fail validation). The nav entry is also hidden in layout.html
// when IsPublic — pinned by the dashboard-page assertion below.
//
// The test invokes the page handler + the dashboard page handler
// DIRECTLY (bypassing the auth middleware) — public mode requires
// AdminAuth wiring that's orthogonal to the gate we're verifying,
// and the renderPage flow is the actual contract under test.
func TestUPnPPage_PublicModeShowsExplanationPanel(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Deployment.Mode = "public"
	cfg.Deployment.AdminTLSTerminatedByProxy = true
	cfg.Autocert.Domain = "bridge.example.com"
	srv.deps.CfgHolder.Store(cfg)

	// /upnp content
	rw := httptest.NewRecorder()
	srv.pageUPnP(rw, httptest.NewRequest("GET", "/upnp", nil))
	if rw.Code != 200 {
		t.Fatalf("/upnp status = %d; want 200", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "upnp-public-mode-panel") {
		t.Errorf("public-mode /upnp page MUST surface the explanation panel; body did not contain `upnp-public-mode-panel`")
	}
	// The action panels (Configured / Discovered / Add manually) MUST
	// NOT render in public mode — they're useless without the feature.
	for _, hiddenID := range []string{"upnp-configured-panel", "upnp-discovered-panel", "upnp-add-manual-panel", "upnp-edit-modal"} {
		if strings.Contains(body, hiddenID) {
			t.Errorf("public-mode /upnp page MUST hide the action panel %q; body contains it", hiddenID)
		}
	}

	// Nav-link suppression: layout.html `{{if not .IsPublic}}` wrap
	// MUST omit the UPnP link. Asserted via the rendered /upnp body
	// itself (layout.html is part of every page render, so the nav
	// markup is in every body).
	if strings.Contains(body, `data-tab="upnp"`) {
		t.Errorf("public-mode nav MUST omit the UPnP link; body contains `data-tab=\"upnp\"`")
	}
}

// TestUPnPPage_LoopbackModeShowsActionPanels is the loopback-side
// regression guard for the public-mode gate above: in the default
// loopback deploy, the action panels MUST render AND the nav link
// MUST be present. Without this assertion a future refactor could
// flip the IsPublic gate the wrong way and the public-mode test
// alone wouldn't catch it.
func TestUPnPPage_LoopbackModeShowsActionPanels(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/upnp", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status = %d; want 200", rw.Code)
	}
	body := rw.Body.String()
	for _, requiredID := range []string{"upnp-configured-panel", "upnp-discovered-panel", "upnp-add-manual-panel", "upnp-edit-modal"} {
		if !strings.Contains(body, requiredID) {
			t.Errorf("loopback-mode /upnp page MUST render action panel %q; body missing it", requiredID)
		}
	}
	if strings.Contains(body, "upnp-public-mode-panel") {
		t.Errorf("loopback-mode /upnp page MUST NOT render the public-mode panel")
	}
	// Nav link present.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "127.0.0.1:54321"
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, req2)
	if !strings.Contains(rw2.Body.String(), `data-tab="upnp"`) {
		t.Errorf("loopback-mode nav MUST include the UPnP link")
	}
}

// TestRenderPage_PublicModeSecurityChrome pins the public-mode
// hardening from the 2026-07-21 review (M10 + M11 + the data.html
// copy Low) on pages rendered through renderPage:
//
//   - M11: every authenticated admin page sends
//     `Content-Security-Policy: frame-ancestors 'self'` (+ legacy
//     X-Frame-Options: SAMEORIGIN) — previously only /login sent XFO,
//     leaving destructive buttons (revoke, delete-all-variants,
//     restart) frameable by a same-site sibling origin.
//   - M10: the header chrome renders a sign-out button wired to
//     POST /logout (previously the route existed with zero UI).
//   - data.html: the "never leaves the loopback admin console" copy
//     is false in public mode and MUST be suppressed.
//
// Like the UPnP public-mode test above, handlers are invoked DIRECTLY
// (bypassing the auth middleware) — the renderPage flow is the
// contract under test.
func TestRenderPage_PublicModeSecurityChrome(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Deployment.Mode = "public"
	cfg.Deployment.AdminTLSTerminatedByProxy = true
	cfg.Autocert.Domain = "bridge.example.com"
	srv.deps.CfgHolder.Store(cfg)

	rw := httptest.NewRecorder()
	srv.pageUPnP(rw, httptest.NewRequest("GET", "/upnp", nil))
	if rw.Code != 200 {
		t.Fatalf("status = %d; want 200", rw.Code)
	}
	if got, want := rw.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'"; got != want {
		t.Errorf("Content-Security-Policy = %q, want %q", got, want)
	}
	if got := rw.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if !strings.Contains(rw.Body.String(), `id="logout-btn"`) {
		t.Error("public-mode layout MUST render the sign-out button")
	}

	rw2 := httptest.NewRecorder()
	srv.pageData(rw2, httptest.NewRequest("GET", "/data", nil))
	if rw2.Code != 200 {
		t.Fatalf("/data status = %d; want 200", rw2.Code)
	}
	if strings.Contains(rw2.Body.String(), "never leaves the loopback admin console") {
		t.Error("public-mode /data MUST NOT claim the console never leaves loopback")
	}
}

// TestRenderPage_LoopbackModeSecurityChromeUnchanged is the loopback-side
// regression guard: the default deployment sends neither the
// frame-ancestors headers (a full CSP is documented future work) nor
// the sign-out button (no session to end), and keeps the owner-visible
// copy on /data. Without this, a flipped IsPublic gate would pass the
// public-mode test alone.
func TestRenderPage_LoopbackModeSecurityChromeUnchanged(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rw := httptest.NewRecorder()
	srv.pageUPnP(rw, httptest.NewRequest("GET", "/upnp", nil))
	if rw.Code != 200 {
		t.Fatalf("status = %d; want 200", rw.Code)
	}
	if got := rw.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("loopback mode MUST NOT send Content-Security-Policy, got %q", got)
	}
	if got := rw.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("loopback mode MUST NOT send X-Frame-Options, got %q", got)
	}
	if strings.Contains(rw.Body.String(), `id="logout-btn"`) {
		t.Error("loopback mode MUST NOT render the sign-out button (no session to end)")
	}

	rw2 := httptest.NewRecorder()
	srv.pageData(rw2, httptest.NewRequest("GET", "/data", nil))
	if !strings.Contains(rw2.Body.String(), "never leaves the loopback admin console") {
		t.Error("loopback-mode /data should keep the owner-visible copy")
	}
}

// TestPageLogin_FrameAncestorsNoneBothModes pins the login page's
// stricter framing guard (gemini security-medium on PR #546): unlike
// renderPage's public-mode-only 'self', the login page refuses framing
// by ANY origin in BOTH modes — XFO DENY has always been unconditional
// here, and the CSP frame-ancestors 'none' twin matches it.
func TestPageLogin_FrameAncestorsNoneBothModes(t *testing.T) {
	for _, public := range []bool{false, true} {
		srv, cfg, _ := newTestServer(t)
		if public {
			cfg.Deployment.Mode = "public"
			cfg.Deployment.AdminTLSTerminatedByProxy = true
			cfg.Autocert.Domain = "bridge.example.com"
			srv.deps.CfgHolder.Store(cfg)
		}
		rw := httptest.NewRecorder()
		srv.pageLogin(rw, httptest.NewRequest("GET", "/login", nil))
		if rw.Code != 200 {
			t.Fatalf("public=%v: status = %d; want 200", public, rw.Code)
		}
		if got, want := rw.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'"; got != want {
			t.Errorf("public=%v: Content-Security-Policy = %q, want %q", public, got, want)
		}
		if got := rw.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("public=%v: X-Frame-Options = %q, want DENY", public, got)
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

// TestSettingsPatchSmartPlaylistsEnabled mirrors the analysis toggle:
// flipping smartPlaylists on marks restart-required + persists; an
// idempotent re-submit does not. The regenerator is wired once at startup.
func TestSettingsPatchSmartPlaylistsEnabled(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	h := srv.Handler()

	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"smartPlaylistsEnabled": true}, &resp)
	if code != 200 {
		t.Fatalf("patch smartPlaylists on: %d", code)
	}
	if !resp.RestartRequired {
		t.Error("smartPlaylistsEnabled change must mark restart required")
	}
	if !srv.deps.CfgHolder.Load().SmartPlaylists.Enabled {
		t.Error("in-memory cfg did not reflect smartPlaylists.enabled=true")
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.SmartPlaylists.Enabled {
		t.Error("smartPlaylists.enabled did not persist to disk")
	}

	resp = settingsPatchResponse{}
	code = doJSON(t, h, "PATCH", "/api/settings",
		map[string]any{"smartPlaylistsEnabled": true}, &resp)
	if code != 200 {
		t.Fatalf("patch idempotent: %d", code)
	}
	if resp.RestartRequired {
		t.Error("idempotent smartPlaylistsEnabled patch must not require restart")
	}
}

// TestPageSmartMixes covers the /smartmixes render: a "Feature disabled"
// state when off, and the seeded families + their member tracks when on.
func TestPageSmartMixes(t *testing.T) {
	srv, cfg, _ := newTestServer(t)

	// Feature off (default) → disabled panel, no family content.
	rw := httptest.NewRecorder()
	srv.pageSmartMixes(rw, httptest.NewRequest("GET", "/smartmixes", nil))
	if rw.Code != 200 {
		t.Fatalf("/smartmixes status = %d; want 200", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "smartmix-disabled-panel") {
		t.Error("feature-off /smartmixes MUST surface the disabled panel")
	}

	// Enable + seed families (flat, time-of-day, and a never-refreshed one).
	cfg.SmartPlaylists.Enabled = true
	srv.deps.CfgHolder.Store(cfg)
	flat := []byte(`[{"position":0,"path":"/Abdullah/Water/01.flac","title":"Song For Sathima","artist":"Abdullah Ibrahim"}]`)
	hourly := []byte(`{"hourly":{"8":[{"position":0,"path":"/x/commute.flac","title":"Morning Drive","artist":"AM"}]}}`)
	if err := srv.deps.Manifest.ReplaceSmartPlaylists(context.Background(), []manifest.StoredSmartPlaylist{
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Subtitle: "most played this fortnight", Position: 0, RefreshedAt: time.Now().UnixNano(), ItemsJSON: flat},
		{Slug: "time-of-day", Kind: "timeOfDay", Title: "Morning Commute", Position: 1, RefreshedAt: time.Now().UnixNano(), ItemsJSON: hourly},
		// RefreshedAt == 0 must render "not yet refreshed", NOT a 1970-epoch
		// relative time (Gemini MEDIUM on PR #401).
		{Slug: "never-run", Kind: "recentlyPlayed", Title: "Never Run", Position: 2, RefreshedAt: 0, ItemsJSON: []byte(`[{"position":0,"path":"/y/z.flac","title":"Z"}]`)},
	}); err != nil {
		t.Fatalf("seed smart playlists: %v", err)
	}

	rw = httptest.NewRecorder()
	srv.pageSmartMixes(rw, httptest.NewRequest("GET", "/smartmixes", nil))
	if rw.Code != 200 {
		t.Fatalf("/smartmixes (enabled) status = %d; want 200", rw.Code)
	}
	body := rw.Body.String()
	if strings.Contains(body, "smartmix-disabled-panel") {
		t.Error("enabled /smartmixes MUST NOT show the disabled panel")
	}
	for _, want := range []string{"Heavy Rotation", "Song For Sathima", "smartmix-regen", "Morning Commute", "Morning Drive", "Never Run", "not yet refreshed",
		// Card-grid rework: per-family regenerate + save-as-playlist actions
		// and the collapsed track-list disclosure.
		"smartmix-grid", "smartmix-regen-one", "smartmix-save", "smartmix-tracklist", "Show tracks"} {
		if !strings.Contains(body, want) {
			t.Errorf("/smartmixes body missing %q", want)
		}
	}
}
