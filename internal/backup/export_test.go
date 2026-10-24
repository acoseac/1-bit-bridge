package backup

import (
	"context"
	"io"
	"os"
)

// SetTryLockForTest replaces the lock a backup's seal completion tries, and
// returns the restore.
func SetTryLockForTest(fn func(*os.File) error) (restore func()) {
	prev := tryLock
	tryLock = fn
	return func() { tryLock = prev }
}

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

// DirectoryRemedy is the one remedy a directory-level failure takes on goos.
func DirectoryRemedy(goos, dir string) string { return directoryRemedy(goos, dir) }

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
