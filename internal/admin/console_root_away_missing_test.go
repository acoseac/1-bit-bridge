package admin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAWriteIntoARootUnreachableButForItsAbsenceIsRefusedWithNoRows: a root
// whose path is not there at all, the index holding nothing of it, takes its
// first upload, which makes its folder (backlog B776's round 2; cmd/bridge's
// TestAPublicBridgesFirstUploadIntoARootNotMadeYetMakesIt). One that reads as
// unreachable for anything but its absence is still refused, though the index
// holds nothing of it either: its folder cannot be told absent.
func TestAWriteIntoARootUnreachableButForItsAbsenceIsRefusedWithNoRows(t *testing.T) {
	t.Run("a permission refused on its way", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a folder's mode bits refuse no read on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads a folder at mode 000")
		}
		srv, _, _ := newTestServer(t)
		enableUploads(t, srv)
		locked := filepath.Join(t.TempDir(), "locked")
		root := filepath.Join(locked, "Music")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		srv.deps.Scanner.SetRoots([]string{root})
		srv.deps.Resolver.SetRoots([]string{root})
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"files": []map[string]any{{"path": "Album/01.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, root, notMountedWords)
	})

	t.Run("a link to nothing", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		enableUploads(t, srv)
		dir := t.TempDir()
		root := filepath.Join(dir, "Music")
		if err := os.Symlink(filepath.Join(dir, "gone"), root); err != nil {
			t.Skipf("this host cannot make a symbolic link: %v", err)
		}
		srv.deps.Scanner.SetRoots([]string{root})
		srv.deps.Resolver.SetRoots([]string{root})
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"files": []map[string]any{{"path": "Album/01.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, root, notMountedWords)
	})
}
