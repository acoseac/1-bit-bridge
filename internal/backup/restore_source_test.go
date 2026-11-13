package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// destinationBox is install B: a stopped install of its own, which a restore
// of another install's snapshot writes into. Every file a restore can write
// holds B's own bytes, so a restore that wrote one shows, and the listing of
// B's data dir is kept, so one that made a file there (a player marker, a
// lock beside bridge.yaml) shows too.
type destinationBox struct {
	dir     string
	targets backup.Targets
	live    map[string][]byte
	listing []string
}

// newDestinationBox lays install B out in a directory of its own.
func newDestinationBox(t *testing.T) *destinationBox {
	t.Helper()
	dir := resolvedTestDir(t, t.TempDir())
	src := primeLiveState(t, dir)
	for p, data := range map[string][]byte{
		src.TokensJSON: []byte(`[{"install":"B"}]`),
		src.ServerCert: []byte("B's certificate"),
		src.ServerKey:  []byte("B's key"),
		src.BridgeYAML: []byte("libraryRoots:\n  - /b\n"),
	} {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := &destinationBox{dir: dir, live: moveOnLiveDatabase(t, src.ManifestDB)}
	for _, p := range []string{src.TokensJSON, src.ServerCert, src.ServerKey, src.BridgeYAML} {
		d.live[p] = readBytes(t, p)
	}
	d.targets = backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
		PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile),
	}
	d.listing = names(t, dir)
	return d
}

// keep records the bytes of the file at p, which the box holds besides its
// own, as a file no restore may write.
func (d *destinationBox) keep(t *testing.T, p string) {
	t.Helper()
	d.live[p] = readBytes(t, p)
	d.listing = names(t, d.dir)
}

// requireUntouched fails the test for each of B's files that what wrote or
// removed, and for any entry it made or removed in B's data dir.
func (d *destinationBox) requireUntouched(t *testing.T, what string) {
	t.Helper()
	for p, want := range d.live {
		if !pathExists(t, p) {
			t.Errorf("%s removed B's %s", what, filepath.Base(p))
		} else if got := readBytes(t, p); !bytes.Equal(got, want) {
			t.Errorf("%s wrote B's %s", what, filepath.Base(p))
		}
	}
	if got := names(t, d.dir); !slices.Equal(got, d.listing) {
		t.Errorf("%s changed what B's data dir holds: %v, was %v", what, got, d.listing)
	}
}

// requireRestored requires B's token store, TLS pair and config to hold the
// bytes of the snapshot in snap: the restore was made whole.
func (d *destinationBox) requireRestored(t *testing.T, snap string) {
	t.Helper()
	for name, target := range map[string]string{
		"tokens.json": d.targets.TokensJSON, "server.crt": d.targets.ServerCert,
		"server.key": d.targets.ServerKey, "bridge.yaml": d.targets.BridgeYAML,
	} {
		if got, want := readBytes(t, target), readBytes(t, filepath.Join(snap, name)); !bytes.Equal(got, want) {
			t.Errorf("after the restore B's %s holds %q, want the snapshot's %q", name, got, want)
		}
	}
}

// snapshotOf takes a snapshot of src that nothing verifies, as resolved: its
// backups folder holds no lock file until a verification, a claim or a prune
// makes one there.
func snapshotOf(t *testing.T, src backup.Sources) string {
	t.Helper()
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return resolvedTestDir(t, dir)
}

// holdLockAsAnotherProcess holds the lock of the backups folder at root as
// another process's verification or prune holds it: through an open of its
// own and no holder of this process's, which flock and LockFileEx refuse
// every other open of the file, in this process or another. It answers the
// release, which the test's end runs too.
func holdLockAsAnotherProcess(t *testing.T, root string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { _ = fsutil.Unlock(f); _ = f.Close() })
	t.Cleanup(release)
	return release
}

// writeStuckMarker leaves beside the lock of the backups folder at root the
// marker a check that did not end writes there, naming snapshot, and answers
// its path.
func writeStuckMarker(t *testing.T, root, snapshot string) string {
	t.Helper()
	data, err := json.Marshal(backup.StuckCheck{Snapshot: snapshot, Step: "its check", Since: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, backup.StuckMarkerName)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sourceWorkerEnds hears the end of every restore's source worker
// (SetOnSourceWorkerEndForTest), installed before the restores it hears.
func sourceWorkerEnds(t *testing.T) <-chan string {
	t.Helper()
	ends := make(chan string, 16)
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(dir string) {
		select {
		case ends <- dir:
		default:
		}
	}))
	return ends
}

// awaitSourceWorkerEnd waits for the end of the source worker of the restore
// of dir, failing the test past a deadline.
func awaitSourceWorkerEnd(t *testing.T, ends <-chan string, dir string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case d := <-ends:
			if d == dir {
				return
			}
		case <-deadline:
			t.Fatalf("the source worker of the restore of %s never ended", filepath.Base(dir))
		}
	}
}

// awaitEntered waits for a parked step to be entered, failing the test past a
// deadline.
func awaitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was never entered", what)
	}
}

// givenUpCheckOf gives a verification of the captured snapshot c up past its
// deadline, its check parked where SQLite opens the snapshot's database (a
// mount that stopped answering), so its hold keeps the backups folder marked
// until the test's end, which lets the check go and joins it.
func givenUpCheckOf(t *testing.T, c *backup.Capture) {
	t.Helper()
	const deadline = 300 * time.Millisecond
	t.Cleanup(backup.SetVerifyDeadlineForTest(deadline))
	entered, release := parkCheck(t, c.Dir)
	callers := joinBackupCallers(t, release)
	done := make(chan backup.Result, 1)
	start := time.Now()
	callers.start(func() { done <- backup.FinishCapture(context.Background(), c, keepOne, backup.VerifyOptions{}) })
	awaitEntered(t, entered, "the check")
	res := requireReturnedWithin(t, done, start, deadline+backup.StopWriteBound/2+2*time.Second, "a verification whose check was given up")
	if !errors.Is(res.VerifyErr, backup.ErrCheckGivenUp) {
		t.Fatalf("premise: the verification answered %v, want its check given up", res.VerifyErr)
	}
}

// TestARestoreRefusesASnapshotAGivenUpCheckHolds is backlog B826's red
// (Codex on B653 PR 1's third round): install A's verification of its
// snapshot is given up past its deadline, its check parked where SQLite opens
// the snapshot's database (a mount that stopped answering), so its hold keeps
// A's backups folder, the snapshot's files open; a restore of that snapshot
// into install B, stopped, is refused at once with ErrCheckStillRunning, B's
// files and its data dir as they were. Red on main, where a restore took no
// lock of the folder its snapshot is in and read no mark: it restored A's
// snapshot into B beside the check.
func TestARestoreRefusesASnapshotAGivenUpCheckHolds(t *testing.T) {
	c := captureT(t, storeWithTracks(t, 5))
	b := newDestinationBox(t)
	givenUpCheckOf(t, c)

	asked := time.Now()
	err := backup.RestoreWithOptions(c.Dir, b.targets, backup.RestoreOptions{})
	if !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("a restore of the snapshot a check given up holds answered %v, want ErrCheckStillRunning", err)
	}
	if took := time.Since(asked); took > 2*time.Second {
		t.Errorf("the restore waited %v to be refused", took)
	}
	b.requireUntouched(t, "the restore beside the check given up")
}

// TestARestoreRefusedByTheSpellingItIsGivenResolvesNothing: a hold given up
// that holds the snapshot's folder by the spelling the restore is given
// refuses it before anything asks a filesystem, so the snapshot's path is
// never resolved, which could hang on the very mount the check stalled on.
// Its control drops that first ask: the restore then resolves the snapshot,
// and the resolved folder's mark refuses it all the same, but only once the
// resolution has answered.
func TestARestoreRefusedByTheSpellingItIsGivenResolvesNothing(t *testing.T) {
	var armed atomic.Bool
	var resolved atomic.Int64
	// Installed before anything resolves, passing every call to the
	// platform's own; counted only once armed.
	t.Cleanup(backup.SetMetadataForTest(func(p string) (string, error) {
		if armed.Load() {
			resolved.Add(1)
		}
		return fsutil.ResolveLinks(p)
	}, nil))
	c := captureT(t, storeWithTracks(t, 5))
	b := newDestinationBox(t)
	givenUpCheckOf(t, c)

	armed.Store(true)
	err := backup.RestoreWithOptions(c.Dir, b.targets, backup.RestoreOptions{})
	armed.Store(false)
	if !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("the restore answered %v, want ErrCheckStillRunning", err)
	}
	if n := resolved.Load(); n != 0 {
		t.Errorf("the restore resolved %d paths beside a mark its spelling names, want none", n)
	}
	b.requireUntouched(t, "the restore beside the check given up")
	if err := backup.SourceStillHeld(c.Dir); !errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("SourceStillHeld = %v, want ErrCheckStillRunning", err)
	}
}

// TestARestoreRefusesASnapshotAnotherProcesssCheckHolds is the case across
// processes: a check given up in another process (A's bridge serve) holds
// the folder's lock, its marker beside it, which counts while the lock is
// held. The restore is refused at once, ErrCheckStillRunning, well inside its
// wait, B untouched, and the marker left where it is, the restore never having
// held the lock.
func TestARestoreRefusesASnapshotAnotherProcesssCheckHolds(t *testing.T) {
	snap := snapshotOf(t, storeWithTracks(t, 5))
	root := filepath.Dir(snap)
	holdLockAsAnotherProcess(t, root)
	marker := writeStuckMarker(t, root, filepath.Base(snap))
	b := newDestinationBox(t)

	asked := time.Now()
	err := backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{LockWait: 20 * time.Second})
	if !errors.Is(err, backup.ErrCheckStillRunning) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("a restore beside another process's check that did not end answered %v, want ErrCheckStillRunning, nothing restored", err)
	}
	if took := time.Since(asked); took > 5*time.Second {
		t.Errorf("the restore waited %v of its 20 s to be refused", took)
	}
	b.requireUntouched(t, "the restore beside another process's check")
	if !pathExists(t, marker) {
		t.Error("the restore removed the marker of a lock it never held")
	}
}

// TestARestoreWaitsForTheSnapshotsLockWithinItsWait: another process's
// verification or prune holds the folder's lock, with no marker: the restore
// waits for it within its wait, and once it is let go takes it and restores
// the snapshot whole; held for all of the wait, the restore is refused,
// ErrBackupsBusy, B untouched. Its control drops the restore's lock: the
// restore then writes B beside the holder.
func TestARestoreWaitsForTheSnapshotsLockWithinItsWait(t *testing.T) {
	t.Run("let go within the wait", func(t *testing.T) {
		snap := snapshotOf(t, storeWithTracks(t, 5))
		release := holdLockAsAnotherProcess(t, filepath.Dir(snap))
		b := newDestinationBox(t)
		var waited atomic.Bool
		// The wait's first refusal lets the holder go: the restore's next
		// try takes the lock.
		t.Cleanup(backup.SetOnLockWaitForTest(func() {
			waited.Store(true)
			release()
		}))
		if err := backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{LockWait: 20 * time.Second}); err != nil {
			t.Fatalf("a restore whose folder's holder let go within its wait answered %v", err)
		}
		if !waited.Load() {
			t.Error("the restore never waited for the holder")
		}
		b.requireRestored(t, snap)
	})
	t.Run("held past the wait", func(t *testing.T) {
		snap := snapshotOf(t, storeWithTracks(t, 5))
		holdLockAsAnotherProcess(t, filepath.Dir(snap))
		b := newDestinationBox(t)
		const wait = 300 * time.Millisecond
		asked := time.Now()
		err := backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{LockWait: wait})
		if !errors.Is(err, backup.ErrBackupsBusy) || !strings.Contains(err.Error(), "nothing was restored") {
			t.Errorf("a restore whose folder's lock was held past its wait answered %v, want ErrBackupsBusy, nothing restored", err)
		}
		if took := time.Since(asked); took < wait {
			t.Errorf("the restore was refused after %v, before its wait of %v", took, wait)
		}
		b.requireUntouched(t, "the restore refused past its wait")
	})
}

// TestARestoreHoldsTheSnapshotsLockThroughItsCopies: with nothing holding the
// folder, the restore takes its lock and finishes, holding it through every
// copy: a prune of the folder while the restore copies (parked just before it
// writes bridge.yaml, the database and the token store restored already)
// skips its round, ErrBackupsBusy, and the snapshot it would have removed is
// still there, and the lock is held as another process asks it. Once the
// restore has returned the lock is free and the prune removes it. Its control
// lets the lock go once the manifest is read: the prune then runs beside the
// copies.
func TestARestoreHoldsTheSnapshotsLockThroughItsCopies(t *testing.T) {
	src := storeWithTracks(t, 5)
	old := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	verifiedSnapshot(t, src)
	root := filepath.Dir(old)
	b := newDestinationBox(t)

	var pr backup.PruneResult
	var prErr, heldErr error
	var held, oldThere bool
	opts := backup.WithBeforeConfigForTest(backup.RestoreOptions{}, func() {
		pr, prErr = backup.Prune(root, 1)
		oldThere = pathExists(t, old)
		held, heldErr = backupsLockHeld(root)
	})
	if err := backup.RestoreWithOptions(old, b.targets, opts); err != nil {
		t.Fatalf("the restore answered %v", err)
	}
	if prErr != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrBackupsBusy) {
		t.Errorf("a prune while the restore copied: %+v, %v; want its round skipped for the restore's lock", pr, prErr)
	}
	if !oldThere {
		t.Error("the prune removed the snapshot the restore was copying")
	}
	if heldErr != nil || !held {
		t.Errorf("the folder's lock while the restore copied: held %v, %v; want it held", held, heldErr)
	}
	b.requireRestored(t, old)

	if held, err := backupsLockHeld(root); err != nil || held {
		t.Errorf("the folder's lock once the restore returned: held %v, %v; want it free", held, err)
	}
	if pr, err := backup.Prune(root, 1); err != nil || pr.Skipped || pathExists(t, old) {
		t.Errorf("the prune once the restore returned: %+v, %v; want it run, the older snapshot gone", pr, err)
	}
}

// TestARestoreLocksALiveFolderThatHoldsNoLockFileYet: a live backups folder
// before its first verification, claim or prune holds no lock file (a
// capture takes no lock), which is no mark of a copy: the restore makes the
// lock file and takes its lock, so a prune of the folder while the restore
// runs skips its round.
func TestARestoreLocksALiveFolderThatHoldsNoLockFileYet(t *testing.T) {
	snap := snapshotOf(t, storeWithTracks(t, 5))
	root := filepath.Dir(snap)
	lockFile := filepath.Join(root, backup.LockFileName)
	if pathExists(t, lockFile) {
		t.Fatal("premise: the folder holds a lock file before anything verified, claimed or pruned there")
	}
	b := newDestinationBox(t)
	var pr backup.PruneResult
	var prErr, heldErr error
	var held bool
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{}, func() {
		held, heldErr = backupsLockHeld(root)
		pr, prErr = backup.Prune(root, 0)
	})
	if err := backup.RestoreWithOptions(snap, b.targets, opts); err != nil {
		t.Fatalf("the restore answered %v", err)
	}
	if heldErr != nil || !held {
		t.Errorf("the lock of a folder that held no lock file, while the restore ran: held %v, %v; want it made and held", held, heldErr)
	}
	if prErr != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrBackupsBusy) {
		t.Errorf("a prune while the restore ran: %+v, %v; want its round skipped", pr, prErr)
	}
	b.requireRestored(t, snap)
}

// TestAStandaloneCopyIsRestoredUnderItsOwnFoldersLock: a snapshot copied out
// of its backups folder, the copy the restore's own refusals advise, is
// restored. Nothing tells a copy from a live folder for certain, so its
// folder is locked as a live one is: the lock file made there, held while the
// restore runs, and let go once it returns.
func TestAStandaloneCopyIsRestoredUnderItsOwnFoldersLock(t *testing.T) {
	snap := snapshotOf(t, storeWithTracks(t, 5))
	copies := resolvedTestDir(t, t.TempDir())
	copied := filepath.Join(copies, filepath.Base(snap))
	if err := os.Mkdir(copied, 0o700); err != nil {
		t.Fatal(err)
	}
	copyTree(t, snap, copied)
	b := newDestinationBox(t)
	var held bool
	var heldErr error
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{}, func() { held, heldErr = backupsLockHeld(copies) })
	if err := backup.RestoreWithOptions(copied, b.targets, opts); err != nil {
		t.Fatalf("the restore of a copy answered %v", err)
	}
	if heldErr != nil || !held {
		t.Errorf("the copy's folder's lock while the restore ran: held %v, %v; want it held", held, heldErr)
	}
	b.requireRestored(t, copied)
	if held, err := backupsLockHeld(copies); err != nil || held {
		t.Errorf("the copy's folder's lock once the restore returned: held %v, %v; want it free", held, err)
	}
}

// TestARestoreThroughALinkTakesTheLockOfTheFolderItLeadsTo: a restore of a
// snapshot named through a link elsewhere (/tmp/x leading to A's snapshot)
// takes the lock of A's backups folder, never one beside the link: with that
// lock held by another process for all of its wait it is refused, nothing
// made beside the link, and with it free it restores.
func TestARestoreThroughALinkTakesTheLockOfTheFolderItLeadsTo(t *testing.T) {
	snap := snapshotOf(t, storeWithTracks(t, 5))
	links := resolvedTestDir(t, t.TempDir())
	link := filepath.Join(links, "x")
	if err := os.Symlink(snap, link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	release := holdLockAsAnotherProcess(t, filepath.Dir(snap))
	b := newDestinationBox(t)
	err := backup.RestoreWithOptions(link, b.targets, backup.RestoreOptions{LockWait: 300 * time.Millisecond})
	if !errors.Is(err, backup.ErrBackupsBusy) {
		t.Errorf("a restore through a link to a snapshot whose folder is held answered %v, want ErrBackupsBusy", err)
	}
	b.requireUntouched(t, "the restore through a link")
	if pathExists(t, filepath.Join(links, backup.LockFileName)) {
		t.Error("the restore made a lock file beside the link rather than in the folder it leads to")
	}
	release()
	if err := backup.RestoreWithOptions(link, b.targets, backup.RestoreOptions{}); err != nil {
		t.Fatalf("the restore through the link once the folder was free answered %v", err)
	}
	b.requireRestored(t, snap)
}

// TestARestoreTakesTheLockOfABackupsFolderReachedThroughALink: an install's
// backups folder that is a link to a folder named otherwise holds its lock in
// that folder, where its bridge's verifications and prunes take it (they
// resolve the folder): a restore of a snapshot named through the install's
// backups folder takes that same lock, refused while another process holds
// it.
func TestARestoreTakesTheLockOfABackupsFolderReachedThroughALink(t *testing.T) {
	src := storeWithTracks(t, 5)
	physical := filepath.Join(resolvedTestDir(t, t.TempDir()), "snapshots-kept-elsewhere")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(src.DataDir, backup.BackupsDirName)
	if err := os.Symlink(physical, backups); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(backups, filepath.Base(dir))
	release := holdLockAsAnotherProcess(t, physical)
	b := newDestinationBox(t)
	err = backup.RestoreWithOptions(named, b.targets, backup.RestoreOptions{LockWait: 300 * time.Millisecond})
	if !errors.Is(err, backup.ErrBackupsBusy) {
		t.Errorf("a restore through a backups folder that is a link, its folder's lock held, answered %v, want ErrBackupsBusy", err)
	}
	b.requireUntouched(t, "the restore through the linked backups folder")
	release()
	if err := backup.RestoreWithOptions(named, b.targets, backup.RestoreOptions{}); err != nil {
		t.Fatalf("the restore once the folder was free answered %v", err)
	}
	b.requireRestored(t, named)
}

// TestARestoreFromTheInstallsOwnBackupsFolderCompletes is the common
// self-restore, bridge restore of /dataA/backups/<snap> into /dataA: it
// neither waits on nor keeps this process's holder of the folder, so it
// completes, and a verification of a snapshot there then takes the folder at
// once.
func TestARestoreFromTheInstallsOwnBackupsFolderCompletes(t *testing.T) {
	b := newRestorableBox(t)
	if err := backup.RestoreWithOptions(b.snap, b.targets, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}); err != nil {
		t.Fatalf("the self-restore answered %v", err)
	}
	if got, want := readBytes(t, b.src.TokensJSON), readBytes(t, filepath.Join(b.snap, "tokens.json")); !bytes.Equal(got, want) {
		t.Errorf("after the self-restore tokens.json holds %q, want the snapshot's %q", got, want)
	}
	if held, err := backupsLockHeld(filepath.Dir(b.snap)); err != nil || held {
		t.Errorf("the backups folder's lock once the self-restore returned: held %v, %v; want it free", held, err)
	}
	if _, err := backup.VerifySnapshot(t.Context(), b.snap, backup.VerifyOptions{LockWait: time.Second}); errors.Is(err, backup.ErrBackupsBusy) || errors.Is(err, backup.ErrCheckStillRunning) {
		t.Errorf("a verification once the self-restore returned answered %v: the restore kept the folder", err)
	}
	awaitClean(t)
}

// TestARestoreClearsAMarkerItsHolderLeftBehind: a marker that a checker
// whose process ended left beside the lock (the kernel's lock gone with the
// process) is removed once the restore holds the lock, as the other holders
// remove it, so another process waiting for the lock meanwhile sees ordinary
// contention, ErrBackupsBusy, never a check still running. Its control
// keeps the marker: the other process then answers ErrCheckStillRunning.
func TestARestoreClearsAMarkerItsHolderLeftBehind(t *testing.T) {
	src := storeWithTracks(t, 5)
	snap := snapshotOf(t, src)
	other := snapshotOf(t, src)
	root := filepath.Dir(snap)
	marker := writeStuckMarker(t, root, "a check whose process ended")
	b := newDestinationBox(t)
	var markerLeft bool
	var competitor error
	opts := backup.WithSourceHeldForTest(backup.RestoreOptions{}, func() {
		markerLeft = pathExists(t, marker)
		_, competitor = backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), other, backup.VerifyOptions{LockWait: 300 * time.Millisecond})
	})
	if err := backup.RestoreWithOptions(snap, b.targets, opts); err != nil {
		t.Fatalf("the restore answered %v", err)
	}
	awaitClean(t)
	if markerLeft {
		t.Error("the marker of a holder whose process ended stood once the restore held the lock")
	}
	if !errors.Is(competitor, backup.ErrBackupsBusy) || errors.Is(competitor, backup.ErrCheckStillRunning) {
		t.Errorf("another process's verification while the restore held the lock answered %v, want ErrBackupsBusy", competitor)
	}
	b.requireRestored(t, snap)
}

// TestASnapshotPrunedWhileTheRestoreAskedIsRefusedBeforeTheDestinationIsTouched:
// bridge restore reads the snapshot's manifest before its prompt, and a prune
// can remove the snapshot while the prompt waits, or while the restore waits
// for the folder's lock, which the prune holds. The restore finds it gone as
// it resolves it, or reads its manifest again once it holds the lock, before
// it completes the player seal its destination left incomplete, and is
// refused, ErrSnapshotGone, nothing written: the seal's markers as they
// were, and every target. Red on main, which completed the seal before it
// read the manifest, writing player-initialized, and only then failed on the
// read; the read under the lock's control moves it back after the completion,
// and the case pruned during the wait then writes the seal.
func TestASnapshotPrunedWhileTheRestoreAskedIsRefusedBeforeTheDestinationIsTouched(t *testing.T) {
	for _, during := range []string{"the prompt", "the wait for the lock"} {
		t.Run(during, func(t *testing.T) {
			reservationStorage(t)
			src := storeWithTracks(t, 5)
			old := verifiedSnapshot(t, src)
			time.Sleep(1100 * time.Millisecond)
			verifiedSnapshot(t, src)
			root := filepath.Dir(old)
			b := newDestinationBox(t)
			// A discard whose second record never landed: its next lifecycle
			// operation completes it, writing player-initialized.
			seal := filepath.Join(b.dir, "player-floor-discarded.json")
			if err := os.WriteFile(seal, []byte(`{"version":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			b.keep(t, seal)
			if !backup.LooksLikeSnapshotDir(old) {
				t.Fatal("premise: the snapshot does not read as one before the prompt")
			}

			// removed hears the removal the prune's stand-in makes on the
			// restore's worker, in the lock's wait: its error is the test
			// goroutine's to fail on (backlog B871's item 8).
			var removed chan error
			if during == "the prompt" {
				if pr, err := backup.Prune(root, 1); err != nil || pr.Deleted != 1 || pathExists(t, old) {
					t.Fatalf("premise: the prune during the prompt: %+v, %v; want the older snapshot removed", pr, err)
				}
			} else {
				// Another process's prune holds the lock as the restore asks
				// for it, removes the snapshot, and lets go.
				release := holdLockAsAnotherProcess(t, root)
				removed = make(chan error, 1)
				var once atomic.Bool
				t.Cleanup(backup.SetOnLockWaitForTest(func() {
					if once.CompareAndSwap(false, true) {
						removed <- os.RemoveAll(old)
						release()
					}
				}))
			}
			err := backup.RestoreWithOptions(old, b.targets, backup.RestoreOptions{LockWait: 20 * time.Second})
			if removed != nil {
				select {
				case rerr := <-removed:
					if rerr != nil {
						t.Fatalf("premise: the prune's removal of the snapshot: %v", rerr)
					}
				default:
					t.Fatal("premise: the restore never waited for the prune's lock")
				}
			}
			if !errors.Is(err, backup.ErrSnapshotGone) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Errorf("a restore of a snapshot pruned since it was read answered %v, want ErrSnapshotGone, nothing restored", err)
			}
			b.requireUntouched(t, "the restore of a snapshot pruned since it was read")
		})
	}
}

// TestARestoreWhoseFolderCanHaveNoLockIsRefused: where no lock can be had on
// the folder that holds the snapshot, read-only media (the lock file's
// making answers EROFS) or a filesystem that keeps no locks, the restore is
// refused, ErrBackupsUnlocked, in a restore's words, nothing restored: a
// missing lock file never lets a restore go on unlocked, since a live folder
// has none before its first verification.
func TestARestoreWhoseFolderCanHaveNoLockIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stand func(root string) (restore func())
	}{
		{"read-only media", func(root string) func() {
			return backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
				if dir == root {
					return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, name), Err: syscall.EROFS}
				}
				return fsutil.OpenLockFile(dir, name)
			})
		}},
		{"a filesystem that keeps no locks", func(string) func() {
			return backup.SetTryLockBackupsForTest(func(f *os.File) error {
				return &fs.PathError{Op: "flock", Path: f.Name(), Err: errors.ErrUnsupported}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			b := newDestinationBox(t)
			t.Cleanup(tc.stand(filepath.Dir(snap)))
			err := backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{})
			if !errors.Is(err, backup.ErrBackupsUnlocked) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Errorf("the restore answered %v, want ErrBackupsUnlocked, nothing restored", err)
			}
			if err != nil && strings.Contains(err.Error(), "verified or pruned") {
				t.Errorf("the restore's refusal %q is in a verification's words", err)
			}
			b.requireUntouched(t, "the restore with no lock to be had")
		})
	}
}

// TestARestoreWhoseSourceDoesNotAnswerIsRefusedWithinItsBound: the folder
// that holds the snapshot stops answering where the restore asks it before it
// holds its lock (the snapshot's resolution, the lock file's open, the read of
// a marker beside a lock another process holds, the try itself). The restore
// stopped, or past its own bound where nothing stops it, answers within
// StopWriteBound, refused, nothing restored; and once the folder answers
// again its worker keeps no lock it takes and removes nothing (a lock the
// stalled try takes late is let go at once, a stale marker left where it is),
// so a restore after it takes the folder, B still as it was.
func TestARestoreWhoseSourceDoesNotAnswerIsRefusedWithinItsBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stand installs the stall, parked in p once armed.
		stand func(t *testing.T, snap string, armed *atomic.Bool, p *parking)
		// stopped: the restore is stopped once the stall is entered; else it
		// is left to its own bound, its wait short.
		stopped bool
	}{
		{"its resolution", func(t *testing.T, snap string, armed *atomic.Bool, p *parking) {
			t.Cleanup(backup.SetMetadataForTest(func(path string) (string, error) {
				if path == snap && armed.CompareAndSwap(true, false) {
					p.park()
				}
				return fsutil.ResolveLinks(path)
			}, nil))
		}, true},
		{"the lock file's open", func(t *testing.T, snap string, armed *atomic.Bool, p *parking) {
			t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
				if dir == filepath.Dir(snap) && armed.CompareAndSwap(true, false) {
					p.park()
				}
				return fsutil.OpenLockFile(dir, name)
			}))
		}, true},
		{"a marker's read", func(t *testing.T, snap string, armed *atomic.Bool, p *parking) {
			t.Cleanup(backup.SetReadStuckMarkerForTest(func(path string) ([]byte, error) {
				if filepath.Dir(path) == filepath.Dir(snap) && armed.CompareAndSwap(true, false) {
					p.park()
				}
				return backup.ReadStuckMarkerBytes(path)
			}))
		}, true},
		{"the try", func(t *testing.T, snap string, armed *atomic.Bool, p *parking) {
			t.Cleanup(backup.SetTryLockBackupsForTest(func(f *os.File) error {
				if filepath.Dir(f.Name()) == filepath.Dir(snap) && armed.CompareAndSwap(true, false) {
					p.park()
				}
				return fsutil.TryLock(f)
			}))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			snap := snapshotOf(t, src)
			root := filepath.Dir(snap)
			b, c := newDestinationBox(t), newDestinationBox(t)
			ends := sourceWorkerEnds(t)
			var armed atomic.Bool
			p := newParking()
			tc.stand(t, snap, &armed, p)
			var releaseHolder func()
			var marker string
			switch tc.name {
			case "a marker's read":
				releaseHolder = holdLockAsAnotherProcess(t, root)
			case "the try":
				// Left by a checker whose process ended: a lock its worker
				// takes once given up removes nothing.
				marker = writeStuckMarker(t, root, "a check whose process ended")
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			callers := joinBackupCallers(t, func() {
				p.release()
				if releaseHolder != nil {
					releaseHolder()
				}
			})
			wait := 20 * time.Second
			if !tc.stopped {
				wait = 100 * time.Millisecond
			}
			done := make(chan error, 1)
			armed.Store(true)
			start := time.Now()
			callers.start(func() {
				done <- backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{Context: ctx, LockWait: wait})
			})
			awaitEntered(t, p.entered, "the stall")
			bound := wait + 3*backup.StopWriteBound + 2*time.Second
			if tc.stopped {
				cancel()
				start, bound = time.Now(), backup.StopWriteBound+2*time.Second
			}
			err := requireReturnedWithin(t, done, start, bound, "a restore whose source does not answer")
			why := context.DeadlineExceeded
			if tc.stopped {
				why = context.Canceled
			}
			if !errors.Is(err, why) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Errorf("the restore answered %v, want a refusal wrapping %v, nothing restored", err, why)
			}
			b.requireUntouched(t, "the restore whose source did not answer")
			// Given up before it held the lock, it holds nothing a mark would
			// say it holds, and marks nothing (backlog B826's second round).
			if s, held := backup.StuckCheckHere(root); held {
				t.Errorf("a restore given up before it held the lock marked the folder: %+v", s)
			}

			p.release()
			awaitSourceWorkerEnd(t, ends, snap)
			if releaseHolder != nil {
				releaseHolder()
			}
			switch {
			case tc.name == "its resolution":
				if pathExists(t, filepath.Join(root, backup.LockFileName)) {
					t.Error("the worker of a restore given up in its resolution made the folder's lock file")
				}
			default:
				if held, err := backupsLockHeld(root); err != nil || held {
					t.Errorf("the folder's lock once the given-up restore's worker ended: held %v, %v; want it free", held, err)
				}
			}
			if marker != "" && !pathExists(t, marker) {
				t.Error("the worker of a restore given up removed the marker beside the lock it took late")
			}
			b.requireUntouched(t, "the worker of the restore given up")
			if err := backup.RestoreWithOptions(snap, c.targets, backup.RestoreOptions{LockWait: 20 * time.Second}); err != nil {
				t.Fatalf("a restore once the folder answered again answered %v", err)
			}
			c.requireRestored(t, snap)
			callers.wait(t)
		})
	}
}
