package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
			var srvURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/search/artist"):
					fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q,"picture_big":""}]}`,
						srvURL+"/picture.jpg")
				case strings.HasSuffix(r.URL.Path, "/picture.jpg"):
					w.Header().Set("Content-Type", "image/jpeg")
					_, _ = w.Write(c.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			srvURL = srv.URL

			artworkDir := filepath.Join(t.TempDir(), "artwork")
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
