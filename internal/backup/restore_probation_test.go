package backup_test

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// userVersionOf is the migration step the database at path is at.
func userVersionOf(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestARestoreThatWouldMigrateIsRefusedWhileTheInstallAwaitsItsConfirmation:
// a restore opens the restored store at once to mint its backup epoch, which
// migrates a store from behind the ladder's top; while the install of this
// version awaits its confirmation after a start that did not end cleanly, a
// command may not migrate it (updater.MigrationHeld), so the restore refuses
// before its first write, nothing restored, rather than half way (backlog
// B660's second round). With no such start, it restores and migrates as
// before.
func TestARestoreThatWouldMigrateIsRefusedWhileTheInstallAwaitsItsConfirmation(t *testing.T) {
	src := t.TempDir()
	old := filepath.Join(src, "bridge.db")
	// The ladder's top, from a store made fresh.
	fresh := filepath.Join(t.TempDir(), "bridge.db")
	s, err := manifest.OpenStore(fresh)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	latest := userVersionOf(t, fresh)
	if err := manifest.WriteDatabaseAtVersionForTest(old, latest-1); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, ManifestDB: old})
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	live := filepath.Join(dst, "bridge.db")
	ls, err := manifest.OpenStore(live)
	if err != nil {
		t.Fatal(err)
	}
	_ = ls.Close()
	before, _ := os.ReadFile(live)
	marker := updater.State{Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: "0.0.0-previous",
		SwapStarted: true, DataDirOpenedBy: version.ServerVersion, FailedStarts: 1}
	if err := updater.SaveState(dst, marker); err != nil {
		t.Fatal(err)
	}
	err = backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{})
	if !errors.Is(err, updater.ErrMigrationHeld) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("a restore of a store behind, the install on probation after a failed start = %v, want it refused, nothing restored", err)
	}
	if after, _ := os.ReadFile(live); !bytes.Equal(before, after) {
		t.Error("the refused restore wrote the live store")
	}
	if st, err := updater.LoadState(dst); err != nil || st.StoreMigratedBy != "" {
		t.Errorf("the marker after the refused restore = %+v, %v; want no migration recorded", st, err)
	}
	marker.FailedStarts = 0
	if err := updater.SaveState(dst, marker); err != nil {
		t.Fatal(err)
	}
	if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{}); err != nil {
		t.Fatalf("the restore with no failed start = %v, want it done", err)
	}
	if v := userVersionOf(t, live); v != latest {
		t.Errorf("the restored store is at %d, want it migrated to %d", v, latest)
	}
	if st, _ := updater.LoadState(dst); st.StoreMigratedBy != version.ServerVersion {
		t.Errorf("the marker after the restore = %+v, want its migration recorded", st)
	}
}
