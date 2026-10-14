package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// portraitStub serves Deezer's artist search naming one artist, whose
// picture at /picture.jpg is body, and counts the requests it answers.
func portraitStub(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "/search/artist"):
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`, srvURL+"/picture.jpg")
		case strings.HasSuffix(r.URL.Path, "/picture.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	return srv, &requests
}

// portraitEnricher returns an enricher whose clients all talk to srv, with
// srv's host allowed for images, caching into artworkDir, which it creates.
func portraitEnricher(t *testing.T, srv *httptest.Server, artworkDir string) *Enricher {
	t.Helper()
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deezer := NewDeezerClient(srv.URL, "t", nil)
	deezer.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(nil,
		NewMusicBrainzClient(srv.URL, "t", nil),
		NewCoverArtClient(srv.URL, "t", nil),
		deezer, artworkDir)
	e.DeezerMinInterval = 0
	return e
}

// TestArtistPortraitIsStoredOnlyWhenItIsAnImage — backlog B168: the Deezer
// portrait path wrote FetchImage's bytes through the buffered
// writeArtworkAtomic, with neither the containment check nor the
// image-signature check the cover writes got in #976 (writeArtworkAtomicStream).
// An allowlisted host serving HTML was stored as the portrait, which
// `/v1/artist-image` serves as image/jpeg. The JPEG case is the positive
// control: the stub reaches the write.
func TestArtistPortraitIsStoredOnlyWhenItIsAnImage(t *testing.T) {
	const mbid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, c := range []struct {
		name   string
		body   []byte
		stored bool
	}{
		{"an HTML body", []byte("<html><body>not an image</body></html>"), false},
		{"a JPEG body", []byte{0xFF, 0xD8, 0xFF, 0xE1, 0xDE, 0xAD}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := portraitStub(t, c.body)
			artworkDir := filepath.Join(t.TempDir(), "artwork")
			e := portraitEnricher(t, srv, artworkDir)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ok, err := e.ensureArtistImageCached(ctx, mbid, "Artist")

			paths := []string{ArtistImagePath(artworkDir, mbid), ArtistImagePathByName(artworkDir, "Artist")}
			if c.stored {
				if err != nil || !ok {
					t.Fatalf("a JPEG portrait was not cached: ok=%v err=%v", ok, err)
				}
				for _, p := range paths {
					if _, statErr := os.Stat(p); statErr != nil {
						t.Errorf("a JPEG portrait is missing at %s: %v", filepath.Base(p), statErr)
					}
				}
				return
			}
			if ok || err == nil {
				t.Errorf("an HTML body was accepted as a portrait: ok=%v err=%v", ok, err)
			}
			for _, p := range paths {
				if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
					body, _ := os.ReadFile(p)
					t.Errorf("an HTML body was stored at %s: %q", filepath.Base(p), body)
				}
			}
			entries, _ := os.ReadDir(artworkDir)
			for _, en := range entries {
				t.Errorf("the cache holds %s after a refused body", en.Name())
			}
		})
	}
}

// TestArtistPortraitPathsStayInsideTheCache — a pre-merge review of B168:
// the stream primitive's containment check covers the canonical it writes,
// never the MBID path the canonical is then hard-linked or copied to
// (linkOrCopy). Every caller checks the MBID's shape first, so this is the
// layer under that check: an MBID that would leave the cache is refused
// before anything is fetched. The cache sits deep in the test's own temp
// directory, so on the unchecked code the escaping link lands inside it.
func TestArtistPortraitPathsStayInsideTheCache(t *testing.T) {
	srv, requests := portraitStub(t, []byte{0xFF, 0xD8, 0xFF, 0xE1, 0xDE, 0xAD})
	base := filepath.Join(t.TempDir(), "a", "b", "c")
	e := portraitEnricher(t, srv, filepath.Join(base, "artwork"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const escaping = "x/../../escape"
	ok, err := e.ensureArtistImageCached(ctx, escaping, "Artist")
	if ok || err == nil {
		t.Errorf("an MBID that leaves the cache was accepted: ok=%v err=%v", ok, err)
	}
	if outside := filepath.Join(base, "escape.jpg"); fileExists(outside) {
		t.Errorf("a portrait was written outside the cache, at %s", outside)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the refused MBID still reached Deezer (%d requests)", n)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
