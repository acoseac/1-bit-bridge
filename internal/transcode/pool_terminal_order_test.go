package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// terminalOrderRel is the one source every test in this file converts. A
// fixed path keeps each retry on the same dedup key as the job before it.
const terminalOrderRel = "Music/Album/01.flac"

func terminalOrderSpec(t *testing.T) JobSpec {
	t.Helper()
	return JobSpec{
		SourceLibraryRel: terminalOrderRel,
		SourceAbsPath:    "/dev/null/missing", // every runner here is a stub
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
}

// newTerminalOrderPool opens a store and a one-worker pool over it. seed
// writes the track row; the store exit needs it ABSENT, because the variant
// row's foreign key is how that exit is reached.
func newTerminalOrderPool(t *testing.T, seed bool) (*Pool, *manifest.Store) {
	t.Helper()
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	if seed {
		seedTrackForPool(t, store, terminalOrderRel)
	}
	return NewPool(store, 1, 4), store
}

// writeSidecarRunner stands in for a sox run that got as far as renaming its
// output into place, so the fsync and store exits have a real file to remove.
func writeSidecarRunner(_ context.Context, spec JobSpec) (RunResult, error) {
	path := spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return RunResult{}, err
	}
	if err := os.WriteFile(path, []byte("fLaC"), 0o644); err != nil {
		return RunResult{}, err
	}
	return RunResult{SizeBytes: 4}, nil
}

// waitForTimeoutRunner fails the way a hung sox does: it outlives the per-job
// deadline.
func waitForTimeoutRunner(ctx context.Context, _ JobSpec) (RunResult, error) {
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

func failingFsync(string) error { return errors.New("synthetic EIO at fsync") }

// parkedExit is one failure exit of processJob that logs before it finishes.
// The worker is parked in logMsg, the exit's own line.
type parkedExit struct {
	name    string
	logMsg  string
	seed    bool
	runner  func(context.Context, JobSpec) (RunResult, error)
	fsync   func(string) error
	timeout time.Duration
	// landed checks the exit's bookkeeping once its failure is counted.
	landed func(t *testing.T, store *manifest.Store, spec JobSpec)
}

// TestACountedTranscodeFailureHasAlreadyReleasedItsPath pins what a failure
// count means in the transcode pool: the job is FINISHED. Its own bookkeeping
// has landed and its path is free, so a retry sent the moment the count moves
// is accepted. Along the way, every snapshot shows each accepted job in exactly
// one place: in flight, done or failed.
//
// The analysis pool had this defect and #987 fixed it there. This pool had the
// same order on every exit: it counted, then did the exit's bookkeeping (the
// strike and its WARN, or the orphan sidecar's removal), and gave the path back
// last. Enqueue answers a held path with ErrDuplicateInflight, and the batch
// Coordinator drops a path that answers that from the batch it is building, so
// a retry landing in the window was dropped from its own batch.
//
// Each case parks the worker in its exit's own log line, the one step of the
// exit a test can hold with no hook in production code. The success exit logs
// nothing and has no case here;
// TestNothingIsCountedOrAnnouncedWhileAJobStillHoldsItsPath covers every exit,
// that one included.
func TestACountedTranscodeFailureHasAlreadyReleasedItsPath(t *testing.T) {
	for _, tc := range []parkedExit{
		{
			name:   "sox failed",
			logMsg: "pool: sox failed",
			seed:   true,
			runner: failRunner(errors.New("sox FAIL formats: can't open input file")),
			fsync:  noopFsync,
			landed: requireStrikes(1),
		},
		{
			name:    "sox timed out",
			logMsg:  "pool: sox timed out",
			seed:    true,
			runner:  waitForTimeoutRunner,
			fsync:   noopFsync,
			timeout: 20 * time.Millisecond,
			landed:  requireStrikes(0),
		},
		{
			name:   "fsync failed",
			logMsg: "pool: fsync sidecar",
			seed:   true,
			runner: writeSidecarRunner,
			fsync:  failingFsync,
			landed: requireSidecarGone,
		},
		{
			name:   "store failed",
			logMsg: "pool: store variant",
			runner: writeSidecarRunner,
			fsync:  noopFsync,
			landed: requireSidecarGone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runParkedExit(t, tc) })
	}
}

func runParkedExit(t *testing.T, tc parkedExit) {
	park := loggingtest.ParkOn(t, tc.logMsg)
	p, store := newTerminalOrderPool(t, tc.seed)
	p.runner = tc.runner
	p.fsyncFn = tc.fsync
	if tc.timeout > 0 {
		p.jobTimeout = tc.timeout
	}
	// Deferred in this order so the worker is let go BEFORE Stop waits for
	// it, on every way out of the test, a failed assertion included.
	defer p.Stop()
	defer park.Release()

	spec := terminalOrderSpec(t)
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	park.Wait(t)
	requireStillRunning(t, p, spec, tc.logMsg)
	park.Release()

	settleTerminal(t, p, func(st PoolStats) bool { return st.Failed == 1 })
	tc.landed(t, store, spec)
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("a retry sent on Failed == 1 returned %v, want it accepted: "+
			"the job had already been counted, so its path must already be free", err)
	}
	settleTerminal(t, p, func(st PoolStats) bool { return st.Failed == 2 })
}

// requireStillRunning is the parked check. The job is still doing its own
// bookkeeping, so it is still running: in flight, and not counted.
func requireStillRunning(t *testing.T, p *Pool, spec JobSpec, logMsg string) {
	t.Helper()
	st := p.Stats()
	if st.Inflight == 1 && st.Done+st.Failed == 0 {
		return
	}
	// Show what a retry gets at this point before failing. Counted early, it
	// is refused as a duplicate of a job the count calls finished; released
	// early, a second attempt is admitted while the first is still writing its
	// strike or removing its sidecar.
	retryErr := p.Enqueue(spec)
	t.Fatalf("parked in its own %q line, the job reads Inflight=%d Done=%d Failed=%d, want 1/0/0: "+
		"it is still doing its own bookkeeping, so it must be in flight and uncounted. "+
		"A retry sent at this moment returned %v",
		logMsg, st.Inflight, st.Done, st.Failed, retryErr)
}

// settleTerminal waits for cond and checks, on every poll rather than only at
// the end, that each accepted job is exactly one of in flight, done or failed:
// never both, never neither.
func settleTerminal(t *testing.T, p *Pool, cond func(PoolStats) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := p.Stats()
		if st.Enqueued != uint64(st.Inflight)+st.Done+st.Failed {
			t.Fatalf("Stats() = %+v: a job is counted while still in flight, or is in neither place", st)
		}
		if cond(st) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within 5s; last snapshot %+v", st)
		}
		time.Sleep(time.Millisecond)
	}
}

// requireStrikes is the runner-error exits' bookkeeping, checked once the
// failure is counted: clearing the album's strikes (the operator's retry
// action) finds want of them. A sox failure's strike lands before its count;
// a timeout, which says as much about a hung mount as about the source,
// records none.
func requireStrikes(want int64) func(*testing.T, *manifest.Store, JobSpec) {
	return func(t *testing.T, store *manifest.Store, _ JobSpec) {
		t.Helper()
		n, err := store.ClearVariantFailuresUnderPrefix(context.Background(), "Music/Album")
		if err != nil || n != want {
			t.Fatalf("clearing the strikes = (%d, %v) once the failure is counted, want %d", n, err, want)
		}
	}
}

// requireSidecarGone is the fsync and store exits' bookkeeping: the orphan
// sidecar is removed before the job is counted, and before its path is free,
// so a retry's fresh output can never be the file an old job deletes.
func requireSidecarGone(t *testing.T, _ *manifest.Store, spec JobSpec) {
	t.Helper()
	if _, err := os.Stat(spec.SidecarPath()); !os.IsNotExist(err) {
		t.Fatalf("stat sidecar = %v once the failure is counted, want it already removed", err)
	}
}

// heldExit is one exit of processJob, driven up to the release of its path
// and held there.
type heldExit struct {
	name string
	seed bool
	// finish is what the runner does once the test lets the job go.
	finish  func(context.Context, JobSpec) (RunResult, error)
	fsync   func(string) error
	timeout time.Duration
	// wantDone is the success exit; every other case is a failure.
	wantDone bool
}

// TestNothingIsCountedOrAnnouncedWhileAJobStillHoldsItsPath holds the one
// statement every exit ends in, the release of the job's path, and checks
// what the job has already made visible by then. Nothing may be: no count,
// and no upscale.complete or jobFailed event, because each of them tells a
// reader the job is over. A consumer that acts on one, the batch Coordinator
// marking a batch finished or the console refreshing on a count, must find
// the path free and the job out of the in-flight figure.
//
// Before the fix every exit counted first, and the fsync and store exits also
// sent their jobFailed event before the release, while fireJobFailed's own
// docblock said the workers send it "after releaseDedup".
//
// The window is held rather than raced. The release takes p.mu, so while the
// test holds p.mu the worker stops exactly there, whatever it did on the way;
// finishJob clears the worker's slot just before it takes the lock, so an
// idle slot says the worker has arrived. The publisher is held in its first
// callback, the enqueue's state change, so an event already sent stays in its
// channel where len() can see it. The success exit logs nothing, which puts it
// out of reach of the parked-log technique, and this holds it all the same.
func TestNothingIsCountedOrAnnouncedWhileAJobStillHoldsItsPath(t *testing.T) {
	for _, tc := range []heldExit{
		{name: "success", seed: true, finish: okRunner(1), fsync: noopFsync, wantDone: true},
		{name: "sox failed", seed: true, finish: failRunner(errors.New("sox FAIL formats: bad header")), fsync: noopFsync},
		{name: "sox timed out", seed: true, finish: waitForTimeoutRunner, fsync: noopFsync, timeout: 20 * time.Millisecond},
		{name: "fsync failed", seed: true, finish: writeSidecarRunner, fsync: failingFsync},
		{name: "store failed", finish: writeSidecarRunner, fsync: noopFsync},
		{name: "panic", seed: true, fsync: noopFsync, finish: func(context.Context, JobSpec) (RunResult, error) {
			panic("synthetic worker panic")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { runHeldExit(t, tc) })
	}
}

func runHeldExit(t *testing.T, tc heldExit) {
	p, _ := newTerminalOrderPool(t, tc.seed)
	// Registered first, so it runs last: the runner and the publisher
	// helpers below register their own let-go cleanups after it.
	t.Cleanup(p.Stop)
	publisherHeld, openPublisher := holdPublisher(t, p)
	announced := recordAnnouncements(p)
	letGo, started := gateRunner(t, p, tc.finish)
	p.fsyncFn = tc.fsync
	if tc.timeout > 0 {
		p.jobTimeout = tc.timeout
	}

	spec := terminalOrderSpec(t)
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	awaitClose(t, started, "the runner")
	awaitClose(t, publisherHeld, "the publisher's first state-change callback")

	at := snapshotAtRelease(t, p, spec, letGo)
	if !at.held {
		t.Fatal("the job's path is not held, so the test is not inside the window it means to hold")
	}
	if at.done+at.failed != 0 {
		t.Errorf("counted (done=%d failed=%d) while the job still holds its path: a snapshot "+
			"shows it in flight AND finished, and a retry sent on that count is refused", at.done, at.failed)
	}
	if at.events != 0 {
		t.Errorf("%d terminal event(s) sent while the job still holds its path: a consumer "+
			"acting on the event re-submits into ErrDuplicateInflight", at.events)
	}

	openPublisher()
	requireAnnouncedFinished(t, announced, tc.wantDone)
}

// holdPublisher holds the pool's publisher in its first callback, the
// enqueue's state change, so an event the worker sends afterwards stays in its
// channel where len() counts it. open lets it go; it is also registered as a
// cleanup, which runs before the caller's earlier-registered Stop.
func holdPublisher(t *testing.T, p *Pool) (held <-chan struct{}, open func()) {
	t.Helper()
	h := make(chan struct{})
	gate := make(chan struct{})
	var openOnce, heldOnce sync.Once
	open = func() { openOnce.Do(func() { close(gate) }) }
	t.Cleanup(open)
	p.SetOnStateChange(func() {
		heldOnce.Do(func() {
			close(h)
			<-gate
		})
	})
	return h, open
}

// recordAnnouncements captures what the pool looked like each time a terminal
// event reached its callback.
func recordAnnouncements(p *Pool) <-chan PoolStats {
	announced := make(chan PoolStats, 2)
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		announced <- p.Stats()
	})
	p.SetOnJobFailed(func(string, string, string, float64, uuid.UUID, time.Time) {
		announced <- p.Stats()
	})
	return announced
}

// gateRunner installs a runner that signals started, then waits for letGo
// before doing what finish does. letGo is also a cleanup, so a failed test
// never leaves the worker blocked in front of Stop.
func gateRunner(t *testing.T, p *Pool, finish func(context.Context, JobSpec) (RunResult, error)) (letGo func(), started <-chan struct{}) {
	t.Helper()
	s := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		close(s)
		<-release
		return finish(ctx, spec)
	}
	return letGo, s
}

// releaseSnapshot is what a job had made visible by the time it reached the
// release of its path.
type releaseSnapshot struct {
	held         bool
	done, failed uint64
	events       int
}

// snapshotAtRelease holds p.mu, lets the job go, and waits for the worker to
// arrive at the release: finishJob clears the worker's slot just before it
// takes the lock this holds. Whatever the job has counted or sent by then, it
// did before giving its path back.
func snapshotAtRelease(t *testing.T, p *Pool, spec JobSpec, letGo func()) releaseSnapshot {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	letGo()
	deadline := time.Now().Add(5 * time.Second)
	for p.ActiveWorkers()[0].Busy {
		if time.Now().After(deadline) {
			t.Fatal("the worker never cleared its slot, so it never reached finishJob. With p.mu " +
				"held here, the likely cause is a worker blocked on the pool lock inside its exit: " +
				"something there counts or releases before the tail does")
		}
		time.Sleep(time.Millisecond)
	}
	_, held := p.inflight[spec.SourceLibraryRel+"|"+spec.VariantID()]
	done, failed := countedLocked(p)
	return releaseSnapshot{
		held:   held,
		done:   done,
		failed: failed,
		events: len(p.jobCompleteChan) + len(p.jobFailedChan),
	}
}

// requireAnnouncedFinished waits for the job's terminal event and checks the
// pool state its callback saw: the job out of flight and counted once.
func requireAnnouncedFinished(t *testing.T, announced <-chan PoolStats, wantDone bool) {
	t.Helper()
	wantDoneN, wantFailedN := uint64(0), uint64(1)
	if wantDone {
		wantDoneN, wantFailedN = 1, 0
	}
	select {
	case st := <-announced:
		if st.Inflight != 0 || st.Done != wantDoneN || st.Failed != wantFailedN {
			t.Errorf("the terminal event saw Inflight=%d Done=%d Failed=%d, want 0/%d/%d: "+
				"an event must describe a job that is over", st.Inflight, st.Done, st.Failed, wantDoneN, wantFailedN)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal event arrived")
	}
}

// countedLocked reads the pool's outcome counters for a caller that already
// holds p.mu, which is why it cannot go through Stats.
func countedLocked(p *Pool) (done, failed uint64) {
	return p.doneCnt, p.failedCnt
}
