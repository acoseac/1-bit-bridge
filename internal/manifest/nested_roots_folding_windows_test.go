//go:build windows

package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestTwoRootsSpelledAlikeInACaseSensitiveFolderStayTwo: Windows can make one
// folder case-sensitive (fsutil.exe file setCaseSensitiveInfo), and two roots
// in such a folder, USB and usb, are two folders: each path is its own root's,
// and a subtree scan of each indexes its file under its own root, where
// filepath.Rel, which folds case in every folder, took both for the root
// listed first (Codex on round 6). This host's own probe answers, with no
// stand-in. Runs where the host lets a folder be made case-sensitive.
func TestTwoRootsSpelledAlikeInACaseSensitiveFolderStayTwo(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "music")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(os.Getenv("SystemRoot"), "System32", "fsutil.exe")
	if out, err := exec.Command(tool, "file", "setCaseSensitiveInfo", music, "enable").CombinedOutput(); err != nil {
		t.Skipf("this host cannot make a folder case-sensitive: %v: %s", err, out)
	}
	upper, lower := filepath.Join(music, "USB"), filepath.Join(music, "usb")
	writeTracks(t, filepath.Join(upper, "Band", "Record", "01.flac"), filepath.Join(lower, "Band", "Record", "01.flac"))
	if got := fsutil.FoldingIn(music); got != fsutil.CaseSensitive {
		t.Fatalf("the folder made case-sensitive answered %s", got)
	}
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{upper, lower}, store, "")
	ctx := context.Background()
	for _, root := range []string{upper, lower} {
		band := filepath.Join(root, "Band")
		if got, _ := scanner.rootOf(band); got != root {
			t.Errorf("%s is %q's, want %q's", band, got, root)
		}
		if _, err := scanner.ScanSubtree(ctx, band); err != nil {
			t.Fatal(err)
		}
	}
	mustHoldRows(t, store, "USB/Band/Record/01.flac", "usb/Band/Record/01.flac")
}
