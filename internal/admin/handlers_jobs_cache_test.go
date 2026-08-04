package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// seedSnapshot writes a snapshot directory with a readable manifest, the
// shape backup.List looks for.
func seedSnapshot(t *testing.T, root, name string, created time.Time) {
	t.Helper()
	dir := filepath.Join(root, backup.BackupsDirName, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"createdAt":     created.Format(time.RFC3339Nano),
		"serverVersion": "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLastBackupAtIsCached pins that /api/jobs stops walking the snapshot
// directory on every poll.
//
// The listing opens and JSON-parses a manifest out of every snapshot
// directory, and /api/jobs is polled every 10s per open admin tab. It ran
// inline and uncached, right beside a database query the same handler was
// careful to bound at 2s.
func TestLastBackupAtIsCached(t *testing.T) {
	s, _, _ := newTestServer(t)
	root := t.TempDir()
	s.deps.BackupSources.DataDir = root

	want := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedSnapshot(t, root, "20260804-120000", want)

	got := s.getLastBackupAt(context.Background())
	if got == nil {
		t.Fatal("no snapshot timestamp read from a seeded backups dir")
	}
	if !got.Equal(want) {
		t.Errorf("timestamp = %v, want %v", got, want)
	}

	// A snapshot added behind the cache's back must NOT appear while the
	// entry is still fresh — that is what "cached" means here, and it is
	// the observable difference from the pre-fix inline read.
	seedSnapshot(t, root, "20260804-130000", time.Now())
	again := s.getLastBackupAt(context.Background())
	if again == nil || !again.Equal(want) {
		t.Errorf("timestamp = %v after a second snapshot landed; want the cached %v", again, want)
	}
}

// TestInvalidateLastBackupIsImmediate pins the other half: an
// operator-triggered snapshot must show up at once. A cache that makes
// someone wonder whether their backup worked has traded one confusing
// page for another, and the answer to that confusion is refresh-spam —
// the load this cache exists to remove.
func TestInvalidateLastBackupIsImmediate(t *testing.T) {
	s, _, _ := newTestServer(t)
	root := t.TempDir()
	s.deps.BackupSources.DataDir = root

	first := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedSnapshot(t, root, "20260804-120000", first)
	if got := s.getLastBackupAt(context.Background()); got == nil || !got.Equal(first) {
		t.Fatalf("priming read = %v, want %v", got, first)
	}

	second := time.Now().Truncate(time.Second)
	seedSnapshot(t, root, "20260804-130000", second)
	s.invalidateLastBackup()

	got := s.getLastBackupAt(context.Background())
	if got == nil || !got.Equal(second) {
		t.Errorf("after invalidate: %v, want the new snapshot %v", got, second)
	}
}

// TestLastBackupAtSingleFlights pins that a burst of concurrent polls —
// several admin tabs ticking together — collapses to one listing rather
// than one per caller.
func TestLastBackupAtSingleFlights(t *testing.T) {
	s, _, _ := newTestServer(t)
	root := t.TempDir()
	s.deps.BackupSources.DataDir = root
	seedSnapshot(t, root, "20260804-120000", time.Now().Truncate(time.Second))

	var wg sync.WaitGroup
	results := make([]*time.Time, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.getLastBackupAt(context.Background())
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r == nil {
			t.Fatalf("caller %d got nil; every joined caller must see the shared result", i)
		}
		if !r.Equal(*results[0]) {
			t.Errorf("caller %d saw %v, caller 0 saw %v", i, r, results[0])
		}
	}
}

// TestLastBackupAtNilWithoutDataDir — no backup sources wired means no
// listing at all, and no panic reaching for a path that isn't there.
func TestLastBackupAtNilWithoutDataDir(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.BackupSources.DataDir = ""
	if got := s.getLastBackupAt(context.Background()); got != nil {
		t.Errorf("got %v with no backup sources wired, want nil", got)
	}
}

// TestAnalysisCoverageStampsOnFailure is review item 5.3.
//
// The TTL was stamped on the success path only, so a FAILING query never
// tripped it and every 10s poll re-ran a full-table scan. The failure is
// most likely to be a timeout — i.e. the query is slow — which is exactly
// when re-running it on every poll is worst.
//
// Driven with a closed store so the query genuinely errors, then asserted
// on the stamp rather than on a call count: the point is that the cache
// backs off, and the stamp is what makes it do so.
func TestAnalysisCoverageStampsOnFailure(t *testing.T) {
	s, _, _ := newTestServer(t)
	if s.deps.Manifest == nil {
		t.Skip("test server has no manifest store wired")
	}
	s.deps.AnalysisSchemaVersion = "wf-test"
	// Close the store out from under the handler so AnalysisCoverage
	// returns an error rather than a snapshot.
	_ = s.deps.Manifest.Close()

	if got := s.getAnalysisCoverage(context.Background()); got != nil {
		t.Errorf("coverage = %v against a closed store, want nil (last good)", got)
	}
	s.analysisCoverageMu.Lock()
	stamped := s.analysisCoverageAt
	s.analysisCoverageMu.Unlock()
	if stamped.IsZero() {
		t.Error("analysisCoverageAt is still zero after a failed query — the TTL never trips, " +
			"so every poll re-runs a full-table scan that is already failing")
	}
}
