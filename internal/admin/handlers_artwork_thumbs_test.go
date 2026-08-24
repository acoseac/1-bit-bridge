package admin

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// encodeSquareJPEG builds a deterministic, compressible-but-not-flat
// JPEG so a downscale measurably shrinks it.
func encodeSquareJPEG(t *testing.T, px int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, px, px))
	seed := uint32(2463534242)
	for y := 0; y < px; y++ {
		for x := 0; x < px; x++ {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			img.SetRGBA(x, y, color.RGBA{uint8(seed), uint8(seed >> 8), uint8(seed >> 16), 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func longestSide(t *testing.T, b []byte) int {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width > cfg.Height {
		return cfg.Width
	}
	return cfg.Height
}

// artworkThumbFixture wires a server whose cover cache holds ONE tier —
// a 1200 px image filed under the historical `-500` suffix, which is
// exactly what stampLocalArtwork produces on a real bridge.
func artworkThumbFixture(t *testing.T) (*Server, string, []byte) {
	t.Helper()
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	srv.deps.ArtworkPath = func(mbid string, size int) string {
		return filepath.Join(dir, fmt.Sprintf("%s-%d.jpg", mbid, size))
	}
	srv.deps.ArtworkThumbPath = func(key string, size int) string {
		return filepath.Join(dir, manifest.ThumbsDirName, fmt.Sprintf("%s-%d.jpg", key, size))
	}
	src := encodeSquareJPEG(t, 1200)
	if err := os.WriteFile(filepath.Join(dir, metaLocalSha+"-500.jpg"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, dir, src
}

func fetchArtwork(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = "127.0.0.1:54321" // past the loopback boundary middleware
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	return rw
}

// TestArtworkSizeIsHonored is the regression this whole tier exists for:
// before it, all three sizes answered with the same oversized bytes,
// because the ladder matched on FILENAME and every local cover is filed
// under `-500` whatever its real dimensions.
func TestArtworkSizeIsHonored(t *testing.T) {
	srv, _, src := artworkThumbFixture(t)

	for _, size := range []int{250, 500} {
		rw := fetchArtwork(t, srv, fmt.Sprintf("/api/library/artwork/%s?size=%d", metaLocalSha, size))
		if rw.Code != http.StatusOK {
			t.Fatalf("size=%d: status %d", size, rw.Code)
		}
		got := rw.Body.Bytes()
		if px := longestSide(t, got); px != size {
			t.Errorf("size=%d served a %d px image", size, px)
		}
		if len(got) >= len(src) {
			t.Errorf("size=%d served %d bytes, source is %d — no saving", size, len(got), len(src))
		}
	}

	// 1200 has nothing larger to derive from, so it serves the source
	// untouched — the pre-existing ladder behaviour, unchanged.
	rw := fetchArtwork(t, srv, "/api/library/artwork/"+metaLocalSha+"?size=1200")
	if !bytes.Equal(rw.Body.Bytes(), src) {
		t.Errorf("size=1200 should serve the stored tier verbatim (%d bytes, got %d)", len(src), rw.Body.Len())
	}
}

// TestArtworkDerivationLeavesTheSourceCacheAlone is the /v1 invariant in
// test form. Derived tiers live in a SUBDIRECTORY precisely so the
// bearer-authed /v1/artwork ladder — which stats the requested size in
// the artwork dir itself — cannot pick them up and silently change the
// bytes iOS receives.
func TestArtworkDerivationLeavesTheSourceCacheAlone(t *testing.T) {
	srv, dir, src := artworkThumbFixture(t)

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rw := fetchArtwork(t, srv, "/api/library/artwork/"+metaLocalSha+"?size=250"); rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one new entry, and it is the thumbs DIRECTORY.
	if len(after) != len(before)+1 {
		t.Fatalf("artwork dir went from %d to %d entries", len(before), len(after))
	}
	var added os.DirEntry
	for _, e := range after {
		found := false
		for _, b := range before {
			if b.Name() == e.Name() {
				found = true
				break
			}
		}
		if !found {
			added = e
		}
	}
	if added == nil || !added.IsDir() || added.Name() != manifest.ThumbsDirName {
		t.Fatalf("derivation added %v to the source cache; want only the %q subdirectory",
			added, manifest.ThumbsDirName)
	}
	// And the source tier's bytes are exactly what they were.
	got, err := os.ReadFile(filepath.Join(dir, metaLocalSha+"-500.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Error("the stored tier was rewritten; /v1 would now serve different bytes")
	}
}

// TestArtworkDerivationIsCachedNotRepeated pins that the second request
// is served from disk rather than re-decoding.
func TestArtworkDerivationIsCachedNotRepeated(t *testing.T) {
	srv, dir, _ := artworkThumbFixture(t)
	thumb := filepath.Join(dir, manifest.ThumbsDirName, metaLocalSha+"-250.jpg")

	fetchArtwork(t, srv, "/api/library/artwork/"+metaLocalSha+"?size=250")
	if _, err := os.Stat(thumb); err != nil {
		t.Fatalf("thumb not written: %v", err)
	}

	// Detect reuse by CONTENT, not mtime: two writes inside one coarse
	// clock tick (Windows is ~15.6 ms) leave identical mtimes, so an
	// mtime comparison would pass even if the thumb had been rebuilt.
	sentinel := []byte("sentinel-not-a-real-jpeg")
	if err := os.WriteFile(thumb, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(thumb, future, future); err != nil {
		t.Fatal(err)
	}

	rw := fetchArtwork(t, srv, "/api/library/artwork/"+metaLocalSha+"?size=250")
	if !bytes.Equal(rw.Body.Bytes(), sentinel) {
		t.Error("thumb was re-derived on a second request instead of served from cache")
	}
}

// TestArtworkFallsBackWhenDerivationIsUnwired keeps the feature strictly
// additive: a bridge that never wires ArtworkThumbPath must behave
// exactly as it did before, serving the stored tier for every size.
func TestArtworkFallsBackWhenDerivationIsUnwired(t *testing.T) {
	srv, _, src := artworkThumbFixture(t)
	srv.deps.ArtworkThumbPath = nil

	for _, size := range []int{250, 500, 1200} {
		rw := fetchArtwork(t, srv, fmt.Sprintf("/api/library/artwork/%s?size=%d", metaLocalSha, size))
		if rw.Code != http.StatusOK {
			t.Fatalf("size=%d: status %d", size, rw.Code)
		}
		if !bytes.Equal(rw.Body.Bytes(), src) {
			t.Errorf("size=%d: expected the stored tier verbatim with derivation unwired", size)
		}
	}
}

// TestArtistImageSizeIsOptional pins that the artist route only changes
// behaviour when a caller opts in. Omitting ?size= must serve the stored
// portrait byte-for-byte.
func TestArtistImageSizeIsOptional(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	src := encodeSquareJPEG(t, 900)
	if err := os.WriteFile(filepath.Join(dir, "artist-"+metaUUIDRelease+".jpg"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.deps.ArtistImagePath = func(mbid string) string {
		return filepath.Join(dir, "artist-"+mbid+".jpg")
	}
	srv.deps.ArtworkThumbPath = func(key string, size int) string {
		return filepath.Join(dir, manifest.ThumbsDirName, fmt.Sprintf("%s-%d.jpg", key, size))
	}

	rw := fetchArtwork(t, srv, "/api/library/artist-image/"+metaUUIDRelease)
	if rw.Code != http.StatusOK || !bytes.Equal(rw.Body.Bytes(), src) {
		t.Errorf("no ?size= must serve the stored portrait verbatim (status %d, %d bytes)", rw.Code, rw.Body.Len())
	}

	rw = fetchArtwork(t, srv, "/api/library/artist-image/"+metaUUIDRelease+"?size=250")
	if rw.Code != http.StatusOK {
		t.Fatalf("sized portrait: status %d", rw.Code)
	}
	if px := longestSide(t, rw.Body.Bytes()); px != 250 {
		t.Errorf("?size=250 served a %d px portrait", px)
	}

	rw = fetchArtwork(t, srv, "/api/library/artist-image/"+metaUUIDRelease+"?size=0")
	if rw.Code != http.StatusBadRequest {
		t.Errorf("?size=0 status = %d, want 400", rw.Code)
	}
}
