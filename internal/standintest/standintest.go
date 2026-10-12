// Package standintest puts the running test binary on PATH as a stand-in
// for a tool the code under test executes. Like net/http/httptest it is
// imported only by tests, so none of it reaches the binary.
package standintest

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Install puts the running test binary in dir under each name, so a test
// can make that directory the whole of PATH. On Windows each name gains
// ".exe". Names are the tool names the code looks up, without an extension.
func Install(t testing.TB, dir string, names ...string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		dst := filepath.Join(dir, name)
		// A hard link of the running binary cannot be removed while the test
		// binary is mapped, which fails TempDir cleanup on Windows. Copy there,
		// and wherever the link itself fails.
		if runtime.GOOS == "windows" || os.Link(exe, dst) != nil {
			copyFile(t, exe, dst)
		}
	}
}

func copyFile(t testing.TB, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
