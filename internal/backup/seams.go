package backup

import "os"

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

// SetSyncVerdictForTest replaces the barrier a verdict's write runs once its
// rename has landed (fsutil.SyncParentDir), and returns the restore.
func SetSyncVerdictForTest(fn func(path string) error) (restore func()) {
	prev := syncVerdictDir
	syncVerdictDir = fn
	return func() { syncVerdictDir = prev }
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
