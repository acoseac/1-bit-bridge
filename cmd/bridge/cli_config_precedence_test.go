package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// closedLoopbackPort returns a loopback port that is guaranteed free: it binds
// one, reads the assigned number, and closes it. Deterministic where a
// hardcoded port would be flaky on a busy machine.
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split reserved addr: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return port
}

// writeInstallAt drops a bridge.yaml at dir/bridge.yaml whose dataDir is
// dir/data, and seeds its manifest DB with one track carrying
// missing_count > 0 — i.e. exactly one row `manifest clear-missing`
// would purge. Returns the config path.
func writeInstallAt(t *testing.T, dir, trackPath string) string {
	t.Helper()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	// A real, CLOSED loopback port — not :0. The CLI's write gate probes this
	// address, and port 0 names no fixed port to probe, so the gate refuses
	// (correctly, and by design: it fails closed rather than guessing). These
	// tests are about config precedence, so they need an address that
	// answers the liveness question cleanly with "nothing there".
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " +
		filepath.Join(dir, "data") + "\nadminAddress: 127.0.0.1:" + closedLoopbackPort(t) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: trackPath, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "Song",
	}); err != nil {
		t.Fatal(err)
	}
	// Threshold well above 1 so the row gains a missing_count without
	// being reaped here.
	if _, err := store.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{trackPath}, 99); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// trackExists reports whether dir's manifest DB still holds trackPath.
func trackExists(t *testing.T, dir, trackPath string) bool {
	t.Helper()
	store, err := manifest.OpenStore(manifest.DefaultDBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetTrack(context.Background(), trackPath)
	if err != nil {
		t.Fatal(err)
	}
	return got != nil
}

// `bridge manifest clear-missing` must resolve its config through the
// SHARED CLI precedence — ./bridge.yaml before the platform config dir.
//
// It used to jump straight to the platform dir unconditionally, which is
// the exact inversion resolveConfigPath's docblock warns about, on the
// one command in the CLI that DELETES rows. An operator with both a
// platform install and a local fixture — the documented `/tmp/bridge-live`
// setup, say — running `bridge manifest clear-missing --yes` from the
// fixture directory opened the PRODUCTION database and purged its rows,
// with nothing printed to say which database that was.
func TestManifestClearMissingPrefersLocalConfigOverPlatform(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)

	// The local fixture the operator is standing in, and the production
	// install they are NOT trying to touch.
	localCfg := writeInstallAt(t, cwd, "local-track.flac")
	writeInstallAt(t, platform, "production-track.flac")

	var so, se bytes.Buffer
	if code := manifestClearMissingCmd(context.Background(), []string{"--yes"}, strings.NewReader(""), &so, &se); code != 0 {
		t.Fatalf("clear-missing exit %d: %s", code, se.String())
	}

	if trackExists(t, platform, "production-track.flac") == false {
		t.Fatalf("clear-missing purged the PLATFORM install's manifest while the "+
			"operator was standing in a directory with its own bridge.yaml — it is "+
			"resolving config platform-first instead of local-first.\nstdout:\n%s", so.String())
	}
	if trackExists(t, cwd, "local-track.flac") {
		t.Fatalf("clear-missing did not purge the LOCAL install's missing row; "+
			"it targeted some other config.\nstdout:\n%s", so.String())
	}

	// And it must SAY which database it opened. `--yes` skips the
	// prompt, so without this a scripted run leaves no record of what it
	// touched — which is how the wrong-database case stayed invisible.
	out := so.String()
	printedCfg := fieldAfter(t, out, "Config:")
	if !filepath.IsAbs(printedCfg) {
		t.Errorf("printed config path %q is not absolute — a bare \"bridge.yaml\" "+
			"names the very ambiguity this line exists to resolve", printedCfg)
	}
	// os.SameFile rather than string equality: on macOS the temp dir
	// reaches the same file through both /var/... and /private/var/...,
	// and the identity is what the operator cares about.
	if !sameFile(t, printedCfg, localCfg) {
		t.Errorf("printed config %q is not the local install's %q:\n%s", printedCfg, localCfg, out)
	}
	if !sameFile(t, fieldAfter(t, out, "Database:"), manifest.DefaultDBPath(filepath.Join(cwd, "data"))) {
		t.Errorf("printed database is not the local install's:\n%s", out)
	}
}

// fieldAfter returns the trimmed remainder of the first line in out
// starting with prefix.
func fieldAfter(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("no %q line in output:\n%s", prefix, out)
	return ""
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Errorf("stat %q: %v", a, err)
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Errorf("stat %q: %v", b, err)
		return false
	}
	return os.SameFile(fa, fb)
}

// With no ./bridge.yaml the platform install is still found — the
// fallback the shared resolver exists for.
func TestManifestClearMissingFallsBackToPlatformConfig(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "production-track.flac")
	if _, err := os.Stat(filepath.Join(cwd, "bridge.yaml")); !os.IsNotExist(err) {
		t.Fatalf("fixture leaked a local bridge.yaml into cwd: %v", err)
	}

	var so, se bytes.Buffer
	if code := manifestClearMissingCmd(context.Background(), []string{"--yes"}, strings.NewReader(""), &so, &se); code != 0 {
		t.Fatalf("clear-missing exit %d: %s", code, se.String())
	}
	if trackExists(t, platform, "production-track.flac") {
		t.Fatalf("clear-missing did not reach the platform install:\nstdout:\n%s", so.String())
	}
}

// `bridge admin reset-password` must resolve its config through the same
// shared precedence, so it can find a PLATFORM install.
//
// It used to fall back to the bare relative `defaultConfigPath`, so it
// only ever looked in the current directory. That dead-ends the
// documented recovery path: runServe's public-mode refuse-to-start
// prints "run `bridge admin reset-password`", and following that
// instruction from anywhere but the config directory died with
// `read config "bridge.yaml": no such file or directory`.
func TestAdminResetPasswordFindsPlatformConfig(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "production-track.flac")
	if _, err := os.Stat(filepath.Join(cwd, "bridge.yaml")); !os.IsNotExist(err) {
		t.Fatalf("fixture leaked a local bridge.yaml into cwd: %v", err)
	}

	var so, se bytes.Buffer
	code := adminResetPasswordCmd([]string{"--from-stdin"},
		strings.NewReader("correct horse battery staple\n"), &so, &se)
	if code != 0 {
		t.Fatalf("admin reset-password exit %d — it cannot find a platform-installed "+
			"config, which is the only place the command it is recommended from "+
			"(public-mode serve) puts one.\nstderr: %s", code, se.String())
	}
	// The credentials must have landed next to the PLATFORM install's
	// data dir, not somewhere derived from cwd.
	storePath := filepath.Join(platform, "data", "adminauth.json")
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("no adminauth store at %s: %v", storePath, err)
	}
}

// An explicit --config still wins for both commands — the override is
// what an operator with several installs reaches for, and a silent
// fallback to a different config would be worse than an error.
func TestAdminResetPasswordExplicitConfigWins(t *testing.T) {
	cwd, platform := isolateConfigEnv(t)
	writeInstallAt(t, cwd, "local-track.flac")
	explicitDir := t.TempDir()
	explicitCfg := writeInstallAt(t, explicitDir, "explicit-track.flac")

	var so, se bytes.Buffer
	if code := adminResetPasswordCmd([]string{"--config", explicitCfg, "--from-stdin"},
		strings.NewReader("correct horse battery staple\n"), &so, &se); code != 0 {
		t.Fatalf("admin reset-password exit %d: %s", code, se.String())
	}
	if _, err := os.Stat(filepath.Join(explicitDir, "data", "adminauth.json")); err != nil {
		t.Fatalf("explicit --config was not honoured: %v", err)
	}
	for _, other := range []string{cwd, platform} {
		if _, err := os.Stat(filepath.Join(other, "data", "adminauth.json")); err == nil {
			t.Errorf("explicit --config was ignored in favour of %s", other)
		}
	}
}
