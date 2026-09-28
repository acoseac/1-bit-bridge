//go:build windows

package fsutil

import "os"

// keepOwner does nothing on Windows: a file created in a directory takes
// that directory's ACL, so a staged file already belongs where its
// replacement does.
func keepOwner(*os.File, string) error { return nil }

// mkdirAll is os.MkdirAll on Windows, where a directory takes its
// parent's ACL. See the POSIX version.
func mkdirAll(path string, perm os.FileMode, _ dirOwnerPolicy, _ string) error {
	return os.MkdirAll(path, perm)
}

// mkdir is os.Mkdir on Windows. See the POSIX version.
func mkdir(path string, perm os.FileMode, _ dirOwnerPolicy, _ string, _ bool) error {
	return os.Mkdir(path, perm)
}

// precreate does nothing on Windows, where whatever a writer creates takes
// its directory's ACL. See the POSIX version.
func precreate(string, os.FileMode, string) error { return nil }

// SimulateRootForTest is a no-op on Windows, where these helpers change no
// owner; a test that uses it skips there. See the POSIX version.
func SimulateRootForTest(int, int) (changes func() []OwnerChange, restore func()) {
	return func() []OwnerChange { return nil }, func() {}
}
