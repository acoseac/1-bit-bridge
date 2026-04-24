package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- extractors ---

func TestExtractFLACTagsAndFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.flac")
	writeMinimalFLAC(t, p, 96000, 24, map[string]string{
		"TITLE":                 "Song",
		"ARTIST":                "An Artist",
		"ALBUM":                 "An Album",
		"TRACKNUMBER":           "3",
		"DATE":                  "2024",
		"GENRE":                 "Jazz",
		"MUSICBRAINZ_ALBUMID":   "album-mbid-1",
		"REPLAYGAIN_TRACK_GAIN": "-8.4 dB",
	})
	tr := &Track{Path: "t.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "Song" || tr.Artist != "An Artist" || tr.Album != "An Album" {
		t.Errorf("tags: %+v", tr)
	}
	if tr.TrackNumber != 3 || tr.Year != 2024 || tr.Genre != "Jazz" {
		t.Errorf("tags: %+v", tr)
	}
	if tr.MusicBrainzAlbumID != "album-mbid-1" {
		t.Errorf("MB album ID: %q", tr.MusicBrainzAlbumID)
	}
	if tr.ReplayGainTrackDB == nil || *tr.ReplayGainTrackDB != -8.4 {
		t.Errorf("replay gain: %v", tr.ReplayGainTrackDB)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 96000 {
		t.Errorf("sample rate: %v", tr.SampleRate)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 24 {
		t.Errorf("bit depth: %v", tr.BitsPerSample)
	}
	if tr.Duration == nil || *tr.Duration < 4.9 || *tr.Duration > 5.1 {
		t.Errorf("duration: %v (want ~5)", tr.Duration)
	}
	if tr.IsDSD {
		t.Error("FLAC flagged as DSD")
	}
}

func TestExtractDSFTagsAndFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.dsf")
	writeMinimalDSF(t, p, 2822400, map[string]string{
		"title":  "DSF Song",
		"artist": "DSF Artist",
		"album":  "DSF Album",
		"track":  "5",
		"year":   "2023",
	})
	tr := &Track{Path: "t.dsf", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "DSF Song" {
		t.Errorf("title = %q", tr.Title)
	}
	if tr.Artist != "DSF Artist" {
		t.Errorf("artist = %q", tr.Artist)
	}
	if tr.Album != "DSF Album" {
		t.Errorf("album = %q", tr.Album)
	}
	if tr.TrackNumber != 5 {
		t.Errorf("track = %d", tr.TrackNumber)
	}
	if !tr.IsDSD {
		t.Error("DSF should have IsDSD = true")
	}
	if tr.SampleRate == nil || *tr.SampleRate != 2822400 {
		t.Errorf("DSD rate = %v, want 2822400", tr.SampleRate)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 1 {
		t.Errorf("bit depth = %v, want 1", tr.BitsPerSample)
	}
	if tr.Duration == nil || *tr.Duration < 4.9 || *tr.Duration > 5.1 {
		t.Errorf("duration: %v", tr.Duration)
	}
}

func TestExtractMP3Tags(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.mp3")
	writeMinimalMP3(t, p, map[string]string{
		"title":  "MP3 Song",
		"artist": "MP3 Artist",
		"album":  "MP3 Album",
	})
	tr := &Track{Path: "t.mp3", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "MP3 Song" || tr.Artist != "MP3 Artist" || tr.Album != "MP3 Album" {
		t.Errorf("mp3 tags: %+v", tr)
	}
}

func TestExtractUntaggedFileIsNotAnError(t *testing.T) {
	// A file that has no tags at all should still index without error;
	// the scanner falls back to path-derived values.
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.flac")
	writeMinimalFLAC(t, p, 44100, 16, nil)
	tr := &Track{Path: "empty.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Errorf("Extract on tag-less FLAC: %v", err)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 44100 {
		t.Errorf("sample rate from tag-less FLAC: %v", tr.SampleRate)
	}
}

// --- store ---

func TestStoreRoundTrip(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	tr := &Track{Path: "a/b.flac", Size: 100, ModTime: now, Title: "X", Artist: "Y"}
	if err := s.UpsertTrack(tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTrack("a/b.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v / %v", err, got)
	}
	if got.Title != "X" || got.Artist != "Y" {
		t.Errorf("roundtrip: %+v", got)
	}

	// Update via upsert.
	tr.Title = "XX"
	if err := s.UpsertTrack(tr); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTrack("a/b.flac")
	if got.Title != "XX" {
		t.Errorf("update: %+v", got)
	}

	// List: one entry.
	all, _ := s.ListTracks(nil)
	if len(all) != 1 {
		t.Errorf("list: %d", len(all))
	}

	// Delete.
	if err := s.DeleteTrack("a/b.flac"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTrack("a/b.flac")
	if got != nil {
		t.Errorf("delete didn't take: %+v", got)
	}
}

// TestStoreHasTrackWithArtworkMBID pins the SQL contract the
// /v1/artwork 202-vs-404 handler depends on. A track tagged with a
// given ArtworkMBID reports true; an arbitrary MBID that no track
// references reports false. Empty MBID always returns false
// (short-circuit). Parallel coverage for ArtistMBID.
func TestStoreHasTrackWithArtworkMBID(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	known := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	unknown := "99999999-9999-4999-8999-999999999999"

	s.UpsertTrack(&Track{
		Path: "a/b.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtworkMBID: known,
	})

	if !s.HasTrackWithArtworkMBID(known) {
		t.Errorf("known MBID should report true")
	}
	if s.HasTrackWithArtworkMBID(unknown) {
		t.Errorf("unknown MBID should report false")
	}
	if s.HasTrackWithArtworkMBID("") {
		t.Errorf("empty MBID should short-circuit to false")
	}
}

func TestStoreHasTrackWithArtistMBID(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	known := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	s.UpsertTrack(&Track{
		Path: "a/b.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtistMBID: known,
	})

	if !s.HasTrackWithArtistMBID(known) {
		t.Errorf("known artist MBID should report true")
	}
	if s.HasTrackWithArtistMBID("not-a-uuid") {
		t.Errorf("unknown artist MBID should report false")
	}
}

func TestStoreSinceFilterIndexedAt(t *testing.T) {
	// ListTracks's `since` is indexed_at, not mtime_ns, so a track with
	// an old file-mtime still surfaces in a delta if it was newly
	// indexed. This covers the "rip from years ago, copy into library
	// today" scenario that the mtime-based filter couldn't see.
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	oldMtime := time.Now().Add(-10 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
	s.UpsertTrack(&Track{Path: "old.flac", Size: 1, ModTime: oldMtime})
	// Sleep spans a couple of Go-time ticks so the cursor lands
	// strictly between the two UpsertTrack calls' indexed_at values.
	time.Sleep(10 * time.Millisecond)
	cursor := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	s.UpsertTrack(&Track{Path: "mid.flac", Size: 1, ModTime: oldMtime})
	s.UpsertTrack(&Track{Path: "new.flac", Size: 1, ModTime: time.Now()})

	all, _ := s.ListTracks(nil)
	if len(all) != 3 {
		t.Errorf("all: %d", len(all))
	}
	newer, _ := s.ListTracks(&cursor)
	if len(newer) != 2 {
		t.Fatalf("newer than cursor: want 2 (mid+new), got %d", len(newer))
	}
	// mid.flac has an ancient file-mtime but a fresh indexed_at — it
	// MUST surface. An mtime-based filter would silently drop it.
	foundMid := false
	for _, tr := range newer {
		if tr.Path == "mid.flac" {
			foundMid = true
		}
	}
	if !foundMid {
		t.Error("mid.flac (ancient mtime, fresh indexed_at) missing from delta")
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s1, _ := OpenStore(path)
	s1.UpsertTrack(&Track{Path: "p.flac", Size: 1, ModTime: time.Now(), Title: "persists"})
	s1.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.GetTrack("p.flac")
	if got == nil || got.Title != "persists" {
		t.Errorf("didn't persist: %+v", got)
	}
}

func TestStoreFolderRoundTrip(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	s.UpsertFolder(&Folder{Path: "a", ModTime: now})
	got, _ := s.FolderMTime("a")
	if !got.Equal(now) {
		t.Errorf("folder mtime: got %v, want %v", got, now)
	}
	folders, _ := s.ListFolders()
	if len(folders) != 1 {
		t.Errorf("folders: %+v", folders)
	}
}

// --- scanner ---

func TestScannerIndexesAllTracks(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)

	n, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != expected {
		t.Errorf("scanned %d, want %d", n, expected)
	}
	all, _ := s.ListTracks(nil)
	if len(all) != expected {
		t.Errorf("stored %d, want %d", len(all), expected)
	}
	// Verify tags propagated.
	found := map[string]bool{}
	for _, tr := range all {
		found[tr.Title] = true
	}
	for _, want := range []string{"Flac Title", "DSF Title", "Mp3 Title"} {
		if !found[want] {
			t.Errorf("missing title %q after scan; found = %v", want, found)
		}
	}
}

func TestScannerSkipsDotFilesAndNonAudio(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	sc.Scan(context.Background())
	for _, p := range mustPaths(t, s) {
		if strings.Contains(p, ".DS_Store") || strings.HasSuffix(p, ".txt") {
			t.Errorf("scanner indexed non-audio path: %q", p)
		}
	}
}

func TestScannerRemovesDeletedTracks(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	sc.Scan(context.Background())

	// Delete one track from disk, rescan, verify its row is gone.
	target := filepath.Join(root, "Artist A", "Album 1", "01 FlacTrack.flac")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	sc.Scan(context.Background())
	got, _ := s.GetTrack("Artist A/Album 1/01 FlacTrack.flac")
	if got != nil {
		t.Errorf("deleted track still in DB: %+v", got)
	}
	// The sibling DSF track should still be there.
	sibling, _ := s.GetTrack("Artist A/Album 1/02 DsfTrack.dsf")
	if sibling == nil {
		t.Error("sibling track lost during deletion pass")
	}
}

func TestScannerSkipsUnchangedFiles(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	sc.Scan(context.Background())

	// Second scan should report the same count; since nothing changed, it
	// doesn't re-extract. We can't easily observe "skipped", but we can
	// verify the DB is still consistent.
	n, _ := sc.Scan(context.Background())
	// n reflects tracks touched/upserted. In a no-change scan, our
	// current implementation still walks but skips the upsert — so n is 0.
	if n != 0 {
		t.Errorf("no-change rescan touched %d tracks, want 0", n)
	}
}

func TestScannerIsScanningFlag(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	if sc.IsScanning() {
		t.Error("fresh scanner reports scanning")
	}
	sc.Scan(context.Background())
	if sc.IsScanning() {
		t.Error("scanner reports scanning after Scan returned")
	}
	if sc.LastFullScan().IsZero() {
		t.Error("LastFullScan is zero after successful scan")
	}
}

// --- manifest ---

func TestBuildManifestShape(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	sc.Scan(context.Background())

	mf, err := BuildManifest(s, []string{root}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if mf.Version != 1 {
		t.Errorf("version = %d", mf.Version)
	}
	if len(mf.Tracks) != expected {
		t.Errorf("tracks = %d, want %d", len(mf.Tracks), expected)
	}
	if len(mf.LibraryRoots) != 1 || mf.LibraryRoots[0] != filepath.Base(root) {
		t.Errorf("libraryRoots = %v", mf.LibraryRoots)
	}
	if mf.GeneratedAt.IsZero() {
		t.Error("generatedAt unset")
	}
	if len(mf.Folders) == 0 {
		t.Error("no folders recorded")
	}
}

func TestBuildManifestSinceFilter(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s)
	sc.Scan(context.Background())

	future := time.Now().Add(time.Hour)
	mf, _ := BuildManifest(s, []string{root}, future)
	if len(mf.Tracks) != 0 {
		t.Errorf("since-future: got %d tracks, want 0", len(mf.Tracks))
	}
}

// --- hot-reloadable roots ---

func TestScannerSetRootsAppliesToNextScan(t *testing.T) {
	a, _ := tempLibrary(t)
	b := filepath.Join(t.TempDir(), "Extra")
	if err := os.MkdirAll(filepath.Join(b, "Artist C", "Album 3"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t,
		filepath.Join(b, "Artist C", "Album 3", "only.flac"),
		44100, 16,
		map[string]string{"TITLE": "Extra", "ARTIST": "Artist C", "ALBUM": "Album 3"},
	)

	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	// Start with only root A.
	sc := NewScanner([]string{a}, s)
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	total, _ := s.CountTracks()
	if total != 3 {
		t.Fatalf("root A count = %d, want 3", total)
	}

	// Transition to multi-root. A 1→N transition changes storage form
	// (tracks get a "<basename>/" prefix), so the admin flow wipes first.
	if err := s.WipeAllTracks(); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	sc.SetRoots([]string{a, b})
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	total, _ = s.CountTracks()
	if total != 4 {
		t.Errorf("A+B count = %d, want 4", total)
	}

	// Per-root counts should match the multi-root storage form.
	nA, _ := s.CountTracksByPrefix(filepath.Base(a) + "/")
	nB, _ := s.CountTracksByPrefix(filepath.Base(b) + "/")
	if nA != 3 || nB != 1 {
		t.Errorf("per-root counts A=%d B=%d, want 3,1", nA, nB)
	}

	// Roots snapshot reflects the update.
	got := sc.Roots()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("Roots() = %v, want [%q, %q]", got, a, b)
	}
}

func TestProviderBuildManifestReflectsSetRoots(t *testing.T) {
	a, _ := tempLibrary(t)
	b := filepath.Join(t.TempDir(), "Other")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{a}, s)
	p := NewProvider(s, sc)

	mfAny, err := p.BuildManifest(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	mf := mfAny.(*Manifest)
	if len(mf.LibraryRoots) != 1 || mf.LibraryRoots[0] != filepath.Base(a) {
		t.Errorf("pre-swap manifest roots = %v", mf.LibraryRoots)
	}

	sc.SetRoots([]string{a, b})
	mfAny, _ = p.BuildManifest(time.Time{})
	mf = mfAny.(*Manifest)
	if len(mf.LibraryRoots) != 2 {
		t.Errorf("post-swap manifest roots = %v, want 2 entries", mf.LibraryRoots)
	}
}

func TestStoreDeleteTracksByPrefix(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	for _, p := range []string{
		"A/1.flac", "A/2.flac", "B/1.flac",
		// Tracks whose path contains SQL LIKE wildcards — the ESCAPE clause
		// must treat these literally rather than as wildcards.
		"A%magic/1.flac", "A_magic/1.flac",
	} {
		if err := s.UpsertTrack(&Track{Path: p, Size: 1, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	nA, _ := s.CountTracksByPrefix("A/")
	if nA != 2 {
		t.Errorf("prefix A/ count = %d, want 2 (the %%/_ variants must not match)", nA)
	}
	removed, err := s.DeleteTracksByPrefix("A/")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("DeleteTracksByPrefix removed %d, want 2", removed)
	}
	total, _ := s.CountTracks()
	if total != 3 {
		t.Errorf("remaining = %d, want 3 (B + two escaped)", total)
	}
}

// --- helpers ---

func mustPaths(t *testing.T, s *Store) []string {
	t.Helper()
	paths, err := s.TrackPaths()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
