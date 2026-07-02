//go:build !windows

package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanFolderArtwork_SymlinkOversizedTargetSkipped pins F7. The size
// cap must be measured against the symlink TARGET, not the link itself.
// Pre-fix, scanFolderArtwork used entry.Info() (lstat — reports the tiny
// link), so an oversized target passed the cap and os.ReadFile then
// followed the link and slurped the whole target into RAM. os.Stat
// follows the link, so the cap now rejects it.
//
// The target is a valid-JPEG-header sparse file larger than the cap:
// with the old code it would pass the size check, sniff as JPEG, and be
// accepted (found=true); with the fix os.Stat sees the oversized target
// and skips it (found=false).
func TestScanFolderArtwork_SymlinkOversizedTargetSkipped(t *testing.T) {
	scanDir := t.TempDir()
	targetDir := t.TempDir()

	target := filepath.Join(targetDir, "huge.jpg")
	f, err := os.Create(target)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := f.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0}); err != nil { // JPEG SOI + APP0 start
		t.Fatalf("write jpeg header: %v", err)
	}
	// Extend sparsely past the cap without writing 25 MiB of real bytes.
	if err := f.Truncate(maxArtworkBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close target: %v", err)
	}

	link := filepath.Join(scanDir, "cover.jpg")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	res := scanFolderArtwork(scanDir, t.TempDir())
	if res.found {
		t.Errorf("scanFolderArtwork found=true for a symlinked oversized cover; "+
			"the %d-byte target exceeds the %d-byte cap and must be skipped", maxArtworkBytes+1, maxArtworkBytes)
	}
}
