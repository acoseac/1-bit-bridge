package manifest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openRescaleTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeRescaleFixture(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunArtworkRescaleOnce_RewritesOnlyOversizedLocalFiles(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()
	ctx := context.Background()

	oversized := encodeTestImage(t, 2400, 1800, 90)
	fine := encodeTestImage(t, 800, 600, 85)
	// Dimensionally inside the 1440 hysteresis AND under 700 KiB — the
	// band that must survive untouched even though new writes target
	// 1200. Solid-color (not the noise helper) so the encode stays far
	// below the size trigger at these dimensions.
	hysteresis := encodeSolidImage(t, 1300, 1300, 60)
	if len(hysteresis) > rescaleMaxBytes {
		t.Fatalf("fixture invalid: hysteresis image %d bytes exceeds the size trigger", len(hysteresis))
	}
	enricherCover := encodeTestImage(t, 2400, 2400, 90) // out of scope: not local-*

	pOversized := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("a", 64)+"-500.jpg", oversized)
	pFine := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("b", 64)+"-500.jpg", fine)
	pHyst := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("c", 64)+"-500.jpg", hysteresis)
	pEnricher := writeRescaleFixture(t, artDir, "cccccccc-cccc-4ccc-8ccc-cccccccccccc-500.jpg", enricherCover)
	pJunk := writeRescaleFixture(t, artDir, "notes.txt", []byte("not an image"))

	RunArtworkRescaleOnce(ctx, store, artDir)

	after, err := os.ReadFile(pOversized)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, oversized) {
		t.Errorf("oversized local cover was not rewritten")
	}
	w, h, format := decodeDims(t, after)
	if format != "jpeg" || w != 1200 || h != 900 {
		t.Errorf("rewritten cover = %dx%d %s, want 1200x900 jpeg", w, h, format)
	}
	for name, pair := range map[string]struct {
		path string
		want []byte
	}{
		"already-fine":        {pFine, fine},
		"hysteresis-band":     {pHyst, hysteresis},
		"enricher-cover":      {pEnricher, enricherCover},
		"out-of-scope (junk)": {pJunk, []byte("not an image")},
	} {
		got, err := os.ReadFile(pair.path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, pair.want) {
			t.Errorf("%s must be byte-identical after the pass", name)
		}
	}

	marker, err := store.GetScanState(ctx, artworkRescaleMarkerKey)
	if err != nil || marker == "" {
		t.Errorf("marker not set after completed pass (marker=%q err=%v)", marker, err)
	}
}

func TestRunArtworkRescaleOnce_ByteSizeTriggerReencodes(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()

	// Dimensions fine (1000 ≤ 1440) but archival-quality heavy: the size
	// trigger must force a q82 re-encode.
	heavy := encodeTestImage(t, 1000, 1000, 100)
	if len(heavy) <= rescaleMaxBytes {
		t.Skipf("fixture too small to exercise the size trigger (%d bytes)", len(heavy))
	}
	p := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("d", 64)+"-500.jpg", heavy)

	RunArtworkRescaleOnce(context.Background(), store, artDir)

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, heavy) {
		t.Fatalf("size-triggered file was not re-encoded")
	}
	if len(after) >= len(heavy) {
		t.Errorf("re-encode did not shrink bytes (%d -> %d)", len(heavy), len(after))
	}
}

func TestRunArtworkRescaleOnce_MarkerMakesSecondRunANoOp(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()
	RunArtworkRescaleOnce(context.Background(), store, artDir) // completes on the empty dir, sets marker

	// A file added AFTER the completed pass must NOT be touched by a
	// second call — the marker is the run-once gate. (New scan writes
	// are right-sized at write time; this file simulates one that
	// isn't, to make the assertion non-vacuous.)
	oversized := encodeTestImage(t, 2400, 1800, 90)
	p := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("e", 64)+"-500.jpg", oversized)

	RunArtworkRescaleOnce(context.Background(), store, artDir)

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, oversized) {
		t.Errorf("marker did not gate the second run — file was rewritten")
	}
}

func TestRunArtworkRescaleOnce_InterruptLeavesNoMarkerThenResumes(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()
	oversized := encodeTestImage(t, 2400, 1800, 90)
	p := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("f", 64)+"-500.jpg", oversized)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	RunArtworkRescaleOnce(cancelled, store, artDir)

	marker, err := store.GetScanState(context.Background(), artworkRescaleMarkerKey)
	if err != nil {
		t.Fatal(err)
	}
	if marker != "" {
		t.Fatalf("interrupted pass must NOT set the marker (got %q)", marker)
	}

	RunArtworkRescaleOnce(context.Background(), store, artDir)
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, oversized) {
		t.Errorf("resumed pass did not rewrite the oversized file")
	}
	marker, err = store.GetScanState(context.Background(), artworkRescaleMarkerKey)
	if err != nil || marker == "" {
		t.Errorf("resumed pass must set the marker (marker=%q err=%v)", marker, err)
	}
}

func TestRunArtworkRescaleOnce_CorruptFileSkippedButPassCompletes(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()

	// Junk bytes over the size trigger under a local-* name: the rewrite
	// decision fires, the decode fails, the file survives untouched, and
	// the pass still completes (marker set) — per-file failures must not
	// hold the marker hostage.
	corrupt := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{0x5A}, rescaleMaxBytes+1024)...)
	pCorrupt := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("9", 64)+"-500.jpg", corrupt)
	oversized := encodeTestImage(t, 2400, 1800, 90)
	pOversized := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("8", 64)+"-500.jpg", oversized)

	RunArtworkRescaleOnce(context.Background(), store, artDir)

	gotCorrupt, err := os.ReadFile(pCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCorrupt, corrupt) {
		t.Errorf("corrupt file must never be modified or deleted")
	}
	gotOversized, err := os.ReadFile(pOversized)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(gotOversized, oversized) {
		t.Errorf("healthy oversized sibling must still be rewritten")
	}
	marker, err := store.GetScanState(context.Background(), artworkRescaleMarkerKey)
	if err != nil || marker == "" {
		t.Errorf("pass with skips must still set the marker (marker=%q err=%v)", marker, err)
	}
}

// A corrupt-header JPEG whose header parse fails entirely: the
// dimension probe answers unknown, so only the size trigger applies.
// Under the size cap it is skipped as fine (never decoded, never
// rewritten) — the conservative direction.
func TestRunArtworkRescaleOnce_UnparseableSmallFileSkipped(t *testing.T) {
	store := openRescaleTestStore(t)
	artDir := t.TempDir()
	blob := append([]byte{}, minimalJPEG...)
	p := writeRescaleFixture(t, artDir, "local-"+strings.Repeat("7", 64)+"-500.jpg", blob)

	RunArtworkRescaleOnce(context.Background(), store, artDir)

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("small unparseable file must be left alone")
	}
}
