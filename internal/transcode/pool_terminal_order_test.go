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
	for _, tc := range []struct {
		name string
		// logMsg is the exit's own log line, where the worker is parked.
		logMsg string
		// seed writes the track row. The store exit needs it ABSENT: the
		// variant row's foreign key is how that exit is reached.
		seed    bool
		runner  func(context.Context, JobSpec) (RunResult, error)
		fsync   func(string) error
		timeout time.Duration
		// landed checks the exit's bookkeeping once its failure is counted.
		landed func(t *testing.T, store *manifest.Store, spec JobSpec)
	}{
		{
			name:   "sox failed",
			logMsg: "pool: sox failed",
			seed:   true,
			runner: failRunner(errors.New("sox FAIL formats: can't open input file")),
			fsync:  noopFsync,
			landed: func(t *testing.T, store *manifest.Store, _ JobSpec) {
				// The strike is recorded before the count, so clearing the
				// album's strikes (the operator's retry action) finds it.
				n, err := store.ClearVariantFailuresUnderPrefix(context.Background(), "Music/Album")
				if err != nil || n != 1 {
					t.Fatalf("clearing the strikes = (%d, %v) once the failure is counted, want 1: "+
						"a count must mean the strike has landed", n, err)
				}
			},
		},
		{
			name:    "sox timed out",
			logMsg:  "pool: sox timed out",
			seed:    true,
			runner:  waitForTimeoutRunner,
			fsync:   noopFsync,
			timeout: 20 * time.Millisecond,
			landed: func(t *testing.T, store *manifest.Store, _ JobSpec) {
				// A deadline says as much about a hung mount as about the
				// source, so it is never a strike against the file.
				n, err := store.ClearVariantFailuresUnderPrefix(context.Background(), "Music/Album")
				if err != nil || n != 0 {
					t.Fatalf("clearing the strikes = (%d, %v) after a timeout, want 0", n, err)
				}
			},
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
		t.Run(tc.name, func(t *testing.T) {
			park := loggingtest.ParkOn(t, tc.logMsg)
			store := openTempStoreForPool(t)
			t.Cleanup(func() { _ = store.Close() })
			if tc.seed {
				seedTrackForPool(t, store, terminalOrderRel)
			}
			p := NewPool(store, 1, 4)
			p.runner = tc.runner
			p.fsyncFn = tc.fsync
			if tc.timeout > 0 {
				p.jobTimeout = tc.timeout
			}
			// Deferred in this order so the worker is let go BEFORE Stop waits
			// for it, on every way out of the test, a failed assertion included.
			defer p.Stop()
			defer park.Release()

			// settle waits for cond and checks, on every poll rather than only
			// at the end, that each accepted job is exactly one of in flight,
			// done or failed: never both, never neither.
			settle := func(cond func(PoolStats) bool) {
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

			spec := terminalOrderSpec(t)
			if err := p.Enqueue(spec); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			park.Wait(t)

			// The job is still doing its own bookkeeping, so it is still
			// running: in flight, and not counted.
			if st := p.Stats(); st.Inflight != 1 || st.Done+st.Failed != 0 {
				// Show what a retry gets at this point before failing. Counted
				// early, it is refused as a duplicate of a job the count calls
				// finished; released early, a second attempt is admitted while
				// the first is still writing its strike or removing its sidecar.
				retryErr := p.Enqueue(spec)
				t.Fatalf("parked in its own %q line, the job reads Inflight=%d Done=%d Failed=%d, want 1/0/0: "+
					"it is still doing its own bookkeeping, so it must be in flight and uncounted. "+
					"A retry sent at this moment returned %v",
					tc.logMsg, st.Inflight, st.Done, st.Failed, retryErr)
			}
			park.Release()

			settle(func(st PoolStats) bool { return st.Failed == 1 })
			tc.landed(t, store, spec)
			if err := p.Enqueue(spec); err != nil {
				t.Fatalf("a retry sent on Failed == 1 returned %v, want it accepted: "+
					"the job had already been counted, so its path must already be free", err)
			}
			settle(func(st PoolStats) bool { return st.Failed == 2 })
		})
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
	for _, tc := range []struct {
		name string
		// seed writes the track row; the store exit needs it absent.
		seed bool
		// finish is what the runner does once the test lets the job go.
		finish  func(context.Context, JobSpec) (RunResult, error)
		fsync   func(string) error
		timeout time.Duration
		// wantDone is the success exit; every other case is a failure.
		wantDone bool
	}{
		{name: "success", seed: true, finish: okRunner(1), fsync: noopFsync, wantDone: true},
		{name: "sox failed", seed: true, finish: failRunner(errors.New("sox FAIL formats: bad header")), fsync: noopFsync},
		{name: "sox timed out", seed: true, finish: waitForTimeoutRunner, fsync: noopFsync, timeout: 20 * time.Millisecond},
		{name: "fsync failed", seed: true, finish: writeSidecarRunner, fsync: failingFsync},
		{name: "store failed", finish: writeSidecarRunner, fsync: noopFsync},
		{name: "panic", seed: true, fsync: noopFsync, finish: func(context.Context, JobSpec) (RunResult, error) {
			panic("synthetic worker panic")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTempStoreForPool(t)
			t.Cleanup(func() { _ = store.Close() })
			if tc.seed {
				seedTrackForPool(t, store, terminalOrderRel)
			}
			p := NewPool(store, 1, 4)
			t.Cleanup(p.Stop)

			// Hold the publisher in its first callback. Registered after Stop,
			// so on every way out of the test it opens before Stop waits for
			// the publisher.
			publisherHeld := make(chan struct{})
			gate := make(chan struct{})
			var openOnce, heldOnce sync.Once
			openGate := func() { openOnce.Do(func() { close(gate) }) }
			t.Cleanup(openGate)
			p.SetOnStateChange(func() {
				heldOnce.Do(func() {
					close(publisherHeld)
					<-gate
				})
			})
			// What the pool looked like when the terminal event arrived.
			announced := make(chan PoolStats, 2)
			p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
				announced <- p.Stats()
			})
			p.SetOnJobFailed(func(string, string, string, float64, uuid.UUID, time.Time) {
				announced <- p.Stats()
			})

			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(letGo)
			p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
				close(started)
				<-release
				return tc.finish(ctx, spec)
			}
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

			p.mu.Lock()
			locked := true
			unlock := func() {
				if locked {
					locked = false
					p.mu.Unlock()
				}
			}
			defer unlock()
			letGo()
			deadline := time.Now().Add(5 * time.Second)
			for p.ActiveWorkers()[0].Busy {
				if time.Now().After(deadline) {
					unlock()
					t.Fatal("the worker never cleared its slot, so it never reached finishJob. With p.mu " +
						"held here, the likely cause is a worker blocked on the pool lock inside its exit: " +
						"something there counts or releases before the tail does")
				}
				time.Sleep(time.Millisecond)
			}
			_, held := p.inflight[spec.SourceLibraryRel+"|"+spec.VariantID()]
			done, failed := countedLocked(p)
			events := len(p.jobCompleteChan) + len(p.jobFailedChan)
			unlock()

			if !held {
				t.Fatal("the job's path is not held, so the test is not inside the window it means to hold")
			}
			if done+failed != 0 {
				t.Errorf("counted (done=%d failed=%d) while the job still holds its path: a snapshot "+
					"shows it in flight AND finished, and a retry sent on that count is refused", done, failed)
			}
			if events != 0 {
				t.Errorf("%d terminal event(s) sent while the job still holds its path: a consumer "+
					"acting on the event re-submits into ErrDuplicateInflight", events)
			}

			openGate()
			select {
			case st := <-announced:
				wantDone, wantFailed := uint64(0), uint64(1)
				if tc.wantDone {
					wantDone, wantFailed = 1, 0
				}
				if st.Inflight != 0 || st.Done != wantDone || st.Failed != wantFailed {
					t.Errorf("the terminal event saw Inflight=%d Done=%d Failed=%d, want 0/%d/%d: "+
						"an event must describe a job that is over", st.Inflight, st.Done, st.Failed, wantDone, wantFailed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no terminal event arrived")
			}
		})
	}
}

// countedLocked reads the pool's outcome counters for a caller that already
// holds p.mu, which is why it cannot go through Stats.
func countedLocked(p *Pool) (done, failed uint64) {
	return p.doneCnt, p.failedCnt
}
