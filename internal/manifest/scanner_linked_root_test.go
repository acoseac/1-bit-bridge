package manifest

// A library root that is itself a link to a directory is walked THROUGH.
//
// filepath.WalkDir Lstats its root and follows no link, so a configured root
// such as `/music -> /mnt/nas/music` (an ordinary way to point at a mount),
// and on Windows a junction, was one entry that is not a directory: Scan and
// ScanSubtree indexed nothing under it, and the watcher registered no watch.
// Worse, an install whose root BECAME a link after it was indexed (the
// library moved to a mount, the old path left pointing at it) was told every
// scan that its mount looked empty, with a hint to place `.bridge-allow-empty`
// at the root. That sentinel is found THROUGH the link, so following the hint
// let the deletion pass delete every row while every file sat on disk. A
// subtree scan of such a root needed no hint at all: it reaped every row at
// the threshold, and so it did when the link DANGLED (the mount gone), where
// Scan rightly spared them.
//
// The walks now start from fsutil.WalkableRoot's path, and every path below
// it keeps the configured spelling, so the rows are what they always were for
// a root that is a directory, and fs.Resolver serves each of them from the
// file the scanner read.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// msgCleanEmpty is the line the clean-empty guard logs for a root it spares.
const msgCleanEmpty = "suspected clean-empty mount failure"

// msgRootUnreachable is the line a full scan logs for a root it cannot see.
const msgRootUnreachable = "root unreachable"

// linkedRootTracks are the library-relative paths seedLinkedRootLibrary
// writes, sorted.
var linkedRootTracks = []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}

// seedLinkedRootLibrary writes linkedRootTracks under dir.
func seedLinkedRootLibrary(t *testing.T, dir string) {
	t.Helper()
	for i, rel := range linkedRootTracks {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": fmt.Sprintf("Track %d", i+1)})
	}
}

// indexedRoot is a root holding linkedRootTracks, indexed while it is still a
// directory, by a scanner that reaps a missing row at the production
// threshold (three scans), not the test default of one.
type indexedRoot struct {
	root    string
	store   *Store
	sc      *Scanner
	indexed map[string]int64 // indexed_at of each row after the first scan
	since   time.Time        // before the first scan, for the deletion journal
}

func newIndexedRoot(t *testing.T) indexedRoot {
	t.Helper()
	root := filepath.Join(t.TempDir(), "music")
	seedLinkedRootLibrary(t, root)
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(3)
	since := time.Now().Add(-time.Minute)
	scanOnce(t, sc, "while the root is a directory")
	f := indexedRoot{root: root, store: store, sc: sc, indexed: map[string]int64{}, since: since}
	for _, rel := range linkedRootTracks {
		f.indexed[rel] = indexedAt(t, store, rel)
	}
	return f
}

// moveBehindLink moves the library to a sibling directory and leaves the
// configured root a link to it, as an operator does who moves a library to
// a mount and points the old path at it. Returns where the files are now.
func (f indexedRoot) moveBehindLink(t *testing.T) string {
	t.Helper()
	moved := f.root + "-on-nas"
	if err := os.Rename(f.root, moved); err != nil {
		t.Fatal(err)
	}
	linkOrSkip(t, moved, f.root)
	return moved
}

// requireRowsKept asserts every row the first scan wrote is still there,
// unrewritten, and that no tombstone went out for any of them.
func (f indexedRoot) requireRowsKept(t *testing.T, label string) {
	t.Helper()
	for rel, was := range f.indexed {
		if st, err := f.store.GetTrackStat(context.Background(), rel); err != nil || st == nil {
			t.Fatalf("%s: the row of %s was deleted while its file is on disk (err %v)", label, rel, err)
		}
		if got := indexedAt(t, f.store, rel); got != was {
			t.Errorf("%s: the row of %s was rewritten (indexed_at %d, was %d)", label, rel, got, was)
		}
	}
	requireNoTombstones(t, f.store, f.since, label)
}

// requireNoTombstones asserts the deletion journal holds nothing since since:
// no paired device was told a track was deleted.
func requireNoTombstones(t *testing.T, store *Store, since time.Time, label string) {
	t.Helper()
	deleted, _, err := store.DeletedSince(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) > 0 {
		t.Fatalf("%s: tombstones went to every paired device for %v", label, deleted)
	}
}

// requireRowsResolveTo asserts that the store holds exactly want (each
// linkedRootTracks path under prefix), and that each resolves through
// fs.Resolver over roots to the very file under dir the scanner read, whose
// size and mtime the row records.
func requireRowsResolveTo(t *testing.T, store *Store, roots []string, prefix, dir string) {
	t.Helper()
	paths, err := store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var want []string
	for _, rel := range linkedRootTracks {
		want = append(want, prefix+rel)
	}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("the store holds %v, want %v", paths, want)
	}
	resolver := bridgefs.New(roots)
	for _, rel := range linkedRootTracks {
		served, info, err := resolver.ResolveChecked(prefix + rel)
		if err != nil {
			t.Fatalf("fs.Resolver cannot serve the row %s: %v", prefix+rel, err)
		}
		read := statOf(t, filepath.Join(dir, filepath.FromSlash(rel)))
		if !os.SameFile(info, read) {
			t.Errorf("the row %s is served from %s, which is not the file the scanner read", prefix+rel, served)
		}
		requireRowStat(t, store, prefix+rel, read, info)
	}
}

// TestScanner_ALinkedRootIsWalkedThrough is the defect: a root that is a link
// to a directory, or a link to such a link, indexed nothing (measured: 0 rows
// against 2 with the same root spelled with a trailing slash), by Scan or by
// a subtree scan of the root. Walked through, the rows are the ones a
// directory root gives, and fs.Resolver serves each from the file the scanner
// read.
func TestScanner_ALinkedRootIsWalkedThrough(t *testing.T) {
	target := t.TempDir()
	seedLinkedRootLibrary(t, target)
	base := t.TempDir()
	link := filepath.Join(base, "music")
	linkOrSkip(t, target, link)
	chain := filepath.Join(base, "chained")
	linkOrSkip(t, link, chain)

	for _, c := range []struct {
		name, root string
		subtree    bool
	}{
		{"a scan of a link", link, false},
		{"a scan of a link to a link", chain, false},
		{"a subtree scan of the root, a link", link, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, sc := newScanFixture(t, c.root)
			if c.subtree {
				if _, err := sc.ScanSubtree(context.Background(), c.root); err != nil {
					t.Fatalf("subtree scan: %v", err)
				}
			} else {
				scanOnce(t, sc, c.name)
			}
			requireRowsResolveTo(t, store, []string{c.root}, "", target)
		})
	}
}

// TestScanner_ALinkedRootInMultiRootModeKeepsItsConfiguredName: in multi-root
// mode a stored path leads with its root's basename, which fs.Resolver routes
// by, so a linked root's rows must lead with the CONFIGURED root's name,
// never its target's. Here the two differ: `music` links to `nas/library`.
func TestScanner_ALinkedRootInMultiRootModeKeepsItsConfiguredName(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nas", "library")
	seedLinkedRootLibrary(t, target)
	link := filepath.Join(base, "music")
	linkOrSkip(t, target, link)
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []string{link, other}

	for _, subtree := range []bool{false, true} {
		t.Run(fmt.Sprintf("subtree=%v", subtree), func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			sc := NewScanner(roots, store, "")
			if subtree {
				if _, err := sc.ScanSubtree(context.Background(), link); err != nil {
					t.Fatalf("subtree scan: %v", err)
				}
			} else {
				scanOnce(t, sc, "multi-root scan")
			}
			requireRowsResolveTo(t, store, roots, "music/", target)
		})
	}
}

// TestScanner_AnInstallWhoseRootBecameALinkKeepsItsRows is the half that
// deleted a library. Once the root became a link, every scan logged a
// suspected clean-empty mount and hinted at `.bridge-allow-empty`; the
// sentinel, created through the link, lands beside the files, and the next
// three scans deleted both rows and sent their tombstones, with both files on
// disk (measured at the production threshold of three). Walked through, the
// root is not empty, nothing is logged, nothing is rewritten, and a sentinel
// placed anyway changes nothing.
func TestScanner_AnInstallWhoseRootBecameALinkKeepsItsRows(t *testing.T) {
	f := newIndexedRoot(t)
	moved := f.moveBehindLink(t)
	rec := loggingtest.Record(t)
	for i := 1; i <= 3; i++ {
		scanOnce(t, f.sc, fmt.Sprintf("scan %d with the root a link", i))
	}
	if lines := rec.Lines(msgCleanEmpty); len(lines) > 0 {
		t.Fatalf("the guard called a root with every file behind its link empty:\n%s", strings.Join(lines, "\n"))
	}
	f.requireRowsKept(t, "with the root a link")

	// The hint the guard gave, followed.
	if err := os.WriteFile(filepath.Join(f.root, allowEmptySentinelFilename), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		scanOnce(t, f.sc, fmt.Sprintf("scan %d with the sentinel", i))
	}
	f.requireRowsKept(t, "with the sentinel placed through the link")
	requireRowsResolveTo(t, f.store, []string{f.root}, "", moved)
}

// TestScanner_ADanglingRootLinkIsNotAnEmptyLibrary: a root whose link
// dangles, because the mount it points into went away, is a root the scanner
// cannot see ("could not see" dominates), never an empty library. Scan
// already spared it; a subtree scan of it walked the link as one entry and
// reaped every row at the threshold (measured: three subtree scans, both rows
// deleted). It now stops before touching a row, and says why. The full
// scan's line names where the link points, which is the path an operator has
// to look at.
func TestScanner_ADanglingRootLinkIsNotAnEmptyLibrary(t *testing.T) {
	f := newIndexedRoot(t)
	moved := f.moveBehindLink(t)
	if err := os.Rename(moved, moved+".unmounted"); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	for i := 1; i <= 3; i++ {
		scanOnce(t, f.sc, fmt.Sprintf("scan %d with the mount gone", i))
		if _, err := f.sc.ScanSubtree(context.Background(), f.root); err == nil {
			t.Fatalf("subtree scan %d of a root it cannot see reported success", i)
		}
	}
	f.requireRowsKept(t, "with the mount gone")
	lines := rec.Lines(msgRootUnreachable)
	if len(lines) != 3 {
		t.Fatalf("%d %q lines over three scans, want 3:\n%s", len(lines), msgRootUnreachable, strings.Join(lines, "\n"))
	}
	dest, err := os.Readlink(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if !strings.Contains(line, " links_to="+dest+" ") {
			t.Errorf("the line does not name where the root links (%s): %s", dest, line)
		}
	}
	if lines := rec.Lines(msgCleanEmpty); len(lines) > 0 {
		t.Errorf("a root that cannot be seen was judged empty:\n%s", strings.Join(lines, "\n"))
	}
}

// TestScanner_ASubtreeScanOfAnEmptiedRootSparesItsRows: the clean-empty guard
// Scan runs for each root now runs for a subtree scan of the root. Nothing
// covered that scan: the owning-root audit answers a SUBTREE that is not
// there, so a subtree scan of an emptied mount point reaped every row at the
// threshold while Scan spared them (measured on a plain root: three subtree
// scans, both rows deleted, no line logged). A linked root whose target is
// the emptied mount point is the same case, and its line names the target
// and says where the sentinel goes. The sentinel still authorises the
// deletion: the positive control.
func TestScanner_ASubtreeScanOfAnEmptiedRootSparesItsRows(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("linked=%v", linked), func(t *testing.T) {
			f := newIndexedRoot(t)
			dir := f.root
			if linked {
				dir = f.moveBehindLink(t)
			}
			if err := os.RemoveAll(filepath.Join(dir, "Artist")); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			for i := 1; i <= 3; i++ {
				if _, err := f.sc.ScanSubtree(context.Background(), f.root); err != nil {
					t.Fatalf("subtree scan %d: %v", i, err)
				}
			}
			f.requireRowsKept(t, "subtree scans of an emptied root")
			requireCleanEmptyLines(t, rec.Lines(msgCleanEmpty), 3, linked, dir)

			if err := os.WriteFile(filepath.Join(f.root, allowEmptySentinelFilename), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 3; i++ {
				if _, err := f.sc.ScanSubtree(context.Background(), f.root); err != nil {
					t.Fatalf("subtree scan %d with the sentinel: %v", i, err)
				}
			}
			if paths, err := f.store.TrackPaths(context.Background()); err != nil || len(paths) != 0 {
				t.Fatalf("the sentinel did not authorise the deletion: %v (err %v)", paths, err)
			}
		})
	}
}

// requireCleanEmptyLines asserts want clean-empty lines, and that for a
// linked root each names the directory the link resolves to and says the
// sentinel goes there.
func requireCleanEmptyLines(t *testing.T, lines []string, want int, linked bool, dir string) {
	t.Helper()
	if len(lines) != want {
		t.Fatalf("%d %q lines, want %d:\n%s", len(lines), msgCleanEmpty, want, strings.Join(lines, "\n"))
	}
	if !linked {
		return
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if !strings.Contains(line, " links_to="+resolved+" ") || !strings.Contains(line, "directory it links to is empty") {
			t.Errorf("the line does not name the empty directory the root links to (%s): %s", resolved, line)
		}
	}
}

// TestWatcherWatchesALinkedLibraryRoot: walked as the link, the watcher's
// walk of a linked root registered no watch at all and returned nil
// (measured: zero watches, against three for the same tree named directly),
// so a file dropped into the library never reached the manifest until the
// next periodic scan. The drop here is two levels down, where only a watch
// the walk registered BELOW the root can see it.
func TestWatcherWatchesALinkedLibraryRoot(t *testing.T) {
	target := t.TempDir()
	album := filepath.Join(target, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "music")
	linkOrSkip(t, target, link)
	store, sc := newScanFixture(t, link)
	w, err := NewWatcher(sc, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	// Registered after the store's Close, so it runs first.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the watcher did not stop on cancel")
		}
	})
	// fsnotify's Add is synchronous; this is headroom for the initial walk.
	time.Sleep(100 * time.Millisecond)

	writeMinimalFLAC(t, filepath.Join(link, "Artist", "Album", "dropped.flac"), 44100, 16, map[string]string{"TITLE": "Dropped"})
	waitForTrack(t, store, "no watch below a linked library root: the dropped file never reached the manifest")
	if got := titleOf(t, store, "Artist/Album/dropped.flac"); got != "Dropped" {
		t.Errorf("title %q, want the dropped file's tag", got)
	}
}
