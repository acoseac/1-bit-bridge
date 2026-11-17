package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// TestADatabaseOnlyRestoreOverAnArmedInstallKeepsEveryRollbackOff is T14's
// ladder-top row and T13's refusals for the database-only restore: over an
// armed install that recorded nothing, the restore of a snapshot at this
// version's ladder top records that this version changed the store and
// opened the data dir, so the crash rollback, the early one and the
// operator's refuse; and the install marker's admission refuses, before
// anything is copied or moved and with nothing recorded, while the install
// awaits its confirmation after a start that did not end cleanly
// (updater.ErrMigrationHeld), under a veto of this version's migrations
// (updater.ErrMigrationVetoed), and where this version is the one the armed
// install replaces (updater.ErrRestoreByPreviousVersion): each before the
// snapshot's database is copied and prepared.
func TestADatabaseOnlyRestoreOverAnArmedInstallKeepsEveryRollbackOff(t *testing.T) {
	t.Run("an armed install", func(t *testing.T) {
		f := newDBFixture(t, withSidecars)
		marker := armedInstall(0)
		if err := updater.SaveState(f.dataDir, marker); err != nil {
			t.Fatal(err)
		}
		if ok, why := updater.CrashRollbackAtLimit(marker, version.ServerVersion); !ok {
			t.Fatalf("the fixture's crash rollback refuses before the restore: %s", why)
		}
		if _, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts()); err != nil {
			t.Fatalf("RestoreDatabase: %v", err)
		}
		st, err := updater.LoadState(f.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		if st.StoreMigratedBy != version.ServerVersion || st.DataDirOpenedBy != version.ServerVersion {
			t.Fatalf("the marker after the restore = %+v, want StoreMigratedBy and DataDirOpenedBy this version", st)
		}
		if ok, _ := updater.CrashRollbackAtLimit(st, version.ServerVersion); ok {
			t.Error("the crash rollback would put the backup back over the restored store")
		}
		if ok, _ := updater.EarlyRollback(st, version.ServerVersion, time.Now().UTC()); ok {
			t.Error("the early rollback would put the backup back over the restored store")
		}
		if updater.RollbackRefusal(st) == "" {
			t.Error("the operator's rollback would put the backup back over the restored store")
		}
	})
	for _, tc := range []struct {
		name string
		mark func() updater.State
		want error
	}{
		{"an install held after a start that did not end cleanly", func() updater.State {
			m := armedInstall(1)
			m.DataDirOpenedBy = version.ServerVersion
			return m
		}, updater.ErrMigrationHeld},
		{"a veto of this version's migrations", func() updater.State {
			m := armedInstall(0)
			m.MigrationVeto = version.ServerVersion
			return m
		}, updater.ErrMigrationVetoed},
		{"this version the one the install replaces", func() updater.State {
			m := armedInstall(0)
			m.TargetVersion, m.PreviousVersion = "0.0.0-next", version.ServerVersion
			return m
		}, updater.ErrRestoreByPreviousVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFixture(t, withSidecars)
			if err := updater.SaveState(f.dataDir, tc.mark()); err != nil {
				t.Fatal(err)
			}
			marker := readBytes(t, filepath.Join(f.dataDir, "update-state.json"))
			before := f.originals(t)
			prepared := false
			t.Cleanup(backup.SetRestoreStepForTest(func(step string) { prepared = prepared || step == "B9" }))
			_, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts())
			if prepared {
				t.Error("the restore copied and prepared the snapshot's database before the marker's refusal")
			}
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Fatalf("RestoreDatabase = %v, want %v, nothing restored", err, tc.want)
			}
			requireUnchanged(t, f, before)
			if got := readBytes(t, filepath.Join(f.dataDir, "update-state.json")); string(got) != string(marker) {
				t.Error("the refused restore changed the install marker")
			}
		})
	}
}

// TestADatabaseOnlyRestoreNeitherReadsNorWritesThePlayersFiles is T32: the
// player's identity, its reservation and its DSD state, and the library's
// files, are byte for byte as they were after a database-only restore, which
// restores nothing but the database.
func TestADatabaseOnlyRestoreNeitherReadsNorWritesThePlayersFiles(t *testing.T) {
	f := newDBFixture(t, withSidecars)
	lib := filepath.Join(f.dataDir, "library-root")
	if err := os.MkdirAll(filepath.Join(lib, "Album"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(f.dataDir, "player-uuid"):            "4b1a7a52-6a5b-4f44-9f3b-0c1a2f6c7d8e\n",
		filepath.Join(f.dataDir, "player-ids.json"):        `{"ids":7,"marks":3}`,
		filepath.Join(f.dataDir, "player-dsd.json"):        `{"schema":1}`,
		filepath.Join(lib, "Album", "01 - Track.flac"):     "not really a flac",
		filepath.Join(f.dataDir, backup.AdminAuthFileName): `{"account":"kept"}`,
		filepath.Join(f.dataDir, "player-initialized"):     "sealed",
	}
	for p, s := range files {
		writeFile(t, p, s)
	}
	if _, err := backup.RestoreDatabase(f.snap, f.dataDir, f.opts()); err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	for p, s := range files {
		if got := string(readBytes(t, p)); got != s {
			t.Errorf("%s changed", filepath.Base(p))
		}
	}
}
