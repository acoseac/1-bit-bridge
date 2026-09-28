package manifest

// The clean-empty guard counts only library content.
//
// A root whose walk finds nothing, over a store that carries rows for it, is
// a suspected clean-empty mount: the deletion pass spares its rows until the
// operator places `.bridge-allow-empty`. The guard counted every entry the
// walk was handed, dot-files included, so a .DS_Store that Finder writes into
// any directory a window opens, an unmounted mount point included, made the
// root "non-empty", and the deletion pass reaped every row under it at the
// threshold; so did a Synology @eaDir, and the same held for the owning-root
// audit a subtree scan runs when its subtree is missing. The guard now counts
// what the walk takes as content (isLibraryEntry), and the walk skips the
// directories operating systems and NAS firmware leave in a volume
// (ShouldSkipDir), which it used to index: a recycle bin's or a snapshot's
// audio files became tracks of their own.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// rootNoise is what an emptied mount point can hold that is not library
// content.
var rootNoise = []struct {
	name  string
	plant func(t *testing.T, dir string)
}{
	// Finder writes one into any directory a window opens.
	{".DS_Store", func(t *testing.T, dir string) {
		writeNoiseFile(t, filepath.Join(dir, ".DS_Store"))
	}},
	// Synology keeps one beside every media folder, holding a directory
	// named like each file it describes.
	{"@eaDir", func(t *testing.T, dir string) {
		writeNoiseFile(t, filepath.Join(dir, "@eaDir", "01.flac", "SYNOINDEX_MEDIA_INFO"))
	}},
}

// writeNoiseFile writes a small file at p, making its directories.
func writeNoiseFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// emptyToNoise removes the root's library and plants noise in its place, as a
// clean unmount leaves a mount point that something then wrote into.
func (f indexedRoot) emptyToNoise(t *testing.T, plant func(*testing.T, string)) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, "Artist")); err != nil {
		t.Fatal(err)
	}
	plant(t, f.root)
}

// placeSentinel writes the operator's `.bridge-allow-empty` into the root.
func (f indexedRoot) placeSentinel(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, allowEmptySentinelFilename), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireRowsGone asserts the sentinel let the deletion pass reap every row
// under the root, and left the other root's row, if there is one, alone.
func (f indexedRoot) requireRowsGone(t *testing.T) {
	t.Helper()
	for rel := range f.indexed {
		if st, err := f.store.GetTrackStat(context.Background(), rel); err != nil || st != nil {
			t.Errorf("the sentinel did not authorise reaping %s (err %v)", rel, err)
		}
	}
	if f.other != "" {
		mustIndexed(t, f.store, f.other)
	}
}

// guardCases are the ways a root is walked: a full scan and a subtree scan
// of the root, each single- and multi-root.
var guardCases = []struct {
	name           string
	subtree, multi bool
}{
	{"scan, single root", false, false},
	{"scan, multi-root", false, true},
	{"subtree scan of the root, single root", true, false},
	{"subtree scan of the root, multi-root", true, true},
}

// TestScanner_AnEmptiedRootHoldingOnlyNoiseSparesItsRows: an emptied mount
// point holding only a .DS_Store, or only a Synology @eaDir, is empty to the
// guard. Counting every entry, the guard read each as a root with content,
// logged nothing, and the third scan reaped every row under it (measured, in
// every case here). The sentinel still decides, and is now the only thing
// that does: with it placed beside the noise, the rows go.
func TestScanner_AnEmptiedRootHoldingOnlyNoiseSparesItsRows(t *testing.T) {
	for _, noise := range rootNoise {
		for _, c := range guardCases {
			t.Run(noise.name+", "+c.name, func(t *testing.T) {
				f := newIndexedRootIn(t, c.multi)
				f.emptyToNoise(t, noise.plant)
				rec := loggingtest.Record(t)
				for i := 1; i <= 3; i++ {
					f.scanRoot(t, c.subtree)
				}
				f.requireRowsKept(t, "a root holding only "+noise.name)
				requireCleanEmptyLines(t, rec.Lines(msgCleanEmpty), 3, false, "")

				f.placeSentinel(t)
				for i := 1; i <= 3; i++ {
					f.scanRoot(t, c.subtree)
				}
				f.requireRowsGone(t)
			})
		}
	}
}

// TestScanner_ASubtreeScanBelowARootHoldingOnlyNoiseIsRefused: a subtree scan
// whose subtree is gone asks the owning root whether it is a dropped mount
// (auditOwningRootOnSubtreeMiss), and that audit counted every entry too, so
// a root holding only a .DS_Store or an @eaDir read as alive and the bounded
// pass reaped the subtree's rows (measured: no error, both rows gone at the
// third scan). The audit refuses now, and the sentinel, which it no longer
// finds by counting entries, still lets the deletion through.
func TestScanner_ASubtreeScanBelowARootHoldingOnlyNoiseIsRefused(t *testing.T) {
	for _, noise := range rootNoise {
		for _, multi := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, multi=%v", noise.name, multi), func(t *testing.T) {
				f := newIndexedRootIn(t, multi)
				f.emptyToNoise(t, noise.plant)
				subtree := filepath.Join(f.root, "Artist")
				for i := 1; i <= 3; i++ {
					if _, err := f.sc.ScanSubtree(context.Background(), subtree); err == nil {
						t.Fatalf("subtree scan %d below a root holding only %s reported success", i, noise.name)
					}
				}
				f.requireRowsKept(t, "subtree scans below a root holding only "+noise.name)

				f.placeSentinel(t)
				for i := 1; i <= 3; i++ {
					if _, err := f.sc.ScanSubtree(context.Background(), subtree); err != nil {
						t.Fatalf("subtree scan %d with the sentinel placed: %v", i, err)
					}
				}
				f.requireRowsGone(t)
			})
		}
	}
}

// detritusTracks are audio files an operating system or NAS firmware leaves
// in a volume or share: deleted files in a recycle bin, copies in a
// snapshot. None is a track of the library.
var detritusTracks = []string{
	"#recycle/Artist/Album/Deleted.flac",
	"#snapshot/GMT+02_2026-09-28-00-00/Artist/Album/01.flac",
	"@Recycle/Artist/Deleted.flac",
	"@Recently-Snapshot/GMT+02_2026-09-28/Artist/01.flac",
	"~snapshot/hourly.0/Artist/01.flac",
	"$RECYCLE.BIN/S-1-5-21-1/$R1A2B3C.flac",
	"$Recycle.Bin/S-1-5-21-1/$R4D5E6F.flac",
	"lost+found/#12345.flac",
	"System Volume Information/Recovered.flac",
}

// TestScanner_OSAndNASDetritusIsNotLibraryContent: the walk skips the
// directories ShouldSkipDir names, in both walks. It descended them: a
// recycle bin's deleted files and a snapshot's copies were indexed as tracks
// of their own, and a Synology @eaDir became folder rows, one per file it
// describes. What the library holds is indexed as before.
func TestScanner_OSAndNASDetritusIsNotLibraryContent(t *testing.T) {
	for _, subtree := range []bool{false, true} {
		t.Run(fmt.Sprintf("subtree=%v", subtree), func(t *testing.T) {
			root := t.TempDir()
			writeLibraryBesideDetritus(t, root)
			store, sc := newScanFixture(t, root)
			scanTheRoot(t, sc, root, subtree)
			requireOnlyTheLibraryIndexed(t, store)
		})
	}
}

// writeLibraryBesideDetritus writes one album track, every file of
// detritusTracks, and the Synology @eaDir entry that describes the track.
func writeLibraryBesideDetritus(t *testing.T, root string) {
	t.Helper()
	for _, rel := range append([]string{"Artist/Album/01.flac"}, detritusTracks...) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": rel})
	}
	writeNoiseFile(t, filepath.Join(root, "Artist", "Album", "@eaDir", "01.flac", "SYNOINDEX_MEDIA_INFO"))
}

// requireOnlyTheLibraryIndexed checks that the store holds the album track
// alone, and the library's own folder rows exactly: an @eaDir holds no
// audio, so only its folder rows can show it was walked.
func requireOnlyTheLibraryIndexed(t *testing.T, store *Store) {
	t.Helper()
	paths, err := store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, " ") != "Artist/Album/01.flac" {
		t.Errorf("indexed %v, want only Artist/Album/01.flac", paths)
	}
	folders, err := store.FolderPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(folders)
	if strings.Join(folders, " ") != ". Artist Artist/Album" {
		t.Errorf("folder rows %v, want only [. Artist Artist/Album]", folders)
	}
}

// TestShouldSkipDirNamesExactlyTheDetritus pins the list and its exactness:
// a folder named for an artist or an album that merely resembles an entry
// is library content.
func TestShouldSkipDirNamesExactlyTheDetritus(t *testing.T) {
	for _, name := range []string{
		".Trashes", ".Spotlight-V100", ".fseventsd", ".recycle", ".zfs", ".snapshot",
		"$RECYCLE.BIN", "$Recycle.Bin", "System Volume Information", "lost+found",
		"@eaDir", "#recycle", "#snapshot", "@Recycle", "@Recently-Snapshot", "~snapshot",
	} {
		if !ShouldSkipDir(name) {
			t.Errorf("ShouldSkipDir(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"Lost+Found", "Lost & Found", "Recycler", "Recycled", "Snapshot", "Snapshots",
		"recycle", "eaDir", "#1 Record", "@Home", "~Blue", "Artist", "CD1",
	} {
		if ShouldSkipDir(name) {
			t.Errorf("ShouldSkipDir(%q) = true, want false: a folder of that name is library content", name)
		}
	}
}
