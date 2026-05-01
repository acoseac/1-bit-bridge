package enrich

import (
	"bytes"
	"io"
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

func TestWriteArtworkAtomicStream_HappyPath(t *testing.T) {
	// Streaming variant writes the body straight from an io.Reader
	// to disk — single allocation, never holds the whole image in
	// memory. Verifies the round-trip plus the size-cap reject path.
	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "stream-mbid-500.jpg")
	payload := bytes.Repeat([]byte("X"), 4096)
	if err := writeArtworkAtomicStream(dst, bytes.NewReader(payload), int64(len(payload)+1)); err != nil {
		t.Fatalf("writeArtworkAtomicStream: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("destination bytes mismatch: got %d, want %d", len(got), len(payload))
	}
	// No leaked tmp file.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp file: %s", e.Name())
		}
	}
}

func TestWriteArtworkAtomicStream_OversizedRejected(t *testing.T) {
	// Stream more than max+1 bytes — must reject without leaking
	// the tmp file. io.LimitReader caps at max+1 so the helper
	// can detect "would have read more" without unbounded memory.
	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "stream-mbid-oversized.jpg")
	payload := bytes.Repeat([]byte("X"), 1024)
	if err := writeArtworkAtomicStream(dst, bytes.NewReader(payload), 100); err == nil {
		t.Fatal("expected oversized error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("destination should not exist after oversized reject; stat err = %v", err)
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp file on oversized reject: %s", e.Name())
		}
	}
}

func TestWriteArtworkAtomicStream_RaceWinnerSizeMatch(t *testing.T) {
	// Streaming counterpart to TestWriteArtworkAtomic_RaceWinnerCleansTmp:
	// when rename fails but the destination already exists with the
	// same size, the streaming helper trusts the existing file and
	// returns nil. The buffered helper does a full bytes.Equal
	// comparison; the streaming helper trusts size because the
	// enricher's URL-to-content mapping is deterministic per
	// (mbid, size).
	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "stream-mbid-race.jpg")
	payload := bytes.Repeat([]byte("X"), 4096)
	if err := os.WriteFile(dst, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomicStream(dst, bytes.NewReader(payload), int64(len(payload)+1)); err != nil {
		t.Fatalf("writeArtworkAtomicStream: %v (expected nil — race winner with matching size)", err)
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp on race-winner path: %s", e.Name())
		}
	}
}

// errReader returns the same error on every Read. Used to verify the
// streaming helper propagates io errors without partial-write fallout.
type errReader struct{ err error }

func (r errReader) Read(p []byte) (int, error) { return 0, r.err }

func TestWriteArtworkAtomicStream_PropagatesReadError(t *testing.T) {
	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "stream-mbid-readerr.jpg")
	if err := writeArtworkAtomicStream(dst, errReader{err: io.ErrUnexpectedEOF}, 1024); err == nil {
		t.Fatal("expected error from failing reader")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("destination should not exist after read error; stat err = %v", err)
	}
	// CodeRabbit nit on PR #123: assert no tmp leak on the read-error
	// cleanup path. The deferred os.Remove must run.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp file on read error: %s", e.Name())
		}
	}
}

func TestWriteArtworkAtomicStream_RenameFailRejectsDifferentBytesOfSameSize(t *testing.T) {
	// Qodo correctness regression on PR #123: pre-fix the streaming
	// helper accepted on size-match alone, which would silently keep a
	// corrupt-but-correct-sized destination over the streamed write.
	// Post-fix: SHA-256 hash compare. Different bytes of the same
	// length must NOT be accepted; the rename error propagates.
	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "stream-mbid-collision.jpg")
	want := bytes.Repeat([]byte("X"), 4096)
	collision := bytes.Repeat([]byte("Y"), 4096) // same size, different bytes
	if err := os.WriteFile(dst, collision, 0o644); err != nil {
		t.Fatal(err)
	}

	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomicStream(dst, bytes.NewReader(want), int64(len(want)+1)); err == nil {
		t.Fatal("writeArtworkAtomicStream returned nil; expected the rename error to propagate when destination is size-equal but byte-different")
	}
	// Cache file must NOT have been overwritten (the rename failed and
	// the streaming helper neither overwrote nor accepted the wrong
	// bytes).
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, collision) {
		t.Error("destination was clobbered despite rename failure")
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".caa-") {
			t.Errorf("leaked tmp on collision-reject path: %s", e.Name())
		}
	}
}
