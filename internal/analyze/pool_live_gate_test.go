package analyze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The serve-side pool is built on every bridge and never stopped (#781), so a
// switch-off reaches it only through the live gate the pool asks when a worker
// takes a job (backlog B155). These tests stand in for that gate with a switch
// they close while jobs wait in the queue.

// gateSwitch is a live gate a test opens and closes, as a settings PATCH
// moves serve's analysisActiveFn.
type gateSwitch struct{ on atomic.Bool }

func newGateSwitch(on bool) *gateSwitch {
	g := &gateSwitch{}
	g.on.Store(on)
	return g
}

func (g *gateSwitch) open() bool { return g.on.Load() }

// holdingRunner records every path it analyses and holds the job for
// `heldPath` until release is closed, so a test can keep the one worker busy
// while it fills the queue behind it.
type holdingRunner struct {
	heldPath string
	started  chan struct{} // closed when the held job is inside the runner
	release  chan struct{}

	mu  sync.Mutex
	ran []string
}

func newHoldingRunner(heldPath string) *holdingRunner {
	return &holdingRunner{heldPath: heldPath, started: make(chan struct{}), release: make(chan struct{})}
}

func (r *holdingRunner) run(ctx context.Context, spec AnalyzeSpec) (Result, error) {
	r.mu.Lock()
	r.ran = append(r.ran, spec.SourceLibraryRel)
	r.mu.Unlock()
	if spec.SourceLibraryRel == r.heldPath {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	return Result{
		WaveformPath:  "/w/" + spec.SourceLibraryRel + ".waveform.bin",
		WaveformTag:   "t-" + spec.SourceLibraryRel,
		WaveformSize:  42,
		SchemaVersion: WaveformSchemaVersion,
	}, nil
}

func (r *holdingRunner) analysed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

// queueBehindAHeldJob starts a one-worker pool under gate, puts heldPath in
// the worker and every path of queued in the queue behind it, and returns
// once all of them are accepted. Every path has a track row, so a job that
// runs writes its analysis row.
func queueBehindAHeldJob(t *testing.T, s *manifest.Store, gate *gateSwitch, runner *holdingRunner, queued []string) *Pool {
	t.Helper()
	putTrack(t, s, runner.heldPath)
	for _, path := range queued {
		putTrack(t, s, path)
	}
	p := NewPool(s, 1, 16, WithRunner(runner.run), WithFsync(noFsync), WithLiveGate(gate.open))
	t.Cleanup(p.Stop)
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: runner.heldPath}); err != nil {
		t.Fatalf("Enqueue(%s): %v", runner.heldPath, err)
	}
	<-runner.started
	for _, path := range queued {
		if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: path}); err != nil {
			t.Fatalf("Enqueue(%s): %v", path, err)
		}
	}
	if got := p.Stats().QueueLen; got != len(queued) {
		t.Fatalf("QueueLen = %d after queueing %d behind the held job, want %d", got, len(queued), len(queued))
	}
	return p
}

// waitIdle waits until no job is queued or running, and checks on the way
// that every snapshot shows each accepted job in exactly one place: in
// flight, done, failed or dropped.
func waitIdle(t *testing.T, p *Pool) PoolStats {
	t.Helper()
	var last PoolStats
	waitFor(t, func() bool {
		last = p.Stats()
		if last.Enqueued != uint64(last.Inflight)+last.Done+last.Failed+last.Dropped {
			t.Fatalf("snapshot %+v: enqueued is not inflight+done+failed+dropped", last)
		}
		return last.Inflight == 0 && last.QueueLen == 0
	})
	return last
}

var gateQueued = []string{"Q/01.flac", "Q/02.flac", "Q/03.flac", "Q/04.flac", "Q/05.flac"}

// TestAQueuedAnalysisDoesNotRunOnceTheGateCloses is B155: five analyses
// queued behind a running one, and analysis switched off before the worker
// reaches them. None of the five may run: each would be a decode, and its
// row an `indexed_at` bump that pushes a delta to every paired device, on a
// bridge whose every surface says analysis is off. The job already running
// when the gate closed finishes (the decision covers queued jobs).
func TestAQueuedAnalysisDoesNotRunOnceTheGateCloses(t *testing.T) {
	s := newStore(t)
	gate := newGateSwitch(true)
	runner := newHoldingRunner("H/held.flac")
	p := queueBehindAHeldJob(t, s, gate, runner, gateQueued)

	gate.on.Store(false)
	close(runner.release)
	st := waitIdle(t, p)

	if got := runner.analysed(); len(got) != 1 || got[0] != runner.heldPath {
		t.Errorf("analysed %v with the gate closed behind the held job, want only [%s]", got, runner.heldPath)
	}
	if st.Done != 1 || st.Failed != 0 || st.Dropped != uint64(len(gateQueued)) {
		t.Errorf("Done=%d Failed=%d Dropped=%d, want Done=1 Failed=0 Dropped=%d: a dropped job is neither done nor failed",
			st.Done, st.Failed, st.Dropped, len(gateQueued))
	}
	for _, path := range gateQueued {
		if row, err := s.GetAnalysis(context.Background(), path); err != nil || row != nil {
			t.Errorf("GetAnalysis(%s) = %v, %v: a dropped job wrote its row", path, row, err)
		}
	}
	if row, err := s.GetAnalysis(context.Background(), runner.heldPath); err != nil || row == nil {
		t.Errorf("GetAnalysis(%s) = %v, %v: the job running when the gate closed must finish", runner.heldPath, row, err)
	}
}

// TestADroppedAnalysisFreesItsPathForTheNextOffer pins the release half: a
// dropped job gives its dedup slot back, so the sweeper's next offer of the
// path, once analysis is on again, is accepted and runs. A drop that kept the
// slot would answer every later offer ErrDuplicateInflight until restart.
func TestADroppedAnalysisFreesItsPathForTheNextOffer(t *testing.T) {
	s := newStore(t)
	gate := newGateSwitch(true)
	runner := newHoldingRunner("H/held.flac")
	p := queueBehindAHeldJob(t, s, gate, runner, gateQueued[:1])

	gate.on.Store(false)
	close(runner.release)
	waitIdle(t, p)

	gate.on.Store(true)
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: gateQueued[0]}); err != nil {
		t.Fatalf("re-offer of the dropped %s: %v, want nil", gateQueued[0], err)
	}
	st := waitIdle(t, p)
	if st.Done != 2 || st.Dropped != 1 {
		t.Errorf("Done=%d Dropped=%d after the re-offer ran, want Done=2 Dropped=1", st.Done, st.Dropped)
	}
	if row, err := s.GetAnalysis(context.Background(), gateQueued[0]); err != nil || row == nil {
		t.Errorf("GetAnalysis(%s) = %v, %v after the re-offer, want its row", gateQueued[0], row, err)
	}
}

// TestAnAnalysisDropBurstLogsOneLineWithItsCount: a switch-off drops a whole
// queue at once, so the pool says so once, with the count, never once per
// job (a 5,000-job queue would be 5,000 lines).
func TestAnAnalysisDropBurstLogsOneLineWithItsCount(t *testing.T) {
	rec := loggingtest.Record(t)
	s := newStore(t)
	gate := newGateSwitch(true)
	runner := newHoldingRunner("H/held.flac")
	p := queueBehindAHeldJob(t, s, gate, runner, gateQueued)

	gate.on.Store(false)
	close(runner.release)
	waitIdle(t, p)

	lines := rec.Lines(dropBurstMsg)
	want := fmt.Sprintf("count=%d", len(gateQueued))
	if len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("drop-burst lines = %q, want one carrying %s", lines, want)
	}
	for _, line := range rec.All() {
		for _, path := range gateQueued {
			if strings.Contains(line, path) {
				t.Errorf("a line names the dropped %s: %q (the burst is one line, not one per job)", path, line)
			}
		}
	}
}

// TestTheGateIsNotAskedForAJobTheStopTook: Stop flips p.closed before it
// cancels anything, and a job a worker takes after that is shutdown's,
// uncounted and unannounced as before. It is no drop, and no burst line.
func TestTheGateIsNotAskedForAJobTheStopTook(t *testing.T) {
	rec := loggingtest.Record(t)
	s := newStore(t)
	var asked atomic.Int32
	gate := func() bool { asked.Add(1); return false }
	release := make(chan struct{})
	started := make(chan struct{})
	runner := func(ctx context.Context, _ AnalyzeSpec) (Result, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{}, errors.New("stopped")
	}
	putTrack(t, s, "H/held.flac")
	putTrack(t, s, "Q/01.flac")
	var opened atomic.Bool
	opened.Store(true)
	p := NewPool(s, 1, 4, WithRunner(runner), WithFsync(noFsync), WithLiveGate(func() bool {
		if opened.Load() {
			return true
		}
		return gate()
	}))
	t.Cleanup(p.Stop)
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "H/held.flac"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "Q/01.flac"}); err != nil {
		t.Fatal(err)
	}
	opened.Store(false)
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	waitFor(t, func() bool { return p.closed.Load() })
	close(release)
	<-stopped

	if n := asked.Load(); n != 0 {
		t.Errorf("the gate was asked %d times for a job the stop took, want 0", n)
	}
	if st := p.Stats(); st.Dropped != 0 || st.Failed != 0 || st.Done != 0 {
		t.Errorf("Stats after Stop = %+v, want nothing done, failed or dropped", st)
	}
	if lines := rec.Lines(dropBurstMsg); len(lines) != 0 {
		t.Errorf("drop-burst lines at shutdown = %q, want none", lines)
	}
}
