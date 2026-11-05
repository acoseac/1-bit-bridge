//go:build darwin

package manifest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// B651's tenth round (Codex on round 9): a nested root configured in another
// case than the folder above lists it in, on a volume that folds case, is
// watched once. fsnotify's kqueue backend and the watcher's account of it key
// a watch by its path's exact spelling, so a tree watched as music/USB and
// listed as music/usb was a second tree to both.

// openCounts is how many descriptors this process holds open on each file:
// the /dev/fd names, by device and inode. A descriptor closed between the
// listing and its stat is skipped.
func openCounts(t *testing.T) map[fileID]int {
	t.Helper()
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[fileID]int)
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		counts[fileID{uint64(st.Dev), st.Ino}]++
	}
	return counts
}

// openTwice is the entries at or under root this process holds open more than
// once, and the number of entries there, each of which kqueue's watches open
// once.
func openTwice(t *testing.T, root string) (twice []string, entries int) {
	t.Helper()
	counts := openCounts(t)
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if counts[fileIDOf(t, p)] > 1 {
			twice = append(twice, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return twice, entries
}

// charged is the number of open files the watcher's account holds, read
// while Run's loop is between events, and published to it through the loop's
// gate before and after, so the read is ordered with the loop's writes.
func charged(t *testing.T, w *Watcher) int {
	t.Helper()
	waitUntilWatcherIdle(t, w)
	n := w.fds.n
	waitUntilWatcherIdle(t, w)
	return n
}

// TestANestedRootInAnotherCaseIsWatchedOnceOnAVolumeThatFoldsCase: a root
// configured as music/USB, nested in the root music on this host's volume,
// which folds case and lists the folder as usb, is watched once: from the
// watcher's start, and after a hold and its resume, when the next change in
// music has fsnotify send a Create for usb, a path it stopped watching with the
// hold. No entry under music is open twice, the account charges the budget
// for the two entries made after alone, a budget the library fits once is not
// spent, and the Create is taken for the root the watcher watches, never for a
// new folder whose tree it adds. Where the volume does not fold case, or
// fsnotify does not watch through kqueue, the two spellings are two folders,
// or nothing counts open files, and the test skips, saying so.
func TestANestedRootInAnotherCaseIsWatchedOnceOnAVolumeThatFoldsCase(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		name := "from the start"
		if resumed {
			name = "after a hold and its resume"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outer := filepath.Join(dir, "music")
			disk, configured := filepath.Join(outer, "usb"), filepath.Join(outer, "USB")
			writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(disk, "Band", "Record", "01.flac"))
			if got := fsutil.FoldingIn(outer); got != fsutil.CaseInsensitive {
				t.Skipf("this host's volume answers %s for %s: USB is no other spelling of usb on it", got, outer)
			}
			store := openStoreIn(t, dir)
			scanner := NewScanner([]string{outer, configured}, store, "")
			if _, err := scanner.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			band, aside := filepath.Join(disk, "Band"), filepath.Join(dir, "Band")
			if resumed {
				// The root's content goes and its folder stays, so the
				// folder's entry watch is not dropped by a removal.
				if err := os.Rename(band, aside); err != nil {
					t.Fatal(err)
				}
				mustReadAsNotMounted(t, scanner, configured)
			}
			w, err := NewWatcher(scanner, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if w.fds == nil {
				t.Skip("fsnotify does not watch through kqueue on this host, so no open file counts a watch")
			}
			if resumed {
				// The library once, Band, Record and 01.flac back with the
				// resume, and the file and the folder made below. At the
				// start the held root is not counted; from the start, the
				// count before the walk (CountWatchSet) takes the nested
				// root's tree for a tree of its own, as every nested root's.
				_, entries := openTwice(t, outer)
				w.fds.budget = entries + 3 + 2
			}
			w.recheckFirst, w.recheckMax = 20*time.Millisecond, 100*time.Millisecond
			w.rootCheckEvery, w.reconcileEvery = time.Hour, time.Hour
			w.immediateFlushForTests = true
			held, back := make(chan string, 8), make(chan string, 8)
			w.afterHoldForTests = func(root string) { held <- root }
			w.afterResumeForTests = func(root string) { back <- root }
			events := &eventNames{seen: make(map[string]bool)}
			w.afterEventForTests = events.note
			rec := loggingtest.Record(t)
			startWatcher(t, w)
			if resumed {
				if got := waitForRoot(t, held, "the watcher never held the nested root"); got != configured {
					t.Fatalf("the watcher held %q, want %q", got, configured)
				}
				if err := os.Rename(aside, band); err != nil {
					t.Fatal(err)
				}
				if got := waitForRoot(t, back, "the watcher never watched the nested root again"); got != configured {
					t.Fatalf("the watcher watched %q again, want %q", got, configured)
				}
			}
			before := charged(t, w)

			writeTracks(t, filepath.Join(outer, "new.flac"))
			const sync = "zz-sync"
			if err := os.Mkdir(filepath.Join(outer, sync), 0o755); err != nil {
				t.Fatal(err)
			}
			watchWaitUntil(t, func() bool { return watched(w, sync) }, func() string {
				if lines := rec.Lines(msgWatcherStopped); len(lines) > 0 {
					return "the watcher stopped, past a budget the library fits once: " + lines[0]
				}
				return ""
			}, "the watcher never watched the folder made in music")
			after := charged(t, w)

			if twice, _ := openTwice(t, outer); len(twice) > 0 {
				t.Errorf("these are open twice under %s: %q", outer, twice)
			}
			if got := after - before; got != 2 {
				t.Errorf("the account charged %d open files for the file and the folder made in music, want 2", got)
			}
			if lower, upper := events.both("usb", "USB"); lower || upper {
				t.Errorf("the watcher handled a Create of the nested root's folder as a new folder")
			}
		})
	}
}
