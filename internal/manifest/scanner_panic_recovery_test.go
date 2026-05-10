package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestScannerWorkerRecoversFromExtractPanic pins the per-iteration
// panic-recover contract documented in CLAUDE.md. A panic inside the
// extract step (dhowden/tag on a malformed file, our own DSF/DFF
// parser on a truncated header, etc.) MUST be recovered so the worker
// keeps draining its `paths` channel and the rest of the scan
// completes. Pre-fix, the panicked goroutine died and reduced pool
// capacity; on a single-CPU host with a single worker the entire
// scan stalled.
//
// The test injects a deterministic panic via afterExtractHookForTests
// against a sentinel file path. The other files in the tmp tree are
// expected to land in the manifest normally; the sentinel file is
// expected to be skipped and to bump the Scanner's PanickedCount.
func TestScannerWorkerRecoversFromExtractPanic(t *testing.T) {
	root := t.TempDir()
	healthy1 := filepath.Join(root, "healthy1.flac")
	healthy2 := filepath.Join(root, "healthy2.flac")
	bad := filepath.Join(root, "bad.flac")
	for _, p := range []string{healthy1, healthy2, bad} {
		if err := os.WriteFile(p, []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Sentinel: panic when the worker reaches the bad path. The hook
	// runs inside the recover scope so the panic is contained to one
	// iteration.
	var hookCalls atomic.Int64
	afterExtractHookForTests = func(absPath string) {
		hookCalls.Add(1)
		if strings.HasSuffix(absPath, "bad.flac") {
			panic("synthetic dhowden/tag panic for test")
		}
	}
	defer func() { afterExtractHookForTests = nil }()

	sc := NewScanner([]string{root}, store, "")
	beforePanics := sc.PanickedCount()

	committed, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// The hook must have been called for every file the worker
	// reached — proves the worker drained its channel past the
	// panicked iteration.
	if got := hookCalls.Load(); got != 3 {
		t.Errorf("afterExtractHookForTests called %d times, want 3 (one per file)", got)
	}

	// PanickedCount must have advanced by exactly 1 (the bad file).
	if got, want := sc.PanickedCount()-beforePanics, int64(1); got != want {
		t.Errorf("PanickedCount delta = %d, want %d", got, want)
	}

	// committed counts rows the writer flushed. The two healthy
	// files must have landed; the bad file must NOT have.
	if committed != 2 {
		t.Errorf("Scan committed %d rows, want 2", committed)
	}

	// And the manifest must reflect that: GetTrack on the healthy
	// files succeeds, GetTrack on the bad file returns nil.
	if got, _ := store.GetTrack("healthy1.flac"); got == nil {
		t.Error("GetTrack(healthy1.flac) returned nil, want a row")
	}
	if got, _ := store.GetTrack("healthy2.flac"); got == nil {
		t.Error("GetTrack(healthy2.flac) returned nil, want a row")
	}
	if got, _ := store.GetTrack("bad.flac"); got != nil {
		t.Errorf("GetTrack(bad.flac) = %+v, want nil (panic should have skipped the write)", got)
	}
}
