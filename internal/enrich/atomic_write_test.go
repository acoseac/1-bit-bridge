package enrich

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteArtworkAtomic_RaceWinnerCleansTmp(t *testing.T) {
	// Regression for the stat-and-accept fallback's tmp-leak bug
	// (gemini-code-assist on PR #100, mirror of the
	// internal/manifest test). When renameWithRetry exhausts its
	// budget but a concurrent writer / prior pass has already
	// published a byte-equivalent destination, writeArtworkAtomic
	// returns nil — but the source tmp file is still on disk and
	// the deferred os.Remove(tmpName) MUST run. Pre-fix, an early
	// `tmpName = ""` in the fallback branch suppressed the cleanup
	// and leaked one `.caa-NNN.jpg.tmp` per race-window hit.
	cacheDir := t.TempDir()
	data := []byte("payload-bytes-for-enricher-race-winner-test")
	dst := filepath.Join(cacheDir, "release-mbid-500.jpg")

	// Pre-stage destination as if a parallel writer / prior pass
	// has already published the file with byte-equivalent content.
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Inject rename failure so the stat-and-accept branch fires
	// without burning the full ~750 ms retry backoff budget.
	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomic(dst, data); err != nil {
		t.Fatalf("writeArtworkAtomic: %v (expected nil — race winner with matching size)", err)
	}

	// Pre-staged destination must be intact (not clobbered).
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("destination clobbered after stat-and-accept: got %q want %q", got, data)
	}

	// And no `.caa-NNN.jpg.tmp` leftover from the failed rename.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp file (deferred cleanup did not run): %s", e.Name())
		}
	}
}

func TestWriteArtworkAtomic_RaceLoserDoesNotAcceptOnSizeCollision(t *testing.T) {
	// Same-size-different-content negative case (CodeRabbit on
	// PR #100). Doubly important here: the enricher's path is
	// `<mbid>-<size>.jpg` with no content hash in the filename, so
	// a future MusicBrainz re-fetch with the same mbid + same size
	// but different bytes (cover-art swap) would have silently
	// taken the same-mbid existing file under a size-only check.
	cacheDir := t.TempDir()
	want := []byte("expected-bytes-the-enricher-tried-to-write")
	collision := make([]byte, len(want))
	copy(collision, want)
	collision[0] ^= 0xFF
	dst := filepath.Join(cacheDir, "release-mbid-500.jpg")
	if err := os.WriteFile(dst, collision, 0o644); err != nil {
		t.Fatal(err)
	}

	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomic(dst, want); err == nil {
		t.Fatal("writeArtworkAtomic returned nil; expected the rename error to propagate when destination is size-equal but byte-different")
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp on size-collision path: %s", e.Name())
		}
	}
}
