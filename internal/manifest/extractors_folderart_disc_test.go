package manifest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Disc-subfolder parent folder-art fallback (extractLocalArtwork): the
// standard multi-disc layout keeps ONE cover at the album root with tracks
// a level deeper (Album/cover.jpg + Album/Disc 1/track.dsf), invisible to
// the own-directory lookup — the confirmed "Puccini: Turandot" grey-tile
// root cause (2026-07-30, production bridge).

// TestIsDiscFolderName pins the disc-folder naming convention both
// directions — the anchored digit is what keeps real album titles
// ("Disco 2", "Discovery") from triggering a parent climb.
func TestIsDiscFolderName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Matches — common multi-disc subfolder shapes.
		{"Disc 1", true},
		{"disc 01", true},
		{"Disc1", true},
		{"DISC 2", true},
		{"Disk 3", true},
		{"disk10", true},
		{"CD 1", true},
		{"CD2", true},
		{"cd03", true},
		{"CD-2", true},
		{"cd_2", true},
		{"cd.1", true},
		{"LP 1", true},
		{"lp2", true},
		{"BD 1", true},
		{"DVD 2", true},
		{"Disc 1 - Bonus Material", true},
		{"Disc 2 (Live)", true},
		{"CD 1 [Remaster]", true},
		{"disc 1 of 2", true},
		{"CD 12", true},
		{"Disc 1 ", true}, // trailing space (Windows-SMB copies) — trimmed
		// Non-matches — album titles and shapes that must NOT climb.
		{"Disco 2", false}, // Pet Shop Boys album
		{"Disco 3", false},
		{"Discovery", false}, // Daft Punk album
		{"Discography", false},
		{"cd", false}, // no number
		{"Disc", false},
		{"CDs", false},
		{"CD 1234", false},  // year/catalog shape (4+ digits)
		{"cd1extra", false}, // suffix must start with a separator
		{"Vol 1", false},    // volumes are usually distinct albums
		{"Volume 2", false},
		{"Side A", false}, // vinyl side — plausible album name
		{"1 CD", false},
		{"Turandot", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isDiscFolderName(c.name); got != c.want {
			t.Errorf("isDiscFolderName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// discArtTree builds Album/cover.jpg + Album/<discDir>/<file> under a temp
// root and returns (root, albumDir, audioPath, cacheDir, coverBytes).
func discArtTree(t *testing.T, discDir, audioName string) (string, string, string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	albumDir := filepath.Join(root, "Puccini", "Turandot")
	trackDir := filepath.Join(albumDir, discDir)
	if err := os.MkdirAll(trackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cover := append([]byte{}, minimalJPEG...)
	cover = append(cover, 'D', 'I', 'S', 'C') // distinct hash from other fixtures
	if err := os.WriteFile(filepath.Join(albumDir, "cover.jpg"), cover, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, albumDir, filepath.Join(trackDir, audioName), cacheDir, cover
}

// The headline case: a DSF (no embedded art) in Album/Disc 1/ picks up the
// album-root cover.jpg — exercising the extractDSFWithContext call site,
// exactly the Turandot shape.
func TestExtractLocalArtwork_DiscSubfolderParentCover(t *testing.T) {
	_, _, audioPath, cacheDir, cover := discArtTree(t, "Disc 1", "01 Nessun Dorma.dsf")
	writeMinimalDSF(t, audioPath, 2822400, map[string]string{
		"title": "Nessun Dorma", "artist": "Zubin Mehta", "album": "Puccini: Turandot",
	})

	tr := &Track{Path: "Puccini/Turandot/Disc 1/01 Nessun Dorma.dsf", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}

	want := expectedLocalMBID(cover)
	if tr.ArtworkMBID != want {
		t.Errorf("ArtworkMBID = %q, want %q (parent-dir cover.jpg via disc fallback)", tr.ArtworkMBID, want)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, want+"-500.jpg")); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

// Negative: a NON-disc-named directory must not inherit its parent's art —
// Artist/cover.jpg above Artist/Album/track would mis-attribute artist
// images to every album.
func TestExtractLocalArtwork_NonDiscDirDoesNotInheritParent(t *testing.T) {
	root := t.TempDir()
	artistDir := filepath.Join(root, "ArtistName")
	albumDir := filepath.Join(artistDir, "AlbumName")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artistDir, "cover.jpg"), minimalJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(albumDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})

	tr := &Track{Path: "ArtistName/AlbumName/track.mp3", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want \"\" (non-disc dir must not climb to the artist folder)", tr.ArtworkMBID)
	}
}

// Own-directory candidates always win over the parent's.
func TestExtractLocalArtwork_OwnDirBeatsParent(t *testing.T) {
	_, _, audioPath, cacheDir, _ := discArtTree(t, "Disc 1", "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})
	ownCover := append([]byte{}, minimalJPEG...)
	ownCover = append(ownCover, 'O', 'W', 'N')
	if err := os.WriteFile(filepath.Join(filepath.Dir(audioPath), "folder.jpg"), ownCover, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Track{Path: "Puccini/Turandot/Disc 1/track.mp3", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if want := expectedLocalMBID(ownCover); tr.ArtworkMBID != want {
		t.Errorf("ArtworkMBID = %q, want %q (own-dir folder.jpg beats the parent cover)", tr.ArtworkMBID, want)
	}
}

// Two disc subfolders share ONE parent lookup through the same
// single-flight cache — same mbid, one parent cache entry.
func TestExtractLocalArtwork_DiscSubfoldersShareOneParentLookup(t *testing.T) {
	_, albumDir, audio1, cacheDir, cover := discArtTree(t, "Disc 1", "a.mp3")
	disc2 := filepath.Join(albumDir, "Disc 2")
	if err := os.MkdirAll(disc2, 0o755); err != nil {
		t.Fatal(err)
	}
	audio2 := filepath.Join(disc2, "b.mp3")
	writeMinimalMP3(t, audio1, map[string]string{"artist": "A", "album": "B"})
	writeMinimalMP3(t, audio2, map[string]string{"artist": "A", "album": "B"})

	cache := &sync.Map{}
	ec := &ExtractContext{ArtworkCacheDir: cacheDir, FolderArtCache: cache}
	tr1 := &Track{Path: "a", Size: 1, ModTime: time.Now()}
	tr2 := &Track{Path: "b", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audio1, tr1, ec); err != nil {
		t.Fatal(err)
	}
	if err := ExtractWithContext(audio2, tr2, ec); err != nil {
		t.Fatal(err)
	}

	want := expectedLocalMBID(cover)
	if tr1.ArtworkMBID != want || tr2.ArtworkMBID != want {
		t.Errorf("mbids = %q / %q, want both %q", tr1.ArtworkMBID, tr2.ArtworkMBID, want)
	}
	// The album root has exactly ONE promise entry, shared by both discs.
	if _, ok := cache.Load(albumDir); !ok {
		t.Errorf("parent dir %q missing from the single-flight cache", albumDir)
	}
	entries := 0
	cache.Range(func(_, _ any) bool { entries++; return true })
	if entries != 3 { // Disc 1 (negative), Disc 2 (negative), album root (positive)
		t.Errorf("cache entries = %d, want 3 (two disc dirs + one shared parent)", entries)
	}
}

// A library ROOT literally named like a disc folder must never read its
// parent — that directory is outside the configured library.
func TestExtractLocalArtwork_RootNamedLikeDiscNotEscaped(t *testing.T) {
	outer := t.TempDir()
	rootDir := filepath.Join(outer, "Disc 1")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cover OUTSIDE the library (in the root's parent).
	if err := os.WriteFile(filepath.Join(outer, "cover.jpg"), minimalJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(rootDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
		LibraryRootDirs: map[string]struct{}{filepath.Clean(rootDir): {}},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want \"\" (a root named like a disc folder must not escape the library)", tr.ArtworkMBID)
	}
}
