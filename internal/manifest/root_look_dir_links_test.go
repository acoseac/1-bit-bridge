package manifest

// B742: the watcher's look at a library root (lookAtRoot) read a root holding
// nothing but links to directories, with tracks indexed under it, as an
// emptied mount point (reads=empty), and the line the watcher logs when it
// holds a root said to check that the root's volume is mounted. The scan's
// clean-empty guard tells that root apart (B74: a link the walk stat'ed
// through to a directory says the volume holding the root is there) and says
// so (msgRootOnlyDirLinks). Nothing was lost, since the watcher holds such a
// root as the scan spares its rows; the hint was wrong. The look reads a
// root as the scan's guards do, through the one reading both share, and only
// the hint changes: every root held before is held, and every row spared
// before is spared.
//
// The look's word for that case, "only-dir-links", is spelled out here, so
// this file builds against the code before the change too.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// readsOnlyDirLinks is the held line's reads= for a root holding nothing but
// links to directories, over tracks the index holds under it.
const readsOnlyDirLinks = "only-dir-links"

// rootLookRow is one shape of a library root, music, after a scan indexed the
// tracks it held (indexed, none for a root never scanned): what the watcher's
// look at it does (held, and the held line's reads= and hint=) and what a
// full scan at a threshold of one leaves of the rows (after), the line the
// scan's guard logs for it (scanSays, "" for none of them).
type rootLookRow struct {
	name     string
	indexed  []string
	shape    func(t *testing.T, root, aside string)
	held     bool
	reads    string
	hint     string
	scanSays string
	after    []string
}

// moveOut renames rel, under root, to the same path under aside, as an
// operator moves a folder or a file to another volume.
func moveOut(t *testing.T, root, aside, rel string) string {
	t.Helper()
	to := filepath.Join(aside, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(rel)), to); err != nil {
		t.Fatal(err)
	}
	return to
}

// linkBackDir moves the folder rel out of root and leaves a link to it (a
// junction on Windows) in its place.
func linkBackDir(t *testing.T, root, aside, rel string) string {
	t.Helper()
	moved := moveOut(t, root, aside, rel)
	linkDirOrSkip(t, moved, filepath.Join(root, filepath.FromSlash(rel)))
	return moved
}

func rootLookRows() []rootLookRow {
	album := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	return []rootLookRow{
		{
			name:    "absent",
			indexed: album,
			shape: func(t *testing.T, root, aside string) {
				moveOut(t, filepath.Dir(root), aside, filepath.Base(root))
			},
			held: true, reads: rootAbsentUnreachable, hint: heldRootHint, scanSays: msgRootUnreachable,
			after: album,
		},
		{
			name:    "empty",
			indexed: album,
			shape: func(t *testing.T, root, aside string) {
				moveOut(t, root, aside, "Artist")
			},
			held: true, reads: rootAbsentEmpty, hint: heldRootHint, scanSays: msgCleanEmpty,
			after: album,
		},
		{
			name:    "only directory links, with indexed rows",
			indexed: album,
			shape: func(t *testing.T, root, aside string) {
				linkBackDir(t, root, aside, "Artist")
			},
			held: true, reads: readsOnlyDirLinks, hint: msgOnlyDirLinksLine, scanSays: msgOnlyDirLinksLine,
			after: album,
		},
		{
			name: "only directory links, without indexed rows",
			shape: func(t *testing.T, root, aside string) {
				writeTracks(t, filepath.Join(aside, "Artist", "Album", "01.flac"))
				linkDirOrSkip(t, filepath.Join(aside, "Artist"), filepath.Join(root, "Artist"))
			},
		},
		{
			name:    "mixed directory and file links",
			indexed: append([]string{"Single.flac"}, album...),
			shape: func(t *testing.T, root, aside string) {
				linkBackDir(t, root, aside, "Artist")
				linkOrSkip(t, moveOut(t, root, aside, "Single.flac"), filepath.Join(root, "Single.flac"))
			},
			after: []string{"Single.flac"},
		},
		{
			name:    "a dangling directory link",
			indexed: album,
			shape: func(t *testing.T, root, aside string) {
				moved := linkBackDir(t, root, aside, "Artist")
				if err := os.RemoveAll(moved); err != nil {
					t.Fatal(err)
				}
			},
			held: true, reads: rootAbsentEmpty, hint: heldRootHint, scanSays: msgCleanEmpty,
			after: album,
		},
		{
			name:    "real content",
			indexed: append([]string{"Gone/Album/01.flac"}, album...),
			shape: func(t *testing.T, root, aside string) {
				moveOut(t, root, aside, "Gone")
			},
			after: album,
		},
	}
}

// heldLines returns the reads= and hint= of each line the watcher logged on
// holding a root.
func heldLines(rec *loggingtest.Recorder) [][2]string {
	var out [][2]string
	for _, r := range rec.Records() {
		if r.Message != msgWatchRootHeld {
			continue
		}
		var reads, hint string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reads":
				reads = a.Value.String()
			case "hint":
				hint = a.Value.String()
			}
			return true
		})
		out = append(out, [2]string{reads, hint})
	}
	return out
}

// TestTheWatchersHoldAndHintReadARootAsTheScanDoes is B742's table: for each
// shape of a library root, the watcher's look at it, as Run takes it before
// it adds any watch (holdAbsentRoots), holds it or watches it, and its held
// line names what it found and gives the hint for it; and a full scan spares
// every row of a root the watcher holds, logging its guard's own line, and
// prunes the rows of files that went from a root it watches.
func TestTheWatchersHoldAndHintReadARootAsTheScanDoes(t *testing.T) {
	for _, row := range rootLookRows() {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			root, aside := filepath.Join(dir, "music"), filepath.Join(dir, "aside")
			for _, d := range []string{root, aside} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var files []string
			for _, rel := range row.indexed {
				files = append(files, filepath.Join(root, filepath.FromSlash(rel)))
			}
			writeTracks(t, files...)
			store := openStoreIn(t, dir)
			sc := NewScanner([]string{root}, store, "")
			sc.SetDeleteThreshold(1)
			if len(row.indexed) > 0 {
				scanOnce(t, sc, "while the root holds its tracks")
				mustHoldRows(t, store, row.indexed...)
			}
			row.shape(t, root, aside)

			w, err := NewWatcher(sc, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.w.Close() })
			rec := loggingtest.Record(t)
			watch := w.holdAbsentRoots(context.Background(), sc.Roots())
			_, held := w.held[root]
			if held != row.held || held == slices.Contains(watch, root) {
				t.Fatalf("the watcher held the root: %v (to watch: %q), want %v", held, watch, row.held)
			}
			lines := heldLines(rec)
			switch {
			case !row.held && len(lines) != 0:
				t.Errorf("the watcher said it held a root it watches: %q", lines)
			case row.held && len(lines) != 1:
				t.Errorf("the watcher said it held the root %d times, want once: %q", len(lines), lines)
			case row.held:
				if lines[0][0] != row.reads {
					t.Errorf("the held line reads=%q, want %q", lines[0][0], row.reads)
				}
				if lines[0][1] != row.hint {
					t.Errorf("the held line's hint is\n  %q\nwant\n  %q", lines[0][1], row.hint)
				}
			}

			scanOnce(t, sc, "after the root changed")
			paths, err := store.TrackPaths(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(paths)
			want := slices.Clone(row.after)
			sort.Strings(want)
			if !slices.Equal(paths, want) {
				t.Errorf("rows %q after a scan at a threshold of one, want %q", paths, want)
			}
			mustHoldRows(t, store, row.after...)
			for _, msg := range []string{msgRootUnreachable, msgCleanEmpty, msgOnlyDirLinksLine} {
				got := len(rec.Lines(msg))
				if want := 0; msg == row.scanSays {
					if got != 1 {
						t.Errorf("the scan said %q %d times, want once", msg, got)
					}
				} else if got != want {
					t.Errorf("the scan said %q %d times, want never: %s", msg, got, strings.Join(rec.Lines(msg), "\n"))
				}
			}
		})
	}
}

// TestAParentScanSparesANestedRootHoldingOnlyLinksToDirectories: the look's
// answer also decides, before a walk of a root, which roots nested in it the
// walk spares (protectNestedRoots). A nested root holding nothing but a link
// to a directory, over tracks indexed under it, is spared by every scan of
// the root it is nested in, as it was: its rows, the outer root's under its
// folder and its own, stay with no missed scan counted, past the threshold.
func TestAParentScanSparesANestedRootHoldingOnlyLinksToDirectories(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	inner := filepath.Join(outer, "usb", "library")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"), filepath.Join(inner, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	sc := NewScanner([]string{outer, inner}, store, "")
	sc.SetDeleteThreshold(3)
	scanOnce(t, sc, "while every folder is a folder")
	rows := []string{"music/Artist/Album/01.flac", "music/usb/library/Band/Record/01.flac", "library/Band/Record/01.flac"}
	mustHoldRows(t, store, rows...)
	linkBackDir(t, inner, filepath.Join(dir, "elsewhere"), "Band")

	ctx := context.Background()
	if look := sc.lookAtRoot(ctx, inner); !look.absent {
		t.Fatalf("the look at the nested root does not spare it: %+v", look)
	}
	for range 4 {
		if _, err := sc.ScanSubtree(ctx, outer); err != nil {
			t.Fatal(err)
		}
		scanOnce(t, sc, "a full scan")
	}
	mustHoldRows(t, store, rows...)
	mustHaveMissingCount(t, store, "folders", "music/usb/library/Band/Record", 0)
	mustHaveMissingCount(t, store, "folders", "library/Band/Record", 0)
}
