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

	"github.com/fsnotify/fsnotify"

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

// TestAReplacedNestedRootIsWatchedAgainOnItsCreate: a nested root's folder is
// moved away while kqueue drops its watch without a word (fsnotify reads a
// change in the folder and its rename as one event and sends neither, which
// the test stands in for by dropping the Rename before the watcher sees it),
// and another folder is put at its path. The Create the folder above sends for
// it is not taken for the root the watcher watches, whose watch is gone: the
// root is looked at again and watched where it is now, and a file written into
// the new folder is seen (B651's eleventh round, Claude on round 10: round
// 10's early return left the new folder unwatched until the check of the
// roots' own watches came round). The look comes with nothing else changing
// in the library: it waits for the flush a new folder's watch waits for, and
// the loop's quiet timer is what asks for it (B651's twelfth round, Codex on
// round 11). Runs where fsnotify watches through kqueue.
func TestAReplacedNestedRootIsWatchedAgainOnItsCreate(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	nested := filepath.Join(outer, "usb")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(nested, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{outer, nested}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(scanner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if w.fds == nil {
		t.Skip("fsnotify does not watch through kqueue on this host, which alone drops a renamed folder's watch without a word")
	}
	w.recheckFirst, w.recheckMax = 20*time.Millisecond, 100*time.Millisecond
	w.rootCheckEvery, w.reconcileEvery = time.Hour, time.Hour
	dropped := make(chan struct{}, 1)
	w.dropEventForTests = func(ev fsnotify.Event) bool {
		if ev.Has(fsnotify.Rename) && filepath.Clean(ev.Name) == nested {
			select {
			case dropped <- struct{}{}:
			default:
			}
			return true
		}
		return false
	}
	events := &eventNames{seen: make(map[string]bool)}
	w.afterEventForTests = events.note
	rec := loggingtest.Record(t)
	startWatcher(t, w)

	if err := os.Rename(nested, filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropped:
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("fsnotify sent no Rename for the moved root, so nothing stood for its watch going without a word")
	}
	writeTracks(t, filepath.Join(dir, "new", "Fresh", "Album", "02.flac"))
	if err := os.Rename(filepath.Join(dir, "new"), nested); err != nil {
		t.Fatal(err)
	}
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgWatchRootRewatched)) > 0 }, nil,
		"the Create of "+nested+", a root whose watch went, asked for no look at it, with nothing else changing in the library")

	const fresh, sync = "03.flac", "zz-sync.flac"
	writeTracks(t, filepath.Join(nested, "Fresh", "Album", fresh), filepath.Join(outer, sync))
	watchWaitUntil(t, func() bool {
		seen, _ := events.both(fresh, "")
		return seen
	}, func() string {
		if sawFresh, sawSync := events.both(fresh, sync); sawSync && !sawFresh {
			return "it saw the file made after it in music, so the new folder is not watched"
		}
		return ""
	}, "the watcher saw no event for a file written into the nested root's new folder")
}

// TestAReplacedRootsCreateLeavesItsLookToTheFlush: on kqueue the Create of a
// watched root whose own watch is gone adds no watch from its handler.
// fsnotify watches the folder at the root's path itself right after sending
// the Create, on its own goroutine, and an Add from the handler races that and
// can leave a descriptor nothing closes, as a new folder's would (B212). The
// look waits for the flush a new folder's watch waits for, and the flush
// watches the root again, records its own watch, asks for its scan and says
// so; unless, by then, the root is held (its retry looks at it) or its own
// watch is back (B651's twelfth round, Codex on round 11). The race itself
// has no seam: fsnotify's goroutine cannot be held between its send and its
// open, so the handler is driven directly and what it leaves for the flush is
// checked. Runs where fsnotify watches through kqueue.
func TestAReplacedRootsCreateLeavesItsLookToTheFlush(t *testing.T) {
	for _, c := range []struct {
		name string
		// meanwhile is what happens to the root between the Create and the
		// flush.
		meanwhile func(t *testing.T, w *Watcher, root string)
		look      bool
	}{
		{"nothing since", nil, true},
		{"its own watch back since", func(t *testing.T, w *Watcher, root string) {
			// As another look has it watched again, without this look's line.
			if err := w.watchAgain(context.Background(), root); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"held since", func(t *testing.T, w *Watcher, root string) {
			w.holdRoot(root, rootLook{absent: true, reason: rootAbsentUnreachable})
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			outer := filepath.Join(dir, "music")
			nested := filepath.Join(outer, "usb")
			writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(nested, "Band", "Record", "01.flac"))
			scanner := NewScanner([]string{outer, nested}, openStoreIn(t, dir), "")
			if _, err := scanner.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			w, err := NewWatcher(scanner, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				w.cancelAllPending()
				_ = w.w.Close()
			})
			if w.fds == nil {
				t.Skip("fsnotify does not watch through kqueue on this host, where a Create's handler watches the folder it names")
			}
			// Both roots watched, as Run's start leaves them; then the nested
			// root's own watch goes as kqueue drops a renamed folder's,
			// without a word to the watcher or its account.
			for _, root := range []string{outer, nested} {
				if err := w.addTree(root, true); err != nil {
					t.Fatal(err)
				}
			}
			w.noteRootWatches([]string{outer, nested})
			if err := w.w.Remove(nested); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)

			w.handleEvent(context.Background(), fsnotify.Event{Name: nested, Op: fsnotify.Create})
			if _, installed := w.watchSet()[nested]; installed {
				t.Fatalf("the Create's handler watched %s itself, racing fsnotify's own watch of the folder at its path", nested)
			}
			if lines := rec.Lines(msgWatchRootRewatched); len(lines) != 0 {
				t.Fatalf("the Create's handler looked at %s again: %q", nested, lines)
			}
			if c.meanwhile != nil {
				c.meanwhile(t, w, nested)
			}

			w.flushPendingAdds(context.Background())
			lines := rec.Lines(msgWatchRootRewatched)
			if !c.look {
				if len(lines) != 0 {
					t.Errorf("the flush looked at %s again: %q", nested, lines)
				}
				return
			}
			if _, installed := w.watchSet()[nested]; !installed {
				t.Fatalf("the flush did not watch %s again", nested)
			}
			if got := w.rootWatches[nested]; got != nested {
				t.Errorf("the flush recorded %s's own watch as %q, want %q", nested, got, nested)
			}
			w.mu.Lock()
			_, scan := w.pending[nested]
			w.mu.Unlock()
			if !scan {
				t.Errorf("the flush asked for no scan of %s", nested)
			}
			if len(lines) != 1 {
				t.Errorf("the flush said %d times that it watched %s again, want once: %q", len(lines), nested, lines)
			}
		})
	}
}

// TestTheCheckOfTheRootsWatchesLeavesAWaitingLookToTheFlush: the timer's check
// of the roots' own watches can fire right after a root's Create, while
// fsnotify may still be opening the folder at the root's path. That Create's
// look waits for the flush for this reason
// (TestAReplacedRootsCreateLeavesItsLookToTheFlush), and the check, finding the
// root's own watch gone, would make the very Add the wait avoids. It leaves
// that root to the flush, and still looks at another root whose watch went
// (Codex and the Claude API reviewer on B651's twelfth round).
func TestTheCheckOfTheRootsWatchesLeavesAWaitingLookToTheFlush(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	nested := filepath.Join(outer, "usb")
	other := filepath.Join(dir, "other")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(nested, "Band", "Record", "01.flac"), filepath.Join(other, "Solo", "Single", "01.flac"))
	roots := []string{outer, nested, other}
	scanner := NewScanner(roots, openStoreIn(t, dir), "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(scanner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.cancelAllPending()
		_ = w.w.Close()
	})
	if w.fds == nil {
		t.Skip("fsnotify does not watch through kqueue on this host, where a Create's handler watches the folder it names")
	}
	for _, root := range roots {
		if err := w.addTree(root, true); err != nil {
			t.Fatal(err)
		}
	}
	w.noteRootWatches(roots)
	for _, gone := range []string{nested, other} {
		if err := w.w.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}
	rec := loggingtest.Record(t)

	w.handleEvent(context.Background(), fsnotify.Event{Name: nested, Op: fsnotify.Create})
	w.checkRootWatches(context.Background())
	if _, installed := w.watchSet()[nested]; installed {
		t.Fatalf("the check watched %s while its Create's look waited for the flush, racing fsnotify's own watch of the folder at its path", nested)
	}
	if _, installed := w.watchSet()[other]; !installed {
		t.Errorf("the check did not watch %s again, which no Create named", other)
	}
	if lines := rec.Lines(msgWatchRootRewatched); len(lines) != 1 {
		t.Errorf("after the check, %d lines say a root was watched again, want one, for %s: %q", len(lines), other, lines)
	}

	w.flushPendingAdds(context.Background())
	if _, installed := w.watchSet()[nested]; !installed {
		t.Fatalf("the flush did not watch %s again", nested)
	}
	if lines := rec.Lines(msgWatchRootRewatched); len(lines) != 2 {
		t.Errorf("after the flush, %d lines say a root was watched again, want two: %q", len(lines), lines)
	}
}
