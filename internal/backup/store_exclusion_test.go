package backup_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// backupLockHelperEnv names the database whose lifetime lock the helper
// process holds exclusively, as a restore in another process would;
// backupLockHelperOpenEnv makes it open the store instead, as a command that
// has the database open does; and backupLockHelperMakeEnv makes it, holding
// the lock, make the database's directory and the database in it first, as a
// restore into a data dir not there yet does.
const (
	backupLockHelperEnv     = "BRIDGE_BACKUP_STORE_LOCK_HELPER"
	backupLockHelperOpenEnv = "BRIDGE_BACKUP_STORE_LOCK_OPEN"
	backupLockHelperMakeEnv = "BRIDGE_BACKUP_STORE_LOCK_MAKE"
)

// TestBackupStoreLockHelperProcess is the second process the store
// exclusion's tests hold the lock in; it skips unless its environment names
// a database. It says "ready" once it holds the lock, and lets go when its
// stdin ends.
func TestBackupStoreLockHelperProcess(t *testing.T) {
	path := os.Getenv(backupLockHelperEnv)
	if path == "" {
		t.Skip("the store exclusion's helper process: run by its tests alone")
	}
	if os.Getenv(backupLockHelperOpenEnv) != "" {
		s, err := manifest.OpenStore(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = s.Close()
		os.Exit(0)
	}
	making := os.Getenv(backupLockHelperMakeEnv) != ""
	if making {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
	}
	h, err := manifest.HoldStoreExclusive(path)
	if err != nil {
		fmt.Printf("refused %v\n", err)
		os.Exit(0)
	}
	if making {
		// The restore's database, put in place under its own hold, which
		// this process's open counts on.
		s, err := manifest.OpenStore(path)
		if err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
		if err := s.Close(); err != nil {
			fmt.Printf("refused %v\n", err)
			os.Exit(0)
		}
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	h.Release()
	os.Exit(0)
}

// restoreInAnotherProcess holds the lifetime lock of the database at path
// exclusively in another process, as a restore does, until the test ends or
// the returned stop is called; env's NAME=VALUE pairs are the helper's too.
func restoreInAnotherProcess(t *testing.T, path string, env ...string) (stop func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackupStoreLockHelperProcess$", "-test.timeout=2m")
	cmd.Env = append(append(os.Environ(), backupLockHelperEnv+"="+path), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = stdin.Close()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if l := sc.Text(); l == "ready" || strings.HasPrefix(l, "refused") {
				line <- l
				return
			}
		}
		line <- ""
	}()
	select {
	case l := <-line:
		if l != "ready" {
			t.Fatalf("the other process could not hold the store: %q", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the other process said nothing for 30 s")
	}
	return stop
}

// treeState is every entry under dir, recursively: its kind, and a regular
// file's size and SHA-256, so two states compare byte for byte.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			out[rel] = fi.Mode().Type().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = fmt.Sprintf("file %d %s", len(b), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// requireSameTree requires dir to be as before was.
func requireSameTree(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	if after := treeState(t, dir); !maps.Equal(before, after) {
		t.Errorf("the data directory changed:\nbefore %v\nafter  %v", before, after)
	}
}

// liveStore opens and closes a store at path, so a database stands there.
func liveStore(t *testing.T, path string) {
	t.Helper()
	s, err := manifest.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// ladderTopSnapshot is a snapshot of a store made fresh in another data
// directory, so at this build's ladder top.
func ladderTopSnapshot(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	db := filepath.Join(src, backup.ManifestDBFileName)
	liveStore(t, db)
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, ManifestDB: db})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// requireSealStillOwed requires the box at dir's lone seal still owed: the restore
// or the capture never reached its completion, which writes the initialized
// marker.
func requireSealStillOwed(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "player-initialized")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the player seal's completion ran (%v), a write before the refusal", err)
	}
}

// TestARestoreHoldKeepsEveryOpenerOut (T2, this package's half): with
// another process's restore holding the store's lifetime lock exclusively, a
// live snapshot is refused (manifest.ErrStoreRestoring) before it writes
// anything: no backups folder made, a player seal it owes not completed, the
// data directory byte for byte as it was (PR 2a's brief review, finding 3).
func TestARestoreHoldKeepsEveryOpenerOut(t *testing.T) {
	reservationStorage(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	liveStore(t, db)
	restoreInAnotherProcess(t, db)
	before := treeState(t, dir)
	res := backup.SnapshotAndVerify(t.Context(), backup.Sources{DataDir: dir, ManifestDB: db,
		PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}, backup.SnapshotOptions{})
	if !errors.Is(res.CaptureErr, manifest.ErrStoreRestoring) || res.Dir != "" {
		t.Fatalf("a snapshot beside a restore's hold = %+v, want its capture refused with ErrStoreRestoring", res)
	}
	requireSameTree(t, dir, before)
	if _, err := os.Lstat(filepath.Join(dir, backup.BackupsDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused snapshot made the backups folder (%v)", err)
	}
	requireSealStillOwed(t, dir)
}

// TestAnOpenOverAnUnfinishedRestoreRefusesAndMakesNothing (T3, the live
// snapshot's half): an unfinished restore's journal and no database: the
// capture is refused (manifest.ErrRestoreIncomplete) before it writes
// anything, so no snapshot without a database is published, no backups
// folder is made, and none of the database's files.
func TestAnOpenOverAnUnfinishedRestoreRefusesAndMakesNothing(t *testing.T) {
	reservationStorage(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	if err := os.WriteFile(manifest.RestoreJournalPath(db), []byte(`{"format":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := backup.SnapshotAndVerify(t.Context(), backup.Sources{DataDir: dir, ManifestDB: db,
		PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}, backup.SnapshotOptions{})
	if !errors.Is(res.CaptureErr, manifest.ErrRestoreIncomplete) || res.Dir != "" {
		t.Fatalf("a snapshot over an unfinished restore = %+v, want its capture refused with ErrRestoreIncomplete", res)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm", filepath.Join(dir, backup.BackupsDirName)} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was made (%v)", filepath.Base(p), err)
		}
	}
	requireSealStillOwed(t, dir)
}

// TestTheFullRestoreRefusesWhileADatabaseRestoreIsUnfinished (T27): a
// journal at the destination refuses the full restore before its first
// write, the player seal's completion: the restore's write hook never runs,
// and the player's files and the database are byte for byte as they were.
func TestTheFullRestoreRefusesWhileADatabaseRestoreIsUnfinished(t *testing.T) {
	reservationStorage(t)
	snap := ladderTopSnapshot(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	liveStore(t, db)
	if err := os.WriteFile(manifest.RestoreJournalPath(db), []byte(`{"format":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, dir)
	wrote := false
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{}, func() { wrote = true })
	err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: db, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}, opts)
	if !errors.Is(err, manifest.ErrRestoreIncomplete) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("a full restore over an unfinished database restore = %v, want ErrRestoreIncomplete, nothing restored", err)
	}
	if wrote {
		t.Error("the refused restore reached its first write")
	}
	requireSameTree(t, dir, before)
	requireSealStillOwed(t, dir)
}

// TestEveryRestoreTakesTheStoresLockItself (PR 2a's brief review, its
// first finding): RestoreWithOptions, called by any caller and not bridge
// restore alone, takes the store's lifetime lock exclusively before it
// touches anything: beside a store another process has open it is refused
// (manifest.ErrStoreHeld), nothing restored, the data directory as it was.
func TestEveryRestoreTakesTheStoresLockItself(t *testing.T) {
	snap := ladderTopSnapshot(t)
	dst := t.TempDir()
	live := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, live)
	restoreInAnotherProcess(t, live, backupLockHelperOpenEnv+"=1")
	before := treeState(t, dst)
	err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live, TokensJSON: filepath.Join(dst, "tokens.json")}, backup.RestoreOptions{})
	if !errors.Is(err, manifest.ErrStoreHeld) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("a restore beside a store another process has open = %v, want ErrStoreHeld, nothing restored", err)
	}
	requireSameTree(t, dst, before)
}

// armedInstall is an install marker of this version, armed, its swap made,
// recent, with nothing recorded: the state in which a crash rollback, and an
// early one, would put the backup back.
func armedInstall(failedStarts int) updater.State {
	return updater.State{Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: "0.0.0-previous",
		SwapStarted: true, AttemptedAt: time.Now().UTC(), FailedStarts: failedStarts}
}

// TestARestoreOverAnArmedInstallKeepsEveryRollbackOff (T35): a full
// restore of a snapshot at this version's ladder top, made in another data
// directory, over an armed install that recorded nothing: the restore records
// that this version changed the store and opened the data dir, so the crash
// rollback and the early one refuse, and so does the operator's
// (RollbackRefusal). While the install awaits its confirmation after a start
// that did not end cleanly the same restore refuses (ErrMigrationHeld) before
// its first write to the restored files, and where its record is due and the
// marker's lock cannot be taken it refuses with nothing written. A snapshot
// that holds no database, and a restore that leaves the database out, record
// nothing (the brief's review, finding 4). Until B653 PR 2a nothing was
// recorded for a snapshot at the top (the only recording case was a migrating
// one).
func TestARestoreOverAnArmedInstallKeepsEveryRollbackOff(t *testing.T) {
	snap := ladderTopSnapshot(t)

	t.Run("an armed install", func(t *testing.T) {
		dst := t.TempDir()
		live := filepath.Join(dst, backup.ManifestDBFileName)
		liveStore(t, live)
		marker := armedInstall(0)
		if err := updater.SaveState(dst, marker); err != nil {
			t.Fatal(err)
		}
		if ok, why := updater.CrashRollbackAtLimit(marker, version.ServerVersion); !ok {
			t.Fatalf("the fixture's crash rollback refuses before the restore: %s", why)
		}
		if ok, why := updater.EarlyRollback(marker, version.ServerVersion, time.Now().UTC()); !ok {
			t.Fatalf("the fixture's early rollback refuses before the restore: %s", why)
		}
		if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{}); err != nil {
			t.Fatalf("the restore = %v", err)
		}
		st, err := updater.LoadState(dst)
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

	t.Run("an install held after a start that did not end cleanly", func(t *testing.T) {
		dst := t.TempDir()
		live := filepath.Join(dst, backup.ManifestDBFileName)
		liveStore(t, live)
		marker := armedInstall(1)
		marker.DataDirOpenedBy = version.ServerVersion
		if err := updater.SaveState(dst, marker); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(live)
		err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{})
		if !errors.Is(err, updater.ErrMigrationHeld) || !strings.Contains(err.Error(), "nothing was restored") {
			t.Fatalf("a restore of a ladder-top snapshot while the install is held = %v, want ErrMigrationHeld, nothing restored", err)
		}
		if after, _ := os.ReadFile(live); !bytes.Equal(before, after) {
			t.Error("the refused restore wrote the live database")
		}
		if st, err := updater.LoadState(dst); err != nil || st.StoreMigratedBy != "" {
			t.Errorf("the marker after the refused restore = %+v, %v; want nothing recorded", st, err)
		}
	})

	// A restore stopped once its record is made and before its first write
	// to the restored files leaves the record, durable (SaveState's barrier),
	// and nothing else: the record step is made here as the restore makes it
	// (updater.DisarmBeforeRestoring, recordRestoredStore's call), standing in
	// for a kill right after it. The rerun records nothing more, completes,
	// and every rollback still refuses.
	t.Run("a rerun after a restore stopped once its record was made", func(t *testing.T) {
		dst := t.TempDir()
		live := filepath.Join(dst, backup.ManifestDBFileName)
		liveStore(t, live)
		if err := updater.SaveState(dst, armedInstall(0)); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(live)
		if _, err := updater.DisarmBeforeRestoring(dst, version.ServerVersion); err != nil {
			t.Fatal(err)
		}
		st, err := updater.LoadState(dst)
		if err != nil || st.StoreMigratedBy != version.ServerVersion {
			t.Fatalf("the marker the stopped restore left = %+v, %v; want its record", st, err)
		}
		if after, _ := os.ReadFile(live); !bytes.Equal(before, after) {
			t.Fatal("the record step wrote the database")
		}
		if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{}); err != nil {
			t.Fatalf("the rerun over the record = %v, want it done", err)
		}
		st, err = updater.LoadState(dst)
		if err != nil || st.StoreMigratedBy != version.ServerVersion || st.DataDirOpenedBy != version.ServerVersion {
			t.Fatalf("the marker after the rerun = %+v, %v; want the record kept", st, err)
		}
		if ok, _ := updater.CrashRollbackAtLimit(st, version.ServerVersion); ok {
			t.Error("the crash rollback would put the backup back after the rerun")
		}
	})

	t.Run("the marker's lock held past its wait", func(t *testing.T) {
		dst := t.TempDir()
		live := filepath.Join(dst, backup.ManifestDBFileName)
		liveStore(t, live)
		if err := updater.SaveState(dst, armedInstall(0)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(updater.SetStateLockWaitForTest(200 * time.Millisecond))
		lock, err := updater.LockState(t.Context(), dst)
		if err != nil {
			t.Fatal(err)
		}
		before := treeState(t, dst)
		err = backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{})
		lock.Release()
		if err == nil || !strings.Contains(err.Error(), "nothing was restored") {
			t.Fatalf("a restore whose record cannot take the marker's lock = %v, want it refused, nothing restored", err)
		}
		requireSameTree(t, dst, before)
	})

	t.Run("a snapshot with no database, and a restore that leaves it out", func(t *testing.T) {
		src := t.TempDir()
		tokens := filepath.Join(src, "tokens.json")
		if err := os.WriteFile(tokens, []byte("[]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		noDB, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, TokensJSON: tokens})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			snap string
			db   bool
		}{{"no database in the snapshot", noDB, true}, {"the database left out", snap, false}} {
			dst := t.TempDir()
			live := filepath.Join(dst, backup.ManifestDBFileName)
			liveStore(t, live)
			if err := updater.SaveState(dst, armedInstall(0)); err != nil {
				t.Fatal(err)
			}
			targets := backup.Targets{TokensJSON: filepath.Join(dst, "tokens.json")}
			if c.db {
				targets.ManifestDB = live
			}
			if err := backup.RestoreWithOptions(c.snap, targets, backup.RestoreOptions{}); err != nil {
				t.Fatalf("%s: the restore = %v", c.name, err)
			}
			if st, err := updater.LoadState(dst); err != nil || st.StoreMigratedBy != "" || st.DataDirOpenedBy != "" {
				t.Errorf("%s: the marker = %+v, %v; want nothing recorded, no store changed", c.name, st, err)
			}
		}
	})
}

// epochOf reads the backup epoch of the database file at path, alone, as it
// stands on disk, with nothing beside it read or made (immutable=1).
func epochOf(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro&immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var epoch string
	if err := db.QueryRow(`SELECT epoch FROM backup_epoch WHERE id = 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

// TestTheFullRestoreKeepsAWALWhoseEpochDidNotCheckpoint (T36, the brief's
// review, finding 5): a real reader transaction, taken once the restored
// store is open and before its epoch is minted, keeps the TRUNCATE
// checkpoint from completing; the restore then keeps the -wal and -shm, says
// the epoch is still in the WAL, and returns, the new epoch in the WAL alone
// (the main file still holds the snapshot's), and the next open reads it.
// Until B653 PR 2a the WAL was removed whatever the checkpoint answered.
func TestTheFullRestoreKeepsAWALWhoseEpochDidNotCheckpoint(t *testing.T) {
	snap := ladderTopSnapshot(t)
	snapEpoch := epochOf(t, filepath.Join(snap, backup.ManifestDBFileName))
	dst := t.TempDir()
	live := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, live)
	var reader *sql.DB
	var tx *sql.Tx
	t.Cleanup(manifest.SetBeforeEpochMintForTest(func(path string) {
		if path != live || reader != nil {
			return
		}
		var err error
		if reader, err = sql.Open("sqlite", dsn.File(path, "_pragma=busy_timeout(5000)")); err != nil {
			t.Error(err)
			return
		}
		if tx, err = reader.Begin(); err != nil {
			t.Error(err)
			return
		}
		var epoch string
		if err := tx.QueryRow(`SELECT epoch FROM backup_epoch WHERE id = 1`).Scan(&epoch); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(func() {
		if tx != nil {
			_ = tx.Rollback()
		}
		if reader != nil {
			_ = reader.Close()
		}
	})
	var warned []string
	err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{Warn: func(s string) { warned = append(warned, s) }})
	if err != nil {
		t.Fatalf("the restore whose epoch's checkpoint was kept from completing = %v, want it done, its WAL kept", err)
	}
	if tx == nil {
		t.Fatal("the reader was never taken")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "still in its WAL") {
		t.Errorf("the restore said %q, want that the new epoch is still in the WAL", warned)
	}
	for _, p := range []string{live + "-wal", live + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil || (p == live+"-wal" && fi.Size() == 0) {
			t.Fatalf("%s after the restore = %v, %v; want it kept, the epoch in it", filepath.Base(p), fi, err)
		}
	}
	// The main file alone still holds the snapshot's epoch: the new one is in
	// the WAL. Read from a copy of the main file, beside nothing.
	alone := filepath.Join(t.TempDir(), "bridge.db")
	b, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alone, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := epochOf(t, alone); got != snapEpoch {
		t.Fatalf("the main file alone holds epoch %s, want the snapshot's %s: the test did not keep the epoch in the WAL", got, snapEpoch)
	}
	_ = tx.Rollback()
	_ = reader.Close()
	tx, reader = nil, nil
	s, err := manifest.OpenStore(live)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.BackupEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == snapEpoch || got == "" {
		t.Fatalf("the next open reads epoch %q, the snapshot's (%s): the new epoch was lost with the WAL", got, snapEpoch)
	}
}

// TestARestoreLeavesItsSnapshotAsItWas (T41): a full restore of a snapshot
// whose database is one step behind the ladder reads its version through
// the snapshot reader, so the snapshot's folder lists what it listed before
// (no lock file, no -shm), and its verification still reads VERIFIED.
func TestARestoreLeavesItsSnapshotAsItWas(t *testing.T) {
	src := t.TempDir()
	old := filepath.Join(src, backup.ManifestDBFileName)
	fresh := filepath.Join(t.TempDir(), backup.ManifestDBFileName)
	liveStore(t, fresh)
	if err := manifest.WriteDatabaseAtVersionForTest(old, userVersionOf(t, fresh)-1); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, ManifestDB: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifySnapshot(t.Context(), snap, backup.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, snap)
	dst := t.TempDir()
	live := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, live)
	if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, backup.RestoreOptions{}); err != nil {
		t.Fatalf("the restore = %v", err)
	}
	requireSameTree(t, snap, before)
	res, err := backup.VerifySnapshot(t.Context(), snap, backup.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.DatabaseState != backup.StateVerified {
		t.Fatalf("the snapshot's verification after the restore = %+v, want its database VERIFIED", res.Record)
	}
}
