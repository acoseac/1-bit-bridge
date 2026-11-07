package manifest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B777: a watched root that stays blind until a restart. fsnotify says
// nothing when the folder at a watched root's path becomes another folder
// while the watch stays where it was: a volume mounted over the bare mount
// point the root was watched on, a volume unmounted and mounted again (kqueue
// keeps its watches on the old mount's revoked folders), a linked root's link
// repointed. Nor did the start look again at a root whose own watch it could
// not add. The check of the roots' own watches (checkRootWatches) compares the
// identity the root's folder had when its watch was added with the folder at
// its path now, and the start confirms each root's own watch.
//
// The tests that mount a volume are darwin's (watcher_mount_test.go: a disk
// image, hdiutil attach -mountpoint, which needs no privileges there); on
// Linux a mount needs rights the test hosts lack. Here a stand-in answers the
// identity read (identityForTests), so the check's own logic runs on every
// host, and a linked root's link repointed leaves a real stale watch: inotify,
// kqueue and ReadDirectoryChangesW all keep watching the folder the link led
// to, and nothing tells the watcher.

// identityAs stands in for the read of a root folder's identity: it answers
// os.Stat of the folder a root is pointed at, the root itself until a test
// points it elsewhere, as a volume mounted over the root's folder makes the
// root's path another folder; or, while fail is set, that error. It counts
// its reads.
type identityAs struct {
	mu    sync.Mutex
	at    map[string]string
	fail  error
	reads atomic.Int64
}

func (s *identityAs) read(root string) (os.FileInfo, error) {
	s.reads.Add(1)
	s.mu.Lock()
	p, ok := s.at[root]
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if !ok {
		p = root
	}
	return os.Stat(p)
}

// point has the identity of root read from folder from now on.
func (s *identityAs) point(root, folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = make(map[string]string)
	}
	s.at[root] = folder
}

// failWith has every read answer err from now on, or the folder's identity
// again once err is nil.
func (s *identityAs) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// noteRootWatchesForTests records each root's own watch after a test added
// its tree itself, with the identity its folder has now, as the start records
// one read before its walk, so the check compares it and finds it unchanged.
// Recorded with none, a root's watch would be added again at the next check,
// its identity taken then (takeRootIdentity).
func noteRootWatchesForTests(w *Watcher, roots ...string) {
	for _, root := range roots {
		id, _ := w.rootIdentity(root)
		w.noteRootWatch(root, id, w.watchSet())
	}
}

// rootChecks counts the checks of the roots' own watches
// (afterRootCheckForTests), so a test can let several pass.
type rootChecks struct{ n atomic.Int64 }

func (c *rootChecks) hook() { c.n.Add(1) }

// pass waits until n more checks have run.
func (c *rootChecks) pass(t *testing.T, n int64) {
	t.Helper()
	from := c.n.Load()
	watchWaitUntil(t, func() bool { return c.n.Load() >= from+n }, nil,
		fmt.Sprintf("the check of the roots' own watches did not run %d more times", n))
}

// countScans counts the debounced scans w dispatches.
func countScans(w *Watcher) *atomic.Int64 {
	var n atomic.Int64
	w.afterDispatchHookForTests = func() { n.Add(1) }
	return &n
}

// waitForRow waits until the store holds rel's row, and fails with msg when the
// wait gives up.
func waitForRow(t *testing.T, store *Store, rel, msg string) {
	t.Helper()
	watchWaitUntil(t, func() bool {
		got, _ := store.GetTrack(context.Background(), rel)
		return got != nil
	}, nil, msg)
}

// repoint points the link at to in one step, a new link renamed over it, as
// a remount or a link repointed puts another folder at a root's path.
func repoint(t *testing.T, link, to string) {
	t.Helper()
	next := link + ".next"
	if err := os.Symlink(to, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatal(err)
	}
}

// linkedRootFixture is a library root that is a link to the folder a volume is
// mounted at (first), indexed while it led there, with another copy of the
// volume's tree (again) for a remount to put at the root's path and an empty
// folder (bare) for an unmount to leave there, and a watcher over it that has
// not started.
type linkedRootFixture struct {
	root, first, again, bare string
	store                    *Store
	w                        *Watcher
}

func newLinkedRootFixture(t *testing.T, debounce time.Duration) *linkedRootFixture {
	t.Helper()
	dir := t.TempDir()
	f := &linkedRootFixture{
		root:  filepath.Join(dir, "music"),
		first: filepath.Join(dir, "first"),
		again: filepath.Join(dir, "again"),
		bare:  filepath.Join(dir, "bare"),
	}
	writeTracks(t, filepath.Join(f.first, filepath.FromSlash(lateMountTrack)), filepath.Join(f.again, filepath.FromSlash(lateMountTrack)))
	if err := os.Mkdir(f.bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.first, f.root); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
	f.store = openStoreIn(t, dir)
	scanner := NewScanner([]string{f.root}, f.store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, f.store, lateMountTrack)
	w, err := NewWatcher(scanner, debounce)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	f.w = w
	return f
}

// hooks returns the roots the watcher holds and the ones it watches again.
func (f *linkedRootFixture) hooks() (held, resumed <-chan string) {
	h, r := make(chan string, 8), make(chan string, 8)
	f.w.afterHoldForTests = func(root string) { h <- root }
	f.w.afterResumeForTests = func(root string) { r <- root }
	return h, r
}

// TestARootsIdentityIsTheFolderAtItsPathWhenItIsRead: the identity the
// watcher records is of the folder at the root's path when it is read, never
// of whatever folder the path names when it is first compared. Windows'
// os.Stat of a plain folder keeps its path and reads the folder's file ID only
// when os.SameFile first asks, so an identity taken before a walk would be of
// the folder there after it, the order B777 depends on lost; elsewhere the
// stat's device and inode are the identity already. Red on Windows alone
// without the read's own comparison (rootIdentity).
func TestARootsIdentityIsTheFolderAtItsPathWhenItIsRead(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(NewScanner([]string{root}, openStoreIn(t, dir), ""), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.w.Close() })
	was, err := w.rootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	// Another folder put at the path, as a mount or a repointed link puts one.
	if err := os.Rename(root, filepath.Join(dir, "was")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(was, now) {
		t.Error("the identity read before another folder was put at the root's path is that folder's")
	}
}

// TestTheWatcherWatchesAgainARootWhoseFolderIsNoLongerTheOneItWatches pins the
// check's comparison (B777 item 1): a root watched while its path was one
// folder, whose path is another folder now (a volume mounted over the bare
// mount point it was watched on), is looked at again and watched where it is
// now, with a scan; once, for one change; and again for a later one, since
// watching it again recorded the folder it watched then. The identity read is
// a stand-in, so the comparison runs on every host.
func TestTheWatcherWatchesAgainARootWhoseFolderIsNoLongerTheOneItWatches(t *testing.T) {
	f := newLateMountFixture(t)
	ids := &identityAs{}
	checks := &rootChecks{}
	f.w.identityForTests = ids.read
	f.w.afterRootCheckForTests = checks.hook
	scans := countScans(f.w)
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	checks.pass(t, 3)
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 0 {
		t.Fatalf("premise: the watcher watched a root again before its folder changed:\n%v", got)
	}

	// As a volume mounted over the folder the root's watch was added on.
	ids.point(f.root, f.volume)
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgWatchRootReplaced)) > 0 }, nil,
		"the watcher never looked again at a root whose path is another folder than the one its watch was added on")
	watchWaitUntil(t, func() bool { return scans.Load() > 0 }, nil, "the watcher asked for no scan of the root it watched again")
	checks.pass(t, 5)
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 1 {
		t.Errorf("the watcher watched the root again %d times for one change of its folder, want once:\n%v", len(got), got)
	}

	// A later change is seen too.
	ids.point(f.root, t.TempDir())
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgWatchRootReplaced)) > 1 }, nil,
		"the watcher never looked again at the root's second change of folder: watching it again kept the first folder's identity")
	f.mustHold(t, lateMountTrack)
}

// TestTheWatcherAddsARootsWatchAgainWhenItsFolderChangedWhileTheWatchWasAdded
// pins the order (B777 item 1): the identity is taken before the root's watch
// is added and read again after, and where they differ the watch is added
// again. Taken once the walks are done, it would be the folder at the path
// then, which a volume mounted as the watch was added makes the mounted one,
// recorded against a watch on the folder it covers: the two compare equal from
// then on, and the root stays blind. Here the stand-in's folder changes as the
// root's own Add is made.
func TestTheWatcherAddsARootsWatchAgainWhenItsFolderChangedWhileTheWatchWasAdded(t *testing.T) {
	f := newLateMountFixture(t)
	ids := &identityAs{}
	checks := &rootChecks{}
	f.w.identityForTests = ids.read
	f.w.afterRootCheckForTests = checks.hook
	var rootAdds atomic.Int64
	f.w.addForTests = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(f.root) && rootAdds.Add(1) == 1 {
			ids.point(f.root, f.volume)
		}
		return nil
	}
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	if n := rootAdds.Load(); n != 2 {
		t.Fatalf("the root's own watch was added %d time(s) at the start, want twice: the folder at its path "+
			"changed while its watch was added, so that watch may be on the folder that was there", n)
	}
	checks.pass(t, 5)
	if n := rootAdds.Load(); n != 2 {
		t.Errorf("the root's own watch was added %d times, want twice: the second watch is on the folder at its path, and nothing changed since", n)
	}
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 1 {
		t.Errorf("the watcher said %d times that it watched the root again, want once:\n%v", len(got), got)
	}
}

// TestTheWatcherWatchesALinkedRootAgainWhereItsLinkLeadsNow: a linked root's
// link repointed leaves every watch on the folder it led to, and nothing tells
// the watcher (B777 item 1, with no stand-in: the watch is stale on every
// platform). The root is watched again where it leads now, and a file written
// there reaches the manifest.
func TestTheWatcherWatchesALinkedRootAgainWhereItsLinkLeadsNow(t *testing.T) {
	f := newLinkedRootFixture(t, 50*time.Millisecond)
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	repoint(t, f.root, f.again)
	writeTracks(t, filepath.Join(f.again, "Artist", "Album", "02.flac"))
	waitForRow(t, f.store, "Artist/Album/02.flac", "a file written where the root's link leads now never reached the manifest")
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 1 {
		t.Errorf("the watcher said %d times that it watched the root again, want once:\n%v", len(got), got)
	}
	mustHoldRows(t, f.store, lateMountTrack)
}

// TestTheWatcherWatchesARootRemountedInsideTheDebounceOfItsResume (B777 item
// 3): a root held while its volume was away is watched again once it is
// mounted (resumeRoot), with a scan; the volume is unmounted and mounted again
// before that scan's debounce runs out, so no look ever finds the root away,
// and the scan reads the volume as it is mounted now. The watches the resume
// added are on the first mount. A file written after the scan is picked up,
// because the check finds the root's path another folder than the one its
// watch was added on. The volume's mounts are folders a link leads to, here,
// for every host; watcher_mount_test.go remounts a disk image.
func TestTheWatcherWatchesARootRemountedInsideTheDebounceOfItsResume(t *testing.T) {
	f := newLinkedRootFixture(t, 300*time.Millisecond)
	repoint(t, f.root, f.bare)
	mustReadAsNotMounted(t, f.w.scanner, f.root)
	held, resumed := f.hooks()
	scans := countScans(f.w)
	startWatcher(t, f.w)
	waitForRoot(t, held, "the watcher never held a root whose volume is away")
	repoint(t, f.root, f.first)
	waitForRoot(t, resumed, "the watcher never watched the root again once its volume was mounted")
	repoint(t, f.root, f.bare)
	repoint(t, f.root, f.again)
	watchWaitUntil(t, func() bool { return scans.Load() > 0 }, nil, "the scan the resume asked for never ran")
	writeTracks(t, filepath.Join(f.again, "Artist", "Album", "02.flac"))
	waitForRow(t, f.store, "Artist/Album/02.flac",
		"a file written on the volume mounted again inside the debounce of the resume's scan never reached the manifest")
	mustHoldRows(t, f.store, lateMountTrack)
}

// TestAVolumeAwayForSeveralChecksIsHeldWithNoScanPerCheck (B777 item 1): a
// root whose path is found another folder by the check, and that reads as a
// volume not mounted, goes to the hold, as holdAbsentRoots holds one, never
// looked at again and watched again at every check for as long as the volume
// is away: ten checks pass with no scan, no watch added again and one hold.
// Once the volume is back, the hold's retry watches it again.
func TestAVolumeAwayForSeveralChecksIsHeldWithNoScanPerCheck(t *testing.T) {
	f := newLinkedRootFixture(t, 50*time.Millisecond)
	checks := &rootChecks{}
	f.w.afterRootCheckForTests = checks.hook
	scans := countScans(f.w)
	held, resumed := f.hooks()
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	// Unmounted: the root's path the bare mount point, its watch still on the
	// volume's folder, and nothing said.
	repoint(t, f.root, f.bare)
	waitForRoot(t, held, "the watcher never held a root whose path is now the bare mount point")
	base := scans.Load()
	checks.pass(t, 10)
	if n := scans.Load() - base; n != 0 {
		t.Errorf("the watcher dispatched %d scan(s) over ten checks while it held the root", n)
	}
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 0 {
		t.Errorf("the watcher watched a root that reads as a volume not mounted again:\n%v", got)
	}
	if got := rec.Failures(msgWatchRootHeld); len(got) != 1 {
		t.Errorf("the watcher said %d times that it held the root, want once:\n%v", len(got), got)
	}
	repoint(t, f.root, f.first)
	waitForRoot(t, resumed, "the watcher never watched the root again once its volume was back")
	writeTracks(t, filepath.Join(f.first, "Artist", "Album", "02.flac"))
	waitForRow(t, f.store, "Artist/Album/02.flac", "a file written once the volume was back never reached the manifest")
}

// TestAFolderWhoseIdentityCannotBeReadIsNeitherWatchedAgainNorScanned (B777
// item 1): a read of the root's identity that fails is no mismatch. The root
// is held where it reads as a volume not mounted, and is otherwise left as it
// is: ten checks pass with no watch added again, no scan and no hold, and the
// root's watch still sees a file dropped once the read answers again.
func TestAFolderWhoseIdentityCannotBeReadIsNeitherWatchedAgainNorScanned(t *testing.T) {
	f := newLateMountFixture(t)
	ids := &identityAs{}
	checks := &rootChecks{}
	f.w.identityForTests = ids.read
	f.w.afterRootCheckForTests = checks.hook
	scans := countScans(f.w)
	held, _ := f.hooks()
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	ids.failWith(&fs.PathError{Op: "stat", Path: f.root, Err: syscall.EIO})
	base := scans.Load()
	checks.pass(t, 10)
	if n := scans.Load() - base; n != 0 {
		t.Errorf("the watcher dispatched %d scan(s) over ten checks whose identity read failed", n)
	}
	for _, msg := range []string{msgWatchRootReplaced, msgWatchRootRewatched} {
		if got := rec.Lines(msg); len(got) != 0 {
			t.Errorf("an identity read that failed was taken for a change:\n%v", got)
		}
	}
	select {
	case r := <-held:
		t.Errorf("the watcher held %s, which reads as it is, for an identity read that failed", r)
	default:
	}
	ids.failWith(nil)
	f.drop(t, "02.flac", "a file dropped into the root once its identity read again never reached the manifest")
}

// TestAnOrdinaryRenameDoesNotLoopTheWatcher (B777 item 1): a folder renamed
// inside a watched root leaves the root's folder as it was, and a root put back
// by a rename is watched again once and compared with the folder it is now,
// not the one it was: ten checks after each, nothing is watched again.
func TestAnOrdinaryRenameDoesNotLoopTheWatcher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to rename a folder while the watcher's ReadDirectoryChangesW handles are open under it (Access is denied)")
	}
	t.Run("a folder renamed inside the root", func(t *testing.T) {
		f := newLateMountFixture(t)
		checks := &rootChecks{}
		f.w.afterRootCheckForTests = checks.hook
		rec := loggingtest.Record(t)
		startWatcher(t, f.w)
		if err := os.Rename(filepath.Join(f.root, "Artist", "Album"), filepath.Join(f.root, "Artist", "Album Renamed")); err != nil {
			t.Fatal(err)
		}
		waitForRow(t, f.store, "Artist/Album Renamed/01.flac", "the renamed folder's track never reached the manifest")
		checks.pass(t, 10)
		for _, msg := range []string{msgWatchRootReplaced, msgWatchRootRewatched} {
			if got := rec.Lines(msg); len(got) != 0 {
				t.Errorf("a folder renamed inside the root had the root watched again:\n%v", got)
			}
		}
	})
	t.Run("the root put back by a rename", func(t *testing.T) {
		f := newLateMountFixture(t)
		checks := &rootChecks{}
		f.w.afterRootCheckForTests = checks.hook
		held, resumed := f.hooks()
		rec := loggingtest.Record(t)
		startWatcher(t, f.w)
		f.unmount(t, false)
		waitForRoot(t, held, "the watcher never held a root that went")
		f.mount(t)
		waitForRoot(t, resumed, "the watcher never watched the root again once it was back")
		checks.pass(t, 10)
		for _, msg := range []string{msgWatchRootReplaced, msgWatchRootRewatched} {
			if got := rec.Lines(msg); len(got) != 0 {
				t.Errorf("the root put back by a rename was watched again at its checks:\n%v", got)
			}
		}
		f.drop(t, "02.flac", "a file dropped into the root put back never reached the manifest")
	})
}

// TestTheWatcherHoldsARootThatWentBetweenItsLookAndItsWalk (B777 item 2): a
// root that reads as it is when the start looks at it (holdAbsentRoots) and is
// gone when its walk adds its own watch was logged as a walk that failed,
// neither held nor recorded, so nothing looked at it again until a restart. The
// walk is parked at the root's own Add, the actual boundary, while the root
// goes; no mount is needed, since a path that is not there involves none. The
// root is held, and watched again once it is back.
func TestTheWatcherHoldsARootThatWentBetweenItsLookAndItsWalk(t *testing.T) {
	f := newLateMountFixture(t)
	parked, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.w.addForTests = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(f.root) {
			once.Do(func() {
				close(parked)
				<-released
			})
		}
		return nil
	}
	moved := make(chan error, 1)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		select {
		case <-parked:
		case <-done:
			return
		}
		moved <- os.Rename(f.root, f.gone)
		close(released)
	}()
	held, resumed := f.hooks()
	startWatcher(t, f.w)
	// The walk waited at the root's own Add for the move, so its answer is in.
	select {
	case err := <-moved:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("premise: the start's walk never reached the root's own Add")
	}
	if got := waitForRoot(t, held, "the watcher never held a root that went between its look and its walk"); got != f.root {
		t.Errorf("the watcher held %q, want %q", got, f.root)
	}
	if err := os.Rename(f.gone, f.root); err != nil {
		t.Fatal(err)
	}
	waitForRoot(t, resumed, "the watcher never watched the root again once it was back")
	f.drop(t, "02.flac", "a file dropped into the root once it was back never reached the manifest")
	f.mustHold(t, lateMountTrack)
}

// TestARootWithAFolderItCannotReadIsWatchedAndScanned (B777 item 2): an Add
// that fails below a root that is there (a folder at mode 000, which inotify
// refuses to watch and the walk cannot list) leaves the root's own watch, and
// the root is watched and scanned as before: neither held nor said to have
// failed. The folder sorts ahead of the album, so a walk that stopped at it
// would leave the album unwatched.
func TestARootWithAFolderItCannotReadIsWatchedAndScanned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a folder's mode bits refuse no read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder at mode 000")
	}
	f := newLateMountFixture(t)
	locked := filepath.Join(f.root, "A-Locked")
	writeTracks(t, filepath.Join(locked, "Album", "01.flac"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	held, _ := f.hooks()
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	f.drop(t, "02.flac", "a file dropped into a root holding a folder it cannot read never reached the manifest")
	select {
	case r := <-held:
		t.Errorf("the watcher held %s, a root that is there", r)
	default:
	}
	if got := rec.Failures(msgInitialWatchFailed); len(got) != 0 {
		t.Errorf("a folder the walk cannot read was said to fail the root's watch:\n%v", got)
	}
	f.mustHold(t, lateMountTrack)
}
