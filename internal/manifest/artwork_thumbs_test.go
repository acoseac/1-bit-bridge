package manifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeArtFixture drops an encoded image at path and returns it.
func writeArtFixture(t *testing.T, path string, w, h, quality int) []byte {
	t.Helper()
	data := encodeTestImage(t, w, h, quality)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return data
}

func TestEnsureThumbDerivesSmallerTier(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "local-abc-500.jpg")
	srcBytes := writeArtFixture(t, src, 1200, 1200, 90)
	dst := filepath.Join(dir, ThumbsDirName, "local-abc-250.jpg")

	if err := EnsureThumb(src, dst, 250); err != nil {
		t.Fatalf("EnsureThumb: %v", err)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read thumb: %v", err)
	}
	w, h, format := decodeDims(t, out)
	if w != 250 || h != 250 {
		t.Errorf("thumb dims = %dx%d, want 250x250", w, h)
	}
	if format != "jpeg" {
		t.Errorf("thumb format = %q, want jpeg", format)
	}
	// The whole point: the derived tier must be materially smaller.
	if len(out) >= len(srcBytes) {
		t.Errorf("thumb is %d bytes, source %d — derivation saved nothing", len(out), len(srcBytes))
	}
	// The source must be untouched. A writer that rewrote it in place
	// would silently change what /v1/artwork serves to iOS.
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if len(after) != len(srcBytes) {
		t.Errorf("source was modified: %d bytes, was %d", len(after), len(srcBytes))
	}
}

func TestEnsureThumbNeverUpscales(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "small.jpg")
	writeArtFixture(t, src, 200, 200, 85)
	dst := filepath.Join(dir, ThumbsDirName, "small-500.jpg")

	err := EnsureThumb(src, dst, 500)
	if !ErrThumbNotNeeded(err) {
		t.Fatalf("EnsureThumb err = %v, want errThumbNotNeeded", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a thumb was written for an already-small source; stat err = %v", statErr)
	}
}

// TestEnsureThumbRederivesWhenSourceIsNewer is the artist-portrait case
// and the reason the freshness check exists at all. Album covers are
// content-keyed (local-<sha256>), so a changed cover is a changed
// filename and its thumb can never go stale. Artist portraits live under
// a fixed `artist-<mbid>.jpg` the enricher OVERWRITES IN PLACE — without
// the mtime comparison a refreshed portrait would serve its old
// thumbnail forever.
func TestEnsureThumbRederivesWhenSourceIsNewer(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "artist-x.jpg")
	writeArtFixture(t, src, 1000, 1000, 90)
	dst := filepath.Join(dir, ThumbsDirName, "artist-x-250.jpg")

	if err := EnsureThumb(src, dst, 250); err != nil {
		t.Fatalf("first derive: %v", err)
	}
	first, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read first thumb: %v", err)
	}

	// A cached thumb that is already fresh must not be re-derived.
	//
	// Detected by CONTENT, not by mtime: two writes inside one coarse
	// clock tick (Windows is ~15.6 ms) leave identical mtimes, so an
	// mtime comparison would silently pass on exactly the platform most
	// likely to break. A sentinel cannot be reproduced by a re-derive.
	sentinel := []byte("sentinel-not-a-real-jpeg")
	if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}
	// Keep the sentinel newer than the source, which is what "fresh"
	// means to EnsureThumb.
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(dst, now, now); err != nil {
		t.Fatalf("chtimes sentinel: %v", err)
	}
	if err := EnsureThumb(src, dst, 250); err != nil {
		t.Fatalf("second derive: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read thumb: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Error("a fresh thumb was re-derived instead of reused")
	}
	// Restore the real thumb for the staleness half below.
	if err := os.WriteFile(dst, first, 0o600); err != nil {
		t.Fatalf("restore thumb: %v", err)
	}

	// Now replace the portrait the way the enricher does — same path,
	// different content, newer mtime — and require a re-derive.
	replaced := encodeTestImage(t, 900, 600, 70)
	if err := os.WriteFile(src, replaced, 0o600); err != nil {
		t.Fatalf("replace source: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(src, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := EnsureThumb(src, dst, 250); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	second, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read re-derived thumb: %v", err)
	}
	if string(first) == string(second) {
		t.Error("thumb was not re-derived after the source changed under a fixed key")
	}
	// The replacement is landscape 900x600, so the longest side rules.
	w, h, _ := decodeDims(t, second)
	if w != 250 || h != 166 {
		t.Errorf("re-derived dims = %dx%d, want 250x166 (longest side capped)", w, h)
	}
}

// TestEnsureThumbDeclinesPassthroughBytes pins the guard against
// scaleLocalArtworkImpl's verbatim paths. A JPEG over the source-
// dimension caps is returned UNCHANGED by the scaler; writing those
// bytes under a `-250` name would make the filename lie exactly the way
// the historical `-500` suffix already does.
func TestEnsureThumbDeclinesPassthroughBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.jpg")
	// Over localArtMaxSourceAxisPx (16000) on the long axis: the scaler
	// refuses to decode and hands the input straight back.
	writeArtFixture(t, src, 16_001, 4, 60)
	dst := filepath.Join(dir, ThumbsDirName, "huge-250.jpg")

	err := EnsureThumb(src, dst, 250)
	if !ErrThumbNotNeeded(err) {
		t.Fatalf("EnsureThumb err = %v, want errThumbNotNeeded", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("passthrough bytes were written as a thumb; stat err = %v", statErr)
	}
}

// TestEnsureThumbWritesEvenWhenTheScaledFileIsLarger pins that the
// decline is DIMENSIONAL, not byte-based. A downscale is not guaranteed
// to shrink a file — a low-detail source re-encoded at q82 can grow —
// and a byte comparison would discard a perfectly good thumbnail and
// serve the oversized source, silently not honouring ?size=.
//
// The fixture forces the awkward case rather than hoping for it: a
// nearly-flat source stored at very low quality (few bytes, many
// pixels), derived at a target whose re-encode is comparable in size.
func TestEnsureThumbWritesEvenWhenTheScaledFileIsLarger(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "flat.jpg")
	writeArtFixture(t, src, 400, 400, 5) // tiny bytes, 400 px
	dst := filepath.Join(dir, ThumbsDirName, "flat-250.jpg")

	if err := EnsureThumb(src, dst, 250); err != nil {
		t.Fatalf("EnsureThumb: %v", err)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read thumb: %v", err)
	}
	w, h, _ := decodeDims(t, out)
	if w != 250 || h != 250 {
		t.Errorf("thumb dims = %dx%d, want 250x250", w, h)
	}
	// Whether it is larger or smaller than the source is not the point
	// and not asserted — what matters is that the SIZE WAS HONOURED.
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source %d bytes @400px -> thumb %d bytes @250px", srcInfo.Size(), len(out))
}

func TestEnsureThumbMissingSourceIsAnError(t *testing.T) {
	dir := t.TempDir()
	err := EnsureThumb(filepath.Join(dir, "nope.jpg"), filepath.Join(dir, "t.jpg"), 250)
	if err == nil || ErrThumbNotNeeded(err) {
		t.Fatalf("EnsureThumb on a missing source err = %v, want a real error", err)
	}
}
