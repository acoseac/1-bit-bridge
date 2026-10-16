// Package pooltest holds what the two job pools' live-gate tests share
// (internal/analyze and internal/transcode, backlog B155): a worker held on a
// job, a queue behind it, a gate switched off, the pool waited on until it is
// idle, and the one line a run of drops writes. Like net/http/httptest it is
// imported only by tests, so none of it reaches the binary.
//
// It exists so there is ONE copy of each scenario. Each pool's tests stay in
// that pool's package, under their own names: a pool's test file adapts its
// pool to Start, and asserts what only that pool does (the rows it writes,
// the events it sends) beside the checks here.
package pooltest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// waitBound bounds every wait here: a pool of stub runners settles in
// milliseconds, so a scenario still waiting after it is red, not slow.
const waitBound = 10 * time.Second

// errStopped is how StopTakesNoDecision's held job ends.
var errStopped = errors.New("held until the pool stopped")

// Stats is what a scenario reads of a pool's snapshot: the fields both
// pools' PoolStats carry.
type Stats struct {
	QueueLen, Inflight              int
	Enqueued, Done, Failed, Dropped uint64
}

// Pool is one job pool, as a scenario drives it.
type Pool struct {
	// Enqueue offers the pool the job for path, as the pool's own spec, with
	// the track row a run of it writes against.
	Enqueue func(path string) error
	Stats   func() Stats
	Stop    func()
	// Closed reports whether Stop has flipped the pool's closed flag.
	Closed func() bool
}

// Start builds a pool of workers with a queue of queueCap, whose runner
// calls run for each job it runs and fails the job with run's error, and
// whose live gate answers gate for each job a worker takes. The pool is
// stopped when the test ends.
type Start func(t *testing.T, workers, queueCap int, run func(ctx context.Context, path string) error, gate func() bool) Pool

// Switch is a live gate a test opens and closes, as a settings PATCH moves
// serve's.
type Switch struct{ on atomic.Bool }

// NewSwitch returns a gate that starts open or closed.
func NewSwitch(open bool) *Switch {
	s := &Switch{}
	s.on.Store(open)
	return s
}

// Open is the gate's answer.
func (s *Switch) Open() bool { return s.on.Load() }

// Set opens or closes the gate.
func (s *Switch) Set(open bool) { s.on.Store(open) }

// Holder is a runner's core. It records the path of every job it runs, and
// holds each job whose path held names until Release, or until the job's
// context ends.
type Holder struct {
	held     func(path string) bool
	started  chan struct{}
	release  chan struct{}
	released sync.Once

	mu  sync.Mutex
	ran []string
}

// NewHolder returns a Holder that holds the jobs held names.
func NewHolder(held func(path string) bool) *Holder {
	return &Holder{held: held, started: make(chan struct{}, 1024), release: make(chan struct{})}
}

// HeldUnder names every path under prefix.
func HeldUnder(prefix string) func(path string) bool {
	return func(path string) bool { return strings.HasPrefix(path, prefix) }
}

// Run is the runner's step: record path, and hold it if it is held.
func (h *Holder) Run(ctx context.Context, path string) error {
	h.mu.Lock()
	h.ran = append(h.ran, path)
	h.mu.Unlock()
	if !h.held(path) {
		return nil
	}
	h.started <- struct{}{}
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitHeld waits until n held jobs are inside the runner.
func (h *Holder) WaitHeld(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(waitBound)
	for i := 0; i < n; i++ {
		select {
		case <-h.started:
		case <-deadline:
			t.Fatalf("%d of %d held jobs reached the runner within %s", i, n, waitBound)
		}
	}
}

// Release lets every held job go, now and after.
func (h *Holder) Release() { h.released.Do(func() { close(h.release) }) }

// Ran returns the paths run so far, in the order they ran.
func (h *Holder) Ran() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ran...)
}

// WaitIdle waits until no job is queued or running, and checks on the way
// that every snapshot shows each accepted job in exactly one place: in
// flight, done, failed or dropped.
func WaitIdle(t *testing.T, stats func() Stats) Stats {
	t.Helper()
	deadline := time.Now().Add(waitBound)
	for {
		st := stats()
		if st.Enqueued != uint64(st.Inflight)+st.Done+st.Failed+st.Dropped {
			t.Fatalf("snapshot %+v: enqueued is not inflight+done+failed+dropped", st)
		}
		if st.Inflight == 0 && st.QueueLen == 0 {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still busy after %s: %+v", waitBound, st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// RequireOneRunLine fails unless rec holds exactly one msg line, carrying
// count, and no line names any of paths: a run of drops is one line, never
// one per job.
func RequireOneRunLine(t *testing.T, rec *loggingtest.Recorder, msg string, count int, paths []string) {
	t.Helper()
	lines := rec.Lines(msg)
	if want := fmt.Sprintf("count=%d", count); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("drop-run lines = %q, want one carrying %s", lines, want)
	}
	for _, line := range rec.All() {
		for _, path := range paths {
			if strings.Contains(line, path) {
				t.Errorf("a line names the dropped %s: %q (a run is one line, not one per job)", path, line)
			}
		}
	}
}

// HeldPath is the job the scenarios hold in a worker.
const HeldPath = "H/held.flac"

// Queued is the queue the scenarios put behind the held job.
var Queued = []string{"Q/01.flac", "Q/02.flac", "Q/03.flac", "Q/04.flac", "Q/05.flac"}

// HeldQueue is a one-worker pool with the job for HeldPath held in its
// worker and a queue behind it, under a gate that is open.
type HeldQueue struct {
	Pool   Pool
	Gate   *Switch
	Holder *Holder
}

// QueueBehindAHeldJob builds a HeldQueue with queued behind the held job,
// and returns once the pool has accepted every one of them.
func QueueBehindAHeldJob(t *testing.T, start Start, queued []string) *HeldQueue {
	t.Helper()
	q := &HeldQueue{Gate: NewSwitch(true), Holder: NewHolder(func(path string) bool { return path == HeldPath })}
	q.Pool = start(t, 1, 16, q.Holder.Run, q.Gate.Open)
	if err := q.Pool.Enqueue(HeldPath); err != nil {
		t.Fatalf("Enqueue(%s): %v", HeldPath, err)
	}
	q.Holder.WaitHeld(t, 1)
	for _, path := range queued {
		if err := q.Pool.Enqueue(path); err != nil {
			t.Fatalf("Enqueue(%s): %v", path, err)
		}
	}
	if got := q.Pool.Stats().QueueLen; got != len(queued) {
		t.Fatalf("QueueLen = %d after queueing %d behind the held job, want %d", got, len(queued), len(queued))
	}
	return q
}

// SwitchOff closes the gate, lets the held job go, and waits until the pool
// is idle.
func (q *HeldQueue) SwitchOff(t *testing.T) Stats {
	t.Helper()
	q.Gate.Set(false)
	q.Holder.Release()
	return WaitIdle(t, q.Pool.Stats)
}

// RequireOnlyTheHeldJobRan fails unless the job running when the gate closed
// is the one job that ran, done, and the dropped jobs are counted as neither
// done nor failed.
func (q *HeldQueue) RequireOnlyTheHeldJobRan(t *testing.T, st Stats, dropped int) {
	t.Helper()
	if got := q.Holder.Ran(); len(got) != 1 || got[0] != HeldPath {
		t.Errorf("ran %v with the gate closed behind the held job, want only [%s]", got, HeldPath)
	}
	if st.Done != 1 || st.Failed != 0 || st.Dropped != uint64(dropped) {
		t.Errorf("Done=%d Failed=%d Dropped=%d, want Done=1 Failed=0 Dropped=%d: a dropped job is neither done nor failed",
			st.Done, st.Failed, st.Dropped, dropped)
	}
}

// ReofferAfterTheDrop opens the gate again and offers the dropped path: the
// pool must accept it, since its drop gave the path back, and run it.
func (q *HeldQueue) ReofferAfterTheDrop(t *testing.T, path string) Stats {
	t.Helper()
	q.Gate.Set(true)
	if err := q.Pool.Enqueue(path); err != nil {
		t.Fatalf("re-offer of the dropped %s: %v, want nil", path, err)
	}
	st := WaitIdle(t, q.Pool.Stats)
	if st.Done != 2 || st.Dropped != 1 {
		t.Errorf("Done=%d Dropped=%d after the re-offer ran, want Done=2 Dropped=1", st.Done, st.Dropped)
	}
	return st
}

// heldWorkers starts a pool of workers, holds one job in each, and queues
// queued jobs behind them, under gate.
func heldWorkers(t *testing.T, start Start, workers, queued int, gate func() bool) (Pool, *Holder) {
	t.Helper()
	h := NewHolder(HeldUnder("H/"))
	p := start(t, workers, queued+workers, h.Run, gate)
	for i := 0; i < workers; i++ {
		if err := p.Enqueue(fmt.Sprintf("H/%d.flac", i)); err != nil {
			t.Fatal(err)
		}
	}
	h.WaitHeld(t, workers)
	for i := 0; i < queued; i++ {
		if err := p.Enqueue(fmt.Sprintf("Q/%03d.flac", i)); err != nil {
			t.Fatal(err)
		}
	}
	return p, h
}

// DropRunAcrossWorkers: four workers share one queue of 200, dropped at a
// switch-off, and it is one run, one line, with every job counted once. It
// does not decide WHEN a run ends (the first worker back usually drops most
// of the queue alone, while the others finish their held jobs);
// UndecidedJobKeepsItsRunOpen does.
func DropRunAcrossWorkers(t *testing.T, start Start, msg string) {
	t.Helper()
	rec := loggingtest.Record(t)
	const workers, queued = 4, 200
	gate := NewSwitch(true)
	p, h := heldWorkers(t, start, workers, queued, gate.Open)
	gate.Set(false)
	h.Release()
	st := WaitIdle(t, p.Stats)
	if st.Done != workers || st.Dropped != queued {
		t.Errorf("Done=%d Dropped=%d, want Done=%d Dropped=%d", st.Done, st.Dropped, workers, queued)
	}
	RequireOneRunLine(t, rec, msg, queued, nil)
}

// UndecidedJobKeepsItsRunOpen pins when a run of drops ends: once no
// accepted job is left undecided. The first queued job is parked inside the
// gate, taken from the queue and not yet decided, while the other worker
// drops the other nine and empties the queue. A run that ended at an empty
// queue would write its line at that last drop, and the parked job's drop a
// second line; this one writes nothing until the parked job is decided, and
// then one line with every drop.
//
// The gate answers in take order: open for the two held jobs, which a
// worker takes before the queue exists, closed for the queue, and the first
// queued job's ask parks until the scenario lets it go.
func UndecidedJobKeepsItsRunOpen(t *testing.T, start Start, msg string) {
	t.Helper()
	rec := loggingtest.Record(t)
	const workers, queued = 2, 10
	var calls atomic.Int32
	parked := make(chan struct{})
	unpark := make(chan struct{})
	var unparked sync.Once
	letGo := func() { unparked.Do(func() { close(unpark) }) }
	t.Cleanup(letGo)
	gate := func() bool {
		n := calls.Add(1)
		if n <= workers {
			return true
		}
		if n == workers+1 {
			close(parked)
			<-unpark
		}
		return false
	}
	p, h := heldWorkers(t, start, workers, queued, gate)
	h.Release()
	select {
	case <-parked:
	case <-time.After(waitBound):
		t.Fatal("the gate was never asked about the first queued job")
	}
	deadline := time.Now().Add(waitBound)
	for p.Stats().Dropped != queued-1 {
		if time.Now().After(deadline) {
			t.Fatalf("Dropped = %d after %s, want %d", p.Stats().Dropped, waitBound, queued-1)
		}
		time.Sleep(time.Millisecond)
	}
	if st := p.Stats(); st.QueueLen != 0 {
		t.Fatalf("QueueLen = %d once the other worker dropped the rest, want 0", st.QueueLen)
	}
	if lines := rec.Lines(msg); len(lines) != 0 {
		t.Errorf("drop-run lines while a taken job is undecided = %q, want none yet", lines)
	}
	letGo()
	WaitIdle(t, p.Stats)
	RequireOneRunLine(t, rec, msg, queued, nil)
}

// StopTakesNoDecision: a job a worker takes after Stop flipped the pool's
// closed flag is shutdown's, uncounted and unannounced as before. It is no
// drop and no line, and the gate is not asked about it. It returns once Stop
// has, so the caller can check what its pool announced.
func StopTakesNoDecision(t *testing.T, start Start, msg string) {
	t.Helper()
	rec := loggingtest.Record(t)
	var asked atomic.Int32
	var open atomic.Bool
	open.Store(true)
	gate := func() bool {
		if open.Load() {
			return true
		}
		asked.Add(1)
		return false
	}
	h := NewHolder(func(path string) bool { return path == HeldPath })
	// The held job fails once it is let go: a failure the pool was stopping
	// under is uncounted whenever it lands, where a success that commits
	// before Stop cancels the job's context counts as done.
	run := func(ctx context.Context, path string) error {
		if err := h.Run(ctx, path); err != nil {
			return err
		}
		return errStopped
	}
	p := start(t, 1, 4, run, gate)
	if err := p.Enqueue(HeldPath); err != nil {
		t.Fatal(err)
	}
	h.WaitHeld(t, 1)
	if err := p.Enqueue(Queued[0]); err != nil {
		t.Fatal(err)
	}
	open.Store(false)
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	deadline := time.Now().Add(waitBound)
	for !p.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("Stop never flipped the pool's closed flag")
		}
		time.Sleep(time.Millisecond)
	}
	h.Release()
	<-stopped

	if n := asked.Load(); n != 0 {
		t.Errorf("the gate was asked %d times for a job the stop took, want 0", n)
	}
	if st := p.Stats(); st.Dropped != 0 || st.Failed != 0 || st.Done != 0 {
		t.Errorf("Stats after Stop = %+v, want nothing done, failed or dropped", st)
	}
	if lines := rec.Lines(msg); len(lines) != 0 {
		t.Errorf("drop-run lines at shutdown = %q, want none", lines)
	}
}
