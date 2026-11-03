package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// The candidate's half of the settings check (backlog B613): `bridge serve
// --validate-config` loads the settings as serve does, says 0 or 78 or 1,
// and writes nothing. internal/updater runs it on a candidate before the
// swap and reads exactly these codes and lines, so they are a contract with
// every later release.

// checkedSettings writes a loopback config serving a library of its own from
// a data dir that does not exist yet, extra appended on line 4 onwards, and
// returns its path and the data dir.
func checkedSettings(t *testing.T, extra string) (cfg, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	cfg = filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\n%s", lib, dataDir, extra)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, dataDir
}

// runSettingsCheck runs `bridge serve --validate-config` on cfg.
func runSettingsCheck(cfg string, extra ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	args := append([]string{"serve", "--" + updater.ConfigCheckFlag, "--config", cfg}, extra...)
	code = run(context.Background(), args, &out, &errs)
	return code, out.String(), errs.String()
}

// refusedSettings are configs this version refuses, each with the words its
// refusal line carries.
var refusedSettings = []struct {
	name, extra, says string
}{
	{"a leading zero", "scanIntervalSec: 010\n", "line 4: a whole number is written with a leading zero"},
	{"a fraction", "scanIntervalSec: 21600.5\n", "line 4: cannot unmarshal a !!float value into int"},
	{"a key this version does not know", "notASetting: 1\n", "line 4: that key is not a field of type"},
	{"a setting its validation refuses", "listenAddress: \":99999\"\n", "port must be a number between 0 and 65535"},
	{"a Tailscale mode it does not know", "tailscale:\n  mode: tnset\n", "tailscale.mode"},
}

func TestTheSettingsCheckLoadsTheSettingsAsServeDoesAndWritesNothing(t *testing.T) {
	t.Run("settings serve loads", func(t *testing.T) {
		// A listener on the address the settings name: a check that bound
		// it would fail, and so would one that served.
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = held.Close() })
		cfg, dataDir := checkedSettings(t, "listenAddress: "+held.Addr().String()+"\n")
		before := readFileBytes(t, cfg)
		code, stdout, stderr := runSettingsCheck(cfg)
		if code != updater.ConfigCheckValidExit {
			t.Fatalf("check = %d, want %d; stderr=%s", code, updater.ConfigCheckValidExit, stderr)
		}
		if !strings.Contains(stdout, "config ok") || strings.Contains(stderr, updater.ConfigRefusedPrefix) {
			t.Errorf("stdout=%q stderr=%q, want an ok and no refusal", stdout, stderr)
		}
		requireCheckWroteNothing(t, cfg, before, dataDir)
	})
	for _, tc := range refusedSettings {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dataDir := checkedSettings(t, tc.extra)
			before := readFileBytes(t, cfg)
			code, _, stderr := runSettingsCheck(cfg)
			if code != updater.ConfigCheckRefusedExit {
				t.Fatalf("check = %d, want %d; stderr=%s", code, updater.ConfigCheckRefusedExit, stderr)
			}
			if !strings.Contains(stderr, updater.ConfigRefusedPrefix) || !strings.Contains(stderr, tc.says) {
				t.Errorf("stderr=%q, want a %q line saying %q", stderr, updater.ConfigRefusedPrefix, tc.says)
			}
			for _, line := range strings.Split(stderr, "\n") {
				if strings.HasPrefix(line, updater.ConfigRefusedPrefix) && strings.Contains(line, filepath.Dir(cfg)) {
					t.Errorf("a refusal line names a path: %q", line)
				}
			}
			requireCheckWroteNothing(t, cfg, before, dataDir)
		})
	}
	t.Run("two library roots with one folder name", func(t *testing.T) {
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a", "Music"), filepath.Join(dir, "b", "Music")
		for _, d := range []string{a, b} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cfg := filepath.Join(dir, "bridge.yaml")
		dataDir := filepath.Join(dir, "data")
		if err := os.WriteFile(cfg, []byte(fmt.Sprintf("libraryRoots:\n  - %s\n  - %s\ndataDir: %s\n", a, b, dataDir)), 0o600); err != nil {
			t.Fatal(err)
		}
		before := readFileBytes(t, cfg)
		code, _, stderr := runSettingsCheck(cfg)
		if code != updater.ConfigCheckRefusedExit || !strings.Contains(stderr, updater.ConfigRefusedPrefix+"libraryRoots") {
			t.Fatalf("check = %d, stderr=%q; want %d and a libraryRoots refusal", code, stderr, updater.ConfigCheckRefusedExit)
		}
		for _, line := range strings.Split(stderr, "\n") {
			if strings.HasPrefix(line, updater.ConfigRefusedPrefix) && strings.Contains(line, dir) {
				t.Errorf("a refusal line names a root's path: %q", line)
			}
		}
		requireCheckWroteNothing(t, cfg, before, dataDir)
	})
	t.Run("no settings file", func(t *testing.T) {
		code, _, stderr := runSettingsCheck(filepath.Join(t.TempDir(), "bridge.yaml"))
		if code != updater.ConfigCheckFailedExit || strings.Contains(stderr, updater.ConfigRefusedPrefix) {
			t.Errorf("check of a missing file = %d (stderr %q), want %d and no refusal", code, stderr, updater.ConfigCheckFailedExit)
		}
	})
	t.Run("with --init-if-missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "bridge.yaml")
		code, _, _ := runSettingsCheck(missing, "--init-if-missing")
		if code != 2 {
			t.Errorf("check with --init-if-missing = %d, want the usage error 2", code)
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Errorf("the check wrote a config (lstat %v)", err)
		}
	})
}

// requireCheckWroteNothing asserts a check left the settings file's bytes as
// they were and made no data dir.
func requireCheckWroteNothing(t *testing.T, cfg string, before []byte, dataDir string) {
	t.Helper()
	if after := readFileBytes(t, cfg); !bytes.Equal(after, before) {
		t.Errorf("the check changed the settings file:\n%s\nwas\n%s", after, before)
	}
	if _, err := os.Lstat(dataDir); !os.IsNotExist(err) {
		t.Errorf("the check made the data dir (lstat %v)", err)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheSettingsCheckRefusesWhatServeRefuses holds the check to serve: each
// config serve refuses at its start the check refuses too, and serve refuses
// it before it writes anything to the data dir, which is where the early
// rollback must find the data dir untouched. The one refusal of serve's the
// check does not make is a library root it cannot reach, which is the host's
// state at that moment and the user's who runs the check, never the
// settings' (CheckLibraryRootsAccessible, under Config in config-lifecycle.md).
func TestTheSettingsCheckRefusesWhatServeRefuses(t *testing.T) {
	for _, tc := range refusedSettings {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dataDir := checkedSettings(t, tc.extra)
			var out, errs bytes.Buffer
			if code := runServe(t.Context(), serveOpts{configPath: cfg}, &out, &errs); code != 2 {
				t.Fatalf("serve = %d, want 2; stderr=%s", code, errs.String())
			}
			if _, err := os.Lstat(dataDir); !os.IsNotExist(err) {
				t.Errorf("serve wrote the data dir before it refused the settings (lstat %v)", err)
			}
			if code, _, stderr := runSettingsCheck(cfg); code != updater.ConfigCheckRefusedExit {
				t.Errorf("check = %d, want %d: serve refuses these settings; stderr=%s", code, updater.ConfigCheckRefusedExit, stderr)
			}
		})
	}
	t.Run("a library root serve cannot reach", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "bridge.yaml")
		body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\n", filepath.Join(dir, "gone"), filepath.Join(dir, "data"))
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errs bytes.Buffer
		if code := runServe(t.Context(), serveOpts{configPath: cfg}, &out, &errs); code != 2 {
			t.Fatalf("serve over a missing root = %d, want 2; stderr=%s", code, errs.String())
		}
		if code, _, stderr := runSettingsCheck(cfg); code != updater.ConfigCheckValidExit {
			t.Errorf("check = %d, want %d: a root's reach is the host's, not the settings'; stderr=%s", code, updater.ConfigCheckValidExit, stderr)
		}
	})
}
