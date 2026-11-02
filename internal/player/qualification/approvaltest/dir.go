// Package approvaltest prepares private directories for owner-approval fixtures.
package approvaltest

import (
	"os"
	"testing"
)

// SecureDir makes an existing fixture directory safe for approval reads.
// Chmod is required because TempDir's effective mode can depend on umask.
func SecureDir(t testing.TB, dir string) string {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TempDir creates a private directory for approval records and evidence.
func TempDir(t testing.TB) string {
	t.Helper()
	return SecureDir(t, t.TempDir())
}
