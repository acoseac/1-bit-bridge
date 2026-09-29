package backup_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// A snapshot's VACUUM INTO can go on after its context is cancelled in three
// ways, and the tests here take one each (backlog B63; the record is in
// ops/engineering-log.md, 2026-09-29). The driver stops a running statement
// with sqlite3_interrupt, and SQLite honours that flag only between the
// steps of a statement that is running.

// cancelTheDriverCannotSee is a cancelled context whose cancellation only a
// reader of Err sees. Its Done channel is nil, as a never-cancelled
// context's is, so database/sql and the driver, which watch Done, carry on
// as if nothing were cancelled, and modernc arms no interrupt at all. That is
// a cancel as SQLite leaves it in two places where the driver's interrupt
// does not reach: a busy wait, whose handler sleeps whatever the flag says,
// and a statement that starts after the interrupt landed, since sqlite3Step
// clears the flag when no statement is active. Whatever stops the snapshot
// under this context is the snapshot's own check.
type cancelTheDriverCannotSee struct{ context.Context }

func (cancelTheDriverCannotSee) Err() error { return context.Canceled }

func driverBlindCancel() context.Context {
	return cancelTheDriverCannotSee{context.Background()}
}

// TestASnapshotCancelledAsItsVacuumStartsCopiesNothing is the first way: the
// cancel lands between the driver's own check of the context and the start
// of the statement, the interrupt arrives while no statement is active, and
// SQLite clears it when the VACUUM starts, so the VACUUM copies the whole
// database. The snapshot checks its context again once the statement is
// running, in the function its INTO target calls, and stops there before
// copying anything.
func TestASnapshotCancelledAsItsVacuumStartsCopiesNothing(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)

	dst, err := backup.Snapshot(driverBlindCancel(), src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a snapshot whose cancel the driver never delivered: err = %v, dst = %q; "+
			"want context.Canceled (the VACUUM copied the database after its snapshot was cancelled)", err, dst)
	}
	// The snapshot's own check at the statement's start is what stopped it:
	// a check anywhere earlier would stop it without the statement starting,
	// and this test would pass without pinning the one that closes the window.
	if !strings.Contains(err.Error(), "as its VACUUM started") {
		t.Errorf("err = %v; want the stop at the statement's start", err)
	}
	if dst != "" {
		t.Errorf("a cancelled snapshot returned the path %s", dst)
	}
	assertEmptyDir(t, filepath.Join(dataDir, backup.BackupsDirName))
}

// TestASnapshotWaitingOnALockedSourceStopsForItsCancel is the second way:
// the source is locked by another connection, and SQLite's busy handler,
// which busy_timeout(5000) gave the whole snapshot, sleeps through the
// interrupt until the timeout and then answers "database is locked". The
// snapshot waits out a lock itself now, and a cancel ends that wait.
func TestASnapshotWaitingOnALockedSourceStopsForItsCancel(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	holdExclusiveLock(t, src.ManifestDB)

	start := time.Now()
	dst, err := backup.Snapshot(driverBlindCancel(), src)
	took := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a snapshot cancelled while it waited on a locked source: err = %v after %v, dst = %q; "+
			"want context.Canceled (it waited the lock out instead of stopping)", err, took, dst)
	}
	assertEmptyDir(t, filepath.Join(dataDir, backup.BackupsDirName))
}

// TestASnapshotStillWaitsOutABriefLock is the control: the wait that a
// cancel now ends is still a wait. A source locked for a moment is snapshotted
// once the lock goes, as busy_timeout(5000) had it.
func TestASnapshotStillWaitsOutABriefLock(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	release := holdExclusiveLock(t, src.ManifestDB)
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	dst, err := backup.Snapshot(context.Background(), src)
	if err != nil {
		t.Fatalf("a snapshot of a source locked for 300ms: %v", err)
	}
	if !backup.LooksLikeSnapshotDir(dst) {
		t.Errorf("Snapshot wrote %s, which is not a complete snapshot", dst)
	}
}

// TestASnapshotWritesItsCopyWhileItCopies is the third way, and the one the
// Windows runners met (gate run 36456513415): VACUUM INTO builds its output
// in the page cache it takes from the source's connection, and the pages
// still there at the end are written by the commit, which checks no
// interrupt. With SQLite's default 2 MB cache a database smaller than that
// is written whole by the commit, so a cancel that lands during it waits for
// every page, measured at 2.3 s of writes under disk contention on Windows,
// after which the finished copy was thrown away. The snapshot's connection
// keeps a small cache, so the output is written while the copy runs, between
// the steps an interrupt stops.
//
// Parked in the middle of the copy, after a filler table larger than that
// cache and far smaller than 2 MB, the output must already hold most of the
// filler. And the output must still be a complete, sound database.
func TestASnapshotWritesItsCopyWhileItCopies(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	const fillerRows, fillerRowBytes = 64, 4000
	writeFiller(t, src.ManifestDB, fillerRows, fillerRowBytes)
	backuptest.WriteSource(t, src.ManifestDB)

	finished := make(chan struct{})
	var dst string
	var err error
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Errorf("the snapshot did not return within 10s of the park's release")
		}
	})
	park := backuptest.ParkVacuum(t)
	go func() {
		defer close(finished)
		dst, err = backup.Snapshot(context.Background(), src)
	}()
	park.Wait(t)

	partial := onlySnapshotDir(t, filepath.Join(dataDir, backup.BackupsDirName))
	info, statErr := os.Stat(filepath.Join(partial, backup.ManifestDBFileName))
	if statErr != nil {
		t.Fatalf("stat the VACUUM's output mid-copy: %v", statErr)
	}
	if want := int64(fillerRows * fillerRowBytes / 2); info.Size() < want {
		t.Errorf("mid-copy, after the filler table, the VACUUM's output holds %d bytes; want at least %d: "+
			"the copy is being held in the page cache for the commit, which no interrupt stops", info.Size(), want)
	}

	park.Disarm()
	<-finished
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	assertSoundCopy(t, filepath.Join(dst, backup.ManifestDBFileName), fillerRows)
}

// holdExclusiveLock takes an exclusive lock on the WAL database at path from
// a connection of its own, the way a connection in exclusive locking mode
// holds one after its first write, and returns a release. The lock is
// released when the test ends at the latest.
func holdExclusiveLock(t *testing.T, path string) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)&_pragma=locking_mode(EXCLUSIVE)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS lock_holder (x)`,
		`INSERT INTO lock_holder VALUES (1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	done := make(chan struct{})
	var closeErr error
	release = func() {
		select {
		case <-done:
			return
		default:
		}
		closeErr = db.Close()
		close(done)
	}
	t.Cleanup(func() {
		release()
		if closeErr != nil {
			t.Errorf("close the lock holder: %v", closeErr)
		}
	})
	return release
}

// writeFiller adds a table of rows random bytes each to the database at
// path, ahead of anything written after it, so a VACUUM copies it first.
func writeFiller(t *testing.T, path string, rows, rowBytes int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE filler (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO filler (b) VALUES (randomblob(?))`, rowBytes); err != nil {
			t.Fatalf("insert filler row %d: %v", i, err)
		}
	}
}

// assertSoundCopy opens the snapshot's database and checks that it passes
// SQLite's integrity check and holds every filler row.
func assertSoundCopy(t *testing.T, path string, fillerRows int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity check of %s: %v", path, err)
	}
	if check != "ok" {
		t.Errorf("integrity check of %s: %s", path, check)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM filler`).Scan(&n); err != nil {
		t.Fatalf("count the copied filler: %v", err)
	}
	if n != fillerRows {
		t.Errorf("the snapshot holds %d filler rows, want %d", n, fillerRows)
	}
}
