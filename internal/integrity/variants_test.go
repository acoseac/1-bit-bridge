package integrity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeLister is a test stub for VariantLister. Returns a fixed
// snapshot; tracks the call count for verifying tick cadence.
type fakeLister struct {
	mu        sync.Mutex
	snapshots [][]VariantSnapshot
	calls     int
	err       error
}

func (f *fakeLister) AllVariants() ([]VariantSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.snapshots) == 0 {
		return nil, nil
	}
	out := f.snapshots[0]
	if len(f.snapshots) > 1 {
		f.snapshots = f.snapshots[1:]
	}
	return out, nil
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeDeleter is a test stub for VariantDeleter. Records every
// delete call; optionally returns a configured error.
type fakeDeleter struct {
	mu      sync.Mutex
	deletes []string // "sourcePath|variantID"
	err     error
}

func (f *fakeDeleter) DeleteVariant(sourcePath, variantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.deletes = append(f.deletes, sourcePath+"|"+variantID)
	return nil
}

func (f *fakeDeleter) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deletes))
	copy(out, f.deletes)
	return out
}

// fakePublisher records every publish call.
type fakePublisher struct {
	mu     sync.Mutex
	events []struct {
		paths      []string
		variantIDs []string
	}
}

func (f *fakePublisher) publish(paths, variantIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pCopy := make([]string, len(paths))
	copy(pCopy, paths)
	vCopy := make([]string, len(variantIDs))
	copy(vCopy, variantIDs)
	f.events = append(f.events, struct {
		paths      []string
		variantIDs []string
	}{pCopy, vCopy})
}

func (f *fakePublisher) eventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakePublisher) lastEvent() (paths, variantIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return nil, nil
	}
	e := f.events[len(f.events)-1]
	return e.paths, e.variantIDs
}

// TestVariantWatcher_missingSidecarTriggersDeleteAndPublish is the
// headline contract: a sidecar that exists in the DB but not on
// disk is dropped from the DB AND a single SSE event fires. The
// happy-path (sidecar present) is untouched.
func TestVariantWatcher_missingSidecarTriggersDeleteAndPublish(t *testing.T) {
	tmpDir := t.TempDir()

	// One sidecar exists, one doesn't.
	presentPath := filepath.Join(tmpDir, "present.flac")
	missingPath := filepath.Join(tmpDir, "missing.flac")
	if err := os.WriteFile(presentPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lister := &fakeLister{snapshots: [][]VariantSnapshot{{
		{SourcePath: "Music/Present/01.flac", VariantID: "v1", SidecarPath: presentPath},
		{SourcePath: "Music/Missing/01.flac", VariantID: "v2", SidecarPath: missingPath},
	}}}
	deleter := &fakeDeleter{}
	publisher := &fakePublisher{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, tmpDir, 1*time.Hour)

	tickDone := make(chan int, 1)
	w.SetOnTickComplete(func(n int) { tickDone <- n })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	t.Cleanup(stop)

	// Wait for the immediate-on-boot sweep.
	select {
	case n := <-tickDone:
		if n != 1 {
			t.Fatalf("first sweep deleted %d, want 1 (the missing one)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first sweep never completed")
	}

	gotDeletes := deleter.deleted()
	if len(gotDeletes) != 1 {
		t.Fatalf("DeleteVariant called %d times, want 1; calls=%v", len(gotDeletes), gotDeletes)
	}
	if gotDeletes[0] != "Music/Missing/01.flac|v2" {
		t.Errorf("deleted wrong row: %q", gotDeletes[0])
	}
	if publisher.eventCount() != 1 {
		t.Fatalf("publisher fired %d events, want 1", publisher.eventCount())
	}
	paths, variantIDs := publisher.lastEvent()
	if len(paths) != 1 || paths[0] != "Music/Missing/01.flac" {
		t.Errorf("event paths: got %v, want [Music/Missing/01.flac]", paths)
	}
	if len(variantIDs) != 1 || variantIDs[0] != "v2" {
		t.Errorf("event variantIDs: got %v, want [v2]", variantIDs)
	}
}

// TestVariantWatcher_multipleMissesBatchIntoSingleEvent pins the
// per-tick batching invariant: N missing sidecars produce ONE
// SSE event with both paths and variantIDs in the payload, NOT
// N events. Critical for iOS — reconciles all affected tracks
// in one pass.
func TestVariantWatcher_multipleMissesBatchIntoSingleEvent(t *testing.T) {
	tmpDir := t.TempDir()
	// Decoy entry so the variants dir is non-empty — the
	// mount-loss guard skips sweeps over an empty dir.
	if err := os.WriteFile(filepath.Join(tmpDir, "decoy.flac"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	publisher := &fakePublisher{}
	lister := &fakeLister{snapshots: [][]VariantSnapshot{{
		{SourcePath: "A/1.flac", VariantID: "vA", SidecarPath: filepath.Join(tmpDir, "missing-a.flac")},
		{SourcePath: "B/1.flac", VariantID: "vB", SidecarPath: filepath.Join(tmpDir, "missing-b.flac")},
		{SourcePath: "C/1.flac", VariantID: "vC", SidecarPath: filepath.Join(tmpDir, "missing-c.flac")},
	}}}
	deleter := &fakeDeleter{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, tmpDir, 1*time.Hour)
	tickDone := make(chan int, 1)
	w.SetOnTickComplete(func(n int) { tickDone <- n })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	t.Cleanup(stop)

	select {
	case n := <-tickDone:
		if n != 3 {
			t.Fatalf("sweep deleted %d, want 3", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweep never completed")
	}

	if publisher.eventCount() != 1 {
		t.Fatalf("publisher fired %d events, want 1 (batched)", publisher.eventCount())
	}
	paths, variantIDs := publisher.lastEvent()
	if len(paths) != 3 || len(variantIDs) != 3 {
		t.Errorf("event payload: paths=%v variantIDs=%v, want both length 3", paths, variantIDs)
	}
}

// TestVariantWatcher_dedupesPathsAcrossMultipleVariants pins the
// `Paths` dedup contract from `internal/api/upscale_deleted_event.go`'s
// `UpscaleDeletedEvent` docstring: paths is the SET of affected
// source paths, not the per-variant repetition. A track with two
// missing variants (e.g. 96k AND 192k variants for the same
// source path, both wiped by an external rm) emits the path
// ONCE in the published event, while `variantIDs` carries both
// variantIDs verbatim (variantIDs are NOT zipped 1:1 to paths
// — they're the union of what disappeared, possibly overlapping
// across the path set). CodeRabbit Minor on PR #209.
func TestVariantWatcher_dedupesPathsAcrossMultipleVariants(t *testing.T) {
	tmpDir := t.TempDir()
	// Decoy entry so the variants dir is non-empty — the
	// mount-loss guard skips sweeps over an empty dir.
	if err := os.WriteFile(filepath.Join(tmpDir, "decoy.flac"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	publisher := &fakePublisher{}
	// Two variants for the SAME source path — both sidecars missing.
	lister := &fakeLister{snapshots: [][]VariantSnapshot{{
		{SourcePath: "A/1.flac", VariantID: "v96", SidecarPath: filepath.Join(tmpDir, "a-v96.flac")},
		{SourcePath: "A/1.flac", VariantID: "v192", SidecarPath: filepath.Join(tmpDir, "a-v192.flac")},
	}}}
	deleter := &fakeDeleter{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, tmpDir, 1*time.Hour)
	tickDone := make(chan int, 1)
	w.SetOnTickComplete(func(n int) { tickDone <- n })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	t.Cleanup(stop)

	select {
	case n := <-tickDone:
		if n != 2 {
			t.Fatalf("sweep deleted %d rows, want 2 (both variants)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweep never completed")
	}

	if publisher.eventCount() != 1 {
		t.Fatalf("publisher fired %d events, want 1", publisher.eventCount())
	}
	paths, variantIDs := publisher.lastEvent()
	if len(paths) != 1 {
		t.Errorf("paths not deduplicated: got %v, want one entry for A/1.flac", paths)
	}
	if paths[0] != "A/1.flac" {
		t.Errorf("paths[0]: got %q, want A/1.flac", paths[0])
	}
	if len(variantIDs) != 2 {
		t.Errorf("variantIDs: got %v, want both [v96, v192]", variantIDs)
	}
}

// TestVariantWatcher_noMissesNoEvent pins the silence invariant:
// every sidecar present → zero SSE events. iOS doesn't want to
// see periodic noise from a healthy bridge.
func TestVariantWatcher_noMissesNoEvent(t *testing.T) {
	tmpDir := t.TempDir()
	sidecar := filepath.Join(tmpDir, "ok.flac")
	if err := os.WriteFile(sidecar, []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	publisher := &fakePublisher{}
	lister := &fakeLister{snapshots: [][]VariantSnapshot{{
		{SourcePath: "Music/01.flac", VariantID: "v", SidecarPath: sidecar},
	}}}
	deleter := &fakeDeleter{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, tmpDir, 1*time.Hour)
	tickDone := make(chan int, 1)
	w.SetOnTickComplete(func(n int) { tickDone <- n })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	t.Cleanup(stop)

	select {
	case n := <-tickDone:
		if n != 0 {
			t.Fatalf("sweep deleted %d on a healthy DB, want 0", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweep never completed")
	}
	if publisher.eventCount() != 0 {
		t.Errorf("publisher fired %d events on healthy DB, want 0", publisher.eventCount())
	}
	if len(deleter.deleted()) != 0 {
		t.Errorf("deleter called on healthy DB: %v", deleter.deleted())
	}
}

// TestVariantWatcher_intervalZeroDisables pins the opt-out path:
// passing zero (or negative) interval returns a no-op stopFn and
// spawns no goroutine. Tested via the lister call count — must
// stay zero.
func TestVariantWatcher_intervalZeroDisables(t *testing.T) {
	lister := &fakeLister{}
	deleter := &fakeDeleter{}
	publisher := &fakePublisher{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	defer stop()

	// Give the watcher a moment in case it incorrectly spawned anyway.
	time.Sleep(50 * time.Millisecond)
	if lister.callCount() != 0 {
		t.Errorf("disabled watcher called AllVariants %d times, want 0", lister.callCount())
	}
}

// TestVariantWatcher_listerErrorDoesNotAbortLoop pins the
// transient-DB-hiccup resilience: a single AllVariants() error
// must NOT abort the loop. The next tick proceeds normally.
func TestVariantWatcher_listerErrorDoesNotAbortLoop(t *testing.T) {
	tmpDir := t.TempDir()
	// Decoy entry so the variants dir is non-empty — the
	// mount-loss guard skips sweeps over an empty dir.
	if err := os.WriteFile(filepath.Join(tmpDir, "decoy.flac"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	missingPath := filepath.Join(tmpDir, "missing.flac")

	// While err is set the first call returns the error WITHOUT
	// consuming a snapshot slot. After we clear err the next call
	// returns the first snapshot — which we want to be the
	// missing-sidecar case so the recovered tick deletes a row.
	lister := &fakeLister{
		snapshots: [][]VariantSnapshot{
			{
				{SourcePath: "A/1.flac", VariantID: "v", SidecarPath: missingPath},
			},
		},
		err: errors.New("transient DB error"),
	}
	deleter := &fakeDeleter{}
	publisher := &fakePublisher{}

	// Use a very short interval so the second tick fires fast.
	w := NewVariantWatcher(lister, deleter, publisher.publish, tmpDir, 50*time.Millisecond)
	tickDone := make(chan int, 4)
	w.SetOnTickComplete(func(n int) { tickDone <- n })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop := w.Start(ctx)
	t.Cleanup(stop)

	// First tick: error, returns 0.
	select {
	case n := <-tickDone:
		if n != 0 {
			t.Fatalf("first sweep returned %d, want 0 (error path)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first sweep never completed")
	}

	// Clear the error so subsequent ticks succeed.
	lister.mu.Lock()
	lister.err = nil
	lister.mu.Unlock()

	// Wait for the next tick (loop continued past the error).
	select {
	case n := <-tickDone:
		if n != 1 {
			t.Fatalf("subsequent sweep returned %d, want 1 (recovered)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop aborted after error; second sweep never fired")
	}
}

// TestVariantWatcher_ctxCancelStopsLoop pins the shutdown
// contract: cancelling the ctx exits the goroutine within
// at most one interval.
func TestVariantWatcher_ctxCancelStopsLoop(t *testing.T) {
	lister := &fakeLister{}
	deleter := &fakeDeleter{}
	publisher := &fakePublisher{}

	w := NewVariantWatcher(lister, deleter, publisher.publish, "", 50*time.Millisecond)

	// Track tick fires via an extra channel.
	tickFired := make(chan struct{}, 8)
	w.SetOnTickComplete(func(int) { tickFired <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	stop := w.Start(ctx)
	t.Cleanup(stop)

	// Wait for at least one tick to verify the loop is running.
	select {
	case <-tickFired:
	case <-time.After(2 * time.Second):
		t.Fatal("loop never started")
	}

	// Cancel and verify the loop stops — no further tick fires
	// after ~3 intervals of additional wait.
	cancel()
	// Drain any in-flight tick that landed concurrently with
	// the cancel.
	timeout := time.After(150 * time.Millisecond)
	for {
		select {
		case <-tickFired:
		case <-timeout:
			goto stopped
		}
	}
stopped:
	// From here, the loop should be parked.
	select {
	case <-tickFired:
		t.Fatal("tick fired after ctx cancellation")
	case <-time.After(200 * time.Millisecond):
		// expected — loop stopped.
	}
}

// TestVariantWatcher_variantsDirGuard pins the mount-loss guard
// (2026-07-21 review H4): with rows in the catalog, a variants
// dir that is missing OR empty (the cleanly-unmounted-mountpoint
// signature) must SKIP the sweep wholesale — zero deletes, zero
// events — while a non-empty dir proceeds to per-row stats, and
// an empty catalog proceeds regardless of dir state.
func TestVariantWatcher_variantsDirGuard(t *testing.T) {
	cases := []struct {
		name string
		// setupDir returns the variantsDir to hand the watcher.
		setupDir func(t *testing.T) string
		rows     int // catalog rows handed to the lister (sidecars always missing)
		wantDel  int
		wantEvts int
	}{
		{
			name: "missing dir with rows skips sweep",
			setupDir: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "unmounted-mountpoint")
			},
			rows:     3,
			wantDel:  0,
			wantEvts: 0,
		},
		{
			name: "empty dir with rows skips sweep",
			setupDir: func(t *testing.T) string {
				return t.TempDir() // exists, holds nothing — the unmounted mountpoint
			},
			rows:     3,
			wantDel:  0,
			wantEvts: 0,
		},
		{
			name: "non-empty dir with rows proceeds",
			setupDir: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "decoy.flac"), []byte("ok"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return dir
			},
			rows:     3,
			wantDel:  3,
			wantEvts: 1,
		},
		{
			name: "zero rows proceeds regardless of missing dir",
			setupDir: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "unmounted-mountpoint")
			},
			rows:     0,
			wantDel:  0,
			wantEvts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			variantsDir := tc.setupDir(t)

			rows := make([]VariantSnapshot, tc.rows)
			for i := range rows {
				rows[i] = VariantSnapshot{
					SourcePath:  "A/1.flac",
					VariantID:   "v" + string(rune('a'+i)),
					SidecarPath: filepath.Join(variantsDir, "missing-"+string(rune('a'+i))+".flac"),
				}
			}
			lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
			deleter := &fakeDeleter{}
			publisher := &fakePublisher{}

			w := NewVariantWatcher(lister, deleter, publisher.publish, variantsDir, 1*time.Hour)
			tickDone := make(chan int, 1)
			w.SetOnTickComplete(func(n int) { tickDone <- n })

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			stop := w.Start(ctx)
			t.Cleanup(stop)

			select {
			case n := <-tickDone:
				if n != tc.wantDel {
					t.Fatalf("sweep deleted %d rows, want %d", n, tc.wantDel)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("sweep never completed")
			}
			if got := len(deleter.deleted()); got != tc.wantDel {
				t.Errorf("DeleteVariant called %d times, want %d", got, tc.wantDel)
			}
			if got := publisher.eventCount(); got != tc.wantEvts {
				t.Errorf("publisher fired %d events, want %d", got, tc.wantEvts)
			}
		})
	}
}
