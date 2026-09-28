//go:build linux

package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCountDirsCountsThroughALinkedRoot: the inotify pre-flight counts the
// directories the watcher will watch, and the watcher walks a root that is
// itself a link to a directory through it (fsutil.WalkableRoot). Walked as
// the link, the count was zero while the watcher registered a watch per
// directory behind it, so the check could call a budget safe that the
// watcher then exhausted.
func TestCountDirsCountsThroughALinkedRoot(t *testing.T) {
	target := t.TempDir()
	for _, sub := range []string{"Artist/Album", "Other", ".Trash/Old"} {
		if err := os.MkdirAll(filepath.Join(target, filepath.FromSlash(sub)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "music")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}

	// The root, Artist, Artist/Album and Other; .Trash is noise the
	// watcher skips.
	const want = 4
	for _, root := range []string{target, link} {
		got, err := countDirs([]string{root}, 0)
		if err != nil || got != want {
			t.Errorf("countDirs(%s) = %d, %v; want %d", root, got, err, want)
		}
	}
}
