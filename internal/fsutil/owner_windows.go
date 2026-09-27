//go:build windows

package fsutil

import "os"

// keepOwner does nothing on Windows: a file created in a directory takes
// that directory's ACL, so a staged file already belongs where its
// replacement does.
func keepOwner(*os.File, string) error { return nil }

// SimulateRootForTest is a no-op on Windows, where KeepOwner changes
// nothing; a test that uses it skips there. See the POSIX version.
func SimulateRootForTest(int, int) (changes func() []OwnerChange, restore func()) {
	return func() []OwnerChange { return nil }, func() {}
}
