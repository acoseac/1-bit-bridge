package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// B651's seventh round (Codex on round 6): how a folder compares names is
// asked of each folder a path's elements are looked up in, and answers
// Sensitive, Insensitive or Unknown; what a scan spares and a walk passes over
// counts Unknown as folding, and which root a path is counts it as
// case-sensitive (Argon on the brief).

// mountedNested is a library root, music, holding the mount point music/usb of
// another volume, and a root of its own on that volume configured as
// music/usb/LIBRARY, a folder the volume lists as library; indexed through the
// outer root's walk while the volume was there.
type mountedNested struct {
	dir, outer, mount, configured string
	store                         *Store
	scanner                       *Scanner
}

func newMountedNested(t *testing.T) *mountedNested {
	t.Helper()
	dir := t.TempDir()
	n := &mountedNested{dir: dir, outer: filepath.Join(dir, "music")}
	n.mount = filepath.Join(n.outer, "usb")
	n.configured = filepath.Join(n.mount, "LIBRARY")
	writeTracks(t, filepath.Join(n.outer, "Artist", "Album", "01.flac"), filepath.Join(n.mount, "library", "Band", "Record", "01.flac"))
	n.store = openStoreIn(t, dir)
	n.scanner = NewScanner([]string{n.outer, n.configured}, n.store, "")
	if _, err := n.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, n.store, "music/Artist/Album/01.flac", "music/usb/library/Band/Record/01.flac")
	return n
}

// answer has the scanner answer that music, the outer root's volume, is
// case-sensitive, that the mount point folds case where mountFolds says so
// (the volume mounted there folding the names in it), and every other folder
// as this host's probe reads it: a bare mount point, holding nothing, answers
// Unknown.
func (n *mountedNested) answer(mountFolds bool) {
	n.scanner.foldingHook = func(d string) fsutil.CaseFolding {
		switch {
		case d == n.outer:
			return fsutil.CaseSensitive
		case d == n.mount && mountFolds:
			return fsutil.CaseInsensitive
		}
		return fsutil.FoldingIn(d)
	}
}

// TestAScanSparesANestedRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot: a
// volume that folds case, mounted inside a library root on one that does not,
// holds a root of its own configured as music/usb/LIBRARY, which the outer
// root's walk indexed as music/usb/library. With the volume unmounted, its
// bare mount point cannot tell how the volume compared names, so the rows in
// that spelling are spared, by subtree scans of the outer root and of the
// mount point and by full scans, past the reap threshold: each element of the
// nested root's path is judged by its own folder, never by the outer root's
// volume alone (Codex on round 6).
func TestAScanSparesANestedRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot(t *testing.T) {
	for _, c := range []struct {
		name string
		scan func(ctx context.Context, n *mountedNested) error
	}{
		{"subtree scans", func(ctx context.Context, n *mountedNested) error {
			if _, err := n.scanner.ScanSubtree(ctx, n.outer); err != nil {
				return err
			}
			_, err := n.scanner.ScanSubtree(ctx, n.mount)
			return err
		}},
		{"full scans", func(ctx context.Context, n *mountedNested) error {
			_, err := n.scanner.Scan(ctx)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newMountedNested(t)
			moveAway(t, n.mount, filepath.Join(n.dir, "gone"), true)
			n.answer(false)
			n.scanner.SetDeleteThreshold(3)
			for range 4 {
				if err := c.scan(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			mustHoldRows(t, n.store, "music/Artist/Album/01.flac", "music/usb/library/Band/Record/01.flac")
			mustHaveMissingCount(t, n.store, "folders", "music/usb/library/Band/Record", 0)
		})
	}
}

// TestTheWatcherPassesOverAHeldRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot:
// the startup walk of a library root on a volume that does not fold case
// passes over a held root configured as music/usb/LIBRARY on a volume that
// does, mounted at music/usb, in the volume's spelling library: the held
// root's path is judged element by element, each by its own folder, never by
// the walked root's volume alone (Codex on round 6). The folder above it is
// still watched.
func TestTheWatcherPassesOverAHeldRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot(t *testing.T) {
	n := newMountedNested(t)
	n.answer(true)
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
	library := filepath.Join(n.mount, "library")
	watchesMount := false
	for _, p := range w.w.WatchList() {
		p = filepath.Clean(p)
		if pathAtOrUnder(p, library) {
			t.Errorf("the watcher watches %s, the held root %s in its volume's spelling", p, n.configured)
		}
		watchesMount = watchesMount || p == n.mount
	}
	if !watchesMount {
		t.Errorf("the watcher does not watch %s, a folder of the outer root (watches: %q)", n.mount, w.w.WatchList())
	}
}

// TestADistinctFolderSpelledLikeAnAbsentNestedRootIsWalkedWhereItsFolderIsCaseSensitive:
// where the folder a nested root's name is looked up in is case-sensitive and
// holds another spelling of the name, that spelling is another folder: with a
// nested root configured as music/USB that is not there, the outer root's
// folder usb is walked by subtree and full scans alike, a new file in it
// indexed and a gone file's row reaped, as before round 6 (the protection's
// opposite direction; Codex on round 6). Runs where this host's volume is
// case-sensitive.
func TestADistinctFolderSpelledLikeAnAbsentNestedRootIsWalkedWhereItsFolderIsCaseSensitive(t *testing.T) {
	for _, name := range []string{"subtree scan", "full scan"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outer := filepath.Join(dir, "music")
			usb := filepath.Join(outer, "usb")
			writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(usb, "Band", "Record", "01.flac"))
			if fsutil.FoldingIn(outer) != fsutil.CaseSensitive {
				t.Skip("this host's volume folds case: music/USB is the folder usb there")
			}
			store := openStoreIn(t, dir)
			scanner := NewScanner([]string{outer, filepath.Join(outer, "USB")}, store, "")
			ctx := context.Background()
			scan := func() {
				t.Helper()
				var err error
				if name == "full scan" {
					_, err = scanner.Scan(ctx)
				} else {
					_, err = scanner.ScanSubtree(ctx, outer)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			scan()
			mustHoldRows(t, store, "music/usb/Band/Record/01.flac")
			if err := os.RemoveAll(filepath.Join(usb, "Band")); err != nil {
				t.Fatal(err)
			}
			writeTracks(t, filepath.Join(usb, "Other", "Album", "01.flac"))
			scan()
			if got, _ := store.GetTrack(ctx, "music/usb/Band/Record/01.flac"); got != nil {
				t.Errorf("the %s spared the row of a file gone from %s, which is not %s", name, usb, filepath.Join(outer, "USB"))
			}
			mustHoldRows(t, store, "music/usb/Other/Album/01.flac")
		})
	}
}

// TestASubtreeScanSparesANestedRootInEverySpellingWhereFoldingIsUnknown: where
// no folder can tell how it compares names, what a scan spares counts that as
// folding: with the volume of a nested root configured as music/USB away,
// subtree scans of the outer root past the reap threshold spare the rows it
// holds under usb and pass over usb's bare mount point (Argon on the brief).
// The answers are stood in for (takeAway).
func TestASubtreeScanSparesANestedRootInEverySpellingWhereFoldingIsUnknown(t *testing.T) {
	foldedScans(t, fsutil.CaseUnknown, subtreeScanOfTheOuterRoot)
}

// TestAFullScanSparesANestedRootInEverySpellingWhereFoldingIsUnknown: the same,
// for full scans.
func TestAFullScanSparesANestedRootInEverySpellingWhereFoldingIsUnknown(t *testing.T) {
	foldedScans(t, fsutil.CaseUnknown, fullScan)
}

// TestASubtreeScanOfAFolderAnAbsentNestedRootMayHoldReapsNothing: where no
// folder can tell how it compares names, a folder the volume spells
// music/usb/Band is not established to be under the nested root configured as
// music/USB, so the outer root owns its subtree scan (which root a path is
// counts Unknown as case-sensitive), and that scan reaps nothing past the reap
// threshold while the nested root reads as not mounted: the nested root may
// hold the folder (what is spared counts Unknown as folding). The volume went
// with its mount point; the answers are stood in for (takeAway).
func TestASubtreeScanOfAFolderAnAbsentNestedRootMayHoldReapsNothing(t *testing.T) {
	n := newFoldedNested(t)
	n.takeAway(t, false, fsutil.CaseUnknown)
	band := filepath.Join(n.disk, "Band")
	if root, _ := n.scanner.rootOf(band); root != n.outer {
		t.Fatalf("premise: %s is %q's, want the outer root's", band, root)
	}
	for range 4 {
		if _, err := n.scanner.ScanSubtree(context.Background(), band); err != nil {
			t.Fatal(err)
		}
	}
	mustHoldRows(t, n.store, "music/usb/Band/Record/01.flac")
}

// TestTwoRootsSpelledAlikeStayTwoWhereTheirFolderIsNotEstablishedToFoldCase:
// two roots configured as music/USB and music/usb, in a folder that does not
// fold case or cannot tell, are two roots: a path in each is its own root's,
// and a subtree scan of a folder of usb indexes its file under usb, never
// under USB, the rewrite of the root's own part never sending the scan into the
// other's folder (Codex on round 6: Windows' filepath.Rel folds case in a
// case-sensitive folder too, and the root listed first took both). The
// folder's answer is stood in for (answerFolding); usb alone is made on disk,
// which every host's volume can hold.
func TestTwoRootsSpelledAlikeStayTwoWhereTheirFolderIsNotEstablishedToFoldCase(t *testing.T) {
	for _, folding := range []fsutil.CaseFolding{fsutil.CaseSensitive, fsutil.CaseUnknown} {
		t.Run(folding.String(), func(t *testing.T) {
			dir := t.TempDir()
			music := filepath.Join(dir, "music")
			upper, lower := filepath.Join(music, "USB"), filepath.Join(music, "usb")
			record := filepath.Join(lower, "Band", "Record")
			writeTracks(t, filepath.Join(record, "01.flac"))
			store := openStoreIn(t, dir)
			scanner := NewScanner([]string{upper, lower}, store, "")
			answerFolding(scanner, folding)
			for _, c := range []struct{ name, root string }{
				{filepath.Join(lower, "Band"), lower},
				{filepath.Join(upper, "Band"), upper},
			} {
				if root, _ := scanner.rootOf(c.name); root != c.root {
					t.Errorf("%s is %q's, want %q's", c.name, root, c.root)
				}
			}
			ctx := context.Background()
			if _, err := scanner.ScanSubtree(ctx, record); err != nil {
				t.Fatal(err)
			}
			mustHoldRows(t, store, "usb/Band/Record/01.flac")
			if got, _ := store.GetTrack(ctx, "USB/Band/Record/01.flac"); got != nil {
				t.Errorf("a subtree scan of %s indexed its file under %s", record, upper)
			}
		})
	}
}
