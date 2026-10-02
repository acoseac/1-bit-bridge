//go:build darwin

package manifest

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// These tests measure the watcher where fsnotify watches through kqueue,
// which opens a descriptor for every watched folder and for every entry in
// one (backlog B212). macOS is the kqueue platform the bridge ships for and
// the one CI runs; the account they pin is watcher_fds.go's.

// libraryFiles is the number of descriptors this process holds open on what
// is under root now: the descriptors /dev/fd names whose device and inode
// are those of an entry a walk of root finds. A descriptor fsnotify kept
// after a rename still counts (a renamed file keeps its inode), and every
// other descriptor of the test process (the store's, the runtime's, one
// another test's goroutine opens or closes meanwhile) counts for nothing.
// Measured on the macOS CI runner, the whole-process count was not stable:
// os.ReadDir of /dev/fd failed there with fstatat's EBADF, a descriptor
// closed between the listing and its stat. Names only, for that reason, and
// a descriptor closed before its fstat is skipped.
func libraryFiles(t *testing.T, root string) int {
	t.Helper()
	type id struct{ dev, ino uint64 }
	under := make(map[id]bool)
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			return err
		}
		under[id{uint64(st.Dev), st.Ino}] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		if under[id{uint64(st.Dev), st.Ino}] {
			n++
		}
	}
	return n
}

// makeAlbums makes n album folders in dir, each holding files files, and
// returns them.
func makeAlbums(t *testing.T, dir string, n, files int) []string {
	t.Helper()
	var albums []string
	for a := 0; a < n; a++ {
		album := filepath.Join(dir, fmt.Sprintf("Album%02d", a))
		if err := os.MkdirAll(album, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < files; f++ {
			if err := os.WriteFile(filepath.Join(album, fmt.Sprintf("%02d.flac", f)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		albums = append(albums, album)
	}
	return albums
}

// syncWatcher makes an empty folder named for i in root and waits until the
// watcher watches it. fsnotify sends events in the order it reads them, the
// watcher handles them in that order, and the folder is watched only once
// its Create is handled, so by then every event that came before it has been
// handled too. The folder holds one open file.
func syncWatcher(t *testing.T, w *Watcher, root string, i int) {
	t.Helper()
	name := fmt.Sprintf("zz-sync-%d", i)
	if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
	watchWaitUntil(t, func() bool { return watched(w, name) }, nil,
		"the watcher never watched the folder made to sync with it")
}

// TestKqueueWatcherReleasesARenamedFoldersFiles: renaming album folders in
// a watched library leaves the watcher holding the open files it held
// before, round after round. fsnotify drops a renamed folder's own watch and
// keeps one on every entry in it, under paths that are gone; and a new
// folder asked for while fsnotify watches it itself is opened twice, one of
// the two descriptors lost to fsnotify for good. Measured on main with this
// test: 130 more open files every round of renaming ten folders of twelve
// files, one per file and one per folder.
func TestKqueueWatcherReleasesARenamedFoldersFiles(t *testing.T) {
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	albums := makeAlbums(t, filepath.Join(libDir, "Artist"), 10, 12)
	startWatcher(t, w)
	base := libraryFiles(t, libDir)

	for round := 1; round <= 3; round++ {
		for i, a := range albums {
			if err := os.Rename(a, a+"r"); err != nil {
				t.Fatal(err)
			}
			albums[i] = a + "r"
		}
		syncWatcher(t, w, libDir, round)
		// Each round adds the sync folder, and nothing else.
		if got, want := libraryFiles(t, libDir), base+round; got != want {
			t.Fatalf("after round %d of renaming 10 folders of 12 files the watcher holds %d open files on the library, want %d", round, got, want)
		}
	}
}

// TestKqueueWatcherOpensAFolderMovedInOnce: an album folder moved into the
// library costs one open file for the folder and one per file in it, never
// two for the folder. fsnotify watches a new folder itself right after it
// sends its Create, and the watcher asks for the folder after fsnotify has
// sent a later event, which it sends only once it has. Measured on main: 9
// or 10 of every 10 folders held twice. The folders are many and small
// because a watcher that asked for each folder at once only after walking
// it first loses the race far less often: with 10 folders this test caught
// it in 12 runs of 20, with 200 in 20 of 20.
func TestKqueueWatcherOpensAFolderMovedInOnce(t *testing.T) {
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	staged := makeAlbums(t, t.TempDir(), 200, 1)
	startWatcher(t, w)
	base := libraryFiles(t, libDir)

	for _, a := range staged {
		if err := os.Rename(a, filepath.Join(libDir, filepath.Base(a))); err != nil {
			t.Fatal(err)
		}
	}
	syncWatcher(t, w, libDir, 1)
	if got, want := libraryFiles(t, libDir), base+200*(1+1)+1; got != want {
		t.Fatalf("200 folders of 1 file moved in hold %d open files, want %d", got-base-1, want-base-1)
	}
}

// TestKqueueWatcherReleasesAFolderFsnotifyDroppedWithoutAnEvent: when
// fsnotify drops a folder's watch and sends nothing, the watcher still
// releases the open files fsnotify kept for the entries in it, at its next
// reconcile. fsnotify does that for a folder renamed after an entry of it
// changed and before it read the change (it reads the two as one event and
// sends neither), which this test stands in for by dropping the folder's
// Rename before the watcher sees it.
func TestKqueueWatcherReleasesAFolderFsnotifyDroppedWithoutAnEvent(t *testing.T) {
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	album := makeAlbums(t, filepath.Join(libDir, "Artist"), 1, 12)[0]
	var dropped atomic.Bool
	w.dropEventForTests = func(ev fsnotify.Event) bool {
		if ev.Has(fsnotify.Rename) && filepath.Clean(ev.Name) == album {
			dropped.Store(true)
			return true
		}
		return false
	}
	passes := make(chan struct{}, 1)
	w.afterReconcileForTests = func() {
		select {
		case passes <- struct{}{}:
		default:
		}
	}
	w.reconcileEvery = 20 * time.Millisecond
	startWatcher(t, w)
	base := libraryFiles(t, libDir)

	if err := os.Rename(album, album+"r"); err != nil {
		t.Fatal(err)
	}
	syncWatcher(t, w, libDir, 1)
	if !dropped.Load() {
		t.Fatal("fsnotify sent no Rename for the renamed folder, so there was nothing to drop")
	}
	// Two passes that end after the sync: the second starts after it, and
	// every event before the sync was handled before it.
	for i := 0; i < 2; i++ {
		select {
		case <-passes:
		case <-watchGiveUp(t, watchWaitReserve):
			t.Fatal("the watcher never reconciled its watches")
		}
	}
	// The renamed folder and its 12 files in place of the old ones, and the
	// sync folder.
	if got, want := libraryFiles(t, libDir), base+1; got != want {
		t.Fatalf("after a rename fsnotify never sent and a reconcile, the watcher holds %d open files on the library, want %d", got, want)
	}
}

// TestKqueueWatcherStopsWhenFilesTakeItPastItsBudget: files dropped into a
// watched folder, one open file each, take the watcher past its budget, and
// it releases every watch, says so once, and holds nothing; and the scan the
// last drop asked for still runs, so the drop reaches the manifest: Run
// waits for its context after the stop rather than returning, which would
// have cancelled that scan.
func TestKqueueWatcherStopsWhenFilesTakeItPastItsBudget(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, "Music", 50*time.Millisecond)
	album := makeAlbums(t, libDir, 1, 4)[0]
	// The root, the album and its 4 files fit; 3 more files do not.
	w.fds.budget = 8
	rec := loggingtest.Record(t)
	startWatcher(t, w)

	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(album, fmt.Sprintf("new%d.flac", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgWatcherStopped)) > 0 }, nil,
		"the watcher never stopped for files that took it past its budget")
	if lines := rec.Lines(msgWatcherStopped); len(lines) != 1 {
		t.Errorf("the watcher logged %q %d times, want once", msgWatcherStopped, len(lines))
	}
	if got := w.w.WatchList(); len(got) != 0 {
		t.Errorf("the watcher still watches %q", got)
	}
	waitForTrack(t, store, "a scan the drops asked for before the watcher stopped never ran")
}

// kqueueChildEnv names the scenario a child of
// TestKqueueWatcherLeavesTheProcessOpenFiles runs.
const kqueueChildEnv = "BRIDGE_B212_KQUEUE_CHILD"

// TestKqueueWatcherLeavesTheProcessOpenFiles: whatever the library does to
// the watcher's open files, the process can still open files. Each scenario
// runs in a child of this test that lowers its own open-file limit to 159
// (Go raised it to kern.maxfilesperproc at start, half of which no test
// library reaches), so the watcher's budget is 79. Measured on main, where
// the watcher had no budget, each child could not open a file once the
// watcher had run: the watcher had taken every descriptor the process had
// left, and kept them.
func TestKqueueWatcherLeavesTheProcessOpenFiles(t *testing.T) {
	if scenario := os.Getenv(kqueueChildEnv); scenario != "" {
		runKqueueChild(t, scenario)
		return
	}
	for _, scenario := range slices.Sorted(maps.Keys(kqueueChildScenarios)) {
		t.Run(scenario, func(t *testing.T) {
			// The child's own timeout bounds a wait that goes wrong, which
			// would otherwise run to the default ten minutes.
			cmd := exec.Command(os.Args[0], "-test.run=^TestKqueueWatcherLeavesTheProcessOpenFiles$", "-test.v", "-test.timeout=2m")
			cmd.Env = append(os.Environ(), kqueueChildEnv+"="+scenario)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the child failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "--- PASS: TestKqueueWatcherLeavesTheProcessOpenFiles") {
				t.Fatalf("the child ran no test:\n%s", out)
			}
		})
	}
}

// kqueueChildScenarios are the libraries the children put the watcher
// through. Each makes its library, runs the watcher over it, and answers
// the warning the watcher must log, once.
var kqueueChildScenarios = map[string]func(t *testing.T, libDir string, w *Watcher, rec *loggingtest.Recorder) string{
	"past-the-budget-at-start": childPastTheBudgetAtStart,
	"grows-past-the-budget":    childGrowsPastTheBudget,
	"runs-out-of-files":        childRunsOutOfFiles,
}

// runKqueueChild runs one scenario of TestKqueueWatcherLeavesTheProcessOpenFiles
// in the child: the watcher must say why it holds nothing, once, and the
// process must be able to open files afterwards.
func runKqueueChild(t *testing.T, scenario string) {
	run, ok := kqueueChildScenarios[scenario]
	if !ok {
		t.Fatalf("no scenario %q", scenario)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: 159, Max: 160}); err != nil {
		t.Fatalf("lowering the open-file limit: %v", err)
	}
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	if w.fds == nil || w.fds.budget != 79 {
		t.Fatalf("the watcher's budget under a limit of 159 is %+v, want 79", w.fds)
	}
	rec := loggingtest.Record(t)
	msg := run(t, libDir, w, rec)

	if lines := rec.Lines(msg); len(lines) != 1 {
		t.Errorf("the watcher logged %q %d times, want once: %q", msg, len(lines), rec.All())
	} else {
		t.Log(lines[0])
	}
	if got := w.w.WatchList(); len(got) != 0 {
		t.Errorf("the watcher still watches %d folders", len(got))
	}
	for i := 0; i < 10; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("after the watcher ran, this process could not open a file: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
	}
}

// childPastTheBudgetAtStart: 2 + 10 + 150 open files, past the budget and
// past what the process has left, so the watcher stays off.
func childPastTheBudgetAtStart(t *testing.T, libDir string, w *Watcher, _ *loggingtest.Recorder) string {
	makeAlbums(t, filepath.Join(libDir, "Artist"), 10, 15)
	runUntilItReturns(t, w)
	return msgWatcherStaysOff
}

// childGrowsPastTheBudget: 2 + 2 + 30 open files, within the budget; then a
// folder of 150 moved in, past the budget and past what the process has
// left, so the watcher stops.
func childGrowsPastTheBudget(t *testing.T, libDir string, w *Watcher, rec *loggingtest.Recorder) string {
	makeAlbums(t, filepath.Join(libDir, "Artist"), 2, 15)
	staged := makeAlbums(t, t.TempDir(), 1, 150)
	startWatcher(t, w)
	if err := os.Rename(staged[0], filepath.Join(libDir, "Artist", "Moved")); err != nil {
		t.Fatal(err)
	}
	// Running out of files first is the fallback catching what the budget
	// should have, which stops the wait at once.
	ranOut := func() string {
		if lines := rec.Lines(msgWatcherOutOfFiles); len(lines) > 0 {
			return "the watcher ran out of open files instead: " + lines[0]
		}
		return ""
	}
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgWatcherStopped)) > 0 }, ranOut,
		"the watcher never stopped for a library grown past its budget")
	return msgWatcherStopped
}

// childRunsOutOfFiles: 2 + 2 + 40 open files, within the budget, with 20
// left to the whole process, so an Add runs out of files and the watcher
// stops.
func childRunsOutOfFiles(t *testing.T, libDir string, w *Watcher, _ *loggingtest.Recorder) string {
	makeAlbums(t, filepath.Join(libDir, "Artist"), 2, 20)
	var hoard []*os.File
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		hoard = append(hoard, f)
	}
	for _, f := range hoard[len(hoard)-20:] {
		_ = f.Close()
	}
	t.Cleanup(func() {
		for _, f := range hoard[:len(hoard)-20] {
			_ = f.Close()
		}
	})
	runUntilItReturns(t, w)
	return msgWatcherOutOfFiles
}

// runUntilItReturns runs w and waits for Run to return, which it does at
// start when the watcher stays off. A watcher that finishes its initial walk
// instead is watching the library, which fails the test. Run gets the test's
// context, which ends before the cleanups run, the one that waits for Run
// included.
func runUntilItReturns(t *testing.T, w *Watcher) {
	t.Helper()
	walked := make(chan struct{})
	w.afterInitialWalkHookForTests = func() { close(walked) }
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(t.Context()) }()
	t.Cleanup(func() { <-done })
	select {
	case <-done:
	case <-walked:
		t.Fatal("the watcher finished its initial walk and watches the library")
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("the watcher neither stayed off nor finished its walk")
	}
}
