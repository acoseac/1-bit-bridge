package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCopyFileStreamsRatherThanReadingTheFileWhole pins that copyFile
// copies through a buffer instead of holding the source in memory.
// Restore copies the manifest database through it, and that file can run
// to gigabytes on a large library. The first cut of the console-account
// restore (backlog B200) read the source whole with io.ReadAll so the
// signed-out account could share the write; Gemini caught it on PR #1200.
// The source is a sparse 64 MiB file, so the copy is cheap and the
// allocation a whole read would need is far above the bound.
func TestCopyFileStreamsRatherThanReadingTheFileWhole(t *testing.T) {
	const size = 64 << 20
	dir := t.TempDir()
	src := filepath.Join(dir, "bridge.db")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "restored.db")

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := copyFile(src, dst, 0o600); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	runtime.ReadMemStats(&after)

	if grew := after.TotalAlloc - before.TotalAlloc; grew > size/8 {
		t.Errorf("copyFile allocated %d bytes to copy a %d-byte file; it must stream, not read the file whole", grew, size)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != size {
		t.Errorf("copy is %d bytes, want %d", st.Size(), size)
	}
}
