//go:build !darwin

package updater

import "os"

// plainFsync is nil off darwin: (*os.File).Sync is the ordinary fsync(2)
// already, and nothing is asked after it.
func plainFsync(*os.File) func() error { return nil }
