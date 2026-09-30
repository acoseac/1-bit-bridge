package manifest

// A folder's cover (cover.jpg, folder.jpg, cover.png, folder.png) reaches the
// tracks beside it when it changes, not only when they do (backlog B141). The
// skip gate kept a track whose audio file was unchanged, and a cover is read
// only when a track is extracted, so a cover added after its tracks were
// indexed, or one whose read failed during the scan that indexed them, never
// reached them: a full scan and a subtree scan of the album both left the
// rows without it (measured on main, 2026-09-30).
//
// Each row now records the identity of the folder art it was extracted
// against (folder_art_key), and the gate re-extracts a row whose folder's
// identity changed, through the version-stale diff-guard, so only rows whose
// art changes reach the delta. These tests drive the real scanner; the
// portable read failures go through the scanner's cover-reader seam
// (Scanner.readArt), and scanner_folder_art_unix_test.go fails a real open
// with chmod 0 where a host allows it.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// artFixture is a library, its store and its scanner (with an artwork
// cache), and a count of the audio files the scanner opened, which is one
// per extraction: the measure of what a scan re-read.
type artFixture struct {
	root  string
	store *Store
	sc    *Scanner

	mu    sync.Mutex
	opens map[string]int
}

// newArtFixture stands up an empty library at a fresh root and a scanner
// over it whose audio opens are counted.
func newArtFixture(t *testing.T) *artFixture {
	t.Helper()
	f := &artFixture{root: t.TempDir(), opens: map[string]int{}}
	f.store, f.sc = newDiscArtScanFixture(t, f.root)
	f.sc.openAudio = func(abs string) (extractSource, error) {
		f.mu.Lock()
		f.opens[abs]++
		f.mu.Unlock()
		file, err := os.Open(abs)
		if err != nil {
			return nil, err // never a nil *os.File inside the interface
		}
		return file, nil
	}
	return f
}

// path is rel's absolute path in the library.
func (f *artFixture) path(rel string) string {
	return filepath.Join(f.root, filepath.FromSlash(rel))
}

// dir creates the library directory rel and returns its absolute path.
func (f *artFixture) dir(t *testing.T, rel string) string {
	t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// flac writes a tagged FLAC, with no picture, at rel.
func (f *artFixture) flac(t *testing.T, rel string) {
	t.Helper()
	f.dir(t, filepath.Dir(rel))
	name := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	writeMinimalFLAC(t, f.path(rel), 44100, 16, map[string]string{
		"TITLE": name, "ARTIST": "Artist", "ALBUMARTIST": "Artist", "ALBUM": "Album", "DATE": "2001",
	})
}

// cover writes data as the cover file rel, with an mtime of its own, so a
// replacement never shares its predecessor's mtime on a coarse clock.
func (f *artFixture) cover(t *testing.T, rel string, data []byte, mtime time.Time) {
	t.Helper()
	f.dir(t, filepath.Dir(rel))
	p := f.path(rel)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// scan runs a full scan and returns how many audio files it opened.
func (f *artFixture) scan(t *testing.T, label string) int {
	t.Helper()
	f.resetOpens()
	scanOnce(t, f.sc, label)
	return f.opened()
}

// subtreeScan runs a subtree scan of rel and returns how many audio files it
// opened.
func (f *artFixture) subtreeScan(t *testing.T, rel string) int {
	t.Helper()
	f.resetOpens()
	if _, err := f.sc.ScanSubtree(context.Background(), f.path(rel)); err != nil {
		t.Fatalf("subtree scan of %s: %v", rel, err)
	}
	return f.opened()
}

// resetOpens forgets the audio opens counted so far.
func (f *artFixture) resetOpens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = map[string]int{}
}

// opened returns the audio opens counted since the last reset.
func (f *artFixture) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.opens {
		n += c
	}
	return n
}

// art returns the ArtworkMBID of the row at rel.
func (f *artFixture) art(t *testing.T, rel string) string {
	t.Helper()
	tr, err := f.store.GetTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack(%s): err=%v nil=%v", rel, err, tr == nil)
	}
	return tr.ArtworkMBID
}

// key returns the folder_art_key column of the row at rel.
func (f *artFixture) key(t *testing.T, rel string) string {
	t.Helper()
	var k string
	if err := f.store.db.QueryRow(`SELECT folder_art_key FROM tracks WHERE path = ?`, rel).Scan(&k); err != nil {
		t.Fatalf("folder_art_key(%s): %v", rel, err)
	}
	return k
}

// requireArt asserts that every row in rels carries want as its art.
func (f *artFixture) requireArt(t *testing.T, want string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if got := f.art(t, rel); got != want {
			t.Errorf("%s: ArtworkMBID = %q, want %q", rel, got, want)
		}
	}
}

// indexedAts returns each row's indexed_at, by path.
func (f *artFixture) indexedAts(t *testing.T, rels ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, rel := range rels {
		out[rel] = trackIndexedAt(t, f.store, rel)
	}
	return out
}

// requireMoved asserts that every row in before has a later indexed_at now:
// the change reaches every paired device's next delta.
func (f *artFixture) requireMoved(t *testing.T, before map[string]int64) {
	t.Helper()
	for rel, at := range before {
		if now := trackIndexedAt(t, f.store, rel); now <= at {
			t.Errorf("%s: indexed_at %d -> %d, want it to advance (the art changed, so every device must pull the row)", rel, at, now)
		}
	}
}

// requireStill asserts that no row in before moved: nothing a client sees
// changed.
func (f *artFixture) requireStill(t *testing.T, before map[string]int64) {
	t.Helper()
	for rel, at := range before {
		if now := trackIndexedAt(t, f.store, rel); now != at {
			t.Errorf("%s: indexed_at %d -> %d, want it unchanged (nothing a client sees changed)", rel, at, now)
		}
	}
}

// requireSettled asserts that a further scan re-reads nothing and moves no
// row: the gate's answer changed once the rows were written.
func (f *artFixture) requireSettled(t *testing.T, rels ...string) {
	t.Helper()
	before := f.indexedAts(t, rels...)
	if n := f.scan(t, "the scan after"); n != 0 {
		t.Errorf("the scan after re-read %d audio files, want 0: the skip gate keeps going back to rows it has written", n)
	}
	f.requireStill(t, before)
}

// coverBytes is a cover the artwork pipeline accepts (a JPEG header it stores
// as it is), made distinct by tag.
func coverBytes(tag string) []byte {
	return append(append([]byte{}, minimalJPEG...), []byte(tag)...)
}

// failingArtRead is a cover reader that fails, with EIO, every read of a
// candidate whose base name is in names, and reads the others as production
// does.
func failingArtRead(names ...string) folderArtReader {
	return func(full string, info os.FileInfo) ([]byte, error) {
		for _, n := range names {
			if filepath.Base(full) == n {
				return nil, &fs.PathError{Op: "read", Path: full, Err: syscall.EIO}
			}
		}
		return readFolderArt(full, info)
	}
}

// requireOneUnreadArtLine asserts that the scan logged exactly one line for
// the tracks whose folder cover it could not read, counting n of them, with
// the failure op.
func requireOneUnreadArtLine(t *testing.T, rec *loggingtest.Recorder, n int, op string) {
	t.Helper()
	lines := rec.Lines(msgUnreadFolderArt)
	if len(lines) != 1 {
		t.Fatalf("lines %q, want one", lines)
	}
	for _, want := range []string{fmt.Sprintf("count=%d", n), "err=" + op} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q, want it to hold %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], os.TempDir()) {
		t.Errorf("line %q names an absolute path; a log line names a library file library-relative", lines[0])
	}
}

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// TestScanner_ACoverAddedAfterIndexingReachesItsTracks pins the case the
// backlog entry measured: two FLACs indexed with no art, then a cover.jpg
// written beside them. A full scan, and a subtree scan of the album, each
// give both rows the cover, move them in the delta, and the scan after
// re-reads nothing.
func TestScanner_ACoverAddedAfterIndexingReachesItsTracks(t *testing.T) {
	for _, how := range []string{"full scan", "subtree scan"} {
		t.Run(how, func(t *testing.T) {
			f := newArtFixture(t)
			rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
			for _, rel := range rels {
				f.flac(t, rel)
			}
			f.scan(t, "index")
			f.requireArt(t, "", rels...)

			data := coverBytes("added")
			f.cover(t, "Artist/Album/cover.jpg", data, t0)
			before := f.indexedAts(t, rels...)
			var opened int
			if how == "full scan" {
				opened = f.scan(t, "after the cover")
			} else {
				opened = f.subtreeScan(t, "Artist/Album")
			}
			f.requireArt(t, expectedLocalMBID(data), rels...)
			f.requireMoved(t, before)
			if opened != len(rels) {
				t.Errorf("the scan re-read %d audio files, want %d (each track beside the new cover, once)", opened, len(rels))
			}
			f.requireSettled(t, rels...)
		})
	}
}

// TestScanner_AReplacedCoverReplacesItsArt pins a cover replaced in place,
// by one of the same size: the rows take the new art.
func TestScanner_AReplacedCoverReplacesItsArt(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	old := coverBytes("old!")
	f.cover(t, "Artist/Album/cover.jpg", old, t0)
	f.scan(t, "index")
	f.requireArt(t, expectedLocalMBID(old), rels...)

	replacement := coverBytes("new!")
	if len(replacement) != len(old) {
		t.Fatal("fixture: the replacement must be the old cover's size")
	}
	f.cover(t, "Artist/Album/cover.jpg", replacement, t0.Add(time.Hour))
	before := f.indexedAts(t, rels...)
	f.scan(t, "after the replacement")
	f.requireArt(t, expectedLocalMBID(replacement), rels...)
	f.requireMoved(t, before)
	f.requireSettled(t, rels...)
}

// TestScanner_ARemovedCoverTakesItsArtAway pins a cover removed: the rows
// lose the art it gave them, and their enrichment is queued again, so the
// enricher can give them a network cover. The copy that keeps enricher-owned
// fields across a re-extract (mergePostScanFields) kept the removed cover's
// art forever before.
func TestScanner_ARemovedCoverTakesItsArtAway(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("gone")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	f.scan(t, "index")
	f.requireArt(t, expectedLocalMBID(data), rels...)
	if _, err := f.store.db.Exec(`UPDATE tracks SET enriched_at = 42`); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(f.path("Artist/Album/cover.jpg")); err != nil {
		t.Fatal(err)
	}
	before := f.indexedAts(t, rels...)
	f.scan(t, "after the removal")
	f.requireArt(t, "", rels...)
	f.requireMoved(t, before)
	for _, rel := range rels {
		if got := trackColumn(t, f.store, rel, "enriched_at"); got != 0 {
			t.Errorf("%s: enriched_at = %d, want 0 (the enricher gives it a network cover)", rel, got)
		}
	}
	f.requireSettled(t, rels...)
}

// TestScanner_ACoverAtTheAlbumRootReachesItsDiscFolders pins the multi-disc
// layout: a cover added at the album root, after the tracks in its disc
// folders were indexed, reaches them (the lookup climbs one level from a disc
// folder, and the key names the parent's cover), by a full scan and by a
// subtree scan of the album.
func TestScanner_ACoverAtTheAlbumRootReachesItsDiscFolders(t *testing.T) {
	for _, how := range []string{"full scan", "subtree scan"} {
		t.Run(how, func(t *testing.T) {
			f := newArtFixture(t)
			rels := []string{"Box/Disc 1/a.flac", "Box/Disc 2/b.flac"}
			for _, rel := range rels {
				f.flac(t, rel)
			}
			f.scan(t, "index")
			f.requireArt(t, "", rels...)

			data := coverBytes("root")
			f.cover(t, "Box/cover.jpg", data, t0)
			before := f.indexedAts(t, rels...)
			if how == "full scan" {
				f.scan(t, "after the cover")
			} else {
				f.subtreeScan(t, "Box")
			}
			f.requireArt(t, expectedLocalMBID(data), rels...)
			f.requireMoved(t, before)
			f.requireSettled(t, rels...)
		})
	}
}

// TestScanner_ACoverBesideAnEmbeddedPictureChangesNothing pins the embedded
// picture's precedence: a cover added beside a track whose own picture wins
// re-reads the track once and changes nothing a client sees (the stamp leg
// records the folder's new key), and the scan after re-reads nothing.
func TestScanner_ACoverBesideAnEmbeddedPictureChangesNothing(t *testing.T) {
	f := newArtFixture(t)
	const rel = "Artist/Album/01.mp3"
	f.dir(t, "Artist/Album")
	picture := coverBytes("embedded")
	writeMP3WithAPIC(t, f.path(rel), map[string]string{"title": "One", "artist": "Artist", "album": "Album"},
		"image/jpeg", picture)
	f.scan(t, "index")
	f.requireArt(t, expectedLocalMBID(picture), rel)

	f.cover(t, "Artist/Album/cover.jpg", coverBytes("folder"), t0)
	before := f.indexedAts(t, rel)
	f.scan(t, "after the cover")
	f.requireArt(t, expectedLocalMBID(picture), rel)
	f.requireStill(t, before)
	if k := f.key(t, rel); !strings.HasPrefix(k, "cover.jpg:") {
		t.Errorf("folder_art_key = %q, want the folder's (the stamp leg records it)", k)
	}
	f.requireSettled(t, rel)
}

// TestScanner_AFolderCoverOutranksTheEnrichersAndItsRemovalHandsItBack pins
// the precedence with a network cover: a cover added beside a track the
// enricher gave a Cover Art Archive cover replaces it (a curated cover
// outranks a remote one, the rule mergePostScanFields states), and removing
// the cover takes it away and queues the track for the enricher again.
func TestScanner_AFolderCoverOutranksTheEnrichersAndItsRemovalHandsItBack(t *testing.T) {
	f := newArtFixture(t)
	const rel = "Artist/Album/01.flac"
	f.flac(t, rel)
	f.scan(t, "index")
	tr, err := f.store.GetTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: err=%v nil=%v", err, tr == nil)
	}
	const release = "6d541211-c604-4344-a799-11adfea40c9d"
	tr.MusicBrainzAlbumID, tr.ArtworkMBID = release, release
	if err := f.store.MarkEnriched(context.Background(), tr); err != nil {
		t.Fatal(err)
	}

	data := coverBytes("curated")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	f.scan(t, "after the cover")
	f.requireArt(t, expectedLocalMBID(data), rel)
	got, err := f.store.GetTrack(context.Background(), rel)
	if err != nil || got == nil {
		t.Fatalf("GetTrack: err=%v nil=%v", err, got == nil)
	}
	if got.MusicBrainzAlbumID != release {
		t.Errorf("MusicBrainzAlbumID = %q, want the enricher's %q kept", got.MusicBrainzAlbumID, release)
	}

	if err := os.Remove(f.path("Artist/Album/cover.jpg")); err != nil {
		t.Fatal(err)
	}
	f.scan(t, "after the removal")
	f.requireArt(t, "", rel)
	if at := trackColumn(t, f.store, rel, "enriched_at"); at != 0 {
		t.Errorf("enriched_at = %d, want 0 (the enricher gives the network cover back)", at)
	}
	f.requireSettled(t, rel)
}

// TestScanner_ACoverThatCouldNotBeReadIsReadOnALaterScan pins the failed read:
// an album indexed while its cover's read fails (an EIO from a NAS) gets no
// art, one line counts its tracks, the scans while it keeps failing re-read
// no audio file (the retry is the cover's read alone), and the first scan
// that reads it gives the tracks the cover.
func TestScanner_ACoverThatCouldNotBeReadIsReadOnALaterScan(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("late")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	f.sc.readArt = failingArtRead("cover.jpg")

	rec := loggingtest.Record(t)
	f.scan(t, "index, the cover's read failing")
	f.requireArt(t, "", rels...)
	for _, rel := range rels {
		if k := f.key(t, rel); k != folderArtUnsettledKey {
			t.Errorf("%s: folder_art_key = %q, want %q", rel, k, folderArtUnsettledKey)
		}
	}
	requireOneUnreadArtLine(t, rec, len(rels), "read")

	rec = loggingtest.Record(t)
	before := f.indexedAts(t, rels...)
	if n := f.scan(t, "the read still failing"); n != 0 {
		t.Errorf("a scan whose cover still cannot be read re-read %d audio files, want 0", n)
	}
	f.requireStill(t, before)
	requireOneUnreadArtLine(t, rec, len(rels), "read")

	f.sc.readArt = nil
	rec = loggingtest.Record(t)
	if n := f.scan(t, "the cover readable"); n != len(rels) {
		t.Errorf("the scan that could read the cover re-read %d audio files, want %d", n, len(rels))
	}
	f.requireArt(t, expectedLocalMBID(data), rels...)
	f.requireMoved(t, before)
	if lines := rec.Lines(msgUnreadFolderArt); len(lines) != 0 {
		t.Errorf("lines %q, want none once the cover reads", lines)
	}
	f.requireSettled(t, rels...)
}

// TestScanner_AReplacedCoverThatCouldNotBeReadKeepsTheOldArt pins the failed
// read of a cover that replaced another: the rows keep the art they had (an
// answer given without the cover is not one, and taking it would change the
// rows twice), and take the new cover once it reads.
func TestScanner_AReplacedCoverThatCouldNotBeReadKeepsTheOldArt(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	old := coverBytes("before")
	f.cover(t, "Artist/Album/cover.jpg", old, t0)
	f.scan(t, "index")

	replacement := coverBytes("after!")
	f.cover(t, "Artist/Album/cover.jpg", replacement, t0.Add(time.Hour))
	f.sc.readArt = failingArtRead("cover.jpg")
	before := f.indexedAts(t, rels...)
	f.scan(t, "the new cover's read failing")
	f.requireArt(t, expectedLocalMBID(old), rels...)
	f.requireStill(t, before)

	f.sc.readArt = nil
	f.scan(t, "the new cover readable")
	f.requireArt(t, expectedLocalMBID(replacement), rels...)
	f.requireMoved(t, before)
	f.requireSettled(t, rels...)
}

// TestScanner_AnUnchangedLibraryIsNotReRead pins the steady state: over
// albums with a cover, without one, with embedded pictures and in disc
// folders, a scan of an unchanged library re-reads no audio file and moves
// no row. And the upgrade: rows written before the folder-art key existed
// (an empty folder_art_key) are re-read once, only in the folders that hold a
// cover, through the stamp leg (no row moves), and never again.
func TestScanner_AnUnchangedLibraryIsNotReRead(t *testing.T) {
	f := newArtFixture(t)
	withCover := []string{"A/Album 1/01.flac", "A/Album 1/02.flac", "Box/Disc 1/a.flac", "Box/Disc 2/b.flac"}
	withoutCover := []string{"A/Album 2/01.flac", "Loose/Disc 1/c.flac"}
	for _, rel := range append(append([]string{}, withCover...), withoutCover...) {
		f.flac(t, rel)
	}
	f.cover(t, "A/Album 1/cover.jpg", coverBytes("one"), t0)
	f.cover(t, "Box/cover.jpg", coverBytes("box"), t0)
	const embedded = "A/Album 3/01.mp3"
	f.dir(t, "A/Album 3")
	writeMP3WithAPIC(t, f.path(embedded), map[string]string{"title": "One", "artist": "A", "album": "Album 3"}, "image/jpeg", coverBytes("pic"))
	f.cover(t, "A/Album 3/folder.jpg", coverBytes("three"), t0)
	withCover = append(withCover, embedded)
	all := append(append([]string{}, withCover...), withoutCover...)

	f.scan(t, "index")
	f.requireSettled(t, all...)

	// Rows as the release before the key wrote them.
	if _, err := f.store.db.Exec(`UPDATE tracks SET folder_art_key = ''`); err != nil {
		t.Fatal(err)
	}
	before := f.indexedAts(t, all...)
	f.resetOpens()
	scanOnce(t, f.sc, "the first scan after the upgrade")
	f.mu.Lock()
	opens := f.opens
	f.mu.Unlock()
	for _, rel := range withCover {
		if opens[f.path(rel)] != 1 {
			t.Errorf("%s (a folder with a cover) was read %d times, want once", rel, opens[f.path(rel)])
		}
	}
	for _, rel := range withoutCover {
		if opens[f.path(rel)] != 0 {
			t.Errorf("%s (a folder with no cover) was read %d times, want never", rel, opens[f.path(rel)])
		}
	}
	f.requireStill(t, before)
	f.requireSettled(t, all...)
}

// TestScanner_AFileItsExtractorRefusesIsNotReReadForItsFolder pins a file its
// extractor refuses (read whole, not its format: a DSF whose header is not
// one; the scan writes it by its name): no cover can change what it is given,
// so its row records that (folderArtNotLookedKey), and a row from before the
// key, re-read once in a folder with a cover, records it without being
// written, never to be read for it again.
func TestScanner_AFileItsExtractorRefusesIsNotReReadForItsFolder(t *testing.T) {
	f := newArtFixture(t)
	const rel = "Artist/Album/bad.dsf"
	f.dir(t, "Artist/Album")
	writeJunkDSF(t, f.path(rel))
	f.cover(t, "Artist/Album/cover.jpg", coverBytes("x"), t0)
	f.scan(t, "index")
	mustIndexed(t, f.store, rel)
	if got := f.art(t, rel); got != "" {
		t.Fatalf("fixture: the refused file was given art %q; this case is about one the pipeline never reaches", got)
	}
	if k := f.key(t, rel); k != folderArtNotLookedKey {
		t.Errorf("folder_art_key = %q, want %q", k, folderArtNotLookedKey)
	}
	f.requireSettled(t, rel)

	if _, err := f.store.db.Exec(`UPDATE tracks SET folder_art_key = ''`); err != nil {
		t.Fatal(err)
	}
	before := f.indexedAts(t, rel)
	if n := f.scan(t, "the first scan after the upgrade"); n != 1 {
		t.Errorf("the refused file was read %d times, want once", n)
	}
	if k := f.key(t, rel); k != folderArtNotLookedKey {
		t.Errorf("folder_art_key = %q, want %q recorded", k, folderArtNotLookedKey)
	}
	f.requireStill(t, before)
	f.requireSettled(t, rel)
}
