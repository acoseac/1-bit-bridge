package analyze

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pooltest"
)

// The serve-side pool is built on every bridge and never stopped (#781), so a
// switch-off reaches it only through the live gate the pool asks when a worker
// takes a job (backlog B155). These tests stand in for that gate with a switch
// they close while jobs wait in the queue; the scenarios they share with the
// transcode pool's tests are internal/pooltest's.

// analysisPools adapts this package's pool to pooltest.Start: a job is the
// analysis of its path, against a track row seeded for it, and a run that
// succeeds commits its analysis row.
func analysisPools(s *manifest.Store) pooltest.Start {
	return func(t *testing.T, workers, queueCap int, run func(context.Context, string) error, gate func() bool) pooltest.Pool {
		t.Helper()
		runner := func(ctx context.Context, spec AnalyzeSpec) (Result, error) {
			if err := run(ctx, spec.SourceLibraryRel); err != nil {
				return Result{}, err
			}
			return Result{
				WaveformPath:  "/w/" + spec.SourceLibraryRel + ".waveform.bin",
				WaveformTag:   "t-" + spec.SourceLibraryRel,
				WaveformSize:  42,
				SchemaVersion: WaveformSchemaVersion,
			}, nil
		}
		p := NewPool(s, workers, queueCap, WithRunner(runner), WithFsync(noFsync), WithLiveGate(gate))
		t.Cleanup(p.Stop)
		return pooltest.Pool{
			Enqueue: func(path string) error {
				putTrack(t, s, path)
				return p.Enqueue(AnalyzeSpec{SourceLibraryRel: path})
			},
			Stats: func() pooltest.Stats {
				st := p.Stats()
				return pooltest.Stats{QueueLen: st.QueueLen, Inflight: st.Inflight,
					Enqueued: st.Enqueued, Done: st.Done, Failed: st.Failed, Dropped: st.Dropped}
			},
			Stop:   p.Stop,
			Closed: p.closed.Load,
		}
	}
}

// TestAQueuedAnalysisDoesNotRunOnceTheGateCloses is B155: five analyses
// queued behind a running one, and analysis switched off before the worker
// reaches them. None of the five may run: each would be a decode, and its
// row an `indexed_at` bump that pushes a delta to every paired device, on a
// bridge whose every surface says analysis is off. The job already running
// when the gate closed finishes (the decision covers queued jobs).
func TestAQueuedAnalysisDoesNotRunOnceTheGateCloses(t *testing.T) {
	s := newStore(t)
	q := pooltest.QueueBehindAHeldJob(t, analysisPools(s), pooltest.Queued)
	st := q.SwitchOff(t)

	q.RequireOnlyTheHeldJobRan(t, st, len(pooltest.Queued))
	for _, path := range pooltest.Queued {
		if row, err := s.GetAnalysis(context.Background(), path); err != nil || row != nil {
			t.Errorf("GetAnalysis(%s) = %v, %v: a dropped job wrote its row", path, row, err)
		}
	}
	if row, err := s.GetAnalysis(context.Background(), pooltest.HeldPath); err != nil || row == nil {
		t.Errorf("GetAnalysis(%s) = %v, %v: the job running when the gate closed must finish", pooltest.HeldPath, row, err)
	}
}

// TestADroppedAnalysisFreesItsPathForTheNextOffer pins the release half: a
// dropped job gives its dedup slot back, so the sweeper's next offer of the
// path, once analysis is on again, is accepted and runs. A drop that kept the
// slot would answer every later offer ErrDuplicateInflight until restart.
func TestADroppedAnalysisFreesItsPathForTheNextOffer(t *testing.T) {
	s := newStore(t)
	q := pooltest.QueueBehindAHeldJob(t, analysisPools(s), pooltest.Queued[:1])
	q.SwitchOff(t)
	q.ReofferAfterTheDrop(t, pooltest.Queued[0])

	if row, err := s.GetAnalysis(context.Background(), pooltest.Queued[0]); err != nil || row == nil {
		t.Errorf("GetAnalysis(%s) = %v, %v after the re-offer, want its row", pooltest.Queued[0], row, err)
	}
}

// TestAnAnalysisDropBurstLogsOneLineWithItsCount: a switch-off drops a whole
// queue at once, so the pool says so once, with the count, never once per
// job (a 5,000-job queue would be 5,000 lines).
func TestAnAnalysisDropBurstLogsOneLineWithItsCount(t *testing.T) {
	rec := loggingtest.Record(t)
	q := pooltest.QueueBehindAHeldJob(t, analysisPools(newStore(t)), pooltest.Queued)
	q.SwitchOff(t)
	pooltest.RequireOneRunLine(t, rec, dropBurstMsg, len(pooltest.Queued), pooltest.Queued)
}

// TestADropBurstAcrossWorkersIsOneLine: four workers share one queue of 200
// dropped at a switch-off, and it is one burst, one line, with every job
// counted once (pooltest.DropRunAcrossWorkers). When a burst ends is
// TestAJobStillBeingDecidedKeepsItsBurstOpen's.
func TestADropBurstAcrossWorkersIsOneLine(t *testing.T) {
	pooltest.DropRunAcrossWorkers(t, analysisPools(newStore(t)), dropBurstMsg)
}

// TestAJobStillBeingDecidedKeepsItsBurstOpen pins when a burst ends: once no
// accepted job is left undecided. A job parked inside the gate, taken from
// the queue and not yet decided, keeps it open while the other worker drops
// the rest and empties the queue (pooltest.UndecidedJobKeepsItsRunOpen).
func TestAJobStillBeingDecidedKeepsItsBurstOpen(t *testing.T) {
	pooltest.UndecidedJobKeepsItsRunOpen(t, analysisPools(newStore(t)), dropBurstMsg)
}

// TestTheGateIsNotAskedForAJobTheStopTook: Stop flips p.closed before it
// cancels anything, and a job a worker takes after that is shutdown's,
// uncounted and unannounced as before. It is no drop, and no burst line
// (pooltest.StopTakesNoDecision).
func TestTheGateIsNotAskedForAJobTheStopTook(t *testing.T) {
	pooltest.StopTakesNoDecision(t, analysisPools(newStore(t)), dropBurstMsg)
}
