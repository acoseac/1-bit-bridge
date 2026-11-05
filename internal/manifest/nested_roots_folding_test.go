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
// outer root's walk while the volume was there (where this host's disk folds
// case, the nested root's own walk indexes it too).
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

// answer has the scanner stand in for the two volumes (simVolume, both of its
// seams): the outer root's volume does not fold case, and the one mounted at
// music/usb does, while mounted says it is there. Whether a folder can tell
// at all is read from its own listing on this host (fsutil.FoldingIn): one
// that offers no name to look up, an empty one or a bare mount point among
// them, answers Unknown on any volume, and one that can answers as its volume
// compares, never as this host's disk does, which folds case on macOS and
// Windows where the outer volume stands for one that does not (B651's eighth
// round: a bare mount point holding an index folder of its own answered there
// for the host's disk, case-insensitive). A lookup finds a name in another
// case in a folder of the volume that folds alone (B651's ninth round).
func (n *mountedNested) answer(mounted bool) {
	simVolume{folding: func(d string) fsutil.CaseFolding {
		switch {
		case fsutil.FoldingIn(d) == fsutil.CaseUnknown:
			return fsutil.CaseUnknown
		case mounted && pathAtOrUnder(d, n.mount):
			return fsutil.CaseInsensitive
		}
		return fsutil.CaseSensitive
	}}.install(n.scanner)
}

// mustReadAsNotMounted fails the test at once unless the nested root reads as
// a volume that is not mounted (Scanner.lookAtRoot, the look the watcher holds
// a root on), where the test is about to wait for its hold: a hold the
// scenario cannot give is otherwise waited for until the test binary's
// deadline, and every test after it is starved (B651's eighth round).
func mustReadAsNotMounted(t *testing.T, s *Scanner, root string) {
	t.Helper()
	if look := s.lookAtRoot(context.Background(), root); !look.absent {
		t.Fatalf("premise: %s reads as it is on this host, so no watcher holds it", root)
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
// volume alone (Codex on round 6). A bare mount point that holds an index
// folder of its own (Synology's @eaDir) answers case-sensitive for the folder
// it is on, not for the volume, and holding no library content it is not
// trusted for the volume's spelling either.
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
		for _, junk := range []string{"", "@eaDir"} {
			name := c.name + ", an empty mount point"
			if junk != "" {
				name = c.name + ", a mount point holding " + junk
			}
			t.Run(name, func(t *testing.T) {
				n := newMountedNested(t)
				moveAway(t, n.mount, filepath.Join(n.dir, "gone"), true)
				want := fsutil.CaseUnknown
				if junk != "" {
					if err := os.Mkdir(filepath.Join(n.mount, junk), 0o755); err != nil {
						t.Fatal(err)
					}
					want = fsutil.CaseSensitive
				}
				n.answer(false)
				if got := n.scanner.foldingIn(n.mount); got != want {
					t.Fatalf("premise: the bare mount point answered %s, want %s", got, want)
				}
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
}

// TestTheWatcherPassesOverAHeldRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot:
// the startup walk of a library root on a volume that does not fold case
// passes over a held root configured as music/usb/LIBRARY on a volume that
// does, mounted at music/usb, in the volume's spelling library: the held
// root's path is judged element by element, each by its own folder, never by
// the walked root's volume alone (Codex on round 6). The folder above it is
// still watched. The held root is the mount point of a volume of its own,
// which is away: its bare mount point, which the volume at music/usb lists as
// library, holds nothing while the index holds the root's tracks, so the root
// reads as not mounted where LIBRARY is library (a disk that folds case, this
// host's on macOS and Windows) and where LIBRARY is no folder at all (one
// that does not). With library left full, the root reads as it is on a disk
// that folds case, and no hold comes (Codex on round 7).
func TestTheWatcherPassesOverAHeldRootOnAVolumeThatFoldsCaseMountedInOneThatDoesNot(t *testing.T) {
	n := newMountedNested(t)
	library := filepath.Join(n.mount, "library")
	moveAway(t, library, filepath.Join(n.dir, "gone"), true)
	n.answer(true)
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

// TestALookupThatFailsIsNoProofThatTwoSpellingsAreOneRoot: in a folder
// established to fold case, two spellings of a name that differ in case alone
// are one entry, for which root a path is, only where the lookups of both
// find one file, or neither finds anything and the folder's answer decides.
// A lookup that fails otherwise, a permission refused or a transient error
// (Codex on round 7), and one that finds a file where the other finds nothing,
// which a folder that folds that name never answers (Claude on round 8), keep
// them two (caseLook.sameName). A path stays the outer root's then, never a
// nested root's spelled like it: no scan is routed to a root on such lookups,
// nor sent into a folder that may be another by the rewrite of the root's own
// part. The volume is stood in for through both seams (simVolume), so every
// leg runs on every host: a permission refusing lookups, which this host's
// disk would not do as root or on Windows, and a volume that folds ASCII case
// and no other, where the folder Ωmega is there and ωmega names nothing,
// beside one that folds every case, where USB finds the folder usb.
func TestALookupThatFailsIsNoProofThatTwoSpellingsAreOneRoot(t *testing.T) {
	for _, c := range []struct {
		name string
		// disk is the folder in music the path asked about is in, made there
		// unless gone; nested is the nested root's name in music.
		disk, nested string
		gone         bool
		vol          simVolume
		outer        bool
	}{
		{"a lookup refused", "usb", "USB", false, simVolume{refused: map[string]bool{"music": true}}, true},
		{"one spelling there and the other not", "Ωmega", "ωmega", false, simVolume{fold: asciiFold}, true},
		{"neither spelling there", "usb", "USB", true, simVolume{}, false},
		{"both spellings one folder", "usb", "USB", false, simVolume{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			music := filepath.Join(dir, "music")
			path := filepath.Join(music, c.disk, "Band")
			files := []string{filepath.Join(music, "Artist", "Album", "01.flac")}
			if !c.gone {
				files = append(files, filepath.Join(path, "Record", "01.flac"))
			}
			writeTracks(t, files...)
			nested := filepath.Join(music, c.nested)
			scanner := NewScanner([]string{music, nested}, openStoreIn(t, dir), "")
			v := c.vol
			v.folding = func(string) fsutil.CaseFolding { return fsutil.CaseInsensitive }
			if v.refused != nil {
				v.refused = map[string]bool{music: true}
			}
			v.install(scanner)
			want := nested
			if c.outer {
				want = music
			}
			if got, _ := scanner.rootOf(path); got != want {
				t.Errorf("%s is %q's, want %q's", path, got, want)
			}
		})
	}
}

// TestTwoRootsThatAreTwoFoldersStayTwoWhateverTheirFolderAnswers: two roots
// configured as music/USB and music/usb that are both there as two folders are
// two roots even where their folder answers that it folds case: the answer is
// read from one name in the folder, and two entries that are not one file are
// the folder's own word that these two are not one name (caseLook.sameName).
// Two such folders can be made on a case-sensitive volume alone; the answer is
// stood in for (answerFolding).
func TestTwoRootsThatAreTwoFoldersStayTwoWhateverTheirFolderAnswers(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "music")
	upper, lower := filepath.Join(music, "USB"), filepath.Join(music, "usb")
	writeTracks(t, filepath.Join(upper, "Band", "Record", "01.flac"))
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Skipf("this host's volume holds one of two spellings of a name: %v", err)
	}
	writeTracks(t, filepath.Join(lower, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{upper, lower}, store, "")
	answerFolding(scanner, fsutil.CaseInsensitive)
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
