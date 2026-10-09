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
	// undiscoverable. The listener stays open and is handed to serve.
	admin := holdLoopback(t)
	cfgPath := filepath.Join(cwd, "bridge.yaml")
	body := fmt.Sprintf("libraryName: Before\nlibraryRoots:\n  - %s\ndataDir: %s\nadminAddress: %s\n",
		lib, filepath.Join(cwd, "data"), admin.addr)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// No config path: the whole point. resolveConfigPath still finds
		// ./bridge.yaml, which isolateConfigEnv put this process in.
		done <- runServe(ctx, serveOpts{
			addrOverride: "127.0.0.1:0", adminListener: admin.ln,
		}, stdout, stderr)
	}()
	// Created HERE, before the drain is registered, though it is not used
	// until the chdir further down. Cleanups run LIFO, so a t.TempDir
	// taken after the drain is registered has its removal run BEFORE the
	// drain — and this is the directory the process is chdir'd into, which
	// Windows will not remove while it is a working directory. Taking it
	// first puts the order back the way this whole change is about: the
	// working directory is restored, then serve is drained, then every
	// directory is removed. (CodeRabbit, PR #944.)
	scratch := t.TempDir()
	// Before the first t.Fatalf below, and after isolateConfigEnv's
	// t.TempDir: this test has several failure paths of its own, and
	// waitForAdminReady can consume `done` on one of them.
	drainServeOnCleanup(t, cancel, exited, done, stderr)
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
	waitForListening(t, stdout, exited, done, stderr)
	waitForAdminReady(t, admin.addr, done, stderr)

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
	// chdir, not a bare os.Chdir: the helper registers the restore, so the
	// process leaves `scratch` before anything tries to remove it.
	chdir(t, scratch)

	adminBase := "http://" + admin.addr
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
}

// waitForAdminReady blocks until the admin console's listener accepts on
// addr. Delegates to waitForListen (200ms cadence, ctx-aware DialContext)
// rather than sleeping, in rounds of a second, and between them reports a
// serve goroutine that already exited, whose exit code is the real story
// (a failed admin bind, a config refusal), instead of waiting on a socket
// that will never bind. A console that never binds fails the test when
// serve's waits give up (serveGiveUp): it binds after the boot's disk
// writes, which have no bound a starved host keeps to (B63).
func waitForAdminReady(t *testing.T, addr string, done <-chan int, stderr *safeBuffer) {
	t.Helper()
	giveUp := serveGiveUpTime(t)
	for !waitForListen(addr, time.Second) {
		select {
		case code := <-done:
			t.Fatalf("serve exited with code %d before the admin console bound %s; stderr=%s",
				code, addr, stderr.String())
		default:
		}
		if !giveUp.IsZero() && time.Now().After(giveUp) {
			t.Fatalf("admin console never bound %s before the test's deadline; stderr=%s\nserve's goroutines:\n%s",
				addr, stderr.String(), serveStacks())
		}
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

// heldLoopback is an ephemeral loopback listener kept open for the
// rest of the test. A serve test writes addr into the config and hands
// ln to serve, so the port cannot be taken between the two (backlog
// B313). Call it before launchServe or drainServeOnCleanup: cleanups
// run last-registered-first, and the drain has to finish before the
// listener is closed.
type heldLoopback struct {
	ln   net.Listener
	addr string
}

func holdLoopback(t *testing.T) heldLoopback {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return heldLoopback{ln: ln, addr: ln.Addr().String()}
}

// freeLoopbackPort reserves and immediately releases an ephemeral
// loopback port, returning its number. It is for a port that is written
// into a config and compared, never one a later serve or probe binds:
// between this close and that bind another listener can take the number
// (backlog B313). A port serve will bind is holdLoopback, handed in on
// serveOpts.
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
