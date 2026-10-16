package transcode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pooltest"
	"github.com/google/uuid"
)

// A batch counts on every job it enqueued calling back: its remaining set
// must drain, or the row sits `running` forever and its files inflate the
// ETA. A job the pool drops at take time, because its kind's live gate closed
// (backlog B155), calls back through OnJobDropped, and the batch ends
// `cancelled` with its counters naming only the work that ran.

var gateBatchPaths = []string{"Album/01.flac", "Album/02.flac", "Album/03.flac", "Album/04.flac"}

// seedGateBatch plants four 44.1/16 FLACs, every one eligible for a 192/24
// upscale.
func seedGateBatch(t *testing.T, s *manifest.Store) {
	t.Helper()
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Album"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range gateBatchPaths {
		rate := 44100.0
		bits := 16
		isDSD := false
		if err := s.UpsertTrack(context.Background(), &manifest.Track{
			Path: path, Size: 1_000_000, SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", path, err)
		}
	}
}

// gateCoordinator wires a coordinator over a pool of `workers` workers that
// runs each job through holder under gate, with the three job callbacks
// wired the way serve wires them. afterDropped, when set, runs after each
// OnJobDropped returns.
func gateCoordinator(t *testing.T, s *manifest.Store, workers int, holder *pooltest.Holder, gate func(JobSpec) bool, afterDropped func(path string)) (*Coordinator, *Pool) {
	t.Helper()
	p := NewPool(s, workers, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = renderWith(holder.Run)
	p.SetLiveGate(gate)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, scannedResolver(s))
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.SetPublish(func(BatchProgressEvent) {})
	p.SetOnJobComplete(c.OnJobComplete)
	p.SetOnJobFailed(c.OnJobFailed)
	p.SetOnJobDropped(func(path, variantID string, batchID uuid.UUID, at time.Time) {
		c.OnJobDropped(path, variantID, batchID, at)
		if afterDropped != nil {
			afterDropped(path)
		}
	})
	return c, p
}

// waitBatchRow waits until the persisted row of id is terminal and returns
// it.
func waitBatchRow(t *testing.T, s *manifest.Store, id uuid.UUID) manifest.UpscaleBatchRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := s.ListUpscaleBatches(context.Background(), 200)
		if err != nil {
			t.Fatalf("ListUpscaleBatches: %v", err)
		}
		for _, r := range rows {
			if r.ID == id && isTerminalStatus(r.Status) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch %s not terminal after 10s: %+v", id, rows)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// remaining reads how many files a live batch still waits on; -1 once it is
// no longer live.
func remaining(c *Coordinator, id uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.liveBatches[id]
	if !ok {
		return -1
	}
	return len(st.RemainingIDs)
}

func requireCancelledByTheSwitchOff(t *testing.T, row manifest.UpscaleBatchRow, processed, failed, total, notRendered int) {
	t.Helper()
	if row.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled: its files stopped because their kind went off, so the batch did not complete", row.Status)
	}
	if row.ProcessedFiles != processed || row.FailedFiles != failed || row.TotalFiles != total {
		t.Errorf("processed/failed/total = %d/%d/%d, want %d/%d/%d: a dropped file is neither processed nor failed, and stays in the total",
			row.ProcessedFiles, row.FailedFiles, row.TotalFiles, processed, failed, total)
	}
	if want := fmt.Sprintf("%d of %d", notRendered, total); !strings.Contains(row.Error, want) {
		t.Errorf("error = %q, want it to say %q files were not rendered", row.Error, want)
	}
}

// TestABatchWhoseQueuedFilesAreDroppedEndsCancelled: one file of four is
// rendering when upscaling goes off, and the other three are dropped as a
// worker reaches them. The batch must still end (its remaining set drains
// through the drops), and end honestly: cancelled, one processed, the three
// that never rendered named in its error. Here the LAST callback is the
// running file's completion, after the drops.
func TestABatchWhoseQueuedFilesAreDroppedEndsCancelled(t *testing.T) {
	s := openTempStoreForBatch(t)
	seedGateBatch(t, s)
	holder := pooltest.NewHolder(func(path string) bool { return path == "Album/01.flac" })
	c, _ := gateCoordinator(t, s, 2, holder, func(spec JobSpec) bool { return spec.SourceLibraryRel == "Album/01.flac" }, nil)

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.EnqueuedCount != len(gateBatchPaths) {
		t.Fatalf("EnqueuedCount = %d, want %d", res.EnqueuedCount, len(gateBatchPaths))
	}
	holder.WaitHeld(t, 1)
	deadline := time.Now().Add(10 * time.Second)
	for remaining(c, res.BatchID) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the batch still waits on %d files, want only the held one", remaining(c, res.BatchID))
		}
		time.Sleep(2 * time.Millisecond)
	}
	holder.Release()

	row := waitBatchRow(t, s, res.BatchID)
	requireCancelledByTheSwitchOff(t, row, 1, 0, 4, 3)
	if n := remaining(c, res.BatchID); n != -1 {
		t.Errorf("batch still live with %d remaining after its terminal row landed", n)
	}
	variantID := JobSpec{TargetSampleRate: 192000, TargetBits: 24}.VariantID()
	for _, path := range gateBatchPaths[1:] {
		if v, err := s.LookupVariant(context.Background(), path, variantID); err != nil || v != nil {
			t.Errorf("LookupVariant(%s) = %v, %v: a dropped file was rendered", path, v, err)
		}
	}
}

// TestABatchWhoseEveryFileIsDroppedEndsCancelled: the gate closes before any
// of the batch's files is taken, so every callback is a drop and the last one
// ends the batch.
func TestABatchWhoseEveryFileIsDroppedEndsCancelled(t *testing.T) {
	s := openTempStoreForBatch(t)
	seedGateBatch(t, s)
	holder := pooltest.NewHolder(func(string) bool { return false })
	c, _ := gateCoordinator(t, s, 2, holder, func(JobSpec) bool { return false }, nil)

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	row := waitBatchRow(t, s, res.BatchID)
	requireCancelledByTheSwitchOff(t, row, 0, 0, 4, 4)
	if got := holder.Ran(); len(got) != 0 {
		t.Errorf("rendered %v with every gate closed, want nothing", got)
	}
}

// TestADroppedFileAndADedupedOneLeaveTheBatchCancelled: the post-loop drain
// check (completeIfDrained) is the third place a batch ends, when its last
// candidate is deduped against an overlapping batch's job after its other
// file was already dropped. It must end cancelled too, not completed.
func TestADroppedFileAndADedupedOneLeaveTheBatchCancelled(t *testing.T) {
	s := openTempStoreForBatch(t)
	seedBatchFixture(t, s) // Album/02 + Album/03 are the eligible candidates.
	const droppedPath = "Album/02.flac"
	const dedupedPath = "Album/03.flac"
	dropped := make(chan struct{})
	c, p := gateCoordinator(t, s, 1, pooltest.NewHolder(func(string) bool { return false }), func(JobSpec) bool { return false }, func(path string) {
		if path == droppedPath {
			close(dropped)
		}
	})
	claimInflight(t, p, dedupedPath, JobSpec{TargetSampleRate: 192000, TargetBits: 24})
	afterEnqueueHookForTests = func(path string) {
		if path != droppedPath {
			return
		}
		select {
		case <-dropped:
		case <-time.After(5 * time.Second):
			t.Error("the dropped file's callback never returned")
		}
	}
	t.Cleanup(func() { afterEnqueueHookForTests = nil })

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	row := waitBatchRow(t, s, res.BatchID)
	requireCancelledByTheSwitchOff(t, row, 0, 0, 1, 1)
}
