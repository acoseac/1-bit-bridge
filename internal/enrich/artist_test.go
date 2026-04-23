package enrich

import (
	"context"
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
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deezerCalls, 1)
		switch {
		case strings.Contains(r.URL.Path, "/search/artist"):
			io.WriteString(w, `{"data":[{"id":1,"name":"Artist","picture_xl":"__IMAGE__/picture.jpg","picture_big":""}]}`)
		default:
			// Picture fetch — serve a JPEG.
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xDE, 0xAD})
		}
	}))

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
	store.UpsertTrack(&manifest.Track{
		Path: "Artist/Album/01.flac", Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	})

	// Use a custom transport that rewrites the "__IMAGE__" placeholder
	// in Deezer's response URLs to the test server's URL. This lets the
	// mock response reference its own host without knowing it at
	// fixture-build time.
	httpClient := &http.Client{Transport: rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}}
	deezerClient := NewDeezerClient(deezerSrv.URL, "t", httpClient)

	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.DeezerMinInterval = 0
	e.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.Done() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Done() == 0 {
		t.Fatal("track never enriched")
	}

	// Verify Track has ArtistMBID set.
	got, _ := store.GetTrack("Artist/Album/01.flac")
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
	deezerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/search/artist") {
			atomic.AddInt32(&deezerSearchCalls, 1)
		}
		io.WriteString(w, `{"data":[{"id":1,"name":"Artist","picture_xl":"__IMAGE__/pic.jpg"}]}`)
	}))
	defer deezerSrv.Close()

	dir := t.TempDir()
	store, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	defer store.Close()
	for _, p := range []string{"a.flac", "b.flac", "c.flac"} {
		store.UpsertTrack(&manifest.Track{
			Path: p, Size: 1, ModTime: time.Now(),
			Artist: "Artist", Album: "Album",
		})
	}

	deezerClient := NewDeezerClient(deezerSrv.URL, "t", nil)
	deezerClient.http.Transport = rewritingTransport{base: deezerSrv.URL, tr: http.DefaultTransport}
	e := NewEnricher(store,
		NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil),
		deezerClient,
		filepath.Join(dir, "artwork"))
	e.MBMinInterval = 0
	e.CAAMinInterval = 0
	e.DeezerMinInterval = 0
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
