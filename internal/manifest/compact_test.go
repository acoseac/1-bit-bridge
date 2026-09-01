package manifest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedForCompact(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		tr := &Track{
			Path:    fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i),
			Title:   fmt.Sprintf("Track number %d with some padding to make rows real", i),
			Artist:  fmt.Sprintf("Artist %02d", i%20),
			Album:   "An Album Of Things",
			Size:    1000,
			ModTime: time.Unix(1, 0),
		}
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
}

// TestCompactReclaimsFreePagesAndShrinksTheFile asserts ALL THREE
// outcomes, and the middle one is the load-bearing assertion: a test that
// only checked freelist_count passes against a compaction that reclaims
// nothing, because the VACUUM's output sits in the WAL until it is
// checkpointed.
func TestCompactReclaimsFreePagesAndShrinksTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	seedForCompact(t, s, 4000)
	for i := 0; i < 3500; i++ {
		if err := s.DeleteTrack(ctx, fmt.Sprintf("Artist%02d/Album/%05d Track.flac", i%20, i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}

	pre, err := s.PageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pre.FreelistCount == 0 {
		t.Fatal("fixture produced no free pages; the test would prove nothing")
	}

	res, err := s.Compact(ctx, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.CheckpointBusy {
		t.Fatal("post-VACUUM checkpoint reported busy in a single-connection test")
	}

	post, err := s.PageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if post.FreelistCount != 0 {
		t.Errorf("freelist_count = %d after compact, want 0", post.FreelistCount)
	}
	// THE assertion. Without the post-VACUUM checkpoint the file is
	// byte-for-byte unchanged here while freelist_count still reads 0.
	if res.AfterBytes >= res.BeforeBytes {
		t.Errorf("file did not shrink: before=%d after=%d — a compaction that reclaims nothing",
			res.BeforeBytes, res.AfterBytes)
	}
	walSize := int64(-1)
	if fi, err := os.Stat(p + "-wal"); err == nil {
		walSize = fi.Size()
	} else if os.IsNotExist(err) {
		walSize = 0
	}
	if walSize != 0 {
		t.Errorf("-wal = %d bytes after compact, want 0 (truncating checkpoint did not land)", walSize)
	}
	t.Logf("before=%d after=%d reclaimed=%d freelist %d -> %d",
		res.BeforeBytes, res.AfterBytes, res.BeforeBytes-res.AfterBytes,
		pre.FreelistCount, post.FreelistCount)
}

func TestCompactRefusesWithoutDiskHeadroom(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedForCompact(t, s, 200)

	// Report less free space than the file itself, let alone 2x.
	_, err = s.Compact(context.Background(), func(string) (int64, error) { return 1, nil })
	if !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Fatalf("want ErrInsufficientDiskSpace, got %v", err)
	}

	// A probe that fails is an error, not a silent skip: not knowing how
	// much room there is is not the same as knowing there is enough.
	sentinel := errors.New("probe exploded")
	_, err = s.Compact(context.Background(), func(string) (int64, error) { return 0, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the probe error to surface, got %v", err)
	}

	// Ample headroom proceeds.
	if _, err := s.Compact(context.Background(), func(string) (int64, error) {
		return 1 << 40, nil
	}); err != nil {
		t.Fatalf("Compact with ample headroom: %v", err)
	}
}

func TestPageStatsIsInternallyConsistent(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedForCompact(t, s, 500)

	ps, err := s.PageStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ps.PageSize <= 0 || ps.PageCount <= 0 {
		t.Fatalf("implausible page stats: %+v", ps)
	}
	if ps.FileBytes != ps.PageSize*ps.PageCount {
		t.Errorf("FileBytes = %d, want PageSize*PageCount = %d", ps.FileBytes, ps.PageSize*ps.PageCount)
	}
	if ps.ReclaimedBytes != ps.PageSize*ps.FreelistCount {
		t.Errorf("ReclaimedBytes = %d, want PageSize*FreelistCount = %d",
			ps.ReclaimedBytes, ps.PageSize*ps.FreelistCount)
	}
}

// TestScanInFlightCoversSubtreeScans pins the difference between the two
// scan predicates. Compact's caller must use the broad one: IsScanning
// deliberately does not see a subtree scan (it drives the admin badge and
// the SSE fast tick, where a watcher-triggered subtree scan is not what
// an operator means by "scanning"), so guarding a VACUUM on it would let
// the vacuum start in the middle of the watcher's writes.
//
// Observed through the duplicate-stamping seam, which fires inside
// ScanSubtree's own tail — i.e. while the scan is genuinely in flight.
func TestScanInFlightCoversSubtreeScans(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	seedTrackDirs(t, filepath.Join(lib, "Artist", "Album"))
	s, sc := newScanFixture(t, lib)

	var inFlight, narrow bool
	var observed bool
	beforeApplyDupeStampsHookForTests = func() {
		observed = true
		inFlight = sc.ScanInFlight()
		narrow = sc.IsScanning()
	}
	t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })

	if _, err := sc.ScanSubtree(context.Background(), lib); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if !observed {
		t.Fatal("the subtree scan never reached its stamping tail; the assertion below would be vacuous")
	}
	if !inFlight {
		t.Error("ScanInFlight was false during a subtree scan — Compact's guard would not fire")
	}
	if narrow {
		t.Error("IsScanning was true during a subtree scan; it is documented as full-scan-only")
	}
	if sc.ScanInFlight() {
		t.Error("ScanInFlight still true after the scan returned")
	}
	_ = s
}
