package manifest

// A link to a directory BELOW a library root is not walked (#1070: loops),
// and until 2026-09-29 nothing said so. Moving an album folder to another
// volume and leaving a link in its place (on Windows a junction, `mklink /J`)
// took it off every paired device in one of two ways, measured on main
// 6dfba62c (macOS APFS symlinks, and Windows 11 junctions):
//
//   - a root holding nothing but such links read as a suspected clean-empty
//     mount: an ERROR every scan, rows kept, and the hint to place
//     `.bridge-allow-empty`. Following it deleted both rows, a tombstone for
//     each, while the file still stat'ed through the link;
//   - a root holding a link beside real content lost the rows under the link
//     at the third scan, with a tombstone for each and no line naming
//     anything (an Info count, "tracks missing this scan missing=2").
//
// The scanner still follows no such link, and a root holding only links still
// keeps its rows. What changed is what it says: the guard's line names the
// links, says the sentinel deletes the rows under them, and says to make a
// linked directory a library root of its own; and when rows under a link
// start going, one Warn says so, once per row's streak rather than once per
// scan (the M-SEARCH rule), naming the link.
//
// The messages are spelled out here rather than taken from the package's
// constants, so this file builds against the code before the change too.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

const (
	// msgOnlyDirLinksLine is the guard's line for a root holding nothing but
	// links to directories.
	msgOnlyDirLinksLine = "library root holds no content but links to directories, which the scanner does not follow; its rows are kept"
	// msgDirLinkRowsLine is the deletion pass's line when rows under such a
	// link start going.
	msgDirLinkRowsLine = "rows under links to directories are counted missing: the scanner does not follow links below a library root"
	// oldSentinelHint is the hint the guard gave for such a root, whose
	// advice deleted the rows under the links.
	oldSentinelHint = "place .bridge-allow-empty at the root to confirm intent"
)

// dirLinkLibrary is a library root indexed while every directory in it is a
// directory, by a scanner that reaps a missing row at the production
// threshold (three scans), in single-root mode or beside a second root.
type dirLinkLibrary struct {
	base   string
	root   string
	prefix string // "" single-root, "music/" multi-root
	roots  []string
	store  *Store
	sc     *Scanner
	since  time.Time
}

// newDirLinkLibrary writes tracks (paths relative to the root) and indexes
// them. In multi-root mode a second root, `other`, holds a track of its own
// and a link to a directory that nothing was ever indexed under, and it is
// walked first: a line about the root under test must name that root's
// links, not every link the scan has met so far.
func newDirLinkLibrary(t *testing.T, multiRoot bool, tracks ...string) dirLinkLibrary {
	t.Helper()
	base := t.TempDir()
	lib := dirLinkLibrary{base: base, root: filepath.Join(base, "music")}
	for i, rel := range tracks {
		p := filepath.Join(lib.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": fmt.Sprintf("Track %d", i+1)})
	}
	lib.roots = []string{lib.root}
	if multiRoot {
		other := filepath.Join(base, "other")
		p := filepath.Join(other, "Other", "Album", "01.flac")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Other"})
		never := filepath.Join(base, "never-indexed")
		if err := os.MkdirAll(never, 0o755); err != nil {
			t.Fatal(err)
		}
		linkDirOrSkip(t, never, filepath.Join(other, "Linked"))
		lib.roots = []string{other, lib.root}
		lib.prefix = "music/"
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lib.store = store
	lib.sc = lib.newScanner()
	lib.since = time.Now().Add(-time.Minute)
	scanOnce(t, lib.sc, "while every directory is a directory")
	return lib
}

// newScanner is a scanner over the library's roots and store at the
// production threshold: what a restart of the bridge builds.
func (l dirLinkLibrary) newScanner() *Scanner {
	sc := NewScanner(l.roots, l.store, "")
	sc.SetDeleteThreshold(3)
	return sc
}

// linkBack moves the directory at rel (relative to the root) out of the root
// and leaves a link to it in its place, as an operator does who moves an
// album folder to another volume. Returns where the directory is now.
func (l dirLinkLibrary) linkBack(t *testing.T, rel string) string {
	t.Helper()
	moved := filepath.Join(l.base, "elsewhere", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(l.root, filepath.FromSlash(rel)), moved); err != nil {
		t.Fatal(err)
	}
	linkDirOrSkip(t, moved, filepath.Join(l.root, filepath.FromSlash(rel)))
	return moved
}

// unlink puts the directory linkBack moved back in place of its link.
func (l dirLinkLibrary) unlink(t *testing.T, rel, moved string) {
	t.Helper()
	link := filepath.Join(l.root, filepath.FromSlash(rel))
	// A junction is removed by os.Remove as a directory; a symlink as a file.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, link); err != nil {
		t.Fatal(err)
	}
}

// scan runs a full scan, or a subtree scan of dir (relative to the root, ""
// for the root itself), failing the test on an error.
func (l dirLinkLibrary) scan(t *testing.T, sc *Scanner, subtree bool, dir string) {
	t.Helper()
	if !subtree {
		scanOnce(t, sc, "full scan")
		return
	}
	abs := filepath.Join(l.root, filepath.FromSlash(dir))
	if _, err := sc.ScanSubtree(context.Background(), abs); err != nil {
		t.Fatalf("subtree scan of %s: %v", abs, err)
	}
}

// rows returns the paths the store holds under the root, sorted.
func (l dirLinkLibrary) rows(t *testing.T) []string {
	t.Helper()
	paths, err := l.store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		if strings.HasPrefix(p, l.prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// tombstones returns the deletion journal's paths since the library was
// built, sorted: what every paired device was told is gone.
func (l dirLinkLibrary) tombstones(t *testing.T) []string {
	t.Helper()
	deleted, _, err := l.store.DeletedSince(context.Background(), l.since)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(deleted)
	return deleted
}

// attrIn returns the value a rendered line gives key, the text up to the next
// " name=" of one of next, or to the end of the line when next is empty.
func attrIn(t *testing.T, line, key string, next ...string) string {
	t.Helper()
	_, rest, ok := strings.Cut(line, " "+key+"=")
	if !ok {
		t.Fatalf("the line has no %s: %s", key, line)
	}
	end := len(rest)
	for _, n := range next {
		if i := strings.Index(rest, " "+n+"="); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

// requireOnlyDirLinksLines asserts want guard lines for a root holding
// nothing but the link at example, each counting rowsInDB rows of which
// underLinks are under the links, naming the link library-relative and never
// the directory it leads to, and giving a hint that says the sentinel deletes
// those rows and how to index a linked directory. The old line, the
// mount-failure one with the old hint, must not be there at all.
func requireOnlyDirLinksLines(t *testing.T, rec *loggingtest.Recorder, want int, example string, rowsInDB, underLinks int, elsewhere string) {
	t.Helper()
	lines := rec.Lines(msgOnlyDirLinksLine)
	if len(lines) != want {
		t.Fatalf("%d %q lines, want %d; every line:\n%s", len(lines), msgOnlyDirLinksLine, want, strings.Join(rec.All(), "\n"))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "ERROR ") {
			t.Errorf("the guard's line is not an ERROR: %s", line)
		}
		for key, value := range map[string]string{
			"rows_in_db":       fmt.Sprint(rowsInDB),
			"rows_under_links": fmt.Sprint(underLinks),
			"dir_links":        "1",
			"example":          example,
		} {
			if got := attrIn(t, line, key, "rows_in_db", "rows_under_links", "dir_links", "example", "hint"); got != value {
				t.Errorf("%s=%s, want %s: %s", key, got, value, line)
			}
		}
		hint := attrIn(t, line, "hint")
		for _, says := range []string{
			"follows no link to a directory below a library root",
			"Placing .bridge-allow-empty lets the scans delete every row kept here",
			"a tombstone to every paired device",
			"add that directory as a library root of its own",
		} {
			if !strings.Contains(hint, says) {
				t.Errorf("the hint does not say %q: %s", says, line)
			}
		}
		if strings.Contains(line, elsewhere) {
			t.Errorf("the line names the directory the link leads to, %s, by its absolute path: %s", elsewhere, line)
		}
	}
	if old := rec.Lines(msgCleanEmpty); len(old) > 0 {
		t.Errorf("a root holding links to directories was called a suspected mount failure:\n%s", strings.Join(old, "\n"))
	}
	for _, line := range rec.All() {
		if strings.Contains(line, oldSentinelHint) {
			t.Errorf("a line still tells the operator to place the sentinel as if it were harmless: %s", line)
		}
	}
}

// requireDirLinkRowsLines asserts want deletion-pass lines, each a Warn naming
// the link at example, rows rows and the threshold, with a hint that says the
// rows go with a tombstone to every paired device and how to keep them.
func requireDirLinkRowsLines(t *testing.T, rec *loggingtest.Recorder, want int, example string, rows int) {
	t.Helper()
	lines := rec.Lines(msgDirLinkRowsLine)
	if len(lines) != want {
		t.Fatalf("%d %q lines, want %d; every line:\n%s", len(lines), msgDirLinkRowsLine, want, strings.Join(rec.All(), "\n"))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "WARN ") {
			t.Errorf("the line is not a Warn: %s", line)
		}
		for key, value := range map[string]string{
			"dir_links": "1",
			"example":   example,
			"rows":      fmt.Sprint(rows),
			"threshold": "3",
		} {
			if got := attrIn(t, line, key, "dir_links", "example", "rows", "threshold", "hint"); got != value {
				t.Errorf("%s=%s, want %s: %s", key, got, value, line)
			}
		}
		hint := attrIn(t, line, "hint")
		for _, says := range []string{"a tombstone to every paired device", "add the directory each link leads to as a library root of its own"} {
			if !strings.Contains(hint, says) {
				t.Errorf("the hint does not say %q: %s", says, line)
			}
		}
	}
}

// TestScanner_ARootHoldingOnlyLinksToDirectoriesNamesThem is shape (a). The
// root's album folder moved to another volume and a link took its place:
// nothing the walk reads is left, so the guard keeps the rows, as it did. Its
// line used to call that a suspected clean-empty mount and to hint at the
// sentinel, which then deleted both rows (a tombstone each) while the files
// were there through the link (measured on 6dfba62c, full scans, subtree
// scans of the root, and multi-root alike). The line now names the link, says
// what the sentinel does to the rows under it, and says to make the linked
// directory a library root. Placed anyway, the sentinel still authorises the
// deletion (the positive control), and the deletion pass then says, once,
// that rows under a link are going.
func TestScanner_ARootHoldingOnlyLinksToDirectoriesNamesThem(t *testing.T) {
	for _, multiRoot := range []bool{false, true} {
		for _, subtree := range []bool{false, true} {
			t.Run(fmt.Sprintf("multiRoot=%v/subtree=%v", multiRoot, subtree), func(t *testing.T) {
				lib := newDirLinkLibrary(t, multiRoot, "Artist/Album/01.flac", "Artist/Album/02.flac")
				indexed := lib.rows(t)
				elsewhere := lib.linkBack(t, "Artist")
				rec := loggingtest.Record(t)
				for i := 1; i <= 3; i++ {
					lib.scan(t, lib.sc, subtree, "")
				}
				if got := lib.rows(t); strings.Join(got, " ") != strings.Join(indexed, " ") {
					t.Fatalf("rows %v after three scans, want every row kept: %v", got, indexed)
				}
				requireNoTombstones(t, lib.store, lib.since, "a root holding only a link")
				requireOnlyDirLinksLines(t, rec, 3, lib.prefix+"Artist", 2, 2, elsewhere)
				requireDirLinkRowsLines(t, rec, 0, "", 0)

				// The sentinel, placed anyway.
				if err := os.WriteFile(filepath.Join(lib.root, allowEmptySentinelFilename), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= 3; i++ {
					lib.scan(t, lib.sc, subtree, "")
				}
				if got := lib.rows(t); len(got) != 0 {
					t.Fatalf("the sentinel did not authorise the deletion: rows %v", got)
				}
				if got := lib.tombstones(t); strings.Join(got, " ") != strings.Join(indexed, " ") {
					t.Errorf("tombstones %v, want one for each row", got)
				}
				if _, err := os.Stat(filepath.Join(lib.root, "Artist", "Album", "01.flac")); err != nil {
					t.Errorf("the file is no longer there through the link: %v", err)
				}
				requireDirLinkRowsLines(t, rec, 1, lib.prefix+"Artist", 2)
			})
		}
	}
}

// TestScanner_RowsUnderALinkBesideContentAreAnnouncedOncePerStreak is shape
// (b). A root holding a link to a directory beside real content is not empty,
// so the rows under the link take the missing-count grace and go at the
// threshold, a tombstone each. That is unchanged: the scanner follows no such
// link. What changed is that it is said, once, when the rows start going, in
// a full scan, a subtree scan of the root, and a subtree scan of the folder
// holding the link (the watcher's scan when a link appears). Before, the only
// line was an Info count naming nothing.
func TestScanner_RowsUnderALinkBesideContentAreAnnouncedOncePerStreak(t *testing.T) {
	for _, tc := range []struct {
		name    string
		link    string // the linked directory, relative to the root
		subtree bool
		scanned string // the subtree scanned, relative to the root
	}{
		{name: "full scans", link: "Artist"},
		{name: "subtree scans of the root", link: "Artist", subtree: true},
		{name: "subtree scans of the folder holding the link", link: "Genre/Artist", subtree: true, scanned: "Genre"},
		{name: "full scans, the link below a folder", link: "Genre/Artist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lib := newDirLinkLibrary(t, false,
				tc.link+"/Album/01.flac", tc.link+"/Album/02.flac", "Other/Album/01.flac")
			underLink := []string{tc.link + "/Album/01.flac", tc.link + "/Album/02.flac"}
			lib.linkBack(t, tc.link)
			rec := loggingtest.Record(t)
			for i := 1; i <= 3; i++ {
				lib.scan(t, lib.sc, tc.subtree, tc.scanned)
			}
			if got := lib.rows(t); strings.Join(got, " ") != "Other/Album/01.flac" {
				t.Fatalf("rows %v after three scans, want only the row beside the link", got)
			}
			if got := lib.tombstones(t); strings.Join(got, " ") != strings.Join(underLink, " ") {
				t.Errorf("tombstones %v, want %v", got, underLink)
			}
			requireDirLinkRowsLines(t, rec, 1, tc.link, 2)
			if lines := rec.Lines(msgOnlyDirLinksLine); len(lines) > 0 {
				t.Errorf("a root holding content beside the link got the guard's line:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// TestScanner_ALinkStreakIsSaidOnceAcrossARestartAndAgainWhenItRestarts: the
// streak is the ROW's (missing_count going from 0), not the process's. A
// bridge restarted mid-streak says nothing more, and rows that came back
// (the link replaced by the directory again, rescanned) and then went again
// start a streak of their own, which is said again.
func TestScanner_ALinkStreakIsSaidOnceAcrossARestartAndAgainWhenItRestarts(t *testing.T) {
	lib := newDirLinkLibrary(t, false, "Artist/Album/01.flac", "Artist/Album/02.flac", "Other/Album/01.flac")
	moved := lib.linkBack(t, "Artist")
	rec := loggingtest.Record(t)
	lib.scan(t, lib.sc, false, "")
	restarted := lib.newScanner()
	lib.scan(t, restarted, false, "")
	requireDirLinkRowsLines(t, rec, 1, "Artist", 2)

	lib.unlink(t, "Artist", moved)
	lib.scan(t, restarted, false, "")
	if got := lib.rows(t); len(got) != 3 {
		t.Fatalf("rows %v once the directory is back, want all three", got)
	}
	lib.linkBack(t, "Artist")
	lib.scan(t, restarted, false, "")
	requireDirLinkRowsLines(t, rec, 2, "Artist", 2)
}

// TestScanner_ALinkToADirectoryNamedLikeATrackIsNotContent: whatever its name,
// a link to a directory is not library content. A folder named like a track
// (`Live.flac`) that became a link counted as content by its name alone, so a
// root holding nothing else passed the guard and its rows went at the
// threshold with no line (measured on 6dfba62c): the guard's case turned into
// the silent one. It is the guard's case now, and the entry is still reported
// as one that is not a file, as it always was.
func TestScanner_ALinkToADirectoryNamedLikeATrackIsNotContent(t *testing.T) {
	lib := newDirLinkLibrary(t, false, "Live.flac/Disc/01.flac", "Live.flac/Disc/02.flac")
	indexed := lib.rows(t)
	elsewhere := lib.linkBack(t, "Live.flac")
	rec := loggingtest.Record(t)
	for i := 1; i <= 3; i++ {
		lib.scan(t, lib.sc, false, "")
	}
	if got := lib.rows(t); strings.Join(got, " ") != strings.Join(indexed, " ") {
		t.Fatalf("rows %v after three scans, want every row kept: %v", got, indexed)
	}
	requireNoTombstones(t, lib.store, lib.since, "a root holding only a link named like a track")
	requireOnlyDirLinksLines(t, rec, 3, "Live.flac", 2, 2, elsewhere)
	requireNotFilesLines(t, rec.Lines(msgNotFiles), 3, 1, "Live.flac", "directory", elsewhere)
}

// TestScanner_ASubtreeMissBesideOnlyLinksNamesThem: the guard's twin for a
// subtree scan, the owning-root audit that refuses to reap a subtree that is
// not there when the root holds nothing else, gave the same advice: "suspected
// mount drop; place .bridge-allow-empty to confirm intent". Over a root whose
// only entries are links to directories, its refusal now says what the
// guard's line says. The rows are kept either way. A link named like a track
// is no more content to the audit than to the walks: counted by its name, it
// let the audit pass and the subtree's rows go (measured on 6dfba62c).
func TestScanner_ASubtreeMissBesideOnlyLinksNamesThem(t *testing.T) {
	for _, link := range []string{"Artist", "Live.flac"} {
		t.Run(link, func(t *testing.T) {
			lib := newDirLinkLibrary(t, false, link+"/Album/01.flac", link+"/Album/02.flac", "Gone/Album/01.flac")
			indexed := lib.rows(t)
			if err := os.RemoveAll(filepath.Join(lib.root, "Gone")); err != nil {
				t.Fatal(err)
			}
			lib.linkBack(t, link)
			rec := loggingtest.Record(t)
			_, err := lib.sc.ScanSubtree(context.Background(), filepath.Join(lib.root, "Gone"))
			if err == nil {
				t.Fatal("a subtree scan over a root holding only a link went on to its deletion pass")
			}
			for _, says := range []string{
				"2 of them under the links to directories it holds (1, e.g. " + link + ")",
				"which the scanner does not follow",
				"placing .bridge-allow-empty would delete them, with a tombstone to every paired device",
				"add that directory as a library root of its own",
			} {
				if !strings.Contains(err.Error(), says) {
					t.Errorf("the refusal does not say %q: %v", says, err)
				}
			}
			if strings.Contains(err.Error(), "suspected mount drop") {
				t.Errorf("the refusal still calls a root holding links a mount drop: %v", err)
			}
			if lines := rec.Lines(msgSubtreeAudit); len(lines) != 1 {
				t.Errorf("%d %q lines, want 1", len(lines), msgSubtreeAudit)
			}
			if got := lib.rows(t); strings.Join(got, " ") != strings.Join(indexed, " ") {
				t.Fatalf("rows %v, want every row kept: %v", got, indexed)
			}
		})
	}
}
