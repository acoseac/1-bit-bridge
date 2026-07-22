package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- Deezer client ---

func TestDeezerSearchArtistPrefersExactName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
			{"id":1,"name":"John Coltrane Quartet","picture_xl":"http://x/fuzzy.jpg","picture_big":"http://x/fuzzy-big.jpg"},
			{"id":2,"name":"John Coltrane","picture_xl":"http://x/exact.jpg","picture_big":"http://x/exact-big.jpg"}
		]}`)
	}))
	defer srv.Close()
	c := NewDeezerClient(srv.URL, "test", nil)
	url, err := c.SearchArtist(context.Background(), "John Coltrane")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://x/exact.jpg" {
		t.Errorf("picked %q, want exact.jpg", url)
	}
}

func TestDeezerSearchArtistFallsBackToBigWhenNoXL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":1,"name":"Artist","picture_xl":"","picture_big":"http://x/big.jpg"}]}`)
	}))
	defer srv.Close()
	c := NewDeezerClient(srv.URL, "test", nil)
	url, _ := c.SearchArtist(context.Background(), "Artist")
	if url != "http://x/big.jpg" {
		t.Errorf("fallback failed, got %q", url)
	}
}

func TestDeezerSearchArtistNoResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	c := NewDeezerClient(srv.URL, "test", nil)
	url, err := c.SearchArtist(context.Background(), "Unknown")
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		t.Errorf("got %q, want empty", url)
	}
}

func TestDeezerFetchImageReturnsBytes(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(want)
	}))
	defer srv.Close()
	c := NewDeezerClient(srv.URL, "test", nil)
	c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	got, err := c.FetchImage(context.Background(), srv.URL+"/picture.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("bytes mismatch")
	}
}

func TestDeezerSendsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	c := NewDeezerClient(srv.URL, "1-bit-bridge/test", nil)
	c.SearchArtist(context.Background(), "X")
	if !strings.Contains(gotUA, "1-bit-bridge") {
		t.Errorf("UA = %q", gotUA)
	}
}

// --- Enricher with Deezer ---

// artistFixture spins up mock MB, CAA, and Deezer servers. MB returns
// one release and one artist; Deezer returns one artist with a picture.
// CAA serves a small JPEG. The Deezer picture URL points at CAA's
// server (reusing the handler) so FetchImage has somewhere to go.
func artistFixture(t *testing.T) (*httptest.Server, *httptest.Server, *httptest.Server, *int32) {
	t.Helper()
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			io.WriteString(w, `{"releases":[{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
		case strings.Contains(r.URL.Path, "/artist/"):
			io.WriteString(w, `{"artists":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"name":"Artist"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))

	var deezerCalls int32
	var deezerBase string
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deezerCalls, 1)
		switch {
		case strings.Contains(r.URL.Path, "/search/artist"):
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`,
				deezerBase+"/picture.jpg")
		default:
			// Picture fetch — serve a JPEG.
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xDE, 0xAD})
		}
	}))
	deezerBase = deezerSrv.URL

	return mbSrv, caaSrv, deezerSrv, &deezerCalls
}

func TestEnricherFetchesAndCachesArtistImage(t *testing.T) {
	mbSrv, caaSrv, deezerSrv, _ := artistFixture(t)
	defer mbSrv.Close()
	defer caaSrv.Close()
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	})

	// Use a custom transport that rewrites the "__IMAGE__" placeholder
	// in Deezer's response URLs to the test server's URL. This lets the
	// mock response reference its own host without knowing it at
	// fixture-build time.
	httpClient := &http.Client{Transport: rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}}
	deezerClient := NewDeezerClient(deezerSrv.URL, "t", httpClient)
	deezerClient.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 3*time.Second)()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched")
	}

	// Verify Track has ArtistMBID set.
	got, _ := store.GetTrack(context.Background(), "Artist/Album/01.flac")
	if got == nil || got.ArtistMBID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Errorf("ArtistMBID not set: %+v", got)
	}

	// Artist image cached on disk.
	imgPath := ArtistImagePath(filepath.Join(dir, "artwork"),
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if _, err := os.Stat(imgPath); err != nil {
		t.Errorf("artist image not cached at %q: %v", imgPath, err)
	}
}

func TestFetchImageRejectsNonDeezerHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should not reach"))
	}))
	defer srv.Close()
	c := NewDeezerClient(DefaultDeezerBase, "test", nil)
	// Default production allowlist (no SetAllowedImageHostsForTest): the
	// httptest host is NOT deezer/dzcdn, so the call must refuse before
	// sending any bytes.
	_, err := c.FetchImage(context.Background(), srv.URL+"/picture.jpg")
	if err == nil {
		t.Fatal("expected SSRF rejection, got nil")
	}
	if !strings.Contains(err.Error(), "refusing non-Deezer") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestFetchImageRejectsOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		// Write maxDeezerImageBytes + 1 bytes.
		buf := make([]byte, 4096)
		written := 0
		for written <= maxDeezerImageBytes {
			n, _ := w.Write(buf)
			if n == 0 {
				return
			}
			written += n
		}
	}))
	defer srv.Close()
	c := NewDeezerClient(DefaultDeezerBase, "test", nil)
	c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	_, err := c.FetchImage(context.Background(), srv.URL+"/huge.jpg")
	if err == nil {
		t.Fatal("expected oversize rejection, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestEnricherDeduplicatesArtistLookups(t *testing.T) {
	var mbArtistCalls int32
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			io.WriteString(w, `{"releases":[{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`)
		case strings.Contains(r.URL.Path, "/artist/"):
			atomic.AddInt32(&mbArtistCalls, 1)
			io.WriteString(w, `{"artists":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"name":"Artist"}]}`)
		}
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	defer caaSrv.Close()
	var deezerSearchCalls int32
	var deezerBase string
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/search/artist") {
			atomic.AddInt32(&deezerSearchCalls, 1)
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q}]}`, deezerBase+"/pic.jpg")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF})
	}))
	deezerBase = deezerSrv.URL
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	for _, p := range []string{"a.flac", "b.flac", "c.flac"} {
		store.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1, ModTime: time.Now(),
			Artist: "Artist", Album: "Album",
		})
	}

	deezerClient := NewDeezerClient(deezerSrv.URL, "t", nil)
	deezerClient.http.Transport = rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}
	deezerClient.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 3*time.Second)()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() < 3 {
		t.Fatalf("only enriched %d of 3", e.Done())
	}
	if mbArtistCalls != 1 {
		t.Errorf("MB artist called %d times, want 1 (sibling dedup broken)", mbArtistCalls)
	}
	if deezerSearchCalls != 1 {
		t.Errorf("Deezer searched %d times, want 1 (pre-cache hit should skip siblings)", deezerSearchCalls)
	}
}

// rewritingTransport replaces the "__IMAGE__" placeholder in URLs with
// the test server's actual URL. Lets the mock response reference its
// own host without knowing it at fixture-build time.
type rewritingTransport struct {
	base string
	tr   http.RoundTripper
}

func (r rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.String(), "__IMAGE__") {
		newURL := strings.ReplaceAll(req.URL.String(), "__IMAGE__", r.base)
		req2, _ := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		req2.Header = req.Header
		return r.tr.RoundTrip(req2)
	}
	return r.tr.RoundTrip(req)
}

// TestArtistImageDedupSkipsSecondDeezerFetch verifies the v1.1 dedup
// behaviour at the ensureArtistImageCached boundary: a SECOND call
// with a different MBID but the SAME artist name must NOT hit Deezer
// a second time. This matters across enricher session restarts when
// MB's release-search returns a different canonical MBID for the
// same artist name (non-deterministic for alternate-name entities).
// Within a single session, artistCache already dedupes by name; the
// name-hashed canonical file is what lets the dedup survive restarts
// without orphaning the prior artist image.
//
// We drive the dedup helper directly (not through Run) so the
// single-session artistCache is not the thing under test — this
// exercises the on-disk canonical + hardlink path.
func TestArtistImageDedupSkipsSecondDeezerFetch(t *testing.T) {
	mbSrv, caaSrv, deezerSrv, deezerCalls := artistFixture(t)
	defer mbSrv.Close()
	defer caaSrv.Close()
	defer deezerSrv.Close()

	dir := t.TempDir()
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	httpClient := &http.Client{Transport: rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}}
	deezerClient := NewDeezerClient(deezerSrv.URL, "t", httpClient)
	deezerClient.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	e := NewEnricher(nil,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		artworkDir)
	e.DeezerMinInterval = 0

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// First call — cold cache, should hit Deezer (search + image).
	ok, err := e.ensureArtistImageCached(ctx, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "Artist")
	if err != nil || !ok {
		t.Fatalf("first call failed: ok=%v err=%v", ok, err)
	}
	firstCallCount := atomic.LoadInt32(deezerCalls)
	if firstCallCount == 0 {
		t.Fatal("first call never reached Deezer — fixture misconfigured")
	}

	// Second call with a DIFFERENT MBID for the SAME artist name.
	// Must NOT make any new Deezer request — the name-hashed canonical
	// already exists; just hardlink the new MBID-keyed path.
	ok, err = e.ensureArtistImageCached(ctx, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "Artist")
	if err != nil || !ok {
		t.Fatalf("second call failed: ok=%v err=%v", ok, err)
	}
	if got := atomic.LoadInt32(deezerCalls); got != firstCallCount {
		t.Errorf("deezer called %d times on dedup path; expected 0 new calls (was %d before)", got-firstCallCount, firstCallCount)
	}

	// Both MBID-keyed paths must exist so the /v1/artist-image handler
	// can serve either MBID without a cache miss.
	for _, mbid := range []string{
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	} {
		p := ArtistImagePath(artworkDir, mbid)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("MBID-keyed path missing: %s", p)
		}
	}

	// Canonical name-hashed file must exist as the source of the hardlinks.
	namePath := ArtistImagePathByName(artworkDir, "Artist")
	if _, err := os.Stat(namePath); err != nil {
		t.Errorf("canonical name-hashed path missing: %s", namePath)
	}
}

// TestCAAReleaseGroupFallbackSalvagesArtwork verifies that when the
// release-level CAA lookup 404s but the release-group has a front
// cover, the enricher writes the release-group bytes to the RELEASE-
// keyed cache path (so iOS's existing `/v1/artwork/{releaseMBID}`
// request serves it transparently).
func TestCAAReleaseGroupFallbackSalvagesArtwork(t *testing.T) {
	wantBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x52, 0x47} // JPEG-like with RG marker

	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"releases":[{"id":"11111111-1111-4111-8111-111111111111","score":100,"title":"Album","artist-credit":[{"name":"Artist"}],"release-group":{"id":"22222222-2222-4222-8222-222222222222","title":"Album","primary-type":"Album"}}]}`)
	}))
	defer mbSrv.Close()

	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/release-group/22222222-2222-4222-8222-222222222222/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(wantBytes)
		case strings.HasPrefix(r.URL.Path, "/release/11111111-1111-4111-8111-111111111111/"):
			http.NotFound(w, r) // release has no CAA cover
		default:
			http.NotFound(w, r)
		}
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	})

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		nil, // no Deezer — artist-image path not exercised here
		filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 3*time.Second)()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched")
	}

	// The release-MBID-keyed path must contain the bytes the
	// release-group endpoint served (transparent to iOS).
	got, err := os.ReadFile(ArtworkCachePath(filepath.Join(dir, "artwork"), "11111111-1111-4111-8111-111111111111", 500))
	if err != nil {
		t.Fatalf("release-keyed artwork missing: %v", err)
	}
	if string(got) != string(wantBytes) {
		t.Errorf("artwork bytes mismatch; got %x, want %x", got, wantBytes)
	}
}
