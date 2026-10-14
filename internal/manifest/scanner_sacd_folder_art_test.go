package manifest

// An SACD image's virtual rows take the cover beside the image, through the
// folder-art lookup a folder's tracks use, and the container's own skip gate
// watches that cover so a later add, change or removal reaches the rows
// without expanding the image again.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

const (
	sacdCoverISO   = "Music/Album.iso"
	sacdCoverTrack = "Music/Album.iso/st/01.dff"
	sacdCoverOther = "Music/Album.iso/st/02.dff"
)

// sacdCoverFixture is an image under Music/ with an artwork cache, counting
// every expansion of the image.
type sacdCoverFixture struct {
	root    string
	store   *Store
	sc      *Scanner
	expands atomic.Int32
}

func newSACDCoverFixture(t *testing.T) *sacdCoverFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, sc := newDiscArtScanFixture(t, root)
	f := &sacdCoverFixture{root: root, store: store, sc: sc}
	sc.openSACD = func(abs string) (sacdContainer, error) {
		f.expands.Add(1)
		file, _, err := fsutil.OpenAsFile(abs)
		if err != nil {
			return nil, err
		}
		return file, nil
	}
	return f
}

func (f *sacdCoverFixture) writeISO(t *testing.T) {
	t.Helper()
	writeSACDFixture(t, filepath.Join(f.root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{year: 2004})
}

func (f *sacdCoverFixture) writeCover(t *testing.T, data []byte, mtime time.Time) {
	t.Helper()
	p := filepath.Join(f.root, "Music", "cover.jpg")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func (f *sacdCoverFixture) removeCover(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.root, "Music", "cover.jpg")); err != nil {
		t.Fatal(err)
	}
}

func (f *sacdCoverFixture) scan(t *testing.T, label string) {
	t.Helper()
	scanOnce(t, f.sc, label)
}

func (f *sacdCoverFixture) art(t *testing.T, rel string) string {
	t.Helper()
	tr, err := f.store.GetTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack(%s): err=%v nil=%v", rel, err, tr == nil)
	}
	return tr.ArtworkMBID
}

func (f *sacdCoverFixture) key(t *testing.T, rel string) string {
	t.Helper()
	var k string
	if err := f.store.db.QueryRow(`SELECT folder_art_key FROM tracks WHERE path = ?`, rel).Scan(&k); err != nil {
		t.Fatalf("folder_art_key(%s): %v", rel, err)
	}
	return k
}

func (f *sacdCoverFixture) indexedAt(t *testing.T, rel string) int64 {
	t.Helper()
	return trackIndexedAt(t, f.store, rel)
}

func (f *sacdCoverFixture) requireArt(t *testing.T, want string) {
	t.Helper()
	for _, rel := range []string{sacdCoverTrack, sacdCoverOther} {
		if got := f.art(t, rel); got != want {
			t.Errorf("%s: ArtworkMBID = %q, want %q", rel, got, want)
		}
	}
}

func (f *sacdCoverFixture) requireExpands(t *testing.T, want int32) {
	t.Helper()
	if got := f.expands.Load(); got != want {
		t.Fatalf("expansions = %d, want %d", got, want)
	}
}

var sacdCoverWhen = time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)

func TestAnSACDImageCarriesTheCoverBesideIt(t *testing.T) {
	f := newSACDCoverFixture(t)
	f.writeISO(t)
	f.writeCover(t, coverBytes("beside"), sacdCoverWhen)

	f.scan(t, "with cover")
	f.requireExpands(t, 1)
	got := f.art(t, sacdCoverTrack)
	if len(got) < len("local-") || got[:6] != "local-" {
		t.Fatalf("ArtworkMBID = %q, want a folder cover", got)
	}
	f.requireArt(t, got)
	if f.key(t, sacdCoverTrack) == "" || f.key(t, sacdCoverTrack) != f.key(t, sacdCoverOther) {
		t.Fatalf("folder-art keys = %q %q", f.key(t, sacdCoverTrack), f.key(t, sacdCoverOther))
	}

	f.scan(t, "unchanged")
	f.requireExpands(t, 1)
	f.requireArt(t, got)
}

func TestAnSACDCoverChangeReachesTheRowsWithoutExpanding(t *testing.T) {
	f := newSACDCoverFixture(t)
	f.writeISO(t)
	f.scan(t, "no cover yet")
	f.requireExpands(t, 1)
	f.requireArt(t, "")

	f.writeCover(t, coverBytes("added"), sacdCoverWhen)
	f.scan(t, "cover added")
	f.requireExpands(t, 1)
	added := f.art(t, sacdCoverTrack)
	if len(added) < len("local-") || added[:6] != "local-" {
		t.Fatalf("added cover: ArtworkMBID = %q", added)
	}
	f.requireArt(t, added)
	addedAt := f.indexedAt(t, sacdCoverTrack)
	addedKey := f.key(t, sacdCoverTrack)

	f.scan(t, "cover still there")
	f.requireExpands(t, 1)
	f.requireArt(t, added)
	if f.indexedAt(t, sacdCoverTrack) != addedAt {
		t.Fatal("an unchanged cover moved indexed_at")
	}

	f.writeCover(t, coverBytes("added"), sacdCoverWhen.Add(time.Hour))
	f.scan(t, "cover touched")
	f.requireExpands(t, 1)
	f.requireArt(t, added)
	if f.indexedAt(t, sacdCoverTrack) != addedAt {
		t.Fatal("the same cover bytes moved indexed_at")
	}
	if f.key(t, sacdCoverTrack) == addedKey {
		t.Fatal("a new cover mtime left the folder-art key")
	}

	f.writeCover(t, coverBytes("replaced"), sacdCoverWhen.Add(2*time.Hour))
	f.scan(t, "cover replaced")
	f.requireExpands(t, 1)
	replaced := f.art(t, sacdCoverTrack)
	if replaced == "" || replaced == added {
		t.Fatalf("replaced cover: ArtworkMBID = %q, was %q", replaced, added)
	}
	f.requireArt(t, replaced)
	if f.indexedAt(t, sacdCoverTrack) == addedAt {
		t.Fatal("a replaced cover left indexed_at")
	}

	f.removeCover(t)
	f.scan(t, "cover removed")
	f.requireExpands(t, 1)
	f.requireArt(t, "")
}

// TestAnSACDCoverThatCouldNotBeReadKeepsItsArtAndIsRetried pins the unsettled
// branch of the container's gate: a cover change whose read fails keeps the art
// the rows had and records folderArtUnsettledKey, without expanding the image,
// and the first scan that reads the cover gives the rows the new one.
func TestAnSACDCoverThatCouldNotBeReadKeepsItsArtAndIsRetried(t *testing.T) {
	f := newSACDCoverFixture(t)
	f.writeISO(t)
	f.writeCover(t, coverBytes("first"), sacdCoverWhen)
	f.scan(t, "with cover")
	first := f.art(t, sacdCoverTrack)

	f.writeCover(t, coverBytes("second"), sacdCoverWhen.Add(time.Hour))
	f.sc.readArt = failingArtRead("cover.jpg")
	f.scan(t, "cover changed, its read failing")
	f.requireExpands(t, 1)
	f.requireArt(t, first)
	if k := f.key(t, sacdCoverTrack); k != folderArtUnsettledKey {
		t.Fatalf("folder_art_key = %q, want %q", k, folderArtUnsettledKey)
	}

	f.sc.readArt = nil
	f.scan(t, "the cover read")
	f.requireExpands(t, 1)
	second := f.art(t, sacdCoverTrack)
	if second == "" || second == first {
		t.Fatalf("ArtworkMBID = %q after the read, was %q", second, first)
	}
	f.requireArt(t, second)
	if k := f.key(t, sacdCoverTrack); k == "" || k == folderArtUnsettledKey {
		t.Fatalf("folder_art_key = %q, want the cover's key", k)
	}
}

// TestAnSACDCoverWhoseCacheIsGoneIsRestoredWithoutExpanding pins the recovery
// branch: an unchanged image whose rows name a local- cover whose cache file is
// gone reads the folder's cover again, without expanding the image.
func TestAnSACDCoverWhoseCacheIsGoneIsRestoredWithoutExpanding(t *testing.T) {
	f := newSACDCoverFixture(t)
	f.writeISO(t)
	f.writeCover(t, coverBytes("cached"), sacdCoverWhen)
	f.scan(t, "with cover")
	art := f.art(t, sacdCoverTrack)
	key := f.key(t, sacdCoverTrack)
	cache := filepath.Join(f.sc.artDir, art+"-500.jpg")
	if err := os.Remove(cache); err != nil {
		t.Fatalf("remove the cover cache: %v", err)
	}

	f.scan(t, "the cache gone")
	f.requireExpands(t, 1)
	f.requireArt(t, art)
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the cover cache was not restored: %v", err)
	}
	if k := f.key(t, sacdCoverTrack); k != key {
		t.Fatalf("folder_art_key = %q, want %q", k, key)
	}
}

func TestAnSACDImageWithNoCoverScansAsBefore(t *testing.T) {
	f := newSACDCoverFixture(t)
	f.writeISO(t)
	f.scan(t, "first")
	f.requireExpands(t, 1)
	f.requireArt(t, "")
	if f.key(t, sacdCoverTrack) != "" || f.key(t, sacdCoverOther) != "" {
		t.Fatalf("keys = %q %q", f.key(t, sacdCoverTrack), f.key(t, sacdCoverOther))
	}
	tr, err := f.store.GetTrack(context.Background(), sacdCoverTrack)
	if err != nil || tr == nil || tr.Title != "Opening" || tr.Codec != "DFF" {
		t.Fatalf("track: %+v err=%v", tr, err)
	}

	f.scan(t, "again")
	f.requireExpands(t, 1)
	f.requireArt(t, "")
	rows := sacdTrackPathsUnder(t, f.store, sacdCoverISO)
	if len(rows) != 2 {
		t.Fatalf("virtual rows: %v", rows)
	}
}
