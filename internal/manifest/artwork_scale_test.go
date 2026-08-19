package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// encodeTestImage renders a w×h image with deterministic per-pixel noise
// (so JPEG can't compress it to nothing — the size-trigger tests need
// real byte weight) and encodes it as JPEG at the given quality, or as
// PNG when quality < 0.
func encodeTestImage(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Deterministic pseudo-noise (no math/rand — reproducible bytes).
	seed := uint32(2463534242)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			img.SetRGBA(x, y, color.RGBA{uint8(seed), uint8(seed >> 8), uint8(seed >> 16), 0xFF})
		}
	}
	var buf bytes.Buffer
	if quality < 0 {
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("encode png: %v", err)
		}
	} else {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			t.Fatalf("encode jpeg: %v", err)
		}
	}
	return buf.Bytes()
}

// encodeSolidImage is the flat-color sibling of encodeTestImage for
// fixtures that must stay BYTE-LIGHT at large dimensions (e.g. the
// rescale hysteresis band, which needs big dims under the size trigger).
func encodeSolidImage(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{0x40, 0x80, 0xC0, 0xFF})
		}
	}
	var buf bytes.Buffer
	if quality < 0 {
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("encode png: %v", err)
		}
	} else {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			t.Fatalf("encode jpeg: %v", err)
		}
	}
	return buf.Bytes()
}

// decodeDims decodes the produced bytes and returns (width, height,
// format).
func decodeDims(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

func TestScaleLocalArtwork_SmallJPEGReturnsVerbatim(t *testing.T) {
	src := encodeTestImage(t, 800, 600, 85)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Errorf("small JPEG must pass through byte-identical (got %d bytes, want %d)", len(out), len(src))
	}
}

func TestScaleLocalArtwork_LargeJPEGDownscaledTo1200(t *testing.T) {
	src := encodeTestImage(t, 2400, 1800, 85)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	w, h, format := decodeDims(t, out)
	if format != "jpeg" {
		t.Errorf("output format = %q, want jpeg", format)
	}
	if w != 1200 || h != 900 {
		t.Errorf("output dims = %dx%d, want 1200x900 (aspect-preserving longest-side cap)", w, h)
	}
	if len(out) >= len(src) {
		t.Errorf("downscale did not shrink bytes (%d -> %d)", len(src), len(out))
	}
}

func TestScaleLocalArtwork_PortraitLongestSideWins(t *testing.T) {
	src := encodeTestImage(t, 900, 2400, 85)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	w, h, _ := decodeDims(t, out)
	if h != 1200 || w != 450 {
		t.Errorf("output dims = %dx%d, want 450x1200", w, h)
	}
}

func TestScaleLocalArtwork_SmallPNGTranscodedToJPEG(t *testing.T) {
	src := encodeTestImage(t, 640, 640, -1)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !looksLikeJPEG(out) {
		t.Fatalf("PNG source must transcode to JPEG output; got prefix %x", out[:4])
	}
	w, h, _ := decodeDims(t, out)
	if w != 640 || h != 640 {
		t.Errorf("within-bounds PNG must keep its dimensions, got %dx%d", w, h)
	}
}

func TestScaleLocalArtwork_LargePNGDownscaledToJPEG(t *testing.T) {
	src := encodeTestImage(t, 2400, 1200, -1)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !looksLikeJPEG(out) {
		t.Fatalf("PNG source must transcode to JPEG output")
	}
	w, h, _ := decodeDims(t, out)
	if w != 1200 || h != 600 {
		t.Errorf("output dims = %dx%d, want 1200x600", w, h)
	}
}

func TestScaleLocalArtwork_JunkRejected(t *testing.T) {
	for name, data := range map[string][]byte{
		"gif":       []byte("GIF89a\x01\x00\x01\x00"),
		"random":    {0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		"empty":     {},
		"png-junk":  append([]byte{0x89, 0x50, 0x4E, 0x47}, bytes.Repeat([]byte{0xAB}, 64)...),
		"truncated": {0x89, 0x50},
	} {
		if _, err := scaleLocalArtwork(data); err == nil {
			t.Errorf("%s: want error for undecodable non-JPEG input", name)
		}
	}
}

func TestScaleLocalArtwork_UnparseableJPEGPassesThrough(t *testing.T) {
	// A JPEG-SOI blob Go's decoder can't parse (the minimalJPEG shape the
	// APIC tests use). Pre-scaling these bytes were stored verbatim;
	// dropping them now would regress a previously-served cover.
	src := append([]byte{}, minimalJPEG...)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Errorf("unparseable JPEG must pass through verbatim")
	}
}

func TestScaleLocalArtwork_OverAxisCapJPEGPassesThrough(t *testing.T) {
	// 16001×1 exceeds localArtMaxSourceAxisPx; decoding is refused and
	// the JPEG passes through verbatim (it was stored verbatim before
	// scaling existed — no regression).
	src := encodeTestImage(t, localArtMaxSourceAxisPx+1, 1, 60)
	out, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Errorf("over-cap JPEG must pass through verbatim")
	}
}

func TestScaleLocalArtwork_OverAxisCapPNGRejected(t *testing.T) {
	// The PNG twin is REFUSED: it was never served before (the JPEG-only
	// gate), and a verbatim PNG write would put PNG bytes behind an
	// image/jpeg label.
	src := encodeTestImage(t, localArtMaxSourceAxisPx+1, 1, -1)
	if _, err := scaleLocalArtwork(src); err == nil {
		t.Errorf("over-cap PNG must be rejected")
	}
}

func TestScaleLocalArtworkImpl_ForceReencodesWithinBoundsJPEG(t *testing.T) {
	// The rescale one-shot's byte-size trigger: dimensions fine, encode
	// heavy. force=true must decode + re-encode (smaller at q82) where
	// the normal path returns verbatim.
	src := encodeTestImage(t, 1000, 1000, 100)
	verbatim, err := scaleLocalArtwork(src)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if !bytes.Equal(verbatim, src) {
		t.Fatalf("precondition: normal path must return within-bounds JPEG verbatim")
	}
	forced, err := scaleLocalArtworkImpl(src, true)
	if err != nil {
		t.Fatalf("forced scale: %v", err)
	}
	if bytes.Equal(forced, src) {
		t.Errorf("force=true must re-encode, not pass through")
	}
	if len(forced) >= len(src) {
		t.Errorf("q82 re-encode of a q100 source should shrink (%d -> %d)", len(src), len(forced))
	}
	w, h, _ := decodeDims(t, forced)
	if w != 1000 || h != 1000 {
		t.Errorf("forced re-encode must keep within-bounds dimensions, got %dx%d", w, h)
	}
}

// TestStampLocalArtwork_HashKeysOriginalBytes pins the key-stability
// contract: the `local-<hash>` sentinel hashes the ORIGINAL bytes even
// though the FILE now holds scaled bytes. The negative control inside:
// the scaled output's own hash differs, so if a future edit hashed the
// scaled bytes instead, the filename assertion below goes red.
func TestStampLocalArtwork_HashKeysOriginalBytes(t *testing.T) {
	cacheDir := t.TempDir()
	src := encodeTestImage(t, 2400, 2400, 85)

	mbid, ok := stampLocalArtwork(src, cacheDir)
	if !ok {
		t.Fatal("stampLocalArtwork failed")
	}
	origSum := sha256.Sum256(src)
	wantMBID := "local-" + hex.EncodeToString(origSum[:])
	if mbid != wantMBID {
		t.Fatalf("mbid = %q, want original-bytes hash %q", mbid, wantMBID)
	}
	cached, err := os.ReadFile(filepath.Join(cacheDir, mbid+"-500.jpg"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if bytes.Equal(cached, src) {
		t.Fatalf("cache file holds the raw bytes — scaling did not run")
	}
	w, h, _ := decodeDims(t, cached)
	if w != 1200 || h != 1200 {
		t.Errorf("cached dims = %dx%d, want 1200x1200", w, h)
	}
	// Negative control for the key claim: the scaled bytes hash to a
	// DIFFERENT sentinel, so hashing post-scale would have produced a
	// different filename and the ReadFile above would have failed.
	scaledSum := sha256.Sum256(cached)
	if hex.EncodeToString(scaledSum[:]) == hex.EncodeToString(origSum[:]) {
		t.Fatal("control invalid: scaled bytes hash equal to original")
	}
}

func TestStampLocalArtwork_ExistingFileSkipsRescale(t *testing.T) {
	// Stat-before-write idempotence survives the scaling change: a file
	// already on disk under the original-bytes hash is NOT rewritten
	// (pre-scaling raw files are the rescale one-shot's job, not the
	// scanner's).
	cacheDir := t.TempDir()
	src := encodeTestImage(t, 2400, 2400, 85)
	sum := sha256.Sum256(src)
	mbid := "local-" + hex.EncodeToString(sum[:])
	path := filepath.Join(cacheDir, mbid+"-500.jpg")
	if err := os.WriteFile(path, src, 0o644); err != nil { // raw pre-scaling bytes
		t.Fatal(err)
	}
	got, ok := stampLocalArtwork(src, cacheDir)
	if !ok || got != mbid {
		t.Fatalf("stamp = (%q, %v), want (%q, true)", got, ok, mbid)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, src) {
		t.Errorf("existing cache file must not be rewritten by the scanner path")
	}
}
