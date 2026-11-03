package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

func requirePathlessMarkerOutput(t *testing.T, dir, output, want string) {
	t.Helper()
	if !strings.Contains(output, want) {
		t.Errorf("missing %q in %s", want, output)
	}
	for _, path := range []string{dir, updater.StatePath(dir)} {
		if strings.Contains(output, path) {
			t.Errorf("marker output carries %q: %s", path, output)
		}
	}
}

func unreadableInstallMarker(t *testing.T, dir string, removable bool) {
	t.Helper()
	path := updater.StatePath(dir)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !removable {
		path = filepath.Join(path, "held")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestServeMarkerDiagnosticsCarryNoPaths(t *testing.T) {
	t.Run("removed marker", func(t *testing.T) {
		dir := t.TempDir()
		unreadableInstallMarker(t, dir, true)
		var stderr bytes.Buffer
		if err := disarmEarlyRollback(t.Context(), dir, serveOpts{}, &stderr); err != nil {
			t.Fatal(err)
		}
		requirePathlessMarkerOutput(t, dir, stderr.String(), "removed an install marker that could not be read")
	})
	t.Run("admission refusal", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		unreadableInstallMarker(t, r.dataDir, false)
		code, stderr := r.serve(t, serveOpts{})
		if code != 1 {
			t.Fatalf("serve exit %d: %s", code, stderr)
		}
		requirePathlessMarkerOutput(t, r.dataDir, stderr, "not starting, so that no rollback")
		requireNoStoreOpened(t, r.dataDir)
	})
	t.Run("store refusal", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		// After admission, before OpenStore: no listener has been bound yet.
		code, stderr := r.serve(t, serveOpts{playerStatePreflight: func(string, bool) error {
			unreadableInstallMarker(t, r.dataDir, false)
			return nil
		}})
		if code != 1 {
			t.Fatalf("serve exit %d: %s", code, stderr)
		}
		requirePathlessMarkerOutput(t, r.dataDir, stderr, "open manifest store:")
	})
	t.Run("boot read", func(t *testing.T) {
		dir := t.TempDir()
		unreadableInstallMarker(t, dir, true)
		var stderr bytes.Buffer
		maybeRollbackOnBoot(t.Context(), &stderr, dir, filepath.Join(t.TempDir(), "bridge"))
		requirePathlessMarkerOutput(t, dir, stderr.String(), "updater: load state:")
	})
	t.Run("early rollback reread", func(t *testing.T) {
		r := newRefusedInstall(t, "scanIntervalSec: 010\n")
		// The first read proves an armed install. Replace the marker when the
		// lock is taken so the second read fails, without racing another thread.
		t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error {
			unreadableInstallMarker(t, r.dataDir, true)
			return nil
		}))
		code, stderr := r.serve(t, serveOpts{})
		if code != 2 {
			t.Fatalf("serve exit %d: %s", code, stderr)
		}
		requirePathlessMarkerOutput(t, r.dataDir, stderr, "the install marker cannot be read again")
	})
}
