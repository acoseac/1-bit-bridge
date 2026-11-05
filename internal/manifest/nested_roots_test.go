package manifest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// B651's sixth round (Codex on round 5): a nested root's protection is
// decided before the walk, from the look at the nested root, so a nested
// root whose directory went with its volume is spared as much as one whose
// bare mount point is there; and every comparison of a path with a root
// folds case where the volume does.

// writeTracks writes each file, with its parents, holding "not a real flac".
func writeTracks(t *testing.T, files ...string) {
	t.Helper()
	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("not a real flac"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// openStoreIn opens a store in dir, closed when the test ends.
func openStoreIn(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// mustHoldRows fails the test unless the store holds each track row with no
// missed scan counted against it.
func mustHoldRows(t *testing.T, store *Store, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if got, _ := store.GetTrack(context.Background(), rel); got == nil {
			t.Errorf("the store does not hold %s", rel)
			continue
		}
		mustHaveMissingCount(t, store, "tracks", rel, 0)
	}
}

// moveAway renames dir aside, as an unmount takes a volume, and leaves a bare
// mount point at its path when bare.
func moveAway(t *testing.T, dir, aside string, bare bool) {
	t.Helper()
	if err := os.Rename(dir, aside); err != nil {
		t.Fatal(err)
	}
	if bare {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// goneNested is a library root, music, with a root of its own nested below a
// volume's mount point inside it (music/usb/library, the volume at music/usb),
// indexed while the volume was there; then the volume goes, which takes the
// nested root's whole directory with it, so no walk of the outer root reaches
// the nested root's folder. A row is reaped at its third missed scan.
type goneNested struct {
	outer, usb string
	store      *Store
	scanner    *Scanner
}

// goneNestedRows is the rows the volume's tracks have: the outer root's, and
// the nested root's own.
var goneNestedRows = []string{"music/Artist/Album/01.flac", "music/usb/library/Band/Record/01.flac", "library/Band/Record/01.flac"}

func newGoneNested(t *testing.T) *goneNested {
	t.Helper()
	dir := t.TempDir()
	n := &goneNested{outer: filepath.Join(dir, "music")}
	n.usb = filepath.Join(n.outer, "usb")
	inner := filepath.Join(n.usb, "library")
	writeTracks(t, filepath.Join(n.outer, "Artist", "Album", "01.flac"), filepath.Join(inner, "Band", "Record", "01.flac"))
	n.store = openStoreIn(t, dir)
	n.scanner = NewScanner([]string{n.outer, inner}, n.store, "")
	n.scanner.SetDeleteThreshold(3)
	if _, err := n.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, n.store, goneNestedRows...)
	moveAway(t, n.usb, filepath.Join(dir, "gone"), true)
	return n
}

// mustHoldTheVolumesRows fails the test unless every row the volume's tracks
// have is there, uncounted, and so is the nested root's folder under the
// outer root.
func (n *goneNested) mustHoldTheVolumesRows(t *testing.T) {
	t.Helper()
	mustHoldRows(t, n.store, goneNestedRows...)
	mustHaveMissingCount(t, n.store, "folders", "music/usb/library/Band/Record", 0)
}

// TestASubtreeScanSparesANestedRootWhoseDirectoryWent: subtree scans of the
// outer root, and of the bare mount point the nested root was under, past the
// reap threshold, spare every row the outer root holds under a nested root
// whose whole directory went with its volume: the protection is recorded
// before the walk, from the look at the nested root, whether or not the walk
// reaches its folder (Codex on round 5).
func TestASubtreeScanSparesANestedRootWhoseDirectoryWent(t *testing.T) {
	n := newGoneNested(t)
	ctx := context.Background()
	for range 4 {
		if _, err := n.scanner.ScanSubtree(ctx, n.outer); err != nil {
			t.Fatal(err)
		}
		if _, err := n.scanner.ScanSubtree(ctx, n.usb); err != nil {
			t.Fatal(err)
		}
	}
	n.mustHoldTheVolumesRows(t)
}

// TestAFullScanSparesANestedRootWhoseDirectoryWent: full scans, past the reap
// threshold, spare every row the outer root holds under a nested root whose
// whole directory went with its volume (Codex on round 5).
func TestAFullScanSparesANestedRootWhoseDirectoryWent(t *testing.T) {
	n := newGoneNested(t)
	for range 4 {
		if _, err := n.scanner.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	n.mustHoldTheVolumesRows(t)
}

// TestTheWatcherHoldsANestedRootWhoseDirectoryWentAndWatchesItOnceItIsBack:
// a nested root whose directory went with its volume is held at the start
// and nothing at or under it is watched (Argon on the brief); when the volume
// comes back, the walk of the folder it comes back in passes over the held
// root (heldBelow), and the root is watched again once a look finds it.
func TestTheWatcherHoldsANestedRootWhoseDirectoryWentAndWatchesItOnceItIsBack(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	usb := filepath.Join(outer, "usb")
	inner := filepath.Join(usb, "library")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(inner, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{outer, inner}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(dir, "gone")
	moveAway(t, usb, aside, true)
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	held, resumed := make(chan string, 8), make(chan string, 8)
	w.afterHoldForTests = func(root string) { held <- root }
	w.afterResumeForTests = func(root string) { resumed <- root }
	startWatcher(t, w)
	if got := waitForRoot(t, held, "the watcher never held a nested root whose directory went"); got != inner {
		t.Errorf("the watcher held %q, want %q", got, inner)
	}
	for _, p := range w.w.WatchList() {
		if pathAtOrUnder(filepath.Clean(p), inner) {
			t.Errorf("the watcher watches %s, at or under %s, a root it holds", p, inner)
		}
	}
	if err := os.Remove(usb); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, usb); err != nil {
		t.Fatal(err)
	}
	if got := waitForRoot(t, resumed, "the watcher never watched the nested root again once its volume was back"); got != inner {
		t.Errorf("the watcher watched %q again, want %q", got, inner)
	}
	mustHoldRows(t, store, "music/usb/library/Band/Record/01.flac", "library/Band/Record/01.flac")
}

// foldedNested is a library root, music, with a root of its own configured as
// music/USB while its volume spells the folder usb; indexed while the volume
// was there, through the outer root's walk (on a volume that folds case, the
// nested root's own walk indexes it too).
type foldedNested struct {
	dir, outer, disk, configured string
	store                        *Store
	scanner                      *Scanner
	// bare is the time the nested root's bare mount point is stamped with
	// (takeAway); zero while there is none.
	bare time.Time
}

func newFoldedNested(t *testing.T) *foldedNested {
	t.Helper()
	dir := t.TempDir()
	n := &foldedNested{dir: dir, outer: filepath.Join(dir, "music")}
	n.disk, n.configured = filepath.Join(n.outer, "usb"), filepath.Join(n.outer, "USB")
	writeTracks(t, filepath.Join(n.outer, "Artist", "Album", "01.flac"), filepath.Join(n.disk, "Band", "Record", "01.flac"))
	n.store = openStoreIn(t, dir)
	n.scanner = NewScanner([]string{n.outer, n.configured}, n.store, "")
	if _, err := n.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, n.store, "music/Artist/Album/01.flac", "music/usb/Band/Record/01.flac")
	return n
}

// foldCase has the scanner answer that every folder folds case: a test stands
// in for macOS's default volume or Windows', which this host's may not be.
func (n *foldedNested) foldCase() { answerFolding(n.scanner, fsutil.CaseInsensitive) }

// answerFolding has the scanner stand in for a volume every folder of which
// answers f for how it compares names (simVolume): CaseInsensitive, a volume
// that folds case, whose lookups find a name in another case.
func answerFolding(s *Scanner, f fsutil.CaseFolding) {
	simVolume{folding: func(string) fsutil.CaseFolding { return f }}.install(s)
}

// simVolume stands in for the volume a test's tree is on, through both of the
// scanner's seams at once, so that the two describe one volume (B651's ninth
// round). foldingHook answers how a folder compares the names in it (folding,
// the probe's question); lookupHook answers what a lookup of a name in a
// folder finds (Scanner.lstat, for caseLook.sameName and mayBeOneName): the
// entry its name spells byte for byte, else, in a folder folding answers
// CaseInsensitive for, the entry whose name matches it under fold, or, in a
// folder whose lookups refused names, a permission refused. The entries are
// the test's tree on this host's disk, read from its listing: this host's own
// lookups decide nothing, so its disk, which folds case on macOS and Windows,
// finds no spelling the stand-in would not. A name is looked up in its folder
// as the folder is spelled, which every fixture spells as the disk does.
type simVolume struct {
	folding func(dir string) fsutil.CaseFolding
	// fold matches two names in a folder that folds case: strings.EqualFold
	// where nil.
	fold func(a, b string) bool
	// refused holds the folders whose lookups a permission refuses.
	refused map[string]bool
}

// install has the scanner answer through v, for both questions.
func (v simVolume) install(s *Scanner) {
	s.foldingHook = v.folding
	s.lookupHook = v.lookup
}

func (v simVolume) lookup(name string) (fs.FileInfo, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	if v.refused[dir] {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EACCES}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() == base {
			return os.Lstat(filepath.Join(dir, e.Name()))
		}
	}
	fold := v.fold
	if fold == nil {
		fold = strings.EqualFold
	}
	if v.folding(dir) == fsutil.CaseInsensitive {
		for _, e := range entries {
			if fold(e.Name(), base) {
				return os.Lstat(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
}

// asciiFold matches two names that are the same with the case of their ASCII
// letters alone folded: a volume that folds ASCII case and no other.
func asciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// takeAway takes the nested root's volume away, leaving its bare mount point
// when bare, stamped an hour before the tracks so that a walk into it shows in
// its folder row; has the scanner answer folding for how every folder compares
// names (answerFolding: CaseInsensitive stands in for a volume that folds
// case, CaseUnknown for one whose folders cannot tell); and has a row reaped
// at its third missed scan. On this host's case-sensitive disk the configured
// spelling is no folder at all, so the nested root reads as not mounted, as
// its bare mount point does where the volume folds case.
func (n *foldedNested) takeAway(t *testing.T, bare bool, folding fsutil.CaseFolding) {
	t.Helper()
	moveAway(t, n.disk, filepath.Join(n.dir, "gone"), bare)
	if bare {
		n.bare = time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		if err := os.Chtimes(n.disk, n.bare, n.bare); err != nil {
			t.Fatal(err)
		}
	}
	answerFolding(n.scanner, folding)
	n.scanner.SetDeleteThreshold(3)
}

// mustHoldTheVolumesRows fails the test unless the rows the outer root holds
// are there, uncounted, and, where the bare mount point is there, no walk went
// into it: a walk passes over a nested root that reads as not mounted
// (nestedPass.has), and one that went in would have written its folder row
// at the mount point's time.
func (n *foldedNested) mustHoldTheVolumesRows(t *testing.T) {
	t.Helper()
	mustHoldRows(t, n.store, "music/Artist/Album/01.flac", "music/usb/Band/Record/01.flac")
	if n.bare.IsZero() {
		return
	}
	got, err := n.store.FolderMTime(context.Background(), "music/usb")
	if err != nil {
		t.Fatal(err)
	}
	if got.Equal(n.bare) {
		t.Errorf("a walk of %s went into %s, the bare mount point of %s, which reads as not mounted", n.outer, n.disk, n.configured)
	}
}

// foldedScans runs scan's cases, every folder answering folding: with the
// nested root's bare mount point there, which a walk of the outer root reaches
// and passes over, and with its folder gone, which no walk reaches.
func foldedScans(t *testing.T, folding fsutil.CaseFolding, scan func(ctx context.Context, n *foldedNested) error) {
	for _, bare := range []bool{true, false} {
		name := "folder gone"
		if bare {
			name = "bare mount point"
		}
		t.Run(name, func(t *testing.T) {
			n := newFoldedNested(t)
			n.takeAway(t, bare, folding)
			for range 4 {
				if err := scan(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			n.mustHoldTheVolumesRows(t)
		})
	}
}

// TestASubtreeScanSparesANestedRootSpelledInAnotherCaseWhereTheVolumeFoldsCase:
// on a volume that folds case, a nested root configured as music/USB is the
// folder the volume spells usb; with its volume away, subtree scans of the
// outer root past the reap threshold spare the rows the outer root holds
// under it in the volume's spelling, whether the walk reaches its bare mount
// point or its folder went (Codex on round 5). The volume's answer is stood in
// for (takeAway).
func TestASubtreeScanSparesANestedRootSpelledInAnotherCaseWhereTheVolumeFoldsCase(t *testing.T) {
	foldedScans(t, fsutil.CaseInsensitive, subtreeScanOfTheOuterRoot)
}

// TestAFullScanSparesANestedRootSpelledInAnotherCaseWhereTheVolumeFoldsCase:
// the same, for full scans (Codex on round 5).
func TestAFullScanSparesANestedRootSpelledInAnotherCaseWhereTheVolumeFoldsCase(t *testing.T) {
	foldedScans(t, fsutil.CaseInsensitive, fullScan)
}

// subtreeScanOfTheOuterRoot and fullScan are the scans foldedScans runs.
func subtreeScanOfTheOuterRoot(ctx context.Context, n *foldedNested) error {
	_, err := n.scanner.ScanSubtree(ctx, n.outer)
	return err
}

func fullScan(ctx context.Context, n *foldedNested) error {
	_, err := n.scanner.Scan(ctx)
	return err
}

// TestASubtreeScanUnderANestedRootSpelledInAnotherCaseIsItsOwn: on a volume
// that folds case, a subtree scan of a folder the volume spells music/usb/Band
// is the scan of the nested root configured as music/USB, the deepest root
// that holds it (Scanner.deepestRootOf), never the outer root's: with the
// nested root's volume away, scans of the folder past the reap threshold reap
// nothing, where the outer root, which reads as it is, would take the folder
// for one its operator deleted (Codex's note on round 5). The volume's answer
// is stood in for (takeAway).
func TestASubtreeScanUnderANestedRootSpelledInAnotherCaseIsItsOwn(t *testing.T) {
	n := newFoldedNested(t)
	n.takeAway(t, true, fsutil.CaseInsensitive)
	band := filepath.Join(n.disk, "Band")
	for range 4 {
		// The nested root's scan refuses a folder under a root that reads as
		// not mounted; what matters is the rows.
		_, _ = n.scanner.ScanSubtree(context.Background(), band)
	}
	mustHoldRows(t, n.store, "music/usb/Band/Record/01.flac")
}

// TestTheWatcherKeepsAHeldRootSpelledInAnotherCaseWhereTheVolumeFoldsCase: on
// a volume that folds case, a held nested root configured as music/USB is the
// folder spelled usb: the startup walk of the outer root watches nothing at or
// under it, an event there is the nested root's and arms no scan, and the
// look a dispatch makes is at the nested root (Codex on round 5). The volume's
// answer is stood in for (foldCase).
func TestTheWatcherKeepsAHeldRootSpelledInAnotherCaseWhereTheVolumeFoldsCase(t *testing.T) {
	n := newFoldedNested(t)
	moveAway(t, n.disk, filepath.Join(n.dir, "gone"), true)
	n.foldCase()
	mustReadAsNotMounted(t, n.scanner, n.configured)
	w, err := NewWatcher(n.scanner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = time.Hour, time.Hour, time.Hour
	held := make(chan string, 8)
	w.afterHoldForTests = func(root string) { held <- root }
	startWatcher(t, w)
	if got := waitForRoot(t, held, "the watcher never held the nested root"); got != n.configured {
		t.Errorf("the watcher held %q, want %q", got, n.configured)
	}
	for _, p := range w.w.WatchList() {
		if pathAtOrUnder(filepath.Clean(p), n.disk) {
			t.Errorf("the watcher watches %s, the held root %s in the volume's spelling", p, n.configured)
		}
	}
	root, look, absent := w.absentRootOf(context.Background(), filepath.Join(n.disk, "Band"))
	if root != n.configured || !absent {
		t.Errorf("the dispatch's look at a folder the volume spells %s found %q absent %v (%s); want %q not mounted",
			filepath.Join(n.disk, "Band"), root, absent, look.reason, n.configured)
	}
}

// TestAnEventUnderANestedRootSpelledInAnotherCaseIsItsOwn: an event the
// outer root's watch names in the volume's spelling (usb) under a nested root
// configured as music/USB is the nested root's where the volume folds case: a
// held nested root's arms no scan. The watcher's steps run on the test's
// goroutine with no Run; the volume's answer is stood in for (foldCase).
func TestAnEventUnderANestedRootSpelledInAnotherCaseIsItsOwn(t *testing.T) {
	n := newFoldedNested(t)
	n.foldCase()
	w, err := NewWatcher(n.scanner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.cancelAllPending()
		_ = w.w.Close()
	})
	w.holdRoot(n.configured, rootLook{absent: true, reason: rootAbsentEmpty, rows: 1})
	w.handleEvent(context.Background(), fsnotify.Event{Name: filepath.Join(n.disk, "Band", "Record", "02.flac"), Op: fsnotify.Create})
	if got := armedUnder(w, n.outer); len(got) != 0 {
		t.Errorf("an event under a held nested root, in the volume's spelling, armed scans of %q", got)
	}
}

// TestHoldingANestedRootSpelledInAnotherCaseLetsGoOfTheWatchesUnderIt: on a
// volume that folds case, the walk of the outer root watches a nested root
// configured as music/USB at the folder the volume spells usb, and the nested
// root's hold lets go of every watch there, as of its own (releaseRoot), with
// kqueue's account of the open files and without; nothing at or under the
// folder stays in the account either. The watcher's steps run on the test's
// goroutine with no Run; the volume's answer is stood in for (foldCase).
func TestHoldingANestedRootSpelledInAnotherCaseLetsGoOfTheWatchesUnderIt(t *testing.T) {
	for _, kqueue := range []bool{false, true} {
		name := "watches"
		if kqueue {
			name = "kqueue's account"
		}
		t.Run(name, func(t *testing.T) {
			if kqueue && runtime.GOOS == "windows" {
				t.Skip("kqueue's account runs where fsnotify watches through kqueue; Linux's inotify stands in for it here, and ReadDirectoryChangesW is never paired with it")
			}
			n := newFoldedNested(t)
			n.foldCase()
			w, err := NewWatcher(n.scanner, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				w.cancelAllPending()
				_ = w.w.Close()
			})
			if kqueue && w.fds == nil {
				w.fds = newWatchFDs(WatchFDLimits{Process: 1 << 20}, nil)
			}
			if err := w.addTree(n.outer, true); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(w.w.WatchList(), func(p string) bool { return filepath.Clean(p) == n.disk }) {
				t.Fatalf("premise: the walk of %s does not watch %s (watches: %q)", n.outer, n.disk, w.w.WatchList())
			}
			w.holdRoot(n.configured, rootLook{absent: true, reason: rootAbsentEmpty, rows: 1})
			for _, p := range w.w.WatchList() {
				if pathAtOrUnder(filepath.Clean(p), n.disk) {
					t.Errorf("holding %s left the watch on %s, in the volume's spelling", n.configured, p)
				}
			}
			if !kqueue || w.fds == nil {
				return
			}
			for p := range w.fds.tops {
				if pathAtOrUnder(p, n.disk) {
					t.Errorf("holding %s left %s in the account", n.configured, p)
				}
			}
			for dir, k := range w.fds.kids {
				for p := range k {
					if pathAtOrUnder(p, n.disk) {
						t.Errorf("holding %s left %s in the account, listed in %s", n.configured, p, dir)
					}
				}
			}
		})
	}
}

// TestANestedRootIsWalkedAsItsFolderListsItOnlyWhereTheFolderTakesBothSpellingsForOne:
// a nested root configured as music/USB is walked from the path the outer
// root's walk reaches its folder by, music/usb, only where the folder above
// takes the two spellings for one name, as which root a path is decides
// (caseLook.sameName: a folder established to fold case, its lookups finding
// one entry); where it is case-sensitive, or cannot tell, music/USB is
// another folder, walked as configured (B651's tenth round, Codex on round
// 9: kqueue keys a watch by its spelling, so the configured one was a second
// tree). A Create of usb from the folder above is the watched root's own
// where the folder takes the spellings for one, and asks for nothing; where
// it does not, it is a new folder's, and a scan of music is armed. The volume
// is stood in for through both seams (simVolume), so every host runs this;
// on this host's own kqueue,
// TestANestedRootInAnotherCaseIsWatchedOnceOnAVolumeThatFoldsCase.
func TestANestedRootIsWalkedAsItsFolderListsItOnlyWhereTheFolderTakesBothSpellingsForOne(t *testing.T) {
	for _, c := range []struct {
		folding fsutil.CaseFolding
		listed  bool
	}{
		{fsutil.CaseInsensitive, true},
		{fsutil.CaseSensitive, false},
		{fsutil.CaseUnknown, false},
	} {
		t.Run(c.folding.String(), func(t *testing.T) {
			dir := t.TempDir()
			outer := filepath.Join(dir, "music")
			disk, configured := filepath.Join(outer, "usb"), filepath.Join(outer, "USB")
			writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(disk, "Band", "Record", "01.flac"))
			scanner := NewScanner([]string{outer, configured}, openStoreIn(t, dir), "")
			answerFolding(scanner, c.folding)
			w, err := NewWatcher(scanner, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				w.cancelAllPending()
				_ = w.w.Close()
			})
			want := configured
			if c.listed {
				want = disk
			}
			if got := w.listedRoot(configured); got != want {
				t.Errorf("%s is walked from %s, want %s", configured, got, want)
			}
			if got := w.listedRoot(outer); got != outer {
				t.Errorf("%s, nested in no root, is walked from %s", outer, got)
			}
			// The nested root watched, its own watch installed, as a resume
			// leaves it. Where USB is no folder of this host's disk (Linux),
			// a root the folder does not take for usb has nothing to watch.
			if err := w.addTree(configured, true); err != nil && c.listed {
				t.Fatal(err)
			}
			w.noteRootWatches([]string{configured})
			w.handleEvent(context.Background(), fsnotify.Event{Name: disk, Op: fsnotify.Create})
			armed := armedUnder(w, outer)
			if c.listed && len(armed) != 0 {
				t.Errorf("a Create of %s, the watched root's own folder, armed scans of %q", disk, armed)
			}
			if !c.listed && len(armed) == 0 {
				t.Errorf("a Create of %s, a folder of its own beside %s, armed no scan", disk, configured)
			}
		})
	}
}

// TestWatchingAHeldNestedRootAgainLetsGoOfItsEntryWatchFirst: on kqueue the
// walk of the outer root holds a held nested root's folder open as an entry of
// the folder above it, and watching the root again lets go of that watch
// before it watches the root (watchAgain's releaseRoot): fsnotify's Add finds
// a watch by its path and keeps it, on what was at the path when it was
// opened, which a volume mounted since covers (B651's ninth round, Codex on
// round 8; on this host's own kqueue with a real mount,
// TestTheWatcherWatchesAHeldNestedRootOnTheVolumeMountedOverItsBareFolder).
// The order is read from kqueue's account, which Linux's inotify stands in
// for; the watcher's steps run on the test's goroutine with no Run.
func TestWatchingAHeldNestedRootAgainLetsGoOfItsEntryWatchFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("kqueue's account runs where fsnotify watches through kqueue; Linux's inotify stands in for it here, and ReadDirectoryChangesW is never paired with it")
	}
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	inner := filepath.Join(outer, "usb", "library")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(inner, "Band", "Record", "01.flac"))
	w, err := NewWatcher(NewScanner([]string{outer, inner}, openStoreIn(t, dir), ""), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.cancelAllPending()
		_ = w.w.Close()
	})
	if w.fds == nil {
		w.fds = newWatchFDs(WatchFDLimits{Process: 1 << 20}, nil)
	}
	w.holdRoot(inner, rootLook{absent: true, reason: rootAbsentEmpty, rows: 1})
	if err := w.addTree(outer, true); err != nil {
		t.Fatal(err)
	}
	if !w.fds.held(inner) || w.fds.listed(inner) {
		t.Fatalf("premise: the walk of %s holds %s %v and lists it %v, want held as an entry and not listed",
			outer, inner, w.fds.held(inner), w.fds.listed(inner))
	}
	released, listedAtRelease := false, false
	w.beforeReleaseForTests = func(paths []string) {
		if slices.Contains(paths, inner) {
			released, listedAtRelease = true, w.fds.listed(inner)
		}
	}
	w.resumeRoot(context.Background(), inner)
	if !released {
		t.Errorf("watching %s again kept the watch the walk of %s opened on its folder", inner, outer)
	}
	if listedAtRelease {
		t.Errorf("watching %s again let go of its entry watch only after watching it", inner)
	}
	if !w.fds.listed(inner) {
		t.Errorf("%s is not watched again", inner)
	}
}

// TestANestedRootSpelledInAnotherCaseIsTheVolumesFolderOnlyWhereItFoldsCase is
// the comparisons on this host's own volume (fsutil.FoldingIn, probed): where
// it folds case (macOS's default, Windows'), a nested root configured as
// music/USB is the folder spelled usb, so a subtree scan there is the nested
// root's, and with its volume away the outer root's scans spare its rows;
// where it does not (Linux's), the two spellings stay two folders: music/USB is
// no folder, a subtree scan of usb is the outer root's, and its rows go with
// its files (Codex on round 5).
func TestANestedRootSpelledInAnotherCaseIsTheVolumesFolderOnlyWhereItFoldsCase(t *testing.T) {
	n := newFoldedNested(t)
	folds := fsutil.FoldingIn(n.outer) == fsutil.CaseInsensitive
	ctx := context.Background()
	record := filepath.Join(n.disk, "Band", "Record")
	writeTracks(t, filepath.Join(record, "02.flac"))
	if _, err := n.scanner.ScanSubtree(ctx, record); err != nil {
		t.Fatal(err)
	}
	asNested, _ := n.store.GetTrack(ctx, "USB/Band/Record/02.flac")
	asOuter, _ := n.store.GetTrack(ctx, "music/usb/Band/Record/02.flac")
	if folds && (asNested == nil || asOuter != nil) {
		t.Errorf("on a volume that folds case, a subtree scan of %s indexed its new file as the nested root's: %v, the outer root's: %v",
			record, asNested != nil, asOuter != nil)
	}
	if !folds && (asNested != nil || asOuter == nil) {
		t.Errorf("on a case-sensitive volume, a subtree scan of %s indexed its new file as music/USB's: %v, the outer root's: %v",
			record, asNested != nil, asOuter != nil)
	}
	moveAway(t, n.disk, filepath.Join(n.dir, "gone"), true)
	if _, err := n.scanner.ScanSubtree(ctx, n.outer); err != nil {
		t.Fatal(err)
	}
	kept, _ := n.store.GetTrack(ctx, "music/usb/Band/Record/01.flac")
	if folds && kept == nil {
		t.Error("on a volume that folds case, a scan of the outer root reaped a row under the nested root while its volume was away")
	}
	if !folds && kept != nil {
		t.Error("on a case-sensitive volume, music/USB spared the rows under usb, another folder")
	}
}

// TestAFolderUnderAGoneNestedRootSpelledInAnotherCaseIsItsOwnOnlyWhereTheVolumeFoldsCase
// asks this host's volume, as the test above does, about a nested root whose
// folder went with its volume: whether a folder there, named in the volume's
// spelling, is the nested root's is answered by the folder its name is looked
// up in, which is still there (fsutil.FoldingIn), never taken as
// case-sensitive for the nested root's being gone (Argon on the sixth round's
// brief). Where the volume folds case, a subtree scan of such a folder is the
// nested root's, which refuses a root it cannot see, so the rows the outer root
// holds there stay; where it does not (Linux's), the folder is the outer
// root's, gone, and its rows go with it.
func TestAFolderUnderAGoneNestedRootSpelledInAnotherCaseIsItsOwnOnlyWhereTheVolumeFoldsCase(t *testing.T) {
	n := newFoldedNested(t)
	folds := fsutil.FoldingIn(n.outer) == fsutil.CaseInsensitive
	ctx := context.Background()
	moveAway(t, n.disk, filepath.Join(n.dir, "gone"), false)
	// The nested root's scan refuses a folder under a root it cannot see;
	// what matters is the rows.
	_, _ = n.scanner.ScanSubtree(ctx, filepath.Join(n.disk, "Band"))
	kept, _ := n.store.GetTrack(ctx, "music/usb/Band/Record/01.flac")
	if folds && kept == nil {
		t.Errorf("on a volume that folds case, a subtree scan of %s, under %s whose folder went, was the outer root's and reaped its rows",
			filepath.Join(n.disk, "Band"), n.configured)
	}
	if !folds && kept != nil {
		t.Errorf("on a case-sensitive volume, a subtree scan of %s, a folder of the outer root that went, kept its rows", filepath.Join(n.disk, "Band"))
	}
}
