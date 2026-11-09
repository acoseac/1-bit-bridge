package backup

import (
	"os"
	"time"
)

// Test seams for packages that drive a verification through their own
// callers (cmd/bridge's ticker and CLI, the console). Each is process-global:
// a test that uses one MUST NOT call t.Parallel(), and puts it back in a
// t.Cleanup once every verification it started has returned. Production
// code never calls them.

// afterCheck, nil in production, runs once a verification's check has
// ended, before its verdict is written, with the backups folder's lock
// held: a test parks a verification there.
var afterCheck func(dir string)

// SetAfterCheckForTest sets the function a verification runs once its check
// has ended and before it writes its verdict, holding the lock, and returns
// the restore.
func SetAfterCheckForTest(fn func(dir string)) (restore func()) {
	prev := afterCheck
	afterCheck = fn
	return func() { afterCheck = prev }
}

// onSlotWait, nil in production, runs when a verification finds this
// process's slot held and begins to wait its turn.
var onSlotWait func()

// SetOnSlotWaitForTest sets the function a verification runs when it begins
// to wait its turn behind another of this process's verifications, and
// returns the restore.
func SetOnSlotWaitForTest(fn func()) (restore func()) {
	prev := onSlotWait
	onSlotWait = fn
	return func() { onSlotWait = prev }
}

// SetSyncVerdictForTest replaces the barrier a verdict's write runs once its
// rename has landed (fsutil.SyncParentDir), and returns the restore.
func SetSyncVerdictForTest(fn func(path string) error) (restore func()) {
	prev := syncVerdictDir
	syncVerdictDir = fn
	return func() { syncVerdictDir = prev }
}

// SetSyncVetoForTest replaces the barrier that makes the veto of a snapshot
// about to be checked again durable (fsutil.SyncParentDir), and returns the
// restore.
func SetSyncVetoForTest(fn func(path string) error) (restore func()) {
	prev := syncVetoDir
	syncVetoDir = fn
	return func() { syncVetoDir = prev }
}

// SetSyncsParentDirForTest sets whether a directory can be synced here, as a
// verification asks before it sets a VERIFIED aside (fsutil.SyncsParentDir:
// false on Windows), and returns the restore.
func SetSyncsParentDirForTest(syncs bool) (restore func()) {
	prev := syncsParentDir
	syncsParentDir = syncs
	return func() { syncsParentDir = prev }
}

// SetMetadataForTest replaces what a verification, a prune and a reader ask
// of a path's metadata (the resolution of its links, the prune's Lstat of
// its folder), each nil for the platform's own, and returns the restore: a
// test stands a mount whose metadata does not answer in for them.
func SetMetadataForTest(resolve func(string) (string, error), lstatFn func(string) (os.FileInfo, error)) (restore func()) {
	prevResolve, prevLstat := resolveLinks, lstat
	if resolve != nil {
		resolveLinks = resolve
	}
	if lstatFn != nil {
		lstat = lstatFn
	}
	return func() { resolveLinks, lstat = prevResolve, prevLstat }
}

// SetReadDirForTest replaces the listing of the backups folder List,
// ReadStatus and the orphan sweep take, and returns the restore.
func SetReadDirForTest(fn func(name string) ([]os.DirEntry, error)) (restore func()) {
	prev := readDir
	readDir = fn
	return func() { readDir = prev }
}

// SetOnPruneForTest sets the function a prune runs as it begins, under the
// backups folder's lock, and returns the restore.
func SetOnPruneForTest(fn func()) (restore func()) {
	prev := onPrune
	onPrune = fn
	return func() { onPrune = prev }
}

// SetOpenProbeForTest replaces the open a probe of the backups folder's lock
// makes (ProbeBackupsLock, StuckCheckOf), and returns the restore.
func SetOpenProbeForTest(fn func(path string) (*os.File, error)) (restore func()) {
	prev := openProbe
	openProbe = fn
	return func() { openProbe = prev }
}

// SetVerifyReadRateForTest sets how many bytes a second a verification
// reads at most (0: unpaced), and returns the restore.
func SetVerifyReadRateForTest(rate int64) (restore func()) {
	prev := verifyReadRate
	verifyReadRate = rate
	return func() { verifyReadRate = prev }
}

// SetTryLockBackupsForTest replaces the try a wait for the backups folder's
// lock makes (fsutil.TryLock), and returns the restore.
func SetTryLockBackupsForTest(fn func(*os.File) error) (restore func()) {
	prev := tryLockBackups
	tryLockBackups = fn
	return func() { tryLockBackups = prev }
}

// SetOnLowPriorityForTest sets the function a verification's thread runs once
// its priority is lowered, and returns the restore.
func SetOnLowPriorityForTest(fn func()) (restore func()) {
	prev := onLowPriority
	onLowPriority = fn
	return func() { onLowPriority = prev }
}

// SetOnDatabaseOpenForTest sets the function a verification runs just
// before SQLite opens the snapshot's database, and returns the restore.
func SetOnDatabaseOpenForTest(fn func()) (restore func()) {
	prev := onDatabaseOpen
	onDatabaseOpen = fn
	return func() { onDatabaseOpen = prev }
}

// VetoFileName is the file a verdict's write keeps in the bundle while the
// write is not known to be durable.
const VetoFileName = vetoFileName

// OwnerLockName is the file in a snapshot whose lock its owner holds.
const OwnerLockName = ownerLockName

// RootVetoPath is where a verdict's write keeps the veto of the snapshot in
// dir when it cannot keep it in the bundle.
func RootVetoPath(dir string) string { return rootVetoPath(dir) }

// SetOnManifestReadForTest sets the function a verification runs before each
// read of a snapshot's manifest, and returns the restore.
func SetOnManifestReadForTest(fn func(path string)) (restore func()) {
	prev := onManifestRead
	onManifestRead = fn
	return func() { onManifestRead = prev }
}

// SetOpenBackupsLockForTest replaces the open of the backups folder's lock
// file (fsutil.OpenLockFile), and returns the restore.
func SetOpenBackupsLockForTest(fn func(dir, name string) (*os.File, error)) (restore func()) {
	prev := openBackupsLock
	openBackupsLock = fn
	return func() { openBackupsLock = prev }
}

// SetVerifyDeadlineForTest sets a verification's deadline (VerifyDeadline),
// and returns the restore.
func SetVerifyDeadlineForTest(d time.Duration) (restore func()) {
	prev := verifyDeadline
	verifyDeadline = d
	return func() { verifyDeadline = prev }
}

// SetOnLockWaitForTest sets the function a wait for the backups folder's
// lock runs once it finds the lock held, and returns the restore.
func SetOnLockWaitForTest(fn func()) (restore func()) {
	prev := onLockWait
	onLockWait = fn
	return func() { onLockWait = prev }
}

// SetVerdictWriteFailuresForTest sets the failures a verdict's write meets:
// at its staging (given the bundle), its rename (given the manifest's path)
// and the making of a veto (given the veto's path), each nil for none, and
// returns the restore.
func SetVerdictWriteFailuresForTest(stage func(dir string) error, rename func(path string) error, veto func(path string) error) (restore func()) {
	prevStage, prevRename, prevVeto := failStage, failRename, failVeto
	failStage, failRename, failVeto = stage, rename, veto
	return func() { failStage, failRename, failVeto = prevStage, prevRename, prevVeto }
}

// TakeOwnerForTest takes the owner lock of the snapshot in dir as a capture
// does, so the snapshot reads as one whose verification is still owed, and
// answers its release.
func TakeOwnerForTest(dir string) (release func(), err error) {
	o, err := takeOwner(dir)
	if err != nil {
		return nil, err
	}
	return o.release, nil
}
