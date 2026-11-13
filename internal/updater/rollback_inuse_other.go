//go:build !windows

package updater

// inUseByAnotherProcess is Windows' alone (rollback_inuse_windows.go): a
// rename here never waits on another process's open file, and no rollback
// here comes through replaceRunningBinary, RollbackBinary being one rename.
func inUseByAnotherProcess(error) bool { return false }
