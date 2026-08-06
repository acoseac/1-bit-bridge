package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serve's --config default is "" (PR #639, so loadCLIConfig can fall
// back to the platform config dir). Everything in runServe that touches
// a config PATH must therefore use the RESOLVED value, never the raw
// flag — filepath.Abs("") is the process CWD and os.Stat("") reports
// IsNotExist, so the raw value is not merely wrong, it is wrong in ways
// that look plausible.
//
// This file pins the two consequences that are cheap to reach directly;
// TestServeWiresResolvedConfigPathIntoAdminAndBackups pins the rest by
// booting the real server.

// `serve --init-if-missing` without --config must NOT re-init when a
// perfectly good ./bridge.yaml is sitting right there.
//
// os.Stat("") returns IsNotExist, so the raw-flag version took the
// auto-init branch on EVERY flag-less invocation, then called
// writeAutoInitConfig("") → Save("") → rename to "": no such file or
// directory → exit 2. The bridge could not start at all that way, with
// an error naming neither the config it ignored nor the one it failed to
// write.
//
// The fixture's library root deliberately does not exist, so a run that
// gets PAST the auto-init branch still exits 2 — at the library-root
// accessibility check, with a different message. That keeps the test
// about which branch was taken rather than about booting a server.
func TestServeInitIfMissingUsesResolvedConfigNotRawFlag(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	cfgPath := filepath.Join(cwd, "bridge.yaml")
	body := "libraryRoots:\n  - " + filepath.Join(cwd, "does-not-exist") +
		"\ndataDir: " + filepath.Join(cwd, "data") + "\nadminAddress: 127.0.0.1:0\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// safeBuffer for the same reason as the sibling test below: runServe
	// may reach its concurrent writers before returning.
	so, se := &safeBuffer{}, &safeBuffer{}
	code := runServe(context.Background(),
		serveOpts{initIfMissing: true}, so, se)

	if strings.Contains(se.String(), "auto-init") {
		t.Fatalf("flag-less `serve --init-if-missing` took the auto-init branch "+
			"despite %s existing — it is stat-ing the raw \"\" flag, which always "+
			"reports IsNotExist.\nexit %d, stderr: %s", cfgPath, code, se.String())
	}
	// It must have reached the config it was supposed to read.
	if !strings.Contains(se.String(), "does-not-exist") {
		t.Errorf("stderr does not mention the fixture's library root, so the run "+
			"never loaded %s:\nexit %d, stderr: %s", cfgPath, code, se.String())
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the operator's bridge.yaml was rewritten by the auto-init path:\n%s", after)
	}
}

// With genuinely no config anywhere, --init-if-missing still seeds one —
// and at the resolver's answer for "where a config should live", not at
// the empty string.
func TestServeInitIfMissingSeedsAtResolvedLocation(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)

	// The seed's own default root is `/library`, and on a case-INSENSITIVE
	// volume (every stock macOS boot disk) that resolves to /Library — so
	// the accessibility check passes, runServe boots a COMPLETE bridge,
	// and it scans /Library for the rest of the package run with nothing
	// ever cancelling it. On Linux the same test exits early. Overriding
	// the root through env (which applyEnvOverrides applies at load, and
	// which wins over the seeded YAML) makes the run exit at the
	// accessibility check on every platform, deterministically, while
	// still writing the seed first — which is the only thing this pins.
	t.Setenv("BRIDGE_LIBRARY_ROOTS", filepath.Join(cwd, "no-such-library"))

	// safeBuffer, not bytes.Buffer: runServe fans out to concurrent
	// writers (the backup ticker and the Tailscale auto-pilot both
	// Fprintf to these streams), so an unsynchronised buffer is a race
	// the moment the run gets far enough to spawn them.
	so, se := &safeBuffer{}, &safeBuffer{}
	// Exits non-zero at the library-root check. The seeding is what this
	// pins.
	_ = runServe(context.Background(), serveOpts{initIfMissing: true}, so, se)

	if strings.Contains(se.String(), "auto-init") {
		t.Fatalf("auto-init failed outright: %s", se.String())
	}
	seeded := filepath.Join(platform, "bridge.yaml")
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("no seed config at the resolved location %s: %v\nstderr: %s",
			seeded, err, se.String())
	}
	// Nothing may be created from the empty-string path — the CWD itself
	// is what filepath.Dir("") resolves to.
	if _, err := os.Stat(filepath.Join(cwd, "bridge.yaml")); err == nil {
		t.Errorf("a config was also written into the CWD")
	}
}

// The real wiring assertion: boot `serve` with NO --config from a
// directory holding bridge.yaml, then drive the two consumers that read
// the path afterwards.
//
//   - admin.Deps.CfgPath is filepath.Abs(*configPath). With the raw flag
//     that is the CWD — a DIRECTORY — which passes admin.New's only
//     guard (CfgPath == "") and reaches Config.Save, whose temp-file
//     rename onto a directory fails. Every admin config mutation
//     (settings PATCH, roots add/remove, variants-dir PATCH, the UPnP
//     upstream CRUD adapter) errored, with the admin console the only
//     place that surfaced it.
//   - buildBackupSources(cfg, *configPath) yields BridgeYAML: "", which
//     backup.Snapshot silently skips — so the periodic snapshot, the
//     thing an operator restores a broken install from, contained no
//     bridge.yaml at all.
func TestServeWiresResolvedConfigPathIntoAdminAndBackups(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	lib := filepath.Join(cwd, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// The admin port has to be known up front: `serve` prints the
	// configured admin address, not the bound one, so :0 would be
	// undiscoverable.
	adminPort := freeLoopbackPort(t)
	cfgPath := filepath.Join(cwd, "bridge.yaml")
	body := fmt.Sprintf("libraryName: Before\nlibraryRoots:\n  - %s\ndataDir: %s\nadminAddress: 127.0.0.1:%d\n",
		lib, filepath.Join(cwd, "data"), adminPort)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	go func() {
		// No --config: the whole point.
		done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0"}, stdout, stderr)
	}()
	// The startup banner means the API listener is up. It says NOTHING
	// about the admin console, and the two are independent: runServe
	// spawns `adminSrv.Serve(adminCtx)` on its OWN goroutine and then
	// proceeds to `net.Listen` + print the banner on the main goroutine,
	// with no synchronisation between them. The admin bind therefore
	// happens at an unsynchronised moment that may be after the banner.
	//
	// On macOS the goroutine reliably wins that race, which is why this
	// read as green locally; on the Windows CI runner it did not, and the
	// PATCH below dialled a socket nothing had bound yet:
	//
	//   dial tcp 127.0.0.1:51187: connectex: No connection could be made
	//   because the target machine actively refused it.
	//
	// So wait for the admin socket itself. waitForListen is the repo's
	// primitive for exactly this ("the process started" ≠ "the socket is
	// bound", the PR #72 rationale behind actRestart's health probe).
	waitForListening(t, stdout, 30*time.Second)
	waitForAdminReady(t, fmt.Sprintf("127.0.0.1:%d", adminPort), done, stderr)

	// Move the process off the config's directory now that the bridge is
	// up. Everything below must still find bridge.yaml, which is only
	// true if the path each consumer holds is ABSOLUTE — and two of
	// resolveConfigPath's branches (including the ./bridge.yaml hit this
	// test takes) return the bare relative "bridge.yaml". Nothing else in
	// the run depends on the CWD: the config is already loaded and its
	// dataDir was resolved to an absolute path at load time.
	//
	// This is not a contrived stress: the installed service units set
	// WorkingDirectory to the DATA dir, and the backup ticker's first
	// snapshot can fire 24h after boot.
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	adminBase := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	client := &http.Client{Timeout: 30 * time.Second}

	// (1) A config mutation through the admin console must persist.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		adminBase+"/api/settings", strings.NewReader(`{"libraryName":"After"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH /api/settings: %v; stderr=%s", err, stderr.String())
	}
	patchBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/settings = %d: %s\n\nadmin.Deps.CfgPath is not the "+
			"resolved bridge.yaml — with the raw \"\" flag it is filepath.Abs(\"\") = "+
			"the CWD, and Config.Save cannot rename its temp file onto a directory. "+
			"Every admin config mutation fails this way.", resp.StatusCode, patchBody)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "After") {
		t.Errorf("the settings PATCH reported success but %s still reads:\n%s", cfgPath, saved)
	}

	// (2) A snapshot must capture bridge.yaml — buildBackupSources got a
	// real path, not "".
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, adminBase+"/api/backups", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/backups: %v; stderr=%s", err, stderr.String())
	}
	backupBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/backups = %d: %s", resp.StatusCode, backupBody)
	}
	if !snapshotCapturedBridgeYAML(t, filepath.Join(cwd, "data", "backups")) {
		t.Errorf("no snapshot captured bridge.yaml — buildBackupSources did not receive "+
			"an absolute, resolved path. backup.Snapshot skips a source that is empty "+
			"OR that os.Stat cannot find, both silently, so the config goes missing "+
			"from every snapshot with no error anywhere.\nresponse: %s", backupBody)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatalf("serve did not shut down within grace window; stderr=%s", stderr.String())
	}
}

// waitForAdminReady blocks until the admin console's listener accepts on
// addr. Delegates to waitForListen (200ms cadence, ctx-aware DialContext)
// rather than sleeping, and reports a serve goroutine that already exited
// instead of burning the whole deadline on a socket that will never bind.
func waitForAdminReady(t *testing.T, addr string, done <-chan int, stderr *safeBuffer) {
	t.Helper()
	if waitForListen(addr, 30*time.Second) {
		return
	}
	// Not up. If serve already returned, its exit code is the real story
	// (a failed admin bind, a config refusal) — a bare timeout message
	// would send the next reader hunting for a flake instead.
	select {
	case code := <-done:
		t.Fatalf("serve exited with code %d before the admin console bound %s; stderr=%s",
			code, addr, stderr.String())
	default:
		t.Fatalf("admin console never bound %s within 30s; stderr=%s", addr, stderr.String())
	}
}

// snapshotCapturedBridgeYAML reports whether any snapshot under root
// holds a bridge.yaml.
func snapshotCapturedBridgeYAML(t *testing.T, root string) bool {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Logf("read backups dir %s: %v", root, err)
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "bridge.yaml")); err == nil {
			return true
		}
	}
	return false
}

// freeLoopbackPort reserves and immediately releases an ephemeral
// loopback port, returning its number.
//
// This does pick the port BEFORE the bridge binds it, which is the
// weaker half of this fixture — but `adminAddress: 127.0.0.1:0` is not
// an option: serve prints the CONFIGURED admin address, not the bound
// one, and the admin's own "console listening" line goes to slog's
// default handler (the real stderr), not to the writers the test passes
// in. There is no channel through which the bound admin port can be
// discovered.
//
// Both failure modes of the gap are loud rather than silent. If nothing
// takes the port, the bridge binds it and waitForAdminReady returns. If
// something else grabs it first, the bridge's admin bind fails and
// waitForAdminReady reports the serve exit; and in the pathological case
// where the squatter is itself an HTTP listener, the test still fails —
// its assertions are on THIS config file's contents and THIS data dir's
// snapshots, which a stranger cannot produce.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
