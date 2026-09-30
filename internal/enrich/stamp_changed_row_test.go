package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The enricher stamps a row seconds or minutes after its batch read it, and
// the store writes the stamp only while the row is still the one read
// (manifest.ErrTrackChanged otherwise, backlog B187). These tests drive the
// real enricher and store, with the scanner's rewrite of the row landing
// during the MusicBrainz search, and pin what the enricher makes of the
// refusal: nothing is counted, done or skipped, and the next batch reads the
// row again and enriches what it holds now.

const changedRowRelease = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

// rewriteOnFirstSearch returns an MB handler that answers every request with
// body, and on the first request rewrites the row at path as the scanner
// would after a retag (a new title and size, enriched_at reset), through the
// store *storeRef holds by then.
func rewriteOnFirstSearch(t *testing.T, storeRef **manifest.Store, path, body string) http.HandlerFunc {
	t.Helper()
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if err := (*storeRef).UpsertTrack(context.Background(), &manifest.Track{
				Path: path, Size: 2, ModTime: time.Now(),
				Title: "Retagged", Artist: "Artist", Album: "Album",
			}); err != nil {
				t.Errorf("the scanner's rewrite: %v", err)
			}
		})
		_, _ = io.WriteString(w, body)
	}
}

// runUntilStamped runs e until the row at path has left the unenriched set
// (a stamp landed: after the refused one, the next batch's) or 3 s pass, and
// joins the run before it returns, so every count and line the run makes is
// in by then. The join is registered as a cleanup too, for a test that fails
// on the way.
func runUntilStamped(t *testing.T, e *Enricher, store *manifest.Store, path string) {
	t.Helper()
	join := startEnricherForTest(e, 3*time.Second)
	t.Cleanup(join)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := store.UnenrichedTracks(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		stamped := true
		for _, p := range pending {
			if p.Path == path {
				stamped = false
			}
		}
		if stamped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	join()
}

// requireOneRefusalAndNoFailure asserts that the run met the refused stamp
// exactly once, which is what says the rewrite landed between the batch's
// read and its stamp, and that a refusal logged nothing at Warn or above: it
// is no failure, and a library retagged while the enricher works through it
// would log a line per track.
func requireOneRefusalAndNoFailure(t *testing.T, rec *loggingtest.Recorder) {
	t.Helper()
	if got := rec.Lines(msgChangedWhileEnriched); len(got) != 1 {
		t.Errorf("the run met %d refused stamps, want 1 (the rewrite lands between the read and the stamp): %q", len(got), got)
	}
	if got := rec.Failures(); len(got) != 0 {
		t.Errorf("a refused stamp is no failure, and the run logged: %q", got)
	}
}

// requireRetaggedAndStamped asserts that the row holds the rewrite's title
// and size, and that the enricher stamped it.
func requireRetaggedAndStamped(t *testing.T, store *manifest.Store, path string) *manifest.Track {
	t.Helper()
	got, err := store.GetTrack(context.Background(), path)
	if err != nil || got == nil {
		t.Fatalf("GetTrack: err=%v nil=%v", err, got == nil)
	}
	if got.Title != "Retagged" || got.Size != 2 {
		t.Errorf("row title %q size %d, want the rewrite's (\"Retagged\", 2): the stamp wrote back the row its batch read",
			got.Title, got.Size)
	}
	pending, err := store.UnenrichedTracks(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.Path == path {
			t.Errorf("%s is still unenriched: the next batch did not enrich the rewritten row", path)
		}
	}
	return got
}

// TestAStampOverARowThatChangedMidEnrichmentIsNotCounted: the release search
// finds a match, the row is rewritten during it, and the stamp is refused.
// The enricher counts nothing for that pass; its next batch reads the
// rewritten row and enriches it (the album resolves from the cache), so the
// run counts one track done, not two, and the row holds the new tags with
// the release the enricher found.
func TestAStampOverARowThatChangedMidEnrichmentIsNotCounted(t *testing.T) {
	rec := loggingtest.Record(t)
	const path = "Artist/Album/01.flac"
	var store *manifest.Store
	body := `{"releases":[{"id":"` + changedRowRelease + `","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`
	mbSrv := httptest.NewServer(rewriteOnFirstSearch(t, &store, path, body))
	t.Cleanup(mbSrv.Close)
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, changedRowRelease) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	t.Cleanup(caaSrv.Close)
	dir := t.TempDir()
	var err error
	store, err = manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: path, Size: 1, ModTime: time.Now(), Title: "Old", Artist: "Artist", Album: "Album",
	}); err != nil {
		t.Fatal(err)
	}
	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))

	runUntilStamped(t, e, store, path)

	if got := e.Done(); got != 1 {
		t.Errorf("Done() = %d, want 1: the refused stamp counted as a track done", got)
	}
	if got := e.skipped.Load(); got != 0 {
		t.Errorf("skipped = %d, want 0", got)
	}
	got := requireRetaggedAndStamped(t, store, path)
	if got.MusicBrainzAlbumID != changedRowRelease {
		t.Errorf("release %q, want %q: the rewritten row was not enriched", got.MusicBrainzAlbumID, changedRowRelease)
	}
	requireOneRefusalAndNoFailure(t, rec)
}

// TestASkipOverARowThatChangedMidEnrichmentIsNotCounted is the markSkipped
// twin: MusicBrainz answers cleanly with nothing, and the row is rewritten
// during the search. The refused stamp records no skip; the next batch skips
// the rewritten row once, so the run counts one no_mb_match, not two.
func TestASkipOverARowThatChangedMidEnrichmentIsNotCounted(t *testing.T) {
	rec := loggingtest.Record(t)
	const path = "Artist/Album/01.flac"
	var store *manifest.Store
	mbSrv := httptest.NewServer(rewriteOnFirstSearch(t, &store, path, `{"releases":[],"artists":[]}`))
	t.Cleanup(mbSrv.Close)
	caaSrv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(caaSrv.Close)
	dir := t.TempDir()
	var err error
	store, err = manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: path, Size: 1, ModTime: time.Now(), Title: "Old", Artist: "Artist", Album: "Album",
	}); err != nil {
		t.Fatal(err)
	}
	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))

	runUntilStamped(t, e, store, path)

	if got := e.skipped.Load(); got != 1 {
		t.Errorf("skipped = %d, want 1: the refused stamp counted as a skip", got)
	}
	if got := e.SkipReasons()[skipReasonNoMBMatch]; got != 1 {
		t.Errorf("%s = %d, want 1", skipReasonNoMBMatch, got)
	}
	if got := e.Done(); got != 0 {
		t.Errorf("Done() = %d, want 0", got)
	}
	requireRetaggedAndStamped(t, store, path)
	requireOneRefusalAndNoFailure(t, rec)
}
