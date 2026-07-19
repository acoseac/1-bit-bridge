package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestScanCtxCancelDuringDrainSkipsCompletion pins B7: a ctx cancel that
// lands after the walk has drained cleanly (walkErr==nil) but before the
// deletion pass MUST abort Scan with ctx.Err() and MUST NOT advance
// last_full_scan — otherwise a cut-short full scan is indistinguishable
// from a complete one and the dashboard's "last full scan" tile lies.
//
// The cancel is injected via afterExtractHookForTests, which fires after
// a worker has extracted the single library file. By then the walk has
// already returned nil (the buffered send preceded the extract), so
// walkErr stays nil and only the new post-drain ctx guard can stop the
// completion path (deletion pass + last_full_scan stamp + reconciliation).
func TestScanCtxCancelDuringDrainSkipsCompletion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel once the single file has been extracted: the walk is done,
	// walkErr==nil, and control is about to reach the deletion-pass guard.
	afterExtractHookForTests = func(string) { cancel() }
	defer func() { afterExtractHookForTests = nil }()

	sc := NewScanner([]string{root}, store, "")
	if !sc.LastFullScan().IsZero() {
		t.Fatalf("precondition: LastFullScan should be zero on a fresh scanner, got %v", sc.LastFullScan())
	}

	if _, err := sc.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan err = %v, want context.Canceled (a cut-short scan must surface cancellation)", err)
	}

	if !sc.LastFullScan().IsZero() {
		t.Errorf("LastFullScan advanced to %v after a cancelled scan; the post-drain guard must skip the last_full_scan stamp", sc.LastFullScan())
	}
	// The persisted scan_state must also stay unstamped.
	if v, _ := store.GetScanState(context.Background(), "last_full_scan"); v != "" {
		t.Errorf("scan_state[last_full_scan] = %q after a cancelled scan; want unset", v)
	}
}
