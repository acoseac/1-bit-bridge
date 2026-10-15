package transcode

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// The serve-side pool is built on every bridge and never stopped (#781), so a
// switch-off reaches it only through the live gate a worker asks when it
// takes a job (backlog B155). These tests stand in for serve's gate with one
// they close while jobs wait in the queue.

// testGate is a live gate a test closes per kind, or for every job, and that
// records each job it was asked about.
type testGate struct {
	mu    sync.Mutex
	all   bool             // every gate closed
	shut  map[JobKind]bool // one kind's gate closed
	only  map[string]bool  // when set, the gate admits these paths alone
	asked []JobSpec
}

func newTestGate() *testGate { return &testGate{shut: map[JobKind]bool{}} }

func (g *testGate) admits(spec JobSpec) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, spec)
	if g.only != nil {
		return g.only[spec.SourceLibraryRel]
	}
	return !g.all && !g.shut[spec.Kind]
}

func (g *testGate) closeAll() {
	g.mu.Lock()
	g.all = true
	g.mu.Unlock()
}

func (g *testGate) openAll() {
	g.mu.Lock()
	g.all = false
	g.mu.Unlock()
}

func (g *testGate) closeKind(k JobKind) {
	g.mu.Lock()
	g.shut[k] = true
	g.mu.Unlock()
}

func (g *testGate) askedPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.asked))
	for _, s := range g.asked {
		out = append(out, s.SourceLibraryRel+"|"+string(s.Kind))
	}
	return out
}

// holdingRunner records every job it runs and holds the one for heldPath
// until release is closed (or the pool stops), so a test keeps a worker busy
// while it fills the queue behind it.
type holdingRunner struct {
	heldPath string
	started  chan struct{}
	release  chan struct{}

	mu  sync.Mutex
	ran []string
}

func newHoldingRunner(heldPath string) *holdingRunner {
	return &holdingRunner{heldPath: heldPath, started: make(chan struct{}), release: make(chan struct{})}
}

func (r *holdingRunner) run(ctx context.Context, spec JobSpec) (RunResult, error) {
	r.mu.Lock()
	r.ran = append(r.ran, spec.SourceLibraryRel)
	r.mu.Unlock()
	if spec.SourceLibraryRel == r.heldPath {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}
	return RunResult{SizeBytes: 10}, nil
}

func (r *holdingRunner) rendered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

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

func gateSpec(t *testing.T, rel string, kind JobKind, outDir string) JobSpec {
	t.Helper()
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

var gateQueued = []string{"Q/01.flac", "Q/02.flac", "Q/03.flac", "Q/04.flac", "Q/05.flac"}

// queueBehindAHeldRender starts a one-worker pool under gate, puts heldPath
// in the worker and every path of queued (as upscale jobs) in the queue
// behind it, and returns once all are accepted. Every path has a track row,
// so a job that runs writes its variant row.
func queueBehindAHeldRender(t *testing.T, gate *testGate, runner *holdingRunner, queued []string) (*Pool, *manifest.Store, *poolEvents, string) {
	t.Helper()
	store := openTempStoreForPool(t)
	seedTrackForPool(t, store, runner.heldPath)
	for _, path := range queued {
		seedTrackForPool(t, store, path)
	}
	p := NewPool(store, 1, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = runner.run
	p.SetLiveGate(gate.admits)
	events := &poolEvents{}
	events.wire(p)
	outDir := t.TempDir()
	if err := p.Enqueue(gateSpec(t, runner.heldPath, JobKindUpscale, outDir)); err != nil {
		t.Fatalf("Enqueue(%s): %v", runner.heldPath, err)
	}
	<-runner.started
	for _, path := range queued {
		if err := p.Enqueue(gateSpec(t, path, JobKindUpscale, outDir)); err != nil {
			t.Fatalf("Enqueue(%s): %v", path, err)
		}
	}
	if got := p.Stats().QueueLen; got != len(queued) {
		t.Fatalf("QueueLen = %d after queueing %d behind the held job, want %d", got, len(queued), len(queued))
	}
	return p, store, events, outDir
}

// waitPoolIdle waits until no job is queued or running, and checks on the way
// that every snapshot shows each accepted job in exactly one place: in
// flight, done, failed or dropped.
func waitPoolIdle(t *testing.T, p *Pool) PoolStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := p.Stats()
		if st.Enqueued != uint64(st.Inflight)+st.Done+st.Failed+st.Dropped {
			t.Fatalf("snapshot %+v: enqueued is not inflight+done+failed+dropped", st)
		}
		if st.Inflight == 0 && st.QueueLen == 0 {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still busy after 10s: %+v", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAQueuedRenderDoesNotRunOnceItsGateCloses is B155 for the transcode
// pool: five renders queued behind a running one, and upscaling switched off
// before the worker reaches them. None of the five may run, each a sox render
// and a sidecar on disk, and none is counted or announced as done or failed:
// each is announced as dropped, which is how a batch learns it. The render
// already running when the gate closed finishes.
func TestAQueuedRenderDoesNotRunOnceItsGateCloses(t *testing.T) {
	gate := newTestGate()
	runner := newHoldingRunner("H/held.flac")
	p, store, events, _ := queueBehindAHeldRender(t, gate, runner, gateQueued)

	gate.closeAll()
	close(runner.release)
	st := waitPoolIdle(t, p)
	p.Stop() // drains the publisher, so every announcement has landed

	if got := runner.rendered(); len(got) != 1 || got[0] != runner.heldPath {
		t.Errorf("rendered %v with the gate closed behind the held job, want only [%s]", got, runner.heldPath)
	}
	if st.Done != 1 || st.Failed != 0 || st.Dropped != uint64(len(gateQueued)) {
		t.Errorf("Done=%d Failed=%d Dropped=%d, want Done=1 Failed=0 Dropped=%d: a dropped job is neither done nor failed",
			st.Done, st.Failed, st.Dropped, len(gateQueued))
	}
	completed, failed, dropped := events.snapshot()
	if !slices.Equal(completed, []string{runner.heldPath}) || len(failed) != 0 {
		t.Errorf("announced complete %v, failed %v; want complete [%s] and no failure", completed, failed, runner.heldPath)
	}
	slices.Sort(dropped)
	if !slices.Equal(dropped, gateQueued) {
		t.Errorf("announced dropped %v, want %v: a batch learns its job was dropped from this event", dropped, gateQueued)
	}
	variantID := gateSpec(t, gateQueued[0], JobKindUpscale, "").VariantID()
	for _, path := range gateQueued {
		if row, err := store.LookupVariant(context.Background(), path, variantID); err != nil || row != nil {
			t.Errorf("LookupVariant(%s) = %v, %v: a dropped job wrote its rendition", path, row, err)
		}
	}
	if row, err := store.LookupVariant(context.Background(), runner.heldPath, variantID); err != nil || row == nil {
		t.Errorf("LookupVariant(%s) = %v, %v: the render running when the gate closed must finish", runner.heldPath, row, err)
	}
}

// TestTheLiveGateIsAskedForEachJobItTakes: the gate decides per job, from the
// job itself, so switching one kind off (the CarPlay switch) drops that
// kind's queued jobs and runs the rest.
func TestTheLiveGateIsAskedForEachJobItTakes(t *testing.T) {
	gate := newTestGate()
	runner := newHoldingRunner("H/held.flac")
	p, store, events, outDir := queueBehindAHeldRender(t, gate, runner, []string{"Q/up.flac"})
	seedTrackForPool(t, store, "Q/opt.flac")
	if err := p.Enqueue(gateSpec(t, "Q/opt.flac", JobKindOptimize, outDir)); err != nil {
		t.Fatalf("Enqueue(Q/opt.flac): %v", err)
	}

	gate.closeKind(JobKindOptimize)
	close(runner.release)
	st := waitPoolIdle(t, p)
	p.Stop()

	if got := runner.rendered(); !slices.Equal(got, []string{"H/held.flac", "Q/up.flac"}) {
		t.Errorf("rendered %v, want [H/held.flac Q/up.flac]: only the optimize kind is switched off", got)
	}
	if st.Done != 2 || st.Dropped != 1 {
		t.Errorf("Done=%d Dropped=%d, want Done=2 Dropped=1", st.Done, st.Dropped)
	}
	if _, _, dropped := events.snapshot(); !slices.Equal(dropped, []string{"Q/opt.flac"}) {
		t.Errorf("announced dropped %v, want [Q/opt.flac]", dropped)
	}
	asked := gate.askedPaths()
	slices.Sort(asked)
	if want := []string{"H/held.flac|upscale", "Q/opt.flac|optimize", "Q/up.flac|upscale"}; !slices.Equal(asked, want) {
		t.Errorf("the gate was asked about %v, want each job once, with its own spec: %v", asked, want)
	}
}

// TestADroppedRenderFreesItsPathForTheNextOffer pins the release half: a
// dropped job gives its (source, variant) slot back, so the next request or
// sweep for it, once the feature is on again, is accepted and runs.
func TestADroppedRenderFreesItsPathForTheNextOffer(t *testing.T) {
	gate := newTestGate()
	runner := newHoldingRunner("H/held.flac")
	p, store, _, outDir := queueBehindAHeldRender(t, gate, runner, gateQueued[:1])

	gate.closeAll()
	close(runner.release)
	waitPoolIdle(t, p)

	gate.openAll()
	spec := gateSpec(t, gateQueued[0], JobKindUpscale, outDir)
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("re-offer of the dropped %s: %v, want nil", gateQueued[0], err)
	}
	st := waitPoolIdle(t, p)
	if st.Done != 2 || st.Dropped != 1 {
		t.Errorf("Done=%d Dropped=%d after the re-offer ran, want Done=2 Dropped=1", st.Done, st.Dropped)
	}
	if row, err := store.LookupVariant(context.Background(), gateQueued[0], spec.VariantID()); err != nil || row == nil {
		t.Errorf("LookupVariant(%s) = %v, %v after the re-offer, want its row", gateQueued[0], row, err)
	}
}

// TestARenderDropBurstLogsOneLineWithItsCount: a switch-off drops a whole
// queue at once, so the pool says so once, with the count, never once per
// job.
func TestARenderDropBurstLogsOneLineWithItsCount(t *testing.T) {
	rec := loggingtest.Record(t)
	gate := newTestGate()
	runner := newHoldingRunner("H/held.flac")
	p, _, _, _ := queueBehindAHeldRender(t, gate, runner, gateQueued)

	gate.closeAll()
	close(runner.release)
	waitPoolIdle(t, p)

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

// TestARenderDropBurstAcrossWorkersIsOneLine: four workers share one queue
// of 200 dropped at a switch-off, and it is one burst, one line, with every
// job counted once. When a burst ends is TestARenderStillBeingDecidedKeepsItsBurstOpen's.
func TestARenderDropBurstAcrossWorkersIsOneLine(t *testing.T) {
	rec := loggingtest.Record(t)
	store := openTempStoreForPool(t)
	const workers, queued = 4, 200
	gate := newTestGate()
	started := make(chan struct{}, workers)
	release := make(chan struct{})
	p := NewPool(store, workers, queued+workers)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		if strings.HasPrefix(spec.SourceLibraryRel, "H/") {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return RunResult{}, ctx.Err()
			}
		}
		return RunResult{SizeBytes: 10}, nil
	}
	p.SetLiveGate(gate.admits)
	outDir := t.TempDir()
	for i := 0; i < workers; i++ {
		path := fmt.Sprintf("H/%d.flac", i)
		seedTrackForPool(t, store, path)
		if err := p.Enqueue(gateSpec(t, path, JobKindUpscale, outDir)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < workers; i++ {
		<-started
	}
	for i := 0; i < queued; i++ {
		if err := p.Enqueue(gateSpec(t, fmt.Sprintf("Q/%03d.flac", i), JobKindUpscale, outDir)); err != nil {
			t.Fatal(err)
		}
	}
	gate.closeAll()
	close(release)
	st := waitPoolIdle(t, p)

	if st.Done != workers || st.Dropped != queued {
		t.Errorf("Done=%d Dropped=%d, want Done=%d Dropped=%d", st.Done, st.Dropped, workers, queued)
	}
	lines := rec.Lines(dropBurstMsg)
	if want := fmt.Sprintf("count=%d", queued); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("drop-burst lines = %q, want one carrying %s", lines, want)
	}
}

// TestARenderStillBeingDecidedKeepsItsBurstOpen pins when a burst ends: once
// no accepted job is left undecided. The first queued job is parked inside the
// gate, taken and not yet decided, while the other worker drops the other nine
// and empties the queue. A burst that ended at an empty queue would log its
// line at that last drop, and the parked job's drop a second line.
func TestARenderStillBeingDecidedKeepsItsBurstOpen(t *testing.T) {
	rec := loggingtest.Record(t)
	store := openTempStoreForPool(t)
	const workers, queued = 2, 10
	started := make(chan struct{}, workers)
	release := make(chan struct{})
	p := NewPool(store, workers, queued+workers)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		if strings.HasPrefix(spec.SourceLibraryRel, "H/") {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return RunResult{}, ctx.Err()
			}
		}
		return RunResult{SizeBytes: 10}, nil
	}
	parked := make(chan struct{})
	unpark := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unpark:
		default:
			close(unpark)
		}
	})
	p.SetLiveGate(func(spec JobSpec) bool {
		if strings.HasPrefix(spec.SourceLibraryRel, "H/") {
			return true
		}
		if spec.SourceLibraryRel == "Q/00.flac" {
			close(parked)
			<-unpark
		}
		return false
	})
	outDir := t.TempDir()
	for i := 0; i < workers; i++ {
		path := fmt.Sprintf("H/%d.flac", i)
		seedTrackForPool(t, store, path)
		if err := p.Enqueue(gateSpec(t, path, JobKindUpscale, outDir)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < workers; i++ {
		<-started
	}
	for i := 0; i < queued; i++ {
		if err := p.Enqueue(gateSpec(t, fmt.Sprintf("Q/%02d.flac", i), JobKindUpscale, outDir)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate was never asked about the first queued job")
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.Stats().Dropped != queued-1 {
		if time.Now().After(deadline) {
			t.Fatalf("Dropped = %d after 10s, want %d", p.Stats().Dropped, queued-1)
		}
		time.Sleep(time.Millisecond)
	}
	if st := p.Stats(); st.QueueLen != 0 {
		t.Fatalf("QueueLen = %d once the other worker dropped the rest, want 0", st.QueueLen)
	}
	if lines := rec.Lines(dropBurstMsg); len(lines) != 0 {
		t.Errorf("drop-burst lines while a taken job is undecided = %q, want none yet", lines)
	}
	close(unpark)
	waitPoolIdle(t, p)
	lines := rec.Lines(dropBurstMsg)
	if want := fmt.Sprintf("count=%d", queued); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("drop-burst lines = %q, want one carrying %s", lines, want)
	}
}

// TestTheGateIsNotAskedForARenderTheStopTook: a job a worker takes after Stop
// flipped p.closed is shutdown's, uncounted and unannounced as before: no
// drop, no burst line, and the gate is not asked.
func TestTheGateIsNotAskedForARenderTheStopTook(t *testing.T) {
	rec := loggingtest.Record(t)
	store := openTempStoreForPool(t)
	var asked atomic.Int32
	var opened atomic.Bool
	opened.Store(true)
	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	release := make(chan struct{})
	started := make(chan struct{})
	p.runner = func(ctx context.Context, _ JobSpec) (RunResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return RunResult{}, errors.New("stopped")
	}
	p.SetLiveGate(func(JobSpec) bool {
		if opened.Load() {
			return true
		}
		asked.Add(1)
		return false
	})
	events := &poolEvents{}
	events.wire(p)
	outDir := t.TempDir()
	if err := p.Enqueue(gateSpec(t, "H/held.flac", JobKindUpscale, outDir)); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Enqueue(gateSpec(t, "Q/01.flac", JobKindUpscale, outDir)); err != nil {
		t.Fatal(err)
	}
	opened.Store(false)
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	deadline := time.Now().Add(10 * time.Second)
	for !p.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Stop never flipped p.closed")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-stopped

	if n := asked.Load(); n != 0 {
		t.Errorf("the gate was asked %d times for a job the stop took, want 0", n)
	}
	if st := p.Stats(); st.Dropped != 0 || st.Failed != 0 || st.Done != 0 {
		t.Errorf("Stats after Stop = %+v, want nothing done, failed or dropped", st)
	}
	if _, _, dropped := events.snapshot(); len(dropped) != 0 {
		t.Errorf("announced dropped %v at shutdown, want none", dropped)
	}
	if lines := rec.Lines(dropBurstMsg); len(lines) != 0 {
		t.Errorf("drop-burst lines at shutdown = %q, want none", lines)
	}
}
