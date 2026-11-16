package backup_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// racedInstall is what an install raced against a restore found and did:
// whether the live database had been replaced when it took the install
// marker's lock (its file another than before, or its bytes: the restore's
// copy is a rename into place), and its marker's write's error.
type racedInstall struct {
	replaced bool
	err      error
}

// liveFile is the file at path, its identity and its bytes, read through one
// open handle. On Windows an Lstat names its file by its path alone, and
// os.SameFile reads the ID through that path when it compares, so an Lstat
// taken before a replacement compares as the file that replaced it; a
// handle's Stat reads the ID at once. The handle is closed before the
// restore runs: it shares no delete (syscall.Open), so held open it would
// refuse the rename that replaces the file. The bytes rule out an ID the old
// file freed being given to the new one.
func liveFile(path string) (os.FileInfo, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	return fi, data, err
}

// raceAnInstall arms an install of a later version over this one (this
// version its PreviousVersion), as bridge update arms one in another
// process: on a goroutine of its own, through the install marker's lock
// (updater.LockState) and within its wait. The hook it returns starts it,
// once, and holds the restore that runs the hook until the install has
// armed or waits for the lock; the channel answers once, when it armed.
func raceAnInstall(t *testing.T, dataDir, live string) (start func(), armed <-chan racedInstall) {
	t.Helper()
	before, beforeBytes, err := liveFile(live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(updater.SetStateLockWaitForTest(20 * time.Second))
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	t.Cleanup(updater.SetStateLockWaitingForTest(func(d string) {
		if d == dataDir {
			waitingOnce.Do(func() { close(waiting) })
		}
	}))
	out := make(chan racedInstall, 1)
	started := false
	start = func() {
		if started {
			return
		}
		started = true
		go func() {
			lock, err := updater.LockState(context.Background(), dataDir)
			if err != nil {
				out <- racedInstall{err: err}
				return
			}
			defer lock.Release()
			now, nowBytes, err := liveFile(live)
			r := racedInstall{replaced: err == nil && (!os.SameFile(before, now) || !bytes.Equal(beforeBytes, nowBytes))}
			r.err = updater.SaveState(dataDir, updater.State{Status: "installing", TargetVersion: "99.0.0-next",
				PreviousVersion: version.ServerVersion, SwapStarted: true, AttemptedAt: time.Now().UTC()})
			out <- r
		}()
		select {
		case r := <-out:
			out <- r // armed at once: the marker's lock was free there
		case <-waiting:
		case <-time.After(10 * time.Second):
			t.Error("the raced install neither armed nor waited for the install marker's lock")
		}
	}
	return start, out
}

// awaitRacedInstall waits, bounded, for the raced install's answer.
func awaitRacedInstall(t *testing.T, armed <-chan racedInstall) racedInstall {
	t.Helper()
	select {
	case r := <-armed:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the raced install never armed")
		return racedInstall{}
	}
}

// TestAnInstallArmedBeforeTheDatabaseIsReplacedWaitsForTheReplacement: an
// install of a later version over this one, armed through the install
// marker's lock while the restore is between its record and the database's
// replacement, waits for that lock until the database is in place: the
// restore holds it from its admission through the replacement, and lets it go
// before the epoch's open. Until B653 PR 2a's third round the record let the
// lock go, and an install armed there found the old database in place: the
// restore then replaced it with nothing recorded.
func TestAnInstallArmedBeforeTheDatabaseIsReplacedWaitsForTheReplacement(t *testing.T) {
	snap := ladderTopSnapshot(t)
	dst := t.TempDir()
	live := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, live)
	start, armed := raceAnInstall(t, dst, live)
	opts := backup.WithBeforeDatabaseCopyForTest(backup.RestoreOptions{}, start)
	if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, opts); err != nil {
		t.Fatalf("the restore = %v", err)
	}
	r := awaitRacedInstall(t, armed)
	if r.err != nil {
		t.Fatalf("the raced install's marker = %v", r.err)
	}
	if !r.replaced {
		t.Fatal("the install armed between the restore's record and the database's replacement: the restore let the marker's lock go before the replacement")
	}
}

// TestAnInstallArmedBetweenTheRestoresChecksWaitsForThem: an install of a
// later version over this one, armed through the install marker's lock
// between the restore's admission and its record (the window just before
// its first write), against a destination whose player seal is incomplete,
// waits for that lock: the record and the replacement come first, the
// install after them, and the restore is done. A refusal there must leave the
// destination as it was. Until B653 PR 2a's third round the admission let the
// lock go and the seal was completed before the record, which then refused
// the restore, by the version the install replaced, with the seal written.
func TestAnInstallArmedBetweenTheRestoresChecksWaitsForThem(t *testing.T) {
	reservationStorage(t)
	snap := ladderTopSnapshot(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	liveStore(t, db)
	start, armed := raceAnInstall(t, dir, db)
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{}, start)
	err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: db, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}, opts)
	if err != nil {
		requireSealStillOwed(t, dir)
		t.Fatalf("a restore with an install armed between its checks = %v; want it done, the install armed after it", err)
	}
	r := awaitRacedInstall(t, armed)
	if r.err != nil || !r.replaced {
		t.Fatalf("the raced install = %+v; want it armed once the database was replaced", r)
	}
}

// TestARecordThatFailsLeavesThePlayerSealOwed: where the restore's record in
// an armed install fails (its marker's write refused: a directory stands
// where its rename lands), the restore is refused before the player seal's
// completion: the seal stays owed. Until B653 PR 2a's third round the seal was
// completed first, and a record that failed left it written.
func TestARecordThatFailsLeavesThePlayerSealOwed(t *testing.T) {
	reservationStorage(t)
	snap := ladderTopSnapshot(t)
	dir := loneSealBox(t)
	db := filepath.Join(dir, backup.ManifestDBFileName)
	liveStore(t, db)
	if err := updater.SaveState(dir, armedInstall(0)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(d string) {
		if d != dir {
			return
		}
		p := updater.StatePath(d)
		_ = os.Remove(p)
		_ = os.MkdirAll(filepath.Join(p, "in-the-way"), 0o755)
	}))
	err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: db, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)},
		backup.RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("a restore whose record cannot be written = %v, want it refused, nothing restored", err)
	}
	requireSealStillOwed(t, dir)
}

// TestAnInstallArmedOnceTheDatabaseIsInPlaceIsLeftToItsOwnRecords: the
// restore lets the install marker's lock go right after the database's copy,
// before the epoch's open, and an install armed in that window finds this
// version's store in place. The epoch's open asks the marker only where a
// migration step is pending (manifest's migrate), so over a snapshot at the
// ladder's top it records nothing, and the restore is done: what the install
// records from then on is its own start's and its own migrations'.
func TestAnInstallArmedOnceTheDatabaseIsInPlaceIsLeftToItsOwnRecords(t *testing.T) {
	snap := ladderTopSnapshot(t)
	dst := t.TempDir()
	live := filepath.Join(dst, backup.ManifestDBFileName)
	liveStore(t, live)
	t.Cleanup(updater.SetStateLockWaitForTest(2 * time.Second))
	var armErr error
	armed := false
	opts := backup.WithAfterDatabaseCopyForTest(backup.RestoreOptions{}, func() {
		if armed {
			return
		}
		armed = true
		lock, err := updater.LockState(context.Background(), dst)
		if err != nil {
			armErr = err
			return
		}
		defer lock.Release()
		armErr = updater.SaveState(dst, updater.State{Status: "installing", TargetVersion: "99.0.0-next",
			PreviousVersion: version.ServerVersion, SwapStarted: true, AttemptedAt: time.Now().UTC()})
	})
	if err := backup.RestoreWithOptions(snap, backup.Targets{ManifestDB: live}, opts); err != nil {
		t.Fatalf("the restore = %v", err)
	}
	if !armed || armErr != nil {
		t.Fatalf("the install armed once the database was in place = %v (armed %v): the marker's lock was still held", armErr, armed)
	}
	st, err := updater.LoadState(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !updater.InstallArmed(st) || st.StoreMigratedBy != "" || st.DataDirOpenedBy != "" {
		t.Fatalf("the marker after the restore = %+v; want the install armed and nothing recorded by the epoch's open", st)
	}
}
