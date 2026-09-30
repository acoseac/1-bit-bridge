//go:build unix

package manifest

// A file the scan could not read is not a file the scan read. A worker that
// cannot open the file the walk handed it knows nothing of what the file
// holds, so it must not write the row the walk's stat and the path alone
// would make (fillFromPath: the file name as the title, the folders as the
// album and the artist): the skip gate compares that stat with the file's on
// every later scan, finds them equal and keeps the guess until the file
// changes again. These tests lock a file out with chmod 0, the one real way
// to make an open fail that needs no fault injection, which only a POSIX host
// honours, and only for a user other than root.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// lockOut makes each path unreadable to this process (chmod 0) and puts the
// mode back when the test ends, and skips the test where that does not stop
// an open (root reads a file whatever its mode).
func lockOut(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	}
	if f, err := os.Open(paths[0]); err == nil {
		_ = f.Close()
		t.Skip("chmod 0 does not stop this user's open (root)")
	}
}

// letIn puts each path's mode back to 0644.
func letIn(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestScanner_AFileThatCannotBeOpenedIsNotWrittenFromItsPath is the defect
// (backlog B134): a changed file whose open failed was written from its path,
// under its new size and mtime, and a new file whose open failed got a row by
// its name, and the skip gate then kept both guesses once the files could be
// read again, since neither file changed after the scan that failed.
func TestScanner_AFileThatCannotBeOpenedIsNotWrittenFromItsPath(t *testing.T) {
	f := newLinkedFixture(t)
	changed := filepath.Join(f.album, "01.flac")
	fresh := filepath.Join(f.album, "02.flac")
	writeMinimalFLAC(t, changed, 44100, 16, map[string]string{"TITLE": "Before"})
	setMTime(t, changed, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	scanOnce(t, f.sc, "the older version")

	writeMinimalFLAC(t, changed, 44100, 16, map[string]string{"TITLE": "After"})
	writeMinimalFLAC(t, fresh, 44100, 16, map[string]string{"TITLE": "New"})
	lockOut(t, changed, fresh)
	rec := loggingtest.Record(t)
	scanOnce(t, f.sc, "the files locked out")

	if title, ok := rowTitle(t, f.store, "Music/Album/01.flac"); !ok || title != "Before" {
		t.Errorf("the changed file's row: title %q (a row: %v), want the row as it was, title \"Before\"", title, ok)
	}
	if title, ok := rowTitle(t, f.store, "Music/Album/02.flac"); ok {
		t.Errorf("the new file got a row it was never read for: title %q", title)
	}
	lines := rec.Lines(msgUnreadAudio)
	if len(lines) != 1 || !strings.Contains(lines[0], "count=2") || !strings.Contains(lines[0], "err=open: permission denied") {
		t.Errorf("lines %q, want one counting both files, their open refused", lines)
	}

	letIn(t, changed, fresh)
	scanOnce(t, f.sc, "the files readable again")

	if title, _ := rowTitle(t, f.store, "Music/Album/01.flac"); title != "After" {
		t.Errorf("once readable, the changed file's row: title %q, want \"After\"", title)
	}
	if title, _ := rowTitle(t, f.store, "Music/Album/02.flac"); title != "New" {
		t.Errorf("once readable, the new file's row: title %q, want \"New\"", title)
	}
}
