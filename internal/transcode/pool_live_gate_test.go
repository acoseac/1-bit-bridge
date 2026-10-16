package transcode

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pooltest"
	"github.com/google/uuid"
)

// The serve-side pool is built on every bridge and never stopped (#781), so a
// switch-off reaches it only through the live gate a worker asks when it
// takes a job (backlog B155). These tests stand in for serve's gate with one
// they close while jobs wait in the queue; the scenarios they share with the
// analysis pool's tests are internal/pooltest's.

// poolEvents counts what the pool announced, through the callbacks serve
// wires.
type poolEvents struct {
	mu        sync.Mutex
	completed []string
	failed    []string
	dropped   []string
}

func (e *poolEvents) wire(p *Pool) {
	p.SetOnJobComplete(func(path, _ string, _, _ int, _ float64, _ uuid.UUID, _ time.Time) {
		e.mu.Lock()
		e.completed = append(e.completed, path)
		e.mu.Unlock()
	})
	p.SetOnJobFailed(func(path, _, _ string, _ float64, _ uuid.UUID, _ time.Time) {
		e.mu.Lock()
		e.failed = append(e.failed, path)
		e.mu.Unlock()
	})
	p.SetOnJobDropped(func(path, _ string, _ uuid.UUID, _ time.Time) {
		e.mu.Lock()
		e.dropped = append(e.dropped, path)
		e.mu.Unlock()
	})
}

func (e *poolEvents) snapshot() (completed, failed, dropped []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.completed), slices.Clone(e.failed), slices.Clone(e.dropped)
}

func gateSpec(rel string, kind JobKind, outDir string) JobSpec {
	return JobSpec{
		SourceLibraryRel: rel,
		SourceAbsPath:    "/dev/null/missing", // every runner here is a stub
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        outDir,
		Kind:             kind,
	}
}

// renderWith is a runner that calls run for each job and, once run returns
// nil, reports a 10-byte rendition.
func renderWith(run func(context.Context, string) error) func(context.Context, JobSpec) (RunResult, error) {
	return func(ctx context.Context, spec JobSpec) (RunResult, error) {
		if err := run(ctx, spec.SourceLibraryRel); err != nil {
			return RunResult{}, err
		}
		return RunResult{SizeBytes: 10}, nil
	}
}

// statsOf reads the pool's snapshot as the scenarios read it.
func statsOf(p *Pool) func() pooltest.Stats {
	return func() pooltest.Stats {
		st := p.Stats()
		return pooltest.Stats{QueueLen: st.QueueLen, Inflight: st.Inflight,
			Enqueued: st.Enqueued, Done: st.Done, Failed: st.Failed, Dropped: st.Dropped}
	}
}

// renderPools adapts this package's pool to pooltest.Start, every job an
// upscale against a track row seeded for it, over one store, and keeps what
// the pool it started announced.
type renderPools struct {
	store  *manifest.Store
	outDir string
	events *poolEvents
}

func newRenderPools(t *testing.T) *renderPools {
	t.Helper()
	return &renderPools{store: openTempStoreForPool(t), outDir: t.TempDir(), events: &poolEvents{}}
}

func (h *renderPools) start(t *testing.T, workers, queueCap int, run func(context.Context, string) error, gate func() bool) pooltest.Pool {
	t.Helper()
	p := NewPool(h.store, workers, queueCap)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = renderWith(run)
	p.SetLiveGate(func(JobSpec) bool { return gate() })
	h.events.wire(p)
	return pooltest.Pool{
		Enqueue: func(path string) error {
			seedTrackForPool(t, h.store, path)
			return p.Enqueue(gateSpec(path, JobKindUpscale, h.outDir))
		},
		Stats:  statsOf(p),
		Stop:   p.Stop,
		Closed: p.closed.Load,
	}
}

// TestAQueuedRenderDoesNotRunOnceItsGateCloses is B155 for the transcode
// pool: five renders queued behind a running one, and upscaling switched off
// before the worker reaches them. None of the five may run, each a sox render
// and a sidecar on disk, and none is counted or announced as done or failed:
// each is announced as dropped, which is how a batch learns it. The render
// already running when the gate closed finishes.
func TestAQueuedRenderDoesNotRunOnceItsGateCloses(t *testing.T) {
	h := newRenderPools(t)
	q := pooltest.QueueBehindAHeldJob(t, h.start, pooltest.Queued)
	st := q.SwitchOff(t)
	q.Pool.Stop() // drains the publisher, so every announcement has landed

	q.RequireOnlyTheHeldJobRan(t, st, len(pooltest.Queued))
	completed, failed, dropped := h.events.snapshot()
	if !slices.Equal(completed, []string{pooltest.HeldPath}) || len(failed) != 0 {
		t.Errorf("announced complete %v, failed %v; want complete [%s] and no failure", completed, failed, pooltest.HeldPath)
	}
	sort.Strings(dropped)
	if !slices.Equal(dropped, pooltest.Queued) {
		t.Errorf("announced dropped %v, want %v: a batch learns its job was dropped from this event", dropped, pooltest.Queued)
	}
	variantID := gateSpec(pooltest.Queued[0], JobKindUpscale, "").VariantID()
	for _, path := range pooltest.Queued {
		if row, err := h.store.LookupVariant(context.Background(), path, variantID); err != nil || row != nil {
			t.Errorf("LookupVariant(%s) = %v, %v: a dropped job wrote its rendition", path, row, err)
		}
	}
	if row, err := h.store.LookupVariant(context.Background(), pooltest.HeldPath, variantID); err != nil || row == nil {
		t.Errorf("LookupVariant(%s) = %v, %v: the render running when the gate closed must finish", pooltest.HeldPath, row, err)
	}
}

// kindGate is a live gate that a test closes for one kind, and that records
// each job it was asked about.
type kindGate struct {
	mu    sync.Mutex
	shut  map[JobKind]bool
	asked []string
}

func (g *kindGate) admits(spec JobSpec) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, spec.SourceLibraryRel+"|"+string(spec.Kind))
	return !g.shut[spec.Kind]
}

func (g *kindGate) closeKind(k JobKind) {
	g.mu.Lock()
	g.shut[k] = true
	g.mu.Unlock()
}

// TestTheLiveGateIsAskedForEachJobItTakes: the gate decides per job, from the
// job itself, so switching one kind off (the CarPlay switch) drops that
// kind's queued jobs and runs the rest.
func TestTheLiveGateIsAskedForEachJobItTakes(t *testing.T) {
	h := newRenderPools(t)
	holder := pooltest.NewHolder(func(path string) bool { return path == pooltest.HeldPath })
	gate := &kindGate{shut: map[JobKind]bool{}}
	p := NewPool(h.store, 1, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = renderWith(holder.Run)
	p.SetLiveGate(gate.admits)
	h.events.wire(p)
	jobs := []struct {
		path string
		kind JobKind
	}{{pooltest.HeldPath, JobKindUpscale}, {"Q/up.flac", JobKindUpscale}, {"Q/opt.flac", JobKindOptimize}}
	for i, j := range jobs {
		seedTrackForPool(t, h.store, j.path)
		if err := p.Enqueue(gateSpec(j.path, j.kind, h.outDir)); err != nil {
			t.Fatalf("Enqueue(%s): %v", j.path, err)
		}
		if i == 0 {
			holder.WaitHeld(t, 1)
		}
	}

	gate.closeKind(JobKindOptimize)
	holder.Release()
	st := pooltest.WaitIdle(t, statsOf(p))
	p.Stop()

	if got := holder.Ran(); !slices.Equal(got, []string{pooltest.HeldPath, "Q/up.flac"}) {
		t.Errorf("rendered %v, want [%s Q/up.flac]: only the optimize kind is switched off", got, pooltest.HeldPath)
	}
	if st.Done != 2 || st.Dropped != 1 {
		t.Errorf("Done=%d Dropped=%d, want Done=2 Dropped=1", st.Done, st.Dropped)
	}
	if _, _, dropped := h.events.snapshot(); !slices.Equal(dropped, []string{"Q/opt.flac"}) {
		t.Errorf("announced dropped %v, want [Q/opt.flac]", dropped)
	}
	gate.mu.Lock()
	asked := slices.Clone(gate.asked)
	gate.mu.Unlock()
	sort.Strings(asked)
	if want := []string{pooltest.HeldPath + "|upscale", "Q/opt.flac|optimize", "Q/up.flac|upscale"}; !slices.Equal(asked, want) {
		t.Errorf("the gate was asked about %v, want each job once, with its own spec: %v", asked, want)
	}
}

// TestADroppedRenderFreesItsPathForTheNextOffer pins the release half: a
// dropped job gives its (source, variant) slot back, so the next request or
// sweep for it, once the feature is on again, is accepted and runs.
func TestADroppedRenderFreesItsPathForTheNextOffer(t *testing.T) {
	h := newRenderPools(t)
	q := pooltest.QueueBehindAHeldJob(t, h.start, pooltest.Queued[:1])
	q.SwitchOff(t)
	q.ReofferAfterTheDrop(t, pooltest.Queued[0])

	variantID := gateSpec(pooltest.Queued[0], JobKindUpscale, "").VariantID()
	if row, err := h.store.LookupVariant(context.Background(), pooltest.Queued[0], variantID); err != nil || row == nil {
		t.Errorf("LookupVariant(%s) = %v, %v after the re-offer, want its row", pooltest.Queued[0], row, err)
	}
}

// TestARenderDropBurstLogsOneLineWithItsCount: a switch-off drops a whole
// queue at once, so the pool says so once, with the count, never once per
// job.
func TestARenderDropBurstLogsOneLineWithItsCount(t *testing.T) {
	rec := loggingtest.Record(t)
	q := pooltest.QueueBehindAHeldJob(t, newRenderPools(t).start, pooltest.Queued)
	q.SwitchOff(t)
	pooltest.RequireOneRunLine(t, rec, dropBurstMsg, len(pooltest.Queued), pooltest.Queued)
}

// TestARenderDropBurstAcrossWorkersIsOneLine: four workers share one queue
// of 200 dropped at a switch-off, and it is one burst, one line, with every
// job counted once (pooltest.DropRunAcrossWorkers). When a burst ends is
// TestARenderStillBeingDecidedKeepsItsBurstOpen's.
func TestARenderDropBurstAcrossWorkersIsOneLine(t *testing.T) {
	pooltest.DropRunAcrossWorkers(t, newRenderPools(t).start, dropBurstMsg)
}

// TestARenderStillBeingDecidedKeepsItsBurstOpen pins when a burst ends: once
// no accepted job is left undecided. A job parked inside the gate, taken and
// not yet decided, keeps it open while the other worker drops the rest and
// empties the queue (pooltest.UndecidedJobKeepsItsRunOpen).
func TestARenderStillBeingDecidedKeepsItsBurstOpen(t *testing.T) {
	pooltest.UndecidedJobKeepsItsRunOpen(t, newRenderPools(t).start, dropBurstMsg)
}

// TestTheGateIsNotAskedForARenderTheStopTook: a job a worker takes after Stop
// flipped p.closed is shutdown's, uncounted and unannounced as before: no
// drop, no burst line, and the gate is not asked (pooltest.StopTakesNoDecision).
// Nothing is announced as dropped either.
func TestTheGateIsNotAskedForARenderTheStopTook(t *testing.T) {
	h := newRenderPools(t)
	pooltest.StopTakesNoDecision(t, h.start, dropBurstMsg)
	if _, _, dropped := h.events.snapshot(); len(dropped) != 0 {
		t.Errorf("announced dropped %v at shutdown, want none", dropped)
	}
}
