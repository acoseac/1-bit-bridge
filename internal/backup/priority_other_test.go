//go:build !linux

package backup_test

// threadPriority is read on Linux alone: elsewhere the test asks only that
// the check ran on the lowered thread.
func threadPriority() int { return 0 }
