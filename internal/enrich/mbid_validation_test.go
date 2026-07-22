package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// F30 (2026-07-20 review): the MusicBrainz SEARCH RESULT mbid was never
// validated — only the embedded file tag was, and that check runs BEFORE the
// search. Both values reach ArtworkCachePath / ArtistImagePath, where the
// mbid is the LEADING component of a filepath.Join, and the artwork writer
// MkdirAll's the parent. The MB base URL is operator-configurable
// (musicbrainz / atlas / custom), so a hostile or misconfigured endpoint is a
// real input channel.

// TestSearchResultMBIDTraversalDoesNotEscapeCacheDir is the end-to-end guard:
// a malicious MB endpoint returns a traversing release id, and nothing may be
// written outside the artwork cache dir.
func TestSearchResultMBIDTraversalDoesNotEscapeCacheDir(t *testing.T) {
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"../../../pwned","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()

	// Serve bytes for ANY cover request, so the only thing standing between
	// the hostile id and a file write is the validation under test.
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	}); err != nil {
		t.Fatal(err)
	}

	artDir := filepath.Join(dir, "artwork")
	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		nil, artDir)
	stop := startEnricherForTest(e, 3*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	// Nothing may have been created beside the cache dir. `pwned` would land
	// three levels up from <dir>/artwork — i.e. in the parent of dir.
	for _, escaped := range []string{
		filepath.Join(dir, "pwned-500.jpg"),
		filepath.Join(filepath.Dir(dir), "pwned-500.jpg"),
		filepath.Join(dir, "pwned"),
	} {
		if _, err := os.Stat(escaped); err == nil {
			t.Fatalf("hostile MB release id escaped the cache dir: %s exists", escaped)
		}
	}
	// And any file that DID get written must sit directly in artDir.
	entries, err := os.ReadDir(artDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading artwork dir: %v", err)
	}
	for _, ent := range entries {
		if strings.Contains(ent.Name(), "..") {
			t.Fatalf("artwork dir contains a traversal-shaped entry: %q", ent.Name())
		}
	}
}

// TestBlankArtistAlbumStillScrubsHostileTagMBID pins the ORDERING half of
// F30. The no-artist/album early return used to fire BEFORE the embedded-tag
// scrub, and markSkipped persists tags_json — so a file with a crafted
// musicbrainz_albumid and a blank artist tag kept the hostile value in
// storage, which is exactly what the booklet-cache writer later consumed.
func TestBlankArtistAlbumStillScrubsHostileTagMBID(t *testing.T) {
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const hostile = "../../pwned"
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		// Album deliberately blank: this is the early-return path.
		Artist:             "Artist",
		Album:              "",
		MusicBrainzAlbumID: hostile,
	}); err != nil {
		t.Fatal(err)
	}

	e := NewEnricher(store, nil, nil, nil, filepath.Join(dir, "artwork"))
	stop := startEnricherForTest(e, 3*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	got, err := store.GetTrack(context.Background(), "Artist/Album/01.flac")
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if got != nil && got.MusicBrainzAlbumID == hostile {
		t.Fatal("hostile embedded MBID survived on the blank-artist/album early-return path")
	}
}
