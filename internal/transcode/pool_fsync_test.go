package transcode

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPoolFsyncFailureSkipsUpsertAndFiresJobFailed pins the durability
// contract: when `fsyncFn` returns an error, the per-job path must
// NOT call UpsertVariant (the DB row would point at a non-durable
// file), must fire `jobFailed` with a descriptive errMsg, and must
// bump the Failed stat. Mirrors the existing
// `TestPoolDoesNotFireOnJobCompleteOnFailure` shape — same wait
// pattern, same callbacks.
func TestPoolFsyncFailureSkipsUpsertAndFiresJobFailed(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Seed the parent track row so UpsertVariant's foreign-key
	// constraint would otherwise be satisfied — proves the test
	// fails for the RIGHT reason (fsync error short-circuit, not
	// an unrelated DB constraint).
	seedTrackForPool(t, store, "Music/Album/durability.flac")

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	// Mock runner: report success without writing a file. In
	// production this would never happen — RunSox writes a real
	// sidecar. Combined with the synthetic fsync failure below
	// it exercises the durability gate cleanly.
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 12345, nil
	}
	fsyncErr := errors.New("synthetic ENOSPC at fsync")
	p.fsyncFn = func(path string) error {
		return fsyncErr
	}

	var jobCompleteFires atomic.Int64
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		jobCompleteFires.Add(1)
	})
	var jobFailedFires atomic.Int64
	var capturedErrMsg atomic.Value
	p.SetOnJobFailed(func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time) {
		jobFailedFires.Add(1)
		capturedErrMsg.Store(errMsg)
	})

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/durability.flac",
		SourceAbsPath:    "/dev/null/missing", // bypass real source IO via the mocked runner
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for the failure path to land (failedCnt bumped + fail event fired).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 && jobFailedFires.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := p.Stats().Failed; got < 1 {
		t.Errorf("Failed stat: got %d, want >= 1 (fsync failure must bump the counter)", got)
	}
	if got := p.Stats().Done; got != 0 {
		t.Errorf("Done stat: got %d, want 0 (UpsertVariant must NOT run after fsync failure)", got)
	}
	if got := jobCompleteFires.Load(); got != 0 {
		t.Errorf("jobComplete fires: got %d, want 0 (success-only contract; fsync failure is a failure)", got)
	}
	if got := jobFailedFires.Load(); got != 1 {
		t.Errorf("jobFailed fires: got %d, want 1", got)
	}
	if msg, _ := capturedErrMsg.Load().(string); !strings.Contains(msg, "fsync sidecar") {
		t.Errorf("jobFailed errMsg should be prefixed with 'fsync sidecar', got %q", msg)
	}
}

// TestPoolFsyncSuccessReachesUpsert pins the inverse contract — when
// `fsyncFn` returns nil, the existing success path runs unchanged.
// Mirrors `TestPoolNilOnJobCompleteDoesNotPanic`'s shape; existing
// tests cover the Done-increment side, so this case proves the
// jobComplete callback still fires after the fsync gate.
func TestPoolFsyncSuccessReachesUpsert(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	seedTrackForPool(t, store, "Music/Album/healthy.flac")

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}
	// Default fsyncFn (fsyncFileAndParent) would fail on the mocked
	// runner's never-written sidecar — swap to noop, same convention
	// as pool_test_helpers.go.
	p.fsyncFn = noopFsync

	var jobCompleteFires atomic.Int64
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		jobCompleteFires.Add(1)
	})

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/healthy.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Done >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Stats().Done; got < 1 {
		t.Errorf("Done stat: got %d, want >= 1 (fsync success must let UpsertVariant run)", got)
	}
	if got := jobCompleteFires.Load(); got < 1 {
		t.Errorf("jobComplete fires: got %d, want >= 1", got)
	}
}
