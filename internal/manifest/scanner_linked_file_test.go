package manifest

// A file the walk reaches through a link is indexed under its TARGET's stat.
//
// filepath.WalkDir hands each entry an lstat, so for a symlinked audio file
// the scanner used to record the LINK's size (the length of the path it
// stores) and the link's mtime, while it read the tags from the target and
// every endpoint serves the target's bytes. The skip gate compared the same
// link stat, so a change to the target was never re-extracted, and a check
// that compares a row with a live stat of the file disagreed with the row:
// the /v1/lyrics drift check answered 410 for its embedded lyrics.
// /v1/list and /v1/stat already describe a link by its target
// (PROTOCOL.md), so the manifest was the one surface that did not.
//
// A link whose target cannot be stat'ed (dangling, or into a mount that went
// away) is a path whose content the walk could not see: its row is kept, as
// it was, and nothing is minted for a link that never had one. A link to a
// DIRECTORY is not a file, and the walk still follows no directory link.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// msgUnreadableLinks is the line a walk logs once for the links it could not
// follow.
const msgUnreadableLinks = "links whose target could not be read; their rows are kept"

// linkOrSkip makes link a symbolic link to target, and skips the test on a
// host that cannot make one (Windows without the privilege).
func linkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}
}

// statOf stats p, following a link.
func statOf(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// lstatOf lstats p: a link's own stat.
func lstatOf(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// setMTime gives p (a link's target, when p is one) the mtime at.
func setMTime(t *testing.T, p string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// requireRowStat asserts that the row at rel records want's size and mtime,
// both in the columns the skip gate reads and in the JSON the manifest serves.
// link is the link's own stat, named in the failure so a regression reads as
// what it is.
func requireRowStat(t *testing.T, store *Store, rel string, want, link os.FileInfo) {
	t.Helper()
	ctx := context.Background()
	st, err := store.GetTrackStat(ctx, rel)
	if err != nil || st == nil {
		t.Fatalf("%s: no row (err %v)", rel, err)
	}
	if st.Size != want.Size() || st.MTimeNS != want.ModTime().UnixNano() {
		t.Errorf("%s: the row records %d bytes at %v; its target is %d bytes at %v (the link itself is %d bytes at %v)",
			rel, st.Size, time.Unix(0, st.MTimeNS).UTC(), want.Size(), want.ModTime().UTC(),
			link.Size(), link.ModTime().UTC())
	}
	tr, err := store.GetTrack(ctx, rel)
	if err != nil || tr == nil {
		t.Fatalf("%s: GetTrack: %v", rel, err)
	}
	if tr.Size != want.Size() || !tr.ModTime.Equal(want.ModTime()) {
		t.Errorf("%s: the manifest serves size %d, mtime %v; the target is %d at %v",
			rel, tr.Size, tr.ModTime, want.Size(), want.ModTime().UTC())
	}
}

// linkedFixture is a library whose album holds links to files parked outside
// it, as an operator parks an album on another volume.
type linkedFixture struct {
	root, album, parked string
	store               *Store
	sc                  *Scanner
}

// newLinkedFixture builds the library, its store and scanner, and the
// directory the links point into.
func newLinkedFixture(t *testing.T) linkedFixture {
	t.Helper()
	root := t.TempDir()
	album := filepath.Join(root, "Music", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	store, sc := newScanFixture(t, root)
	return linkedFixture{root: root, album: album, parked: t.TempDir(), store: store, sc: sc}
}

// linkedFormat writes one audio format with a given title.
type linkedFormat struct {
	name  string
	file  string
	write func(t *testing.T, path, title string)
}

var linkedFormats = []linkedFormat{
	{"FLAC", "01.flac", func(t *testing.T, path, title string) {
		writeMinimalFLAC(t, path, 44100, 16, map[string]string{"TITLE": title, "ALBUM": "Parked"})
	}},
	{"DSF", "02.dsf", func(t *testing.T, path, title string) {
		writeMinimalDSF(t, path, 2822400, map[string]string{"title": title, "album": "Parked"})
	}},
}

// TestScanner_ALinkedTrackIsIndexedUnderItsTargetsStat is the defect: the
// row of a symlinked file recorded the link's own size and mtime, which the
// phone stores as the file's size and bounds its byte-range reads by.
func TestScanner_ALinkedTrackIsIndexedUnderItsTargetsStat(t *testing.T) {
	f := newLinkedFixture(t)
	parkedAt := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, lf := range linkedFormats {
		target := filepath.Join(f.parked, lf.file)
		lf.write(t, target, "Linked "+lf.name)
		setMTime(t, target, parkedAt)
		linkOrSkip(t, target, filepath.Join(f.album, lf.file))
	}
	scanOnce(t, f.sc, "initial")

	indexed := make(map[string]int64, len(linkedFormats))
	for _, lf := range linkedFormats {
		rel := "Music/Album/" + lf.file
		link := filepath.Join(f.album, lf.file)
		requireRowStat(t, f.store, rel, statOf(t, link), lstatOf(t, link))
		if got, want := titleOf(t, f.store, rel), "Linked "+lf.name; got != want {
			t.Errorf("%s: title %q, want %q: the tags come from the target", rel, got, want)
		}
		indexed[rel] = indexedAt(t, f.store, rel)
	}

	// A subtree scan (the watcher's) takes the same stat, so it finds every
	// row unchanged and rewrites none. Taking the link's own would read
	// each row as changed and rewrite it under the link's stat.
	if _, err := f.sc.ScanSubtree(context.Background(), f.album); err != nil {
		t.Fatalf("subtree scan: %v", err)
	}
	for rel, was := range indexed {
		if got := indexedAt(t, f.store, rel); got != was {
			t.Errorf("%s: the subtree scan rewrote the row (indexed_at %d, was %d)", rel, got, was)
		}
	}
}

// TestWalkedFileInfoStatsThroughEverythingButARegularFile drives the decision
// alone, for shapes a host may not be able to make. Since Go 1.23 a Windows
// junction reports ModeIrregular and no ModeDir, so to the walk it is neither
// a regular file nor a directory, and only a stat through it says it names a
// directory; a Windows cloud placeholder is ModeIrregular too, and a stat
// through it answers for itself. A regular file answers from its own stat and
// never pays the second one.
func TestWalkedFileInfoStatsThroughEverythingButARegularFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "01.flac")
	if err := os.WriteFile(file, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileInfo, dirInfo := statOf(t, file), statOf(t, dir)
	failed := errors.New("stat failed")
	type answer struct {
		info os.FileInfo
		err  error
	}
	for _, tc := range []struct {
		name         string
		typ          fs.FileMode
		own, through answer
		want         walkedFileVerdict
		wantInfo     os.FileInfo
		wantThrough  int
	}{
		{"a regular file", 0, answer{fileInfo, nil}, answer{nil, failed},
			walkedFileIndex, fileInfo, 0},
		{"a regular file whose own stat failed", 0, answer{nil, failed}, answer{fileInfo, nil},
			walkedFileStatFailed, nil, 0},
		{"a symlink to a file", fs.ModeSymlink, answer{nil, failed}, answer{fileInfo, nil},
			walkedFileIndex, fileInfo, 1},
		{"a dangling symlink", fs.ModeSymlink, answer{fileInfo, nil}, answer{nil, failed},
			walkedFileTargetUnreadable, nil, 1},
		{"a symlink to a directory", fs.ModeSymlink, answer{fileInfo, nil}, answer{dirInfo, nil},
			walkedFileNotAFile, nil, 1},
		{"a Windows junction to a directory", fs.ModeIrregular, answer{fileInfo, nil}, answer{dirInfo, nil},
			walkedFileNotAFile, nil, 1},
		{"a Windows cloud placeholder", fs.ModeIrregular, answer{nil, failed}, answer{fileInfo, nil},
			walkedFileIndex, fileInfo, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var throughCalls int
			info, verdict, err := walkedFileInfo(tc.typ,
				func() (fs.FileInfo, error) { return tc.own.info, tc.own.err },
				func() (fs.FileInfo, error) { throughCalls++; return tc.through.info, tc.through.err })
			if verdict != tc.want {
				t.Fatalf("verdict %d, want %d", verdict, tc.want)
			}
			if info != tc.wantInfo {
				t.Errorf("indexed under %v, want %v", info, tc.wantInfo)
			}
			if (err != nil) != (tc.want == walkedFileStatFailed || tc.want == walkedFileTargetUnreadable) {
				t.Errorf("err %v beside verdict %d", err, verdict)
			}
			if throughCalls != tc.wantThrough {
				t.Errorf("stat through the entry %d times, want %d", throughCalls, tc.wantThrough)
			}
		})
	}
}

// TestScanner_ALinkedTrackIsReExtractedWhenOnlyItsTargetChanged: the skip
// gate compares the row with the walk's stat, so while that stat was the
// link's, retagging the target (which leaves the link as it was) was never
// re-extracted, and the phone kept the old tags forever.
func TestScanner_ALinkedTrackIsReExtractedWhenOnlyItsTargetChanged(t *testing.T) {
	for _, lf := range linkedFormats {
		t.Run(lf.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			target := filepath.Join(f.parked, lf.file)
			link := filepath.Join(f.album, lf.file)
			rel := "Music/Album/" + lf.file
			lf.write(t, target, "Before")
			setMTime(t, target, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
			linkOrSkip(t, target, link)
			scanOnce(t, f.sc, "initial")
			if got := titleOf(t, f.store, rel); got != "Before" {
				t.Fatalf("precondition: title %q", got)
			}

			linkBefore := lstatOf(t, link)
			lf.write(t, target, "After, retagged")
			setMTime(t, target, time.Date(2022, 5, 6, 7, 8, 9, 0, time.UTC))
			if linkAfter := lstatOf(t, link); linkAfter.Size() != linkBefore.Size() ||
				!linkAfter.ModTime().Equal(linkBefore.ModTime()) {
				t.Fatalf("fixture: retagging the target moved the link itself, so the case tests nothing")
			}
			scanOnce(t, f.sc, "rescan after retagging the target")

			if got := titleOf(t, f.store, rel); got != "After, retagged" {
				t.Fatalf("title %q after the target was retagged: it was never re-extracted", got)
			}
			requireRowStat(t, f.store, rel, statOf(t, link), lstatOf(t, link))
		})
	}
}

// TestScanner_ALinkedSACDContainerIsExpandedUnderItsTargetsStat: a virtual
// row carries its container's size and mtime (PROTOCOL.md), which for a
// symlinked image was the link's, and a re-authored target was never
// re-expanded for the same reason a retagged file was never re-extracted.
func TestScanner_ALinkedSACDContainerIsExpandedUnderItsTargetsStat(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	parked := t.TempDir()
	target := writeSACDFixture(t, parked, "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	setMTime(t, target, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	link := filepath.Join(root, "Music", "Album.iso")
	linkOrSkip(t, target, link)
	scanOnce(t, sc, "initial")

	for _, rel := range []string{"Music/Album.iso/st/01.dff", "Music/Album.iso/st/02.dff"} {
		requireRowStat(t, store, rel, statOf(t, link), lstatOf(t, link))
	}

	// Re-author the image behind the link: the link itself does not move.
	writeSACDFixture(t, parked, "Album.iso", twoFixtureTracks(),
		sacdFixtureOptions{albumTitle: "Second Pressing"})
	setMTime(t, target, time.Date(2022, 5, 6, 7, 8, 9, 0, time.UTC))
	scanOnce(t, sc, "rescan after re-authoring the target")

	tr, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if tr.Album != "Second Pressing" {
		t.Fatalf("album %q after the target was re-authored: it was never re-expanded", tr.Album)
	}
	requireRowStat(t, store, "Music/Album.iso/st/01.dff", statOf(t, link), lstatOf(t, link))
}

// TestScanner_ALinkedSACDContainerWrittenAfterTheWalkKeepsItsRows closes the
// residual #1061 recorded for a symlinked container: its in-motion guard
// compared the walk's stat with an lstat, both the LINK's, so a copy that
// truncated the TARGET after the walk and was idle while it was read went
// unseen, and the completed read of the short file retired the album at
// threshold 1, with a tombstone to every paired device.
//
// The link is re-made under a different spelling of the same target so that
// the scan re-expands the container whichever stat it compares (the link's
// changes with the spelling, the target's with the touch before the scan).
func TestScanner_ALinkedSACDContainerWrittenAfterTheWalkKeepsItsRows(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	parked := t.TempDir()
	target := writeSACDFixture(t, parked, "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	setMTime(t, target, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	link := filepath.Join(root, "Music", "Album.iso")
	linkOrSkip(t, target, link)
	scanOnce(t, sc, "initial")
	first, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || first == nil {
		t.Fatalf("precondition: %v", err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	respelled := parked + string(filepath.Separator) + "." + string(filepath.Separator) + "Album.iso"
	linkOrSkip(t, respelled, link)
	touched := time.Date(2022, 5, 6, 7, 8, 9, 0, time.UTC)
	setMTime(t, target, touched)

	var moved atomic.Int64
	installSACDOpener(sc, func(abs string, f *os.File) sacdContainer {
		// The copy that truncated the target after the walk: its mtime moves,
		// and its bytes end early. It is idle while the expansion reads.
		if err := os.Chtimes(abs, touched.Add(time.Hour), touched.Add(time.Hour)); err == nil {
			moved.Add(1)
		}
		return movingISO{File: f, cut: 1024, move: func() {}, once: new(sync.Once)}
	})
	rec := loggingtest.Record(t)
	scanOnce(t, sc, "rescan of a linked container written after the walk")

	if moved.Load() != 1 {
		t.Fatalf("the target moved %d times, want once", moved.Load())
	}
	requireSACDRowsUntouched(t, store, first.ModTime)
	lines := rec.Lines(msgSACDInMotion)
	if len(lines) != 1 || !strings.Contains(lines[0], "path=Music/Album.iso change="+sacdChangedSinceWalk) {
		t.Fatalf("the skip was not logged once as %q:\n%s", sacdChangedSinceWalk, strings.Join(lines, "\n"))
	}
}

// requireLinkedRowsKept asserts that every row in indexed is still there and
// unrewritten (its indexed_at as recorded), and that the linked FLAC's row
// kept its tag. label names the moment, so a failure says which one broke.
func requireLinkedRowsKept(t *testing.T, store *Store, label string, indexed map[string]int64) {
	t.Helper()
	for rel, was := range indexed {
		if st, err := store.GetTrackStat(context.Background(), rel); err != nil || st == nil {
			t.Fatalf("%s: the row of %s was reaped while its target was out of sight (err %v)", label, rel, err)
		}
		if got := indexedAt(t, store, rel); got != was {
			t.Fatalf("%s: the row of %s was rewritten (indexed_at %d, was %d)", label, rel, got, was)
		}
	}
	if got := titleOf(t, store, "Music/Album/01.flac"); got != "On the NAS" {
		t.Fatalf("%s: the row's tags were replaced: title %q", label, got)
	}
}

// requireUnreadableLinksLines asserts want lines about links that could not
// be followed, each counting links links under an example named
// library-relative, as a log line names a library file (#1055), and none
// naming any of the absolute paths given.
func requireUnreadableLinksLines(t *testing.T, lines []string, want, links int, absolute ...string) {
	t.Helper()
	if len(lines) != want {
		t.Fatalf("%d lines about links that could not be followed, want %d:\n%s",
			len(lines), want, strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if !strings.Contains(line, fmt.Sprintf("count=%d", links)) || !strings.Contains(line, "example=Music/Album/") {
			t.Errorf("the line does not count %d links, library-relative: %s", links, line)
		}
		for _, abs := range absolute {
			if strings.Contains(line, abs) {
				t.Errorf("the line names the absolute path %s: %s", abs, line)
			}
		}
	}
}

// TestScanner_ALinkWhoseTargetWentAwayKeepsItsRow: "we could not see this
// path" dominates. A link into a mount that went away keeps its row as it
// was, tags and all, however many scans run while the mount is gone (the test
// scanner reaps a missing row at the first scan), and nothing is rewritten
// when the mount returns. The same holds for a subtree scan, and for the
// virtual rows of a linked SACD container. The walks say so once per scan,
// whatever the number of links: three scans, two links behind each.
func TestScanner_ALinkWhoseTargetWentAwayKeepsItsRow(t *testing.T) {
	f := newLinkedFixture(t)
	flacTarget := filepath.Join(f.parked, "01.flac")
	writeMinimalFLAC(t, flacTarget, 44100, 16, map[string]string{"TITLE": "On the NAS"})
	setMTime(t, flacTarget, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	linkOrSkip(t, flacTarget, filepath.Join(f.album, "01.flac"))
	isoTarget := writeSACDFixture(t, f.parked, "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	linkOrSkip(t, isoTarget, filepath.Join(f.album, "Album.iso"))
	scanOnce(t, f.sc, "initial")

	indexed := make(map[string]int64, 3)
	for _, rel := range []string{"Music/Album/01.flac", "Music/Album/Album.iso/st/01.dff", "Music/Album/Album.iso/st/02.dff"} {
		indexed[rel] = indexedAt(t, f.store, rel)
	}

	gone := f.parked + ".unmounted"
	if err := os.Rename(f.parked, gone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(gone, f.parked) })
	rec := loggingtest.Record(t)
	scanOnce(t, f.sc, "first scan with the mount gone")
	scanOnce(t, f.sc, "second scan with the mount gone")
	if _, err := f.sc.ScanSubtree(context.Background(), f.album); err != nil {
		t.Fatalf("subtree scan with the mount gone: %v", err)
	}
	requireLinkedRowsKept(t, f.store, "while the mount is gone", indexed)
	requireUnreadableLinksLines(t, rec.Lines(msgUnreadableLinks), 3, 2, f.root, f.parked, gone)

	if err := os.Rename(gone, f.parked); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, f.sc, "scan with the mount back")
	requireLinkedRowsKept(t, f.store, "once the mount is back", indexed)
}

// TestScanner_ALinkFirstSeenDanglingIsIndexedOnceItsTargetAppears: a link
// the walk cannot follow mints no row. It used to mint one from the path
// alone, under the link's own stat, which the skip gate then kept once the
// target appeared: the file played with the tags its path suggested, forever.
func TestScanner_ALinkFirstSeenDanglingIsIndexedOnceItsTargetAppears(t *testing.T) {
	f := newLinkedFixture(t)
	target := filepath.Join(f.parked, "01.flac")
	link := filepath.Join(f.album, "01.flac")
	linkOrSkip(t, target, link)
	scanOnce(t, f.sc, "scan while the target is not there")
	if tr, err := f.store.GetTrack(context.Background(), "Music/Album/01.flac"); err != nil || tr != nil {
		t.Fatalf("a link that could not be followed minted a row: %+v (err %v)", tr, err)
	}

	writeMinimalFLAC(t, target, 44100, 16, map[string]string{"TITLE": "Arrived"})
	scanOnce(t, f.sc, "scan once the target is there")
	if got := titleOf(t, f.store, "Music/Album/01.flac"); got != "Arrived" {
		t.Fatalf("title %q once the target arrived, want its tag", got)
	}
	requireRowStat(t, f.store, "Music/Album/01.flac", statOf(t, link), lstatOf(t, link))
}

// TestScanner_ALinkToADirectoryIsNotATrack: an audio-named link to a
// directory is not a file, whatever its name says. It used to be indexed as
// one, from its path alone.
func TestScanner_ALinkToADirectoryIsNotATrack(t *testing.T) {
	f := newLinkedFixture(t)
	linkOrSkip(t, f.parked, filepath.Join(f.album, "Bonus.flac"))
	linkOrSkip(t, f.parked, filepath.Join(f.album, "Bonus.iso"))
	scanOnce(t, f.sc, "initial")
	for _, rel := range []string{"Music/Album/Bonus.flac", "Music/Album/Bonus.iso/st/01.dff"} {
		if tr, err := f.store.GetTrack(context.Background(), rel); err != nil || tr != nil {
			t.Fatalf("a link to a directory was indexed as %s: %+v (err %v)", rel, tr, err)
		}
	}
}

// TestScanner_TheFirstScanAfterTheFixRewritesOnlyTheLinkedRows bounds the
// fix's own delta. A row written before it carries its link's stat, so the
// first scan re-extracts it once (a full upsert: one delta row to every
// paired device, and a re-enrichment), and no other row moves. The scan after
// that rewrites nothing. No ExtractorVersion bump is needed for it: the skip
// gate sees the stored stat disagree with the walk's.
func TestScanner_TheFirstScanAfterTheFixRewritesOnlyTheLinkedRows(t *testing.T) {
	f := newLinkedFixture(t)
	ctx := context.Background()
	writeMinimalFLAC(t, filepath.Join(f.album, "01.flac"), 44100, 16, map[string]string{"TITLE": "Plain"})
	target := filepath.Join(f.parked, "02.flac")
	writeMinimalFLAC(t, target, 44100, 16, map[string]string{"TITLE": "Linked"})
	setMTime(t, target, time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	link := filepath.Join(f.album, "02.flac")
	linkOrSkip(t, target, link)
	scanOnce(t, f.sc, "initial")

	// The row as the scanner wrote it before the fix: the link's own stat.
	old, err := f.store.GetTrack(ctx, "Music/Album/02.flac")
	if err != nil || old == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	ls := lstatOf(t, link)
	old.Size, old.ModTime = ls.Size(), ls.ModTime().UTC()
	if err := f.store.UpsertTrack(ctx, old); err != nil {
		t.Fatal(err)
	}
	plainBefore := indexedAt(t, f.store, "Music/Album/01.flac")
	linkedBefore := indexedAt(t, f.store, "Music/Album/02.flac")

	scanOnce(t, f.sc, "the first scan after the fix")
	if got := indexedAt(t, f.store, "Music/Album/02.flac"); got <= linkedBefore {
		t.Fatalf("the linked row kept its link's stat: indexed_at %d did not advance past %d", got, linkedBefore)
	}
	requireRowStat(t, f.store, "Music/Album/02.flac", statOf(t, link), ls)
	if got := indexedAt(t, f.store, "Music/Album/01.flac"); got != plainBefore {
		t.Fatalf("a row that is not a link was rewritten: indexed_at %d, was %d", got, plainBefore)
	}

	linkedAfter := indexedAt(t, f.store, "Music/Album/02.flac")
	scanOnce(t, f.sc, "the scan after that")
	if got := indexedAt(t, f.store, "Music/Album/02.flac"); got != linkedAfter {
		t.Fatalf("the linked row was rewritten again: indexed_at %d, was %d", got, linkedAfter)
	}
	if got := indexedAt(t, f.store, "Music/Album/01.flac"); got != plainBefore {
		t.Fatalf("a row that is not a link was rewritten: indexed_at %d, was %d", got, plainBefore)
	}
}
