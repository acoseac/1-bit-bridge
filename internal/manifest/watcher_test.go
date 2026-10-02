package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// newWatcherFixture stands up a library directory, a store, and a
// Watcher over it. Extracted because four tests in this file (plus the
// dot-named-root case) were carrying byte-identical setup.
//
// `libName` is the library root's BASENAME — the dot-named-root test
// needs to control it, since that is precisely what it exercises.
func newWatcherFixture(t *testing.T, libName string, debounce time.Duration) (libDir string, store *Store, w *Watcher) {
	t.Helper()
	dir := t.TempDir()
	libDir = filepath.Join(dir, libName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	w, err = NewWatcher(NewScanner([]string{libDir}, store, ""), debounce)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	return libDir, store, w
}

// A test waits on the watcher for an EVENT (its initial walk, a watch it
// registered, a row its scan wrote) and gives up only at the test binary's
// deadline, less watchWaitReserve. Each wait covers the kernel's event
// delivery and a subtree scan's SQLite writes, and neither has a bound a
// starved host keeps to: the tests here slept 100 or 150 ms for the walk and
// gave the row 3 s, and on a macOS CI runner a file dropped into a linked
// root never reached the manifest (backlog B104). On a Linux host starved by
// a CPU hog the initial walk ended up to 288 ms after the watcher started,
// and 11 of 110 runs of that test failed as CI had. A wait that reaches the
// deadline means the event never came; run with a shorter -timeout to see
// that sooner. It is B63's rule for serve tests, in cmd/bridge.

// watchWaitReserve is the part of the test binary's deadline a wait on the
// watcher leaves unspent: the watcher's join, the store's close and the
// report a failure runs next.
const watchWaitReserve = 30 * time.Second

// watchDrainReserve is the part the watcher's join at cleanup leaves
// unspent. Smaller than watchWaitReserve, so a join after a wait that gave
// up still sees the watcher out.
const watchDrainReserve = 10 * time.Second

// watchGiveUp fires when a wait on the watcher gives up: the test binary's
// deadline less reserve, or less half of what is left when that is less, so
// a short -timeout still leaves the wait time to wait (CodeRabbit on #1115:
// under -timeout 30s a whole reserve put every give-up in the past). It
// fires at once if the deadline has passed, and never when the test binary
// runs with no deadline.
func watchGiveUp(t *testing.T, reserve time.Duration) <-chan time.Time {
	deadline, ok := t.Deadline()
	if !ok {
		return nil
	}
	left := time.Until(deadline)
	return time.After(left - min(reserve, left/2))
}

// watchWaitUntil polls ready until it holds, and fails the test with msg
// when the wait gives up (watchGiveUp). stop, when not nil, ends the wait
// sooner with the failure it names: an event that says the one awaited is
// not coming.
func watchWaitUntil(t *testing.T, ready func() bool, stop func() string, msg string) {
	t.Helper()
	giveUp := watchGiveUp(t, watchWaitReserve)
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for !ready() {
		if stop != nil {
			if why := stop(); why != "" {
				t.Fatalf("%s: %s", msg, why)
			}
		}
		select {
		case <-giveUp:
			t.Fatal(msg)
		case <-poll.C:
		}
	}
}

// startWatcher runs w, joins it at cleanup, and returns once its initial
// walk has registered every watch (afterInitialWalkHookForTests): a file
// created before then is one no watch sees. The join is registered after
// the fixture's store close, so it runs first: the watcher stops before the
// store it writes to is closed.
func startWatcher(t *testing.T, w *Watcher) {
	t.Helper()
	walked := make(chan struct{})
	w.afterInitialWalkHookForTests = func() { close(walked) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-watchGiveUp(t, watchDrainReserve):
			t.Error("the watcher did not stop on cancel")
		}
	})
	select {
	case <-walked:
	case <-done:
		t.Fatal("the watcher returned before its initial walk finished")
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("the watcher's initial walk never finished")
	}
}

// watched reports whether w holds a watch on a directory named base, in
// whichever spelling it registered it: a linked root's own directory and
// what appears below it are watched where the root resolves to (on Windows,
// a junction as configured).
func watched(w *Watcher, base string) bool {
	for _, p := range w.w.WatchList() {
		if filepath.Base(p) == base {
			return true
		}
	}
	return false
}

// waitForTrack waits until a track appears in the manifest, or fails with
// msg when the wait gives up.
func waitForTrack(t *testing.T, store *Store, msg string) {
	t.Helper()
	watchWaitUntil(t, func() bool {
		got, _ := store.ListTracks(context.Background(), nil)
		return len(got) > 0
	}, nil, msg)
}

// TestWatcherDebounce drops a file into a watched directory and
// asserts ScanSubtree fires and the new track lands in the manifest.
// End-to-end check of the watch → debounce → ScanSubtree →
// UpsertTrackBatch path, with a short debounce window (50 ms).
func TestWatcherDebounce(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, "Music", 50*time.Millisecond)
	startWatcher(t, w)

	target := filepath.Join(libDir, "test.flac")
	// Write a placeholder — the scanner's Extract may fail to
	// parse it but the upsert path uses fillFromPath fallbacks
	// so a Track row still lands.
	if err := os.WriteFile(target, []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForTrack(t, store, "expected at least one track in manifest within deadline; got 0")
}

// TestWatcherShutdownDrainsInflightScan is the B8 regression guard: Run's
// shutdown path (cancel → deferred cancelAllPending + waitForInflightScans)
// must return cleanly and NOT deadlock, so the caller can safely close the
// store the instant Run returns. A file drop arms + fires a debounced
// ScanSubtree; cancelling mid-flight exercises the new wait. Pre-fix, Run
// returned without waiting for the fired dispatch (which could then race the
// store close); the fix makes Run block on the in-flight scan — this test
// pins that the block terminates (no deadlock) and the scan's write landed.
func TestWatcherShutdownDrainsInflightScan(t *testing.T) {
	// Held open at the dispatch's tail so the cancel below provably lands
	// while a scan is still counted in flight. The previous shape slept
	// 100ms for the watches to register and 60ms for a 20ms debounce to
	// fire — which was wrong in both directions. Too short (routinely, on
	// Windows: slower fsnotify registration and ~15.6ms clock granularity)
	// and nothing had been dispatched, so the run failed with "expected
	// the in-flight scan's track to have landed". Too long — the ordinary
	// case — and the one-file scan had already FINISHED, scanWG was back
	// to zero, and the assertion passed without the drain doing anything
	// at all. So it was flaky and weak at once; this version is neither.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	libDir, store, w := newWatcherFixture(t, "Music", 20*time.Millisecond)
	// Set BEFORE Run's goroutine starts: goroutine creation is the
	// happens-before edge that makes this write visible to the dispatch
	// goroutines without a race. (A package-level var could not offer
	// that — see the field's docblock.)
	w.afterDispatchHookForTests = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = w.Run(ctx); close(runDone) }()

	// Write until a dispatch actually happens, rather than sleeping a
	// guess at how long watch registration takes. Re-writing the same
	// path is harmless and re-arms the debounce, so a write that lands
	// before the watch exists simply costs one more iteration.
	//
	// stopWriter is what makes the failure path survivable, and that is not
	// a nicety: the timeout below fires exactly when no dispatch ever
	// happened — the Windows case this test exists to report — and without
	// a second exit condition the writer would spin on `entered`, which is
	// never closed there, and the deferred `<-writerDone` would hang the
	// test binary instead of printing the failure. Closed unconditionally
	// in cleanup, so both paths drain it.
	writerDone := make(chan struct{})
	stopWriter := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-entered:
				return
			case <-stopWriter:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(libDir, "x.flac"), []byte("x"), 0o644)
			select {
			case <-entered:
				return
			case <-stopWriter:
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		// cancel here too, not just in the body: the timeout path below
		// Fatals WITHOUT having cancelled, and cleanups run LIFO, so this
		// one fires before the fixture's store.Close(). Without it Run
		// would still be watching when the store closes underneath it —
		// the very B8 shape this test is about.
		cancel()
		close(stopWriter)
		<-writerDone
		// Bounded, deliberately. If Run itself has deadlocked — the exact
		// defect these assertions detect — an unbounded wait here would
		// hang the test binary and bury the failure that was just
		// reported. Leaking a goroutine into an already-finishing process
		// is the cheaper of the two.
		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
		}
	})

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("no subtree scan was ever dispatched — the watch never registered")
	}

	// A dispatch is now parked at its tail with scanWG at 1, and the scan
	// it just ran has already written. Cancel here is what the drain has
	// to survive.
	cancel()

	// The assertion the whole test exists for: Run MUST NOT return while
	// that dispatch is outstanding. The window is a heuristic but it fails
	// SAFE — a broken drain returns in microseconds, and a slow machine
	// only makes "has not returned yet" more true, never less.
	select {
	case <-runDone:
		close(release)
		t.Fatal("Run returned while a dispatch was still in flight — " +
			"waitForInflightScans did not wait")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-runDone: // Run's defer (cancelAllPending + waitForInflightScans) completed
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return once the in-flight scan finished — " +
			"waitForInflightScans deadlocked")
	}
	// The dispatched scan completed before Run returned (Run waited for it),
	// so its UpsertTrackBatch landed against the still-open store.
	got, _ := store.ListTracks(context.Background(), nil)
	if len(got) == 0 {
		t.Error("expected the in-flight scan's track to have landed before Run returned")
	}
}

// TestWatcherIgnoresDotfiles asserts a dotfile dropped into a watched
// directory never becomes a track. Its event does dispatch a subtree scan
// (handleEvent filters by operation, not by name), and the scan skips the
// file; this said until 2026-09-29 that the event triggers no scan, which
// the watcher has never done.
//
// It waited 200 ms after the drop and counted no track, which passes
// whether or not a scan ran: after a drop that beat the initial walk (a
// 100 ms sleep stood for it) no event came at all (backlog B104). So a
// track is dropped after the dotfile, and the event is a scan that indexed
// the track having returned (afterDispatchHookForTests): that scan listed
// the directory the dotfile was already in, and every row it wrote has
// landed.
func TestWatcherIgnoresDotfiles(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, "Music", 50*time.Millisecond)
	scanned := make(chan struct{})
	var once sync.Once
	w.afterDispatchHookForTests = func() {
		if st, err := store.GetTrackStat(context.Background(), "track.flac"); err == nil && st != nil {
			once.Do(func() { close(scanned) })
		}
	}
	startWatcher(t, w)

	// `._track.flac` is the AppleDouble file macOS writes beside a copy on
	// an exFAT or SMB volume: audio-named, so only the dot keeps it out,
	// where `.DS_Store` would be kept out by its extension alone.
	for _, name := range []string{".DS_Store", "._track.flac"} {
		if err := os.WriteFile(filepath.Join(libDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(libDir, "track.flac"), []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scanned:
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("no scan indexed the track dropped beside the dotfile")
	}

	got, _ := store.ListTracks(context.Background(), nil)
	if len(got) != 1 || got[0].Path != "track.flac" {
		paths := make([]string, len(got))
		for i, tr := range got {
			paths[i] = tr.Path
		}
		t.Errorf("tracks %q after a scan over the dotfile, want only track.flac", paths)
	}
}

// TestIsWatchLimitError pins the substring matcher against the
// canonical fsnotify WATCH-budget messages. fd-exhaustion ("too many
// open files") is deliberately NOT a watch-limit error — it routes to
// isOpenFileLimitError so addTree emits the ulimit-oriented hint
// instead of the max_user_watches one.
func TestIsWatchLimitError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"inotify_add_watch: no space left on device", true},
		{"too many open files", false}, // fd-exhaustion, not a watch-limit — see isOpenFileLimitError
		{"watch limit reached", true},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isWatchLimitError(&strErr{tc.err})
		if got != tc.want {
			t.Errorf("isWatchLimitError(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestIsOpenFileLimitError pins the fd-exhaustion classifier that B9
// split out of isWatchLimitError. Only "too many open files"
// (EMFILE/ENFILE) matches; the watch-budget messages must NOT, so the
// two paths emit distinct operator hints.
func TestIsOpenFileLimitError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"pipe: too many open files", true},
		{"too many open files", true},
		{"inotify_add_watch: no space left on device", false},
		{"watch limit reached", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isOpenFileLimitError(&strErr{tc.err})
		if got != tc.want {
			t.Errorf("isOpenFileLimitError(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

// TestWatcherScheduleScanCoalescesAndCleans drives repeated scheduleScan
// calls for one dir and asserts they collapse to a single pending entry
// (the reschedule path) that the fired callback then removes — the map
// self-cleans, no leaked timers. ctx is cancelled so the callback returns
// before ScanSubtree; only the debounce-map bookkeeping is exercised.
func TestWatcherScheduleScanCoalescesAndCleans(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	scanner := NewScanner([]string{dir}, store, "")
	w, err := NewWatcher(scanner, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.w.Close() // NewWatcher allocates an fsnotify.Watcher; these tests don't Run() it.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // callback skips the scan; we only check map bookkeeping.

	const target = "/watched/dir"
	for i := 0; i < 5; i++ {
		w.scheduleScan(ctx, target)
	}
	w.mu.Lock()
	n := len(w.pending)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("pending entries = %d, want 1 (five events for one dir must coalesce)", n)
	}

	// After the debounce window the callback fires and removes the entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n = len(w.pending)
		w.mu.Unlock()
		if n == 0 {
			return // self-cleaned
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending map not cleaned after debounce; still %d entries", n)
}

// TestWatcherStaleTimerDoesNotEvictFreshEntry reproduces the debounce
// identity race: a timer fires, its callback parks on wt.mu, and while it
// is parked a FRESH entry is installed for the same dir. The stale
// callback must NOT delete the fresh entry. Pre-fix (unconditional
// delete) it did — which under an event storm let the map lose track of
// the live timer and spawn overlapping scans.
func TestWatcherStaleTimerDoesNotEvictFreshEntry(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	scanner := NewScanner([]string{dir}, store, "")
	w, err := NewWatcher(scanner, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.w.Close() // NewWatcher allocates an fsnotify.Watcher; these tests don't Run() it.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stale callback returns before ScanSubtree.

	const target = "/watched/dir"
	w.scheduleScan(ctx, target) // arms ps1 (fires in 20ms)

	// Hold the lock across the debounce window so ps1's callback fires but
	// parks on wt.mu before it can run its delete check.
	w.mu.Lock()
	ps1 := w.pending[target]
	time.Sleep(60 * time.Millisecond) // > debounce: ps1 fires, callback parks on wt.mu.

	// Install a FRESH entry for the same dir (a long timer that won't fire
	// during the test), mimicking scheduleScan's Stop()==false path.
	ps2 := &pendingScan{}
	ps2.timer = time.AfterFunc(time.Hour, func() {})
	defer ps2.timer.Stop()
	w.pending[target] = ps2
	w.mu.Unlock() // release: ps1's parked callback now proceeds.

	if ps1 == ps2 {
		t.Fatal("test setup error: ps1 and ps2 must be distinct")
	}

	// Give the stale ps1 callback time to run its delete check + return.
	time.Sleep(150 * time.Millisecond)

	w.mu.Lock()
	got := w.pending[target]
	w.mu.Unlock()
	if got != ps2 {
		t.Fatalf("fresh entry evicted by the stale timer callback (got %p, want ps2 %p); identity guard failed", got, ps2)
	}
}

// addTree must register watches for a library root whose OWN basename
// starts with a dot.
//
// filepath.WalkDir calls the callback for the root itself, so an
// unguarded ShouldSkipDir(d.Name()) returned SkipDir on entry and the
// whole root got ZERO watches. Worse, addTree then returns nil, so the
// caller's "initial watch add failed (partial coverage)" warning never
// fired either — the library silently lost instant-update coverage with
// no operator signal at all.
//
// End-to-end rather than a unit test on addTree: what matters is that a
// file dropped into the root actually reaches the manifest.
func TestWatcherWatchesDotNamedLibraryRoot(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, ".music", 50*time.Millisecond)
	startWatcher(t, w)
	// The decision itself, read once the walk is done: without it the drop
	// below waits for a row until the test's deadline.
	if !watched(w, ".music") {
		t.Fatalf("the initial walk registered no watch for a dot-named library root (watches: %q)", w.w.WatchList())
	}

	if err := os.WriteFile(filepath.Join(libDir, "dropped.flac"),
		[]byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForTrack(t, store, "a file dropped into a dot-named library root never reached the manifest")
}

// A dot-directory that appears at RUNTIME gets no carve-out — the
// exemption belongs to operator-configured roots only.
//
// handleEvent's Create branch calls addTree with the just-created
// directory as its `root`, so the old `path != root` form exempted every
// such directory from itself: a `.Trashes` / `.stversions` that showed up
// under a watched library got a watch, a later event under it dispatched
// ScanSubtree INSIDE it (whose own walker exempts the directory it was
// pointed at), and its files were indexed as
// `.Trashes/501/Album/track.flac`. The full Scan never sees those paths —
// ShouldSkipDir prunes them as descendants — so they accrued
// missing_count, were reaped three scans later, and reappeared on the
// next drop: deleted albums cycling in and out of /v1/manifest.
//
// Driven through handleEvent rather than a raw addTree call so the
// wiring (which caller passes which intent) is what's pinned. WatchList
// is the assertion because "is a watch registered here" is exactly the
// decision under test; everything downstream follows from it.
func TestWatcherRuntimeDotDirGetsNoRootExemption(t *testing.T) {
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	t.Cleanup(func() {
		w.cancelAllPending()
		_ = w.w.Close()
	})

	trash := filepath.Join(libDir, ".Trashes")
	if err := os.MkdirAll(filepath.Join(trash, "501"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The watcher sees the directory appear under an already-watched root.
	// On kqueue the new folder's watch waits for fsnotify to have watched
	// the folder itself (flushPendingAdds), which Run would ask for at the
	// next event; elsewhere the flush has nothing to do.
	w.handleEvent(context.Background(), fsnotify.Event{Name: trash, Op: fsnotify.Create})
	w.flushPendingAdds()

	for _, p := range w.w.WatchList() {
		if p == trash || strings.HasPrefix(p, trash+string(os.PathSeparator)) {
			t.Fatalf("watch registered inside a runtime-created dot-directory: %q "+
				"— events from there dispatch ScanSubtree into it and index its files", p)
		}
	}

	// Contrast, and the reason the parameter exists rather than a blanket
	// skip: the same directory AS a configured root is still watched.
	if err := w.addTree(trash, true); err != nil {
		t.Fatalf("addTree(configured root): %v", err)
	}
	var watched bool
	for _, p := range w.w.WatchList() {
		if p == trash {
			watched = true
		}
	}
	if !watched {
		t.Error("a dot-named CONFIGURED root must still be watched")
	}
}
