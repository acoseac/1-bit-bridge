package manifest

// The two writers that write back a row they read earlier, the enricher's
// stamp (MarkEnriched) and the post-scan reconciliation passes
// (applyReconciledTracks), write only while the row is still the one they
// read. Each wrote the whole tags_json it had read, whatever had happened to
// the row since, and neither resets enriched_at, so a row overwritten that
// way was never repaired: the skip gate reads the size and mtime COLUMNS,
// which neither writer touches, and the enricher never revisits a row it
// stamped. These tests drive the real Store and Scanner through each of the
// three interleavings the pre-v0.2.1 review measured.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// enricherRead is the enricher's read of the row at rel: the batch it takes
// from UnenrichedTracks, which is where every Track it stamps comes from.
func enricherRead(t *testing.T, store *Store, rel string) *Track {
	t.Helper()
	batch, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	for i := range batch {
		if batch[i].Path == rel {
			return &batch[i]
		}
	}
	t.Fatalf("UnenrichedTracks did not offer %s", rel)
	return nil
}

// offeredToTheEnricher reports whether the enricher's next batch holds rel.
func offeredToTheEnricher(t *testing.T, store *Store, rel string) bool {
	t.Helper()
	batch, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	for i := range batch {
		if batch[i].Path == rel {
			return true
		}
	}
	return false
}

// The enricher's MBIDs, as the stamp writes them.
const (
	stampRelease = "6d541211-c604-4344-a799-11adfea40c9d"
	stampArtist  = "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493"
)

// TestAStampOverARowTheScannerRewroteWritesNothing is the first measured
// case: a file retagged while the enricher worked on it. The scanner's
// upsert of the new tags lands between the enricher's read and its stamp.
// The stamp wrote back the tags it had read (the old title, and the old
// size inside tags_json) and set enriched_at, and nothing repaired it: the
// size and mtime columns the skip gate compares were the new file's, so
// every later scan skipped the row, and the phone, which checks a download
// against the size it was told, failed every offline download of it.
//
// Now the stamp writes nothing, says the row changed, and leaves the row as
// the scanner wrote it and unenriched, so the enricher's next batch offers
// it again, with its new tags.
func TestAStampOverARowTheScannerRewroteWritesNothing(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	const rel = "a.flac"
	p := filepath.Join(root, rel)
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Old", "ARTIST": "Artist", "ALBUM": "Album"})
	if err := os.Chtimes(p, t0, t0); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "index")

	read := enricherRead(t, store, rel)

	// The file is retagged while the enricher works: a longer title, so the
	// file's size changes as well as its mtime.
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE": "A New Title Much Longer Than The Old One", "ARTIST": "Artist", "ALBUM": "Album",
	})
	if err := os.Chtimes(p, t0.Add(time.Hour), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "the retag")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	// The enricher's stamp, with what it found.
	read.MusicBrainzAlbumID, read.ArtworkMBID, read.ArtistMBID = stampRelease, stampRelease, stampArtist
	if err := store.MarkEnriched(ctx, read); err == nil {
		t.Error("MarkEnriched reported success over a row the scanner rewrote after the enricher read it")
	}

	requireTheRetaggedRow := func(label string) {
		t.Helper()
		got, err := store.GetTrack(ctx, rel)
		if err != nil || got == nil {
			t.Fatalf("%s: GetTrack: err=%v nil=%v", label, err, got == nil)
		}
		if got.Title != "A New Title Much Longer Than The Old One" {
			t.Errorf("%s: title %q, want the retagged file's", label, got.Title)
		}
		if got.Size != info.Size() {
			t.Errorf("%s: tags_json size %d, want the file's %d (the phone checks a download against it)",
				label, got.Size, info.Size())
		}
		if !got.ModTime.Equal(info.ModTime()) {
			t.Errorf("%s: tags_json mtime %v, want the file's %v", label, got.ModTime, info.ModTime())
		}
		if got.MusicBrainzAlbumID != "" || got.ArtistMBID != "" {
			t.Errorf("%s: MBIDs %q / %q, found for the old tags, landed on the new ones",
				label, got.MusicBrainzAlbumID, got.ArtistMBID)
		}
	}
	requireTheRetaggedRow("after the stamp")
	if at := enrichedAtOf(t, store, rel); at != 0 {
		t.Errorf("enriched_at = %d, want 0: the new tags have not been enriched", at)
	}
	if !offeredToTheEnricher(t, store, rel) {
		t.Error("the enricher's next batch does not offer the row, so its new tags are never enriched")
	}
	for i := 0; i < 3; i++ {
		scanOnce(t, sc, "a later scan")
	}
	requireTheRetaggedRow("three scans later")
}

// TestAStampOverARowWhoseCoverArrivedWritesNothing is the second measured
// case: a cover.jpg added beside a track while the enricher worked on it.
// The scan gives the row the cover (a local- ArtworkMBID) and records the
// folder's art in folder_art_key; the stamp then wrote back the ArtworkMBID
// it had read, "", and the skip gate, finding folder_art_key current, never
// looked again.
func TestAStampOverARowWhoseCoverArrivedWritesNothing(t *testing.T) {
	f := newArtFixture(t)
	const rel = "Album/01.flac"
	f.flac(t, rel)
	f.scan(t, "index")

	read := enricherRead(t, f.store, rel)

	f.cover(t, "Album/cover.jpg", coverBytes("a"), t0)
	f.scan(t, "the cover")
	withCover := f.art(t, rel)
	if !isLocalArtworkMBID(withCover) {
		t.Fatalf("fixture: the scan did not give the row the cover: ArtworkMBID %q", withCover)
	}

	read.ArtistMBID = stampArtist // what the enricher found; no release, no cover
	if err := f.store.MarkEnriched(context.Background(), read); err == nil {
		t.Error("MarkEnriched reported success over a row whose cover arrived after the enricher read it")
	}
	f.requireArt(t, withCover, rel)
	f.scan(t, "a later scan")
	f.requireArt(t, withCover, rel)
	if !offeredToTheEnricher(t, f.store, rel) {
		t.Error("the enricher's next batch does not offer the row")
	}
}

// TestAReconcileWriteOverAStampedRowWritesNothing is the third measured
// case: the album-artist pass reads a row, the enricher stamps that row, and
// the pass writes back the row it read. The pass's write carried the
// pre-stamp tags_json, so all three MBIDs went back to "" while enriched_at
// stayed set: the enricher never looks at the row again, and only an
// operator's "Retry missing" would.
//
// Now the pass skips a row that changed since it read it, and the next scan
// reconciles it, MBIDs and all.
func TestAReconcileWriteOverAStampedRowWritesNothing(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(root, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []struct{ name, albumArtist string }{
		{"01.flac", "Band"}, {"02.flac", "Band"}, {"03.flac", "Leader; Band"},
	} {
		writeMinimalFLAC(t, filepath.Join(root, "Album", tr.name), 44100, 16, map[string]string{
			"TITLE": tr.name, "ARTIST": "Band", "ALBUMARTIST": tr.albumArtist, "ALBUM": "Album",
		})
	}
	const rel = "Album/03.flac"

	stamped := false
	beforeApplyReconciledHookForTests = func(paths []string) {
		if stamped || !slices.Contains(paths, rel) {
			return
		}
		stamped = true
		// The enricher reads the row the pass has just read, and stamps
		// it before the pass writes.
		read := enricherRead(t, store, rel)
		read.MusicBrainzAlbumID, read.ArtworkMBID, read.ArtistMBID = stampRelease, stampRelease, stampArtist
		if err := store.MarkEnriched(ctx, read); err != nil {
			t.Errorf("the enricher's stamp: %v", err)
		}
	}
	t.Cleanup(func() { beforeApplyReconciledHookForTests = nil })
	scanOnce(t, sc, "index")
	beforeApplyReconciledHookForTests = nil
	if !stamped {
		t.Fatal("fixture: the album-artist pass never wrote the row")
	}

	requireTheStamp := func(label string) {
		t.Helper()
		got, err := store.GetTrack(ctx, rel)
		if err != nil || got == nil {
			t.Fatalf("%s: GetTrack: err=%v nil=%v", label, err, got == nil)
		}
		if got.MusicBrainzAlbumID != stampRelease || got.ArtworkMBID != stampRelease || got.ArtistMBID != stampArtist {
			t.Errorf("%s: MBIDs %q / %q / %q, want the enricher's: a pass wrote back the row it read before the stamp",
				label, got.MusicBrainzAlbumID, got.ArtworkMBID, got.ArtistMBID)
		}
		if enrichedAtOf(t, store, rel) == 0 {
			t.Errorf("%s: enriched_at = 0, want the stamp's", label)
		}
	}
	requireTheStamp("after the scan")

	scanOnce(t, sc, "the next scan")
	requireTheStamp("after the next scan")
	got, err := store.GetTrack(ctx, rel)
	if err != nil || got == nil {
		t.Fatalf("GetTrack: err=%v nil=%v", err, got == nil)
	}
	if got.AlbumArtist != "Band" {
		t.Errorf("album artist %q, want the next scan to reconcile it to %q", got.AlbumArtist, "Band")
	}
}

// TestAWriteBackOfATrackTheStoreDidNotHandOutIsRefused: a Track built by hand
// carries no version, and both writers refuse it before they write anything.
// The reconcile batch writes none of its rows either, not even one read from
// the store: a batch is one pass's verdict about a directory, and half of it
// is not.
func TestAWriteBackOfATrackTheStoreDidNotHandOutIsRefused(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := store.UpsertTrack(ctx, &Track{Path: p, Size: 1, ModTime: t0, Title: "T", Album: "A"}); err != nil {
			t.Fatal(err)
		}
	}

	handBuilt := &Track{Path: "A/1.flac", Title: "T", Album: "A", MusicBrainzAlbumID: stampRelease}
	if err := store.MarkEnriched(ctx, handBuilt); !errors.Is(err, errTrackNotRead) {
		t.Fatalf("MarkEnriched of a hand-built Track = %v, want errTrackNotRead", err)
	}
	if got := readTrack(t, store, "A/1.flac"); got.MusicBrainzAlbumID != "" {
		t.Errorf("the refused stamp wrote its release %q", got.MusicBrainzAlbumID)
	}
	if !offeredToTheEnricher(t, store, "A/1.flac") {
		t.Error("the refused stamp set enriched_at")
	}

	read := *readTrack(t, store, "A/2.flac")
	read.AlbumArtist = "Band"
	n, err := store.applyReconciledTracks(ctx, []Track{read, {Path: "A/1.flac", Title: "T", Album: "A", AlbumArtist: "Band"}})
	if !errors.Is(err, errTrackNotRead) || n != 0 {
		t.Fatalf("applyReconciledTracks with a hand-built row = (%d, %v), want (0, errTrackNotRead)", n, err)
	}
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if got := readTrack(t, store, p); got.AlbumArtist != "" {
			t.Errorf("%s: album artist %q, written by a batch that was refused", p, got.AlbumArtist)
		}
	}
}

// TestAStampRecordsTheVersionItWrote: a successful stamp leaves the Track
// holding the version it wrote, so the same Track can be stamped again, and a
// copy taken before the first stamp, which holds the old version, cannot.
func TestAStampRecordsTheVersionItWrote(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const rel = "A/1.flac"
	if err := store.UpsertTrack(ctx, &Track{Path: rel, Size: 1, ModTime: t0, Title: "T", Album: "A"}); err != nil {
		t.Fatal(err)
	}
	tr := enricherRead(t, store, rel)
	stale := *tr

	tr.MusicBrainzAlbumID = stampRelease
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	tr.ArtistMBID = stampArtist
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatalf("a second stamp through the same Track = %v: the first did not record the version it wrote", err)
	}
	stale.MusicBrainzAlbumID = "00000000-0000-4000-8000-000000000000"
	if err := store.MarkEnriched(ctx, &stale); !errors.Is(err, ErrTrackChanged) {
		t.Fatalf("a stamp through a copy read before the first = %v, want ErrTrackChanged", err)
	}
	got := readTrack(t, store, rel)
	if got.MusicBrainzAlbumID != stampRelease || got.ArtistMBID != stampArtist {
		t.Errorf("row holds release %q artist %q, want the two stamps' %q and %q",
			got.MusicBrainzAlbumID, got.ArtistMBID, stampRelease, stampArtist)
	}
}

// TestAStampOverADeletedRowWritesNothing: a row reaped between the enricher's
// read and its stamp is a changed row too. The stamp reports it and creates
// nothing.
func TestAStampOverADeletedRowWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const rel = "A/1.flac"
	if err := store.UpsertTrack(ctx, &Track{Path: rel, Size: 1, ModTime: t0, Title: "T", Album: "A"}); err != nil {
		t.Fatal(err)
	}
	tr := enricherRead(t, store, rel)
	if err := store.DeleteTrack(ctx, rel); err != nil {
		t.Fatal(err)
	}
	tr.MusicBrainzAlbumID = stampRelease
	if err := store.MarkEnriched(ctx, tr); !errors.Is(err, ErrTrackChanged) {
		t.Fatalf("MarkEnriched over a deleted row = %v, want ErrTrackChanged", err)
	}
	if got, err := store.GetTrack(ctx, rel); err != nil || got != nil {
		t.Fatalf("after the stamp: GetTrack = (%v, %v), want no row", got, err)
	}
}
