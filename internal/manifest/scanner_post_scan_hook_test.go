package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// TestScannerPostScanHookFiresOnScanSuccess pins the post-scan hook
// contract: fired exactly once per SUCCESSFUL full Scan (cmd/bridge
// wires it to the auto-analysis sweeper's nudge channel), and fired
// again on each subsequent scan.
func TestScannerPostScanHookFiresOnScanSuccess(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")

	fired := 0
	sc.SetPostScanHook(func() { fired++ })

	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times after first scan, want 1", fired)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	if fired != 2 {
		t.Fatalf("hook fired %d times after second scan, want 2", fired)
	}
}

// TestScannerPostScanHookSkippedOnFailure pins the two no-fire cases:
// an errored scan (store closed under it) and a scan whose context was
// already cancelled. A failed or aborted scan must never nudge
// downstream consumers — the analysis sweeper would walk a library the
// scan never actually (re)indexed.
func TestScannerPostScanHookSkippedOnFailure(t *testing.T) {
	root, _ := tempLibrary(t)

	t.Run("scan error", func(t *testing.T) {
		s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
		sc := NewScanner([]string{root}, s, "")
		fired := 0
		sc.SetPostScanHook(func() { fired++ })
		s.Close() // force the TrackPaths list to fail → Scan errors
		if _, err := sc.Scan(context.Background()); err == nil {
			t.Fatal("Scan on closed store should error")
		}
		if fired != 0 {
			t.Fatalf("hook fired %d times on errored scan, want 0", fired)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
		defer s.Close()
		sc := NewScanner([]string{root}, s, "")
		fired := 0
		sc.SetPostScanHook(func() { fired++ })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = sc.Scan(ctx) // errors or completes-quietly depending on where the cancel lands
		if fired != 0 {
			t.Fatalf("hook fired %d times on cancelled scan, want 0", fired)
		}
	})
}

// TestScannerNilPostScanHookIsNoOp — a scanner without a hook (every
// non-serve construction site) scans exactly as before.
func TestScannerNilPostScanHookIsNoOp(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.SetPostScanHook(nil) // explicit nil is ignored, not stored
	n, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != expected {
		t.Errorf("scanned %d, want %d", n, expected)
	}
}
