package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// VerifySnapshotAsAnotherProcessForTest is VerifySnapshot as another
// process's verification runs: outside this process's slot and its holder
// of the backups folder, so the kernel's lock on LockFileName alone keeps it
// from this process's.
func VerifySnapshotAsAnotherProcessForTest(ctx context.Context, dir string, o VerifyOptions) (VerifyResult, error) {
	o.asAnotherProcess = true
	return VerifySnapshot(ctx, dir, o)
}

// OwnerHeldAsAnotherProcessForTest reports whether the owner lock of the
// snapshot in dir is held, asked as another process asks it: an exclusive try
// on a file of its own, never this process's record (ownedHere).
func OwnerHeldAsAnotherProcessForTest(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	switch err := fsutil.TryLock(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
		return false
	case errors.Is(err, fsutil.ErrLocked):
		return true
	}
	return false
}

// StuckAnywhereForTest reports whether any hold given up in this process is
// marked still.
func StuckAnywhereForTest() bool {
	_, held := stuckAnywhere()
	return held
}

// FolderTokenHeldForTest reports whether this process's one holder of the
// backups folder at root is taken (folderTokens): a try on it, given back at
// once.
func FolderTokenHeldForTest(root string) bool {
	token := folderToken(ownerKey(root))
	select {
	case token <- struct{}{}:
		<-token
		return false
	default:
		return true
	}
}

// SlotHeldForTest reports whether a verification holds this process's slot:
// a try on it, given back at once.
func SlotHeldForTest() bool {
	select {
	case verifySlot <- struct{}{}:
		<-verifySlot
		return false
	default:
		return true
	}
}

// SetAfterIdentityCopyForTest sets the hook Snapshot calls between copying
// the player's UUID and reading its floor, and returns the restore.
func SetAfterIdentityCopyForTest(fn func()) (restore func()) {
	prev := afterIdentityCopy
	afterIdentityCopy = fn
	return func() { afterIdentityCopy = prev }
}

// SetTryLockForTest replaces the lock a backup tries on server.lock, and
// returns the restore.
func SetTryLockForTest(fn func(*os.File) error) (restore func()) {
	prev := tryLock
	tryLock = fn
	return func() { tryLock = prev }
}

// ErrNotADirectory is what a listing of the backups folder answers when
// something that is not a folder stands where it goes.
var ErrNotADirectory = errNotADirectory

// CompletePlayerSeal is completePlayerSeal, driven directly where a backup's
// lock open would fail first.
func CompletePlayerSeal(dir string) error { return completePlayerSeal(dir) }

// EnsurePlayerMarker is ensurePlayerMarker, driven directly.
func EnsurePlayerMarker(dir, name string) error { return ensurePlayerMarker(dir, name) }

// PlayerEntryNotAFile is playerEntryNotAFile, driven directly.
func PlayerEntryNotAFile(dir, name string) (bool, error) { return playerEntryNotAFile(dir, name) }

// PlayerAllocationEvidence is playerAllocationEvidence, driven directly.
func PlayerAllocationEvidence(dir string) (bool, error) { return playerAllocationEvidence(dir) }

// OpenSealLock is a backup's seal-completion open of server.lock, which it
// closes again, driven directly where an earlier step would fail first.
func OpenSealLock(dir string) error {
	f, err := openSealLock(dir)
	if f != nil {
		_ = f.Close()
	}
	return err
}

// DirectoryRemedy is the one remedy a failure at the data directory alone
// takes on goos.
func DirectoryRemedy(goos, dir string) string { return directoryRemedy(goos, dir) }

// EntryRemedy is the union a failure of a step naming dir/name takes on goos.
func EntryRemedy(goos, dir, name, access string) string { return entryRemedy(goos, dir, name, access) }

// LockFileFailure is the tag a failure to open or lock server.lock takes.
func LockFileFailure(dir string, err error) error { return lockFileFailure(dir, err) }

// MarkerWriteFailure is the tag a marker's failed durable write takes.
func MarkerWriteFailure(dir, name string, err error) error { return markerWriteFailure(dir, name, err) }

// SealCompletionAdviceOn is what a backup logs on goos for a seal completion
// that failed with err.
func SealCompletionAdviceOn(goos string, err error) string {
	return sealCompletionAdvice(goos, err).Error()
}

// SealCompletionAdvice is what a backup logs for a seal completion that
// failed with err.
func SealCompletionAdvice(err error) string { return sealCompletionFailed(err).Error() }

// ReadMarkerBytes is the real open and bounded read of a local marker, for a
// test's stand-in to fall back on.
func ReadMarkerBytes(path string) ([]byte, error) { return readMarkerBytes(path) }

// SetReadMarkerForTest replaces the open and bounded read of a local marker,
// and returns the restore.
func SetReadMarkerForTest(fn func(path string) ([]byte, error)) (restore func()) {
	prev := readMarker
	readMarker = fn
	return func() { readMarker = prev }
}

// UnlistedFloorError is restore's verdict on the Lstat of a floor its
// snapshot's manifest does not list.
func UnlistedFloorError(err error) error { return unlistedFloorError(err) }

// AdoptPlayerStateWith is AdoptPlayerState drawing the adoption's random
// part from random, as restore's adoption does: a source box adopting at a
// chosen draw (TestLegacyCeilingCoversASourceThatAdoptedAfterItsSnapshot).
func AdoptPlayerStateWith(dir string, random io.Reader) error {
	return adoptPlayerState(dir, random)
}

// SnapshotBusyPatience is snapshotBusyPatience, for the external tests: a
// snapshot that waited a lock out in SQLite's busy handler cannot answer
// sooner.
const SnapshotBusyPatience = snapshotBusyPatience

// WithBusyObserver returns ctx carrying observe, which a snapshot run on it
// calls each time an attempt is refused a lock.
func WithBusyObserver(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, busyObserverKey{}, observe)
}

// ResetPlayerAfterStep exposes the interruption seam to external tests.
func ResetPlayerAfterStep(dir string, after func(string) error) (bool, error) {
	return resetPlayer(dir, false, after)
}

// DiscardPlayerFloorAfterStep exposes durable interruption points.
func DiscardPlayerFloorAfterStep(dir string, after func(string) error) (bool, error) {
	return resetPlayer(dir, true, after)
}

// WithBeforeConfigForTest returns opts with fn as what a restore runs just
// before it writes the snapshot's bridge.yaml, in its turn of the file's
// lock.
func WithBeforeConfigForTest(opts RestoreOptions, fn func()) RestoreOptions {
	opts.beforeConfig = fn
	return opts
}

// WithBeforeWritesForTest returns opts with fn as what a restore runs once it
// holds its locks and has read the snapshot, just before its first write.
func WithBeforeWritesForTest(opts RestoreOptions, fn func()) RestoreOptions {
	opts.beforeWrites = fn
	return opts
}

// WithSourceHeldForTest returns opts with fn as what a restore runs once it
// holds the lock of the folder that holds its snapshot and has read the
// manifest there, before it touches the destination.
func WithSourceHeldForTest(opts RestoreOptions, fn func()) RestoreOptions {
	opts.sourceHeld = fn
	return opts
}

// SetReadStuckMarkerForTest replaces the read of the marker a check that did
// not end leaves beside the backups folder's lock, and returns the restore.
func SetReadStuckMarkerForTest(fn func(path string) ([]byte, error)) (restore func()) {
	prev := readStuckMarker
	readStuckMarker = fn
	return func() { readStuckMarker = prev }
}

// ReadStuckMarkerBytes is the real read of that marker, for a test's stand-in
// to fall back on.
func ReadStuckMarkerBytes(path string) ([]byte, error) { return fsutil.ReadAsFile(path) }

// SetRemoveStaleMarkerForTest replaces the removal of a marker a process that
// ended left beside a backups folder's lock (os.Remove), given its path, and
// returns the restore.
func SetRemoveStaleMarkerForTest(fn func(path string) error) (restore func()) {
	prev := removeStaleMarker
	removeStaleMarker = fn
	return func() { removeStaleMarker = prev }
}

// SetOnSourceStepForTest installs the hook a restore's source worker runs as
// it passes a step under the lock ("held", "cleared", "read"), given the snapshot as
// the restore was given it, and returns the restore. Its callback must return
// once its test lets it, and ignore every snapshot but its test's.
func SetOnSourceStepForTest(fn func(dir, step string)) (restore func()) {
	var p *func(string, string)
	if fn != nil {
		p = &fn
	}
	prev := onSourceStep.Swap(p)
	return func() { onSourceStep.Store(prev) }
}

// SetFailSourceOpenForTest installs fn as the open of every file a restore
// copies out of its snapshot, given its path where the snapshot resolves: a
// non-nil answer is that open's error, nil lets the open run; and returns the
// restore.
func SetFailSourceOpenForTest(fn func(path string) error) (restore func()) {
	var p *func(string) error
	if fn != nil {
		p = &fn
	}
	prev := failSourceOpen.Swap(p)
	return func() { failSourceOpen.Store(prev) }
}

// SetOnSourceWorkerEndForTest installs the hook a restore's source worker
// runs last, given the snapshot as the restore was given it, once it has
// handed its hold over or let go of what it took, and returns the restore.
// Its callback must return, and ignore every snapshot but its test's.
func SetOnSourceWorkerEndForTest(fn func(dir string)) (restore func()) {
	var p *func(string)
	if fn != nil {
		p = &fn
	}
	prev := onSourceWorkerEnd.Swap(p)
	return func() { onSourceWorkerEnd.Store(prev) }
}
