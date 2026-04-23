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

// --- MusicBrainz client ---

func TestMusicBrainzSearchReleaseHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/release/") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{
			"releases": [
				{"id": "11111111-1111-4111-8111-111111111111", "score": 100, "title": "Blue Train", "artist-credit":[{"name":"John Coltrane"}]},
				{"id": "22222222-2222-4222-8222-222222222222", "score": 85,  "title": "Blue Train (Reissue)", "artist-credit":[{"name":"John Coltrane"}]}
			]
		}`)
	}))
	defer srv.Close()

	c := NewMusicBrainzClient(srv.URL, "test-agent", nil)
	res, err := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("no result")
	}
	if res.MBID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("picked wrong release: %s", res.MBID)
	}
}

// TestMusicBrainzDecodeRealResponseShape locks in the wire shape the
// public MB API actually sends — specifically, `release-group` is an
// OBJECT ({id, title, primary-type}), not a bare string. The initial
// implementation had `ReleaseGroupID string` which silently failed
// every search against prod. This fixture is a trimmed version of a
// real live search response; if MB changes the shape in a
// backward-incompatible way, this test fails loudly instead of
// silently breaking every enrichment in the field.
func TestMusicBrainzDecodeRealResponseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"created": "2024-01-01T00:00:00.000Z",
			"count": 1,
			"offset": 0,
			"releases": [
				{
					"id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
					"score": 100,
					"title": "Blue Train",
					"status": "Official",
					"artist-credit": [
						{"name": "John Coltrane", "artist": {"id": "bbbb", "name": "John Coltrane"}}
					],
					"release-group": {
						"id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
						"title": "Blue Train",
						"primary-type": "Album"
					},
					"country": "US",
					"date": "1958",
					"track-count": 5
				}
			]
		}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, err := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if err != nil {
		t.Fatalf("decode real MB response: %v", err)
	}
	if res == nil || res.MBID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Errorf("got %+v", res)
	}
}

func TestMusicBrainzSearchReleaseNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases": []}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, err := c.SearchRelease(context.Background(), "Unknown", "Artist")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("unexpected match: %+v", res)
	}
}

func TestMusicBrainzSearchReleaseScoreThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"x","score":50,"title":"Blue Train","artist-credit":[{"name":"John Coltrane"}]}]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if res != nil {
		t.Errorf("low-score release was not filtered: %+v", res)
	}
}

func TestMusicBrainzSearchReleaseIgnoresTitleMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"x","score":100,"title":"Totally Different Album","artist-credit":[{"name":"John Coltrane"}]}]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchRelease(context.Background(), "John Coltrane", "Blue Train")
	if res != nil {
		t.Errorf("title-mismatch release leaked through: %+v", res)
	}
}

func TestMusicBrainzSendsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, `{"releases":[]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "1-bit-bridge/0.0.1 (+https://example.com)", nil)
	c.SearchRelease(context.Background(), "A", "B")
	if !strings.Contains(gotUA, "1-bit-bridge") {
		t.Errorf("UA not set: %q", gotUA)
	}
}

func TestMusicBrainzSearchArtistPrefersExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"artists":[
			{"id":"fuzzy","score":95,"name":"Coltrane"},
			{"id":"exact","score":90,"name":"John Coltrane"}
		]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	res, _ := c.SearchArtist(context.Background(), "John Coltrane")
	if res == nil || res.MBID != "exact" {
		t.Errorf("picked %+v, wanted exact name match", res)
	}
}

// --- Cover Art client ---

func TestCoverArtFetchFrontReturnsBytes(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF} // JPEG SOI
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(want)
	}))
	defer srv.Close()
	c := NewCoverArtClient(srv.URL, "test", nil)
	got, err := c.FetchReleaseFront(context.Background(), "11111111-1111-4111-8111-111111111111", 500)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestCoverArtFetchFront404ReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewCoverArtClient(srv.URL, "test", nil)
	_, err := c.FetchReleaseFront(context.Background(), "11111111-1111-4111-8111-111111111111", 500)
	if !IsNotFound(err) {
		t.Errorf("want IsNotFound, got %v", err)
	}
}

func TestCoverArtSizeValidation(t *testing.T) {
	c := NewCoverArtClient("http://unused", "test", nil)
	_, err := c.FetchReleaseFront(context.Background(), "valid-mbid", 42)
	if err == nil || !strings.Contains(err.Error(), "unsupported size") {
		t.Errorf("bad-size error = %v", err)
	}
}

func TestParseSize(t *testing.T) {
	got, err := ParseSize("")
	if err != nil || got != 500 {
		t.Errorf("ParseSize('') = %d, %v", got, err)
	}
	got, err = ParseSize("250")
	if err != nil || got != 250 {
		t.Errorf("ParseSize('250') = %d, %v", got, err)
	}
	if _, err := ParseSize("42"); err == nil {
		t.Error("unsupported size should error")
	}
	if _, err := ParseSize("not-a-number"); err == nil {
		t.Error("non-integer should error")
	}
}

// --- Enricher end-to-end ---

func TestEnricherProcessesTracksEndToEnd(t *testing.T) {
	// Mock MusicBrainz.
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()

	// Mock Cover Art Archive — returns a fake JPEG for the expected MBID.
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed two un-enriched tracks on the same album.
	for _, path := range []string{"Artist/Album/01.flac", "Artist/Album/02.flac"} {
		store.UpsertTrack(&manifest.Track{
			Path:    path,
			Size:    100,
			ModTime: time.Now(),
			Artist:  "Artist",
			Album:   "Album",
			Title:   filepath.Base(path),
		})
	}

	e := NewEnricher(
		store,
		NewMusicBrainzClient(mbSrv.URL, "test", nil),
		NewCoverArtClient(caaSrv.URL, "test", nil),
		nil, // no Deezer in this test — artist-image fallback path tested separately
		filepath.Join(dir, "artwork"),
	)
	e.MBMinInterval = 0 // no pacing in tests
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.Run(ctx)

	// Wait for both to be enriched.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Done() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := e.Done(); got < 2 {
		t.Fatalf("enriched %d, want 2 (skipped=%d)", got, e.skipped.Load())
	}

	// Verify both tracks have ArtworkMBID set.
	all, _ := store.ListTracks(nil)
	for _, tr := range all {
		if tr.ArtworkMBID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Errorf("%s ArtworkMBID = %q", tr.Path, tr.ArtworkMBID)
		}
		if tr.MusicBrainzAlbumID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Errorf("%s MusicBrainzAlbumID = %q", tr.Path, tr.MusicBrainzAlbumID)
		}
	}

	// Artwork cached on disk.
	wantPath := ArtworkCachePath(filepath.Join(dir, "artwork"),
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 500)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("artwork not cached at %q: %v", wantPath, err)
	}
}

func TestEnricherDeduplicatesAlbumLookups(t *testing.T) {
	// Three tracks on the same album should result in exactly one MB
	// release search — the enricher's (artist, album) cache kicks in
	// for the second and third. We count only /release/ hits so the
	// artist-resolution path doesn't muddy the signal.
	var mbReleaseCalls int
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			mbReleaseCalls++
			io.WriteString(w, `{"releases":[{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
		case strings.Contains(r.URL.Path, "/artist/"):
			io.WriteString(w, `{"artists":[]}`)
		}
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	for _, p := range []string{"a.flac", "b.flac", "c.flac"} {
		store.UpsertTrack(&manifest.Track{
			Path: p, Size: 1, ModTime: time.Now(),
			Artist: "Artist", Album: "Album",
		})
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() < 3 {
		t.Fatalf("only enriched %d of 3", e.Done())
	}
	if mbReleaseCalls != 1 {
		t.Errorf("MB /release/ called %d times, want 1 (sibling-track dedup broken)", mbReleaseCalls)
	}
}

func TestEnricherSkipsUnsearchableTracks(t *testing.T) {
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	// Track with no artist/album → can't be searched.
	store.UpsertTrack(&manifest.Track{Path: "orphan.flac", Size: 1, ModTime: time.Now()})

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.skipped.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.skipped.Load() == 0 {
		t.Error("unsearchable track never marked skipped")
	}

	// Track's enriched_at should be non-zero so the worker doesn't spin.
	remaining, _ := store.UnenrichedTracks(100)
	if len(remaining) != 0 {
		t.Errorf("skipped track still un-enriched: %+v", remaining)
	}
}

func TestEnricherSkipsNetworkCallIfCoverAlreadyCached(t *testing.T) {
	var caaCalls int
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caaCalls++
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	artDir := filepath.Join(dir, "artwork")
	// Pre-populate the cache file as if a previous run already cached it.
	os.MkdirAll(artDir, 0o755)
	preCached := ArtworkCachePath(artDir, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", 500)
	os.WriteFile(preCached, []byte("cached"), 0o644)

	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	store.UpsertTrack(&manifest.Track{Path: "x.flac", Size: 1, ModTime: time.Now(), Artist: "Artist", Album: "Album"})

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, artDir)
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched")
	}
	if caaCalls != 0 {
		t.Errorf("CAA called %d times despite pre-cached file", caaCalls)
	}
}

func TestEscapeLucene(t *testing.T) {
	got := escapeLucene(`title "with" specials [and] brackets`)
	for _, want := range []string{`\"`, `\[`, `\]`} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped %q missing %q", got, want)
		}
	}
}

func TestArtworkCachePathFormat(t *testing.T) {
	got := ArtworkCachePath("/x", "aaaa", 500)
	want := filepath.Join("/x", "aaaa-500.jpg")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
