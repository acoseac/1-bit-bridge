package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/analyze"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestCollectAnalysisCandidatesSkipsZeroByteSource pins that a zero-byte
// source file (a failed/incomplete upload — sox can't probe it and the ffmpeg
// fallback hits EOF) is skipped at collection time instead of being enqueued
// and re-failed on every sweep. A non-empty sibling still becomes a candidate,
// and the skip self-heals on re-upload (size > 0). Field-reported on
// bridge.ars.md: 26 zero-byte FLACs from truncated B2 syncs spammed
// "analyze: failed" each sweep.
func TestCollectAnalysisCandidatesSkipsZeroByteSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.flac"), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.flac"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, tr := range []struct {
		path string
		size int64
	}{{"good.flac", 18}, {"empty.flac", 0}} {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: tr.path, Size: tr.size, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.path, err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", res.emptySkipped)
	}
	var gotGood, gotEmpty bool
	for _, c := range res.candidates {
		switch c.SourceLibraryRel {
		case "good.flac":
			gotGood = true
		case "empty.flac":
			gotEmpty = true
		}
	}
	if !gotGood {
		t.Error("good.flac (non-empty) should be a candidate")
	}
	if gotEmpty {
		t.Error("empty.flac (zero-byte) must NOT be enqueued for analysis")
	}
}

// TestAnalysisCoverageLockstepWithCollector pins the predicate-parity
// contract between Store.AnalysisCoverage's SQL buckets (the Jobs
// page's approximate whole-library tile) and
// collectAnalysisCandidates' control flow (the sweeper's exact truth):
// same DSD-by-extension rule, same zero-byte rule, same precedence
// (DSD before zero-byte). If either side's predicate drifts, this
// fails before an operator sees disagreeing numbers.
func TestAnalysisCoverageLockstepWithCollector(t *testing.T) {
	root := t.TempDir()
	files := []struct {
		name string
		data []byte
	}{
		{"good.flac", []byte("fLaC-nonzero")},
		{"more.mp3", []byte("ID3-nonzero")},
		{"disc.dsf", []byte("DSD-nonzero")},
		{"disc.dff", []byte("FRM8-nonzero")},
		{"empty.flac", nil},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f.name), f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, f := range files {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: f.name, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", f.name, err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	cov, err := store.AnalysisCoverage(ctx, analyze.WaveformSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}

	if cov.TotalLocal != res.total {
		t.Errorf("TotalLocal = %d, collector total = %d", cov.TotalLocal, res.total)
	}
	if cov.DSDExcluded != res.dsdSkipped {
		t.Errorf("DSDExcluded = %d, collector dsdSkipped = %d", cov.DSDExcluded, res.dsdSkipped)
	}
	if cov.ZeroByteExcluded != res.emptySkipped {
		t.Errorf("ZeroByteExcluded = %d, collector emptySkipped = %d", cov.ZeroByteExcluded, res.emptySkipped)
	}
	// Sanity: the fixture has no missing/routed rows, so eligible ==
	// candidates + fresh-skipped (all zero analysis rows here).
	eligible := cov.TotalLocal - cov.DSDExcluded - cov.ZeroByteExcluded
	if eligible != len(res.candidates)+res.skipped {
		t.Errorf("eligible = %d, collector candidates+skipped = %d", eligible, len(res.candidates)+res.skipped)
	}
}

// waitForSweep polls the recorder until a completed sweep newer than
// `after` is visible or the deadline hits. Returns the lastEnd observed.
func waitForSweep(t *testing.T, status *sweepStatus[admin.AnalysisSweepCounts], after time.Time, deadline time.Duration) time.Time {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		running, _, lastEnd, _, _ := status.snapshot()
		if !running && lastEnd.After(after) {
			return lastEnd
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no sweep completed after %v within %v", after, deadline)
	return time.Time{}
}

// TestRunAnalysisSweeperNudgeTriggersImmediateSweep pins the nudge
// contract: after the settle-delay initial sweep, a nudge (post-scan
// hook / admin "Analyze now") fires a sweep immediately instead of
// waiting out the periodic interval — and a nudge that arrived DURING
// the settle window is drained by the initial sweep, not double-run.
func TestRunAnalysisSweeperNudgeTriggersImmediateSweep(t *testing.T) {
	oldSettle := analysisSweeperSettleDelay
	analysisSweeperSettleDelay = 5 * time.Millisecond
	t.Cleanup(func() { analysisSweeperSettleDelay = oldSettle })

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := analyze.NewPool(store, 1, 8)
	defer pool.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nudge := make(chan struct{}, 1)
	status := &sweepStatus[admin.AnalysisSweepCounts]{}

	// A nudge sent BEFORE the sweeper wakes (i.e. during settle) must be
	// swallowed by the initial sweep — asserted below via lastStart count.
	nudge <- struct{}{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 1h interval: the ticker can't fire inside this test — any sweep
		// after the initial one can only come from a nudge.
		runAnalysisSweeper(ctx, store, bridgefs.New([]string{t.TempDir()}), t.TempDir(),
			pool, time.Hour, nudge, status)
	}()

	firstEnd := waitForSweep(t, status, time.Time{}, 5*time.Second)

	// Settle-window nudge was drained: give a stray follow-up sweep a
	// moment to (not) happen, then confirm nothing moved.
	time.Sleep(50 * time.Millisecond)
	if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.Equal(firstEnd) {
		t.Fatalf("settle-window nudge triggered an extra sweep: lastEnd %v → %v", firstEnd, lastEnd)
	}

	// A post-settle nudge fires a sweep promptly (no 1h ticker wait).
	nudge <- struct{}{}
	secondEnd := waitForSweep(t, status, firstEnd, 5*time.Second)
	if !secondEnd.After(firstEnd) {
		t.Fatalf("nudge did not trigger a new sweep (lastEnd %v)", secondEnd)
	}

	// Counts recorded for the empty library: everything zero.
	_, _, _, _, last := status.snapshot()
	if last == nil || last.Total != 0 || last.Enqueued != 0 {
		t.Errorf("sweep counts = %+v, want zeroed for empty library", last)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not exit on ctx cancel")
	}
}

// TestRunAnalysisSweeperRecordsNextDue — with a periodic interval the
// recorder carries a nextDue in the future; the admin card derives its
// "next sweep in …" countdown from it browser-side.
func TestRunAnalysisSweeperRecordsNextDue(t *testing.T) {
	oldSettle := analysisSweeperSettleDelay
	analysisSweeperSettleDelay = 5 * time.Millisecond
	t.Cleanup(func() { analysisSweeperSettleDelay = oldSettle })

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := analyze.NewPool(store, 1, 8)
	defer pool.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := &sweepStatus[admin.AnalysisSweepCounts]{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnalysisSweeper(ctx, store, bridgefs.New([]string{t.TempDir()}), t.TempDir(),
			pool, time.Hour, make(chan struct{}, 1), status)
	}()
	waitForSweep(t, status, time.Time{}, 5*time.Second)
	_, _, _, nextDue, _ := status.snapshot()
	if nextDue.IsZero() || !nextDue.After(time.Now().Add(30*time.Minute)) {
		t.Errorf("nextDue = %v, want ~1h out", nextDue)
	}
	cancel()
	<-done
}
