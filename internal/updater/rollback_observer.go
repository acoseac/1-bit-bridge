package updater

import "sync/atomic"

// RollbackStage names one of the renames replaceRunningBinary makes to put a
// backup back over the binary (Windows' RollbackBinary), which a test's
// observer is told of when it fails (SetRollbackObserverForTest).
type RollbackStage string

const (
	// RollbackStageInstall is the backup's rename onto a binary path where
	// nothing stands.
	RollbackStageInstall RollbackStage = "install"
	// RollbackStageVacate is the running binary's rename aside.
	RollbackStageVacate RollbackStage = "vacate"
	// RollbackStageBackup is the backup's rename onto the vacated path.
	RollbackStageBackup RollbackStage = "backup"
	// RollbackStagePutBack is the rename of the binary set aside back onto
	// its path, once the backup's rename was refused.
	RollbackStagePutBack RollbackStage = "putback"
)

// RollbackRename is a rename of the rollback's that failed: its stage, its
// source and destination, and the error the rename answered.
type RollbackRename struct {
	Stage    RollbackStage
	From, To string
	Err      error
}

// rollbackObserver, nil in production, hears every rename replaceRunningBinary
// makes that fails (SetRollbackObserverForTest).
var rollbackObserver atomic.Pointer[func(RollbackRename)]

// SetRollbackObserverForTest makes fn hear every rename of the rollback's
// put-back (replaceRunningBinary) that fails, with its stage, both paths and
// the error, on the goroutine that made it and before the rollback's next
// step, and returns what puts the observer before it back. Its one use is a
// test's diagnostic of what holds a file Windows refused to rename (backlog
// B872). Production code MUST NOT call it: with none set, a rename that fails
// costs one atomic load, and the rollback answers what it answered without
// it. Process-global: a test that sets it MUST NOT call t.Parallel(), and
// restores it in a t.Cleanup once every rollback it started has returned.
func SetRollbackObserverForTest(fn func(RollbackRename)) (restore func()) {
	var p *func(RollbackRename)
	if fn != nil {
		p = &fn
	}
	prev := rollbackObserver.Swap(p)
	return func() { rollbackObserver.Store(prev) }
}

// observeRollbackRename tells the observer a test set, where one is, of a
// rename of the rollback's that failed.
func observeRollbackRename(stage RollbackStage, from, to string, err error) {
	if fn := rollbackObserver.Load(); fn != nil {
		(*fn)(RollbackRename{Stage: stage, From: from, To: to, Err: err})
	}
}
