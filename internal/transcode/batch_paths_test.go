package transcode

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedSharedDirBatchFixture stages two albums flat in ONE directory —
// the layout that makes a folder scope unusable for an album. The
// reference library keeps 18 albums this way under a single
// `Hi-Res Masters/` folder; 69 of its 880 albums share a directory
// with a neighbour.
//
// All four tracks are 44.1/16 FLAC: upscale-eligible below a 192/24
// target, and NOT optimize-eligible (already at the CarPlay floor), so
// the upscale path is the one to assert enqueue counts on.
func seedSharedDirBatchFixture(t *testing.T, s *manifest.Store) (target, neighbour []string) {
	t.Helper()
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Shared"}); err != nil {
		t.Fatal(err)
	}
	rate, bits, isDSD := 44100.0, 16, false
	target = []string{"Shared/So - 01.flac", "Shared/So - 02.flac"}
	neighbour = []string{"Shared/Us - 01.flac", "Shared/Us - 02.flac"}
	for _, p := range append(append([]string{}, target...), neighbour...) {
		if err := s.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1_000_000, SampleRate: &rate,
			BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
	}
	return target, neighbour
}

// blockRunner parks every job until the test ends. It is ctx-aware on
// purpose: Pool.Stop cancels the pool context and then waits on its
// workers, so a runner that only watched a test-owned channel would
// deadlock Stop whenever t.Cleanup ordering put Stop first.
func blockRunner(t *testing.T, p *Pool) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return spec.SourceSize, "", nil
	}
}

func enqueuedPaths(p *Pool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.inflight))
	for key := range p.inflight {
		if i := strings.Index(key, "|"); i >= 0 {
			out = append(out, key[:i])
		}
	}
	sort.Strings(out)
	return out
}

// TestSubmitPathsEnqueuesOnlyTheGivenSet is the coordinator-level pin
// for neighbour isolation: the identity form must enqueue the album it
// was handed, and the folder form over the same directory must show
// that a prefix scope cannot express that.
//
// The pool's runner is stubbed to block until the test releases it, so
// the inflight set can be read before jobs drain.
func TestSubmitPathsEnqueuesOnlyTheGivenSet(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	target, neighbour := seedSharedDirBatchFixture(t, s)

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)

	res, err := c.SubmitPaths(context.Background(), "Shared/So", target, 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPaths: %v", err)
	}
	if res.EnqueuedCount != len(target) {
		t.Errorf("EnqueuedCount = %d, want %d", res.EnqueuedCount, len(target))
	}
	if res.Path != "Shared/So" {
		t.Errorf("batch label = %q, want the caller's label", res.Path)
	}
	got := enqueuedPaths(p)
	if strings.Join(got, "|") != strings.Join(target, "|") {
		t.Fatalf("enqueued %v, want exactly %v", got, target)
	}
	for _, p := range got {
		for _, n := range neighbour {
			if p == n {
				t.Errorf("neighbouring album's track %q was enqueued by an album-scoped submit", p)
			}
		}
	}
}

// TestSubmitPrefixSweepsTheWholeDirectory is the companion assertion:
// the folder form over the same directory picks up all four tracks.
// Not a bug — a subtree scope is exactly what Submit promises — but
// without it a reader has to take on trust that the two forms differ.
func TestSubmitPrefixSweepsTheWholeDirectory(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	target, neighbour := seedSharedDirBatchFixture(t, s)

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)

	res, err := c.Submit(context.Background(), "Shared", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if want := len(target) + len(neighbour); res.EnqueuedCount != want {
		t.Fatalf("prefix scope enqueued %d, want %d — the fixture no longer models "+
			"two albums sharing a directory", res.EnqueuedCount, want)
	}
}

// TestSubmitPathsHandlesASingleTrack pins the other half of why the
// prefix form cannot serve identity scopes: it matches strict
// descendants, so a file path is an empty scope through Submit and a
// one-track scope through SubmitPaths.
func TestSubmitPathsHandlesASingleTrack(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirBatchFixture(t, s)

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)

	res, err := c.SubmitPaths(context.Background(), target[0], target[:1], 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPaths: %v", err)
	}
	if res.EnqueuedCount != 1 {
		t.Errorf("single-track scope enqueued %d, want 1", res.EnqueuedCount)
	}

	viaPrefix, err := c.Submit(context.Background(), target[0], 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit on a file path: %v", err)
	}
	if viaPrefix.EnqueuedCount != 0 {
		t.Errorf("prefix scope on a file path enqueued %d, want 0 — the `<base>/%%` "+
			"pattern this test documents has changed", viaPrefix.EnqueuedCount)
	}
}

// TestSubmitPathsSharesTheEligibilityGates: the identity form runs the
// SAME filter as the prefix form, so an ineligible track is skipped
// identically. This is the property that keeps the two scopes from
// drifting apart — they differ only in how the projection set is
// chosen, never in what is done with it.
func TestSubmitPathsSharesTheEligibilityGates(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s) // 01 covered, 04 at target, 02+03 eligible

	// The two submits need to see the SAME library, and they must not
	// interfere. A shared coordinator gives neither: the default
	// stubbed runner completes instantly, so the first submit's
	// variants would land before the second projected — and blocking
	// one shared runner instead just moves the interference to the
	// pool's inflight dedup, which zeroes the second submit's
	// EnqueuedCount and hence its TotalFiles.
	//
	// Two coordinators over one store, both with blocked runners: no
	// variants are ever written, and neither pool has seen the other's
	// jobs.
	cPaths, pPaths, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, pPaths)
	cPrefix, pPrefix, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, pPrefix)

	all := []string{"Album/01.flac", "Album/02.flac", "Album/03.flac", "Album/04.flac"}
	byPaths, err := cPaths.SubmitPaths(context.Background(), "Album", all, 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPaths: %v", err)
	}
	byPrefix, err := cPrefix.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if byPaths.AlreadyCovered != byPrefix.AlreadyCovered {
		t.Errorf("AlreadyCovered: paths=%d prefix=%d — the gates diverged",
			byPaths.AlreadyCovered, byPrefix.AlreadyCovered)
	}
	if byPaths.TotalFiles != byPrefix.TotalFiles {
		t.Errorf("TotalFiles: paths=%d prefix=%d — the gates diverged",
			byPaths.TotalFiles, byPrefix.TotalFiles)
	}
	if byPaths.ProjectedSizeBytes != byPrefix.ProjectedSizeBytes {
		t.Errorf("ProjectedSizeBytes: paths=%d prefix=%d — the gates diverged",
			byPaths.ProjectedSizeBytes, byPrefix.ProjectedSizeBytes)
	}
	if byPaths.EnqueuedCount != byPrefix.EnqueuedCount {
		t.Errorf("EnqueuedCount: paths=%d prefix=%d — the gates diverged",
			byPaths.EnqueuedCount, byPrefix.EnqueuedCount)
	}
}

// TestSubmitOptimizePathsEnqueuesOnlyTheGivenSet mirrors the upscale
// case for the CarPlay path. Hi-res sources, so every track is
// optimize-eligible.
func TestSubmitOptimizePathsEnqueuesOnlyTheGivenSet(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Shared"}); err != nil {
		t.Fatal(err)
	}
	rate, bits, isDSD := 96000.0, 24, false
	target := []string{"Shared/So - 01.flac", "Shared/So - 02.flac"}
	all := append(append([]string{}, target...), "Shared/Us - 01.flac")
	for _, p := range all {
		if err := s.UpsertTrack(context.Background(), &manifest.Track{
			Path: p, Size: 1_000_000, SampleRate: &rate,
			BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
		}); err != nil {
			t.Fatal(err)
		}
	}

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	blockRunner(t, p)

	res, err := c.SubmitOptimizePaths(context.Background(), "Shared/So", target, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitOptimizePaths: %v", err)
	}
	if res.EnqueuedCount != len(target) {
		t.Fatalf("EnqueuedCount = %d, want %d", res.EnqueuedCount, len(target))
	}
	got := enqueuedPaths(p)
	if strings.Join(got, "|") != strings.Join(target, "|") {
		t.Errorf("enqueued %v, want exactly %v", got, target)
	}
}

// TestSubmitPathsValidatesTheTarget: the identity form must reject a
// bad target the same way the prefix form does, rather than reaching
// the pool with a nonsense VariantID.
func TestSubmitPathsValidatesTheTarget(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirBatchFixture(t, s)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	for _, tc := range []struct{ rate, bits int }{{0, 24}, {192000, 20}, {-1, 24}} {
		if _, err := c.SubmitPaths(context.Background(), "x", target, tc.rate, tc.bits, t.TempDir()); err == nil {
			t.Errorf("SubmitPaths(%d/%d) = nil error, want a rejection", tc.rate, tc.bits)
		}
	}
}

// TestSubmitPathsEmptyScopeCompletesImmediately: an empty set is a
// valid empty batch, and it must not sit `pending` forever waiting for
// a worker callback that will never fire.
func TestSubmitPathsEmptyScopeCompletesImmediately(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	res, err := c.SubmitPaths(context.Background(), "nothing", nil, 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitPaths: %v", err)
	}
	if res.EnqueuedCount != 0 || res.TotalFiles != 0 {
		t.Errorf("empty scope: enqueued=%d total=%d, want 0/0", res.EnqueuedCount, res.TotalFiles)
	}
	rows, err := s.ListUpscaleBatches(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "completed" {
		t.Fatalf("batch rows = %+v, want one completed row", rows)
	}
}
