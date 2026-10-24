//go:build unix

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestFailedSealCompletionKeepsItsErrorAndNamesWriteAccess: a seal
// completion that fails for any reason but a busy lock is not deferred: it
// recurs at the next serve boot or start. The backup goes on and logs the
// failure with its error and the remedy, restoring write access to the data
// directory, never that the next lifecycle operation completes it.
func TestFailedSealCompletionKeepsItsErrorAndNamesWriteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory whatever its mode")
	}
	reservationStorage(t)
	dir := loneSealBox(t)
	if err := os.MkdirAll(filepath.Join(dir, backup.BackupsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := fsutil.OpenLockFile(dir, "server.lock")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// The marker's rewrite is what fails: the data directory takes no new file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	rec := loggingtest.Record(t)
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
		t.Fatalf("failed completion stopped the backup: %v", err)
	}
	lines := rec.Lines("player seal completion failed")
	if len(lines) != 1 || !strings.Contains(lines[0], "permission denied") || !strings.Contains(lines[0], "restore write access to the data directory") ||
		strings.Contains(lines[0], "the next lifecycle operation") || strings.Contains(lines[0], "stop the service") {
		t.Fatalf("failed completion's text: %v", lines)
	}
	if deferred := rec.Lines("player seal completion deferred"); len(deferred) != 0 {
		t.Fatalf("failed completion logged as deferred: %v", deferred)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the failure did not recur at the next start: %v", err)
	}
}
