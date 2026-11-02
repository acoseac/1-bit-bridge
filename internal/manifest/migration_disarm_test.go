package manifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The store's open is where a version's migrations run, whichever command
// opens it (serve, doctor, clear-missing, a scan), so before its first step
// it records in an armed install marker beside the database that this
// version has opened the data dir (updater.State.DataDirOpenedBy): neither
// the early rollback nor the boot rollback then puts back a backup that
// predates what the migration writes (backlog B613's round 2, Codex's
// finding 1). An open that migrates nothing records nothing.

// olderStoreIn writes the store in dataDir as the version before this one
// left it, one migration behind, and returns its path.
func olderStoreIn(t *testing.T, dataDir string) string {
	t.Helper()
	return databaseAtVersionAt(t, DefaultDBPath(dataDir), latestVersion-1)
}

// armedInstall writes in dataDir the marker an install of this version over
// another leaves, and returns it.
func armedInstall(t *testing.T, dataDir string) updater.State {
	t.Helper()
	st := updater.State{Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: "0.0.0-previous",
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}
	if err := updater.SaveState(dataDir, st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAStoreOpenThatMigratesRecordsItInAnArmedInstallMarker(t *testing.T) {
	dataDir := t.TempDir()
	db := olderStoreIn(t, dataDir)
	armed := armedInstall(t, dataDir)
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if v := versionOnDisk(t, db); v != latestVersion {
		t.Fatalf("user_version %d after the open, want %d", v, latestVersion)
	}
	st, err := updater.LoadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker after a migrating open = %+v, want it to record that %s opened the data dir", st, version.ServerVersion)
	}
	st.DataDirOpenedBy = ""
	if !st.AttemptedAt.Equal(armed.AttemptedAt) || st.Status != armed.Status || st.TargetVersion != armed.TargetVersion ||
		st.PreviousVersion != armed.PreviousVersion || !st.SwapStarted {
		t.Errorf("the marker after a migrating open = %+v, want the install it records kept as it was (%+v)", st, armed)
	}
}

func TestAStoreOpenThatMigratesNothingLeavesTheMarkerAsItIs(t *testing.T) {
	dataDir := t.TempDir()
	db := DefaultDBPath(dataDir)
	// A migrating open with no marker beside it writes none.
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(updater.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("a migrating open with no install armed left a marker (lstat %v)", err)
	}
	armedInstall(t, dataDir)
	before, err := os.ReadFile(updater.StatePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(updater.StatePath(dataDir)); err != nil || !bytes.Equal(after, before) {
		t.Errorf("an open that migrated nothing changed the marker (%v): %s", err, after)
	}
}

// TestAStoreOpenRefusesToMigrateWhileARollbackOfItsVersionIsUnderWay: the
// early rollback's record of an install of this version (backlog B613's
// round 3) says the backup may already be back over this binary, so a
// command of this version, running from the image it was started with,
// must not migrate the store past what the restored version knows. An open
// that migrates nothing is not refused.
func TestAStoreOpenRefusesToMigrateWhileARollbackOfItsVersionIsUnderWay(t *testing.T) {
	dataDir := t.TempDir()
	db := olderStoreIn(t, dataDir)
	record := updater.RollbackRecord(armedInstall(t, dataDir), version.ServerVersion,
		version.ServerVersion+" refused this bridge's settings at its first start: line 4: x", "a digest")
	if err := updater.SaveState(dataDir, record); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(updater.StatePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(db)
	if err == nil {
		s.Close()
		t.Fatal("the store opened, and migrated, while a rollback of this version's install was under way")
	}
	if !errors.Is(err, updater.ErrRollbackUnderWay) {
		t.Errorf("OpenStore = %v, want the rollback under way named", err)
	}
	if v := versionOnDisk(t, db); v != latestVersion-1 {
		t.Errorf("user_version %d after the refused open, want %d: a step ran", v, latestVersion-1)
	}
	if after, err := os.ReadFile(updater.StatePath(dataDir)); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the refused open changed the marker (%v): %s", err, after)
	}

	current := DefaultDBPath(t.TempDir())
	s, err = OpenStore(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := updater.SaveState(filepath.Dir(current), record); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(current)
	if err != nil {
		t.Fatalf("an open that migrates nothing beside the record = %v, want it opened", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAStoreOpenThatCannotRecordItsMigrationMigratesNothing: a marker that can
// neither be read nor removed (here a directory with something in it) cannot
// be told an armed install's or not, so the open refuses before its first
// step, as serve refuses to start over a disarm that fails.
func TestAStoreOpenThatCannotRecordItsMigrationMigratesNothing(t *testing.T) {
	dataDir := t.TempDir()
	db := olderStoreIn(t, dataDir)
	if err := os.MkdirAll(filepath.Join(updater.StatePath(dataDir), "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(db)
	if err == nil {
		s.Close()
		t.Fatal("the store opened, and migrated, over an install marker it could not read or remove")
	}
	if !strings.Contains(err.Error(), "install marker") {
		t.Errorf("OpenStore = %v, want it to say the install marker stopped the migration", err)
	}
	if v := versionOnDisk(t, db); v != latestVersion-1 {
		t.Errorf("user_version %d after the refused open, want %d: a step ran", v, latestVersion-1)
	}
}
