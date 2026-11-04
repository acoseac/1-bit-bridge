package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAScanSparesARootBelowAVolumeThatIsNotMountedYet pins the scan's half of
// B651: a NAS that starts the bridge before its volume is mounted. A root
// below the volume's mount point (/mnt/usb/music, the volume at /mnt/usb) is
// then not there at all, ENOENT, while the bare mount point is. The full scan
// spares every row under it and says so ("root unreachable", walkRoot's
// upfront fsutil.WalkableRoot), a subtree scan of the root stops before any row
// is touched, and so does one below it (the owning-root audit). A bare
// Scanner's threshold is 1, so a scan that counted the rows missing would
// have deleted them.
//
// The root that IS the mount point, there and empty, is the clean-empty
// guard's (TestScannerSparesRowsOnCleanEmptyMount, and the noise cases in
// scanner_root_noise_test.go), and a root of several that is gone the
// unreachable case's (TestScannerSparesRowsUnderUnreachableRoot).
func TestAScanSparesARootBelowAVolumeThatIsNotMountedYet(t *testing.T) {
	ctx := context.Background()
	mountPoint := filepath.Join(t.TempDir(), "usb")
	root := filepath.Join(mountPoint, "music")
	album := filepath.Join(root, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(album, "01.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	const row = "Artist/Album/01.flac"
	if got, _ := s.GetTrack(ctx, row); got == nil {
		t.Fatalf("the first scan did not index %s", row)
	}

	// The volume goes: the root below its mount point is gone, and the mount
	// point is left, empty.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	const scans = 3
	for i := 0; i < scans; i++ {
		if _, err := sc.Scan(ctx); err != nil {
			t.Fatalf("scan %d with the volume gone: %v", i+1, err)
		}
	}
	if got := rec.Failures(msgRootUnreachable); len(got) != scans {
		t.Errorf("%d scans of a root whose volume is not mounted said %q %d times, want once each:\n%v",
			scans, msgRootUnreachable, len(got), got)
	}
	// A root that is not there is spared as one the scan cannot see, never
	// as a clean-empty mount, the guard behind it, which says another thing
	// to the operator: two layers, and this one is the first.
	if got := rec.Failures(msgCleanEmpty); len(got) != 0 {
		t.Errorf("a root that is not there was taken for a clean-empty mount:\n%v", got)
	}
	if _, err := sc.ScanSubtree(ctx, root); err == nil {
		t.Error("a subtree scan of a root whose volume is not mounted answered nil; it must stop before its deletion pass")
	}
	if _, err := sc.ScanSubtree(ctx, album); err == nil {
		t.Error("a subtree scan below a root whose volume is not mounted answered nil; the owning-root audit must stop it")
	}
	if got, _ := s.GetTrack(ctx, row); got == nil {
		t.Fatalf("%s was deleted while its root's volume was not mounted", row)
	}
	mustHaveMissingCount(t, s, "tracks", row, 0)
}
