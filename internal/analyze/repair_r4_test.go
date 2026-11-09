package analyze

import (
	"context"
	"errors"
	"io/fs"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAHookWhoseOwnStepFailsEndsItsCase: the two halves of round 3's hang on
// Windows, where the root's handle refused a hook's rename of the waveform
// folder on the pool's worker, the hook's t.Fatal ended that goroutine alone,
// the job was never counted, and the wait for it ran to the package's
// timeout. A hook's own step that fails on the worker reaches the test's
// goroutine as that step's failure once the repair it ran in has returned and
// been counted; and a wait on a job that is never counted gives up at its
// bound, as does a stop of a pool whose job does not end. On every platform:
// the step is the cases' own replacement, of a folder that is not there
// (backlog B652, round 4).
func TestAHookWhoseOwnStepFailsEndsItsCase(t *testing.T) {
	t.Run("a step that fails on the pool's worker", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := repairPool(t, f, func(string) error {
			f.fault("replace the waveform folder", replaceFolder(f.dir+".not-there"))
			return nil
		})
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		st, err := f.settle(t, p)
		if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "replace the waveform folder") {
			t.Fatalf("the wait answered %v, want the hook's failed replacement", err)
		}
		if st.Enqueued != 1 || st.Inflight != 0 || st.Done+st.Failed != 1 {
			t.Errorf("pool = %+v, want the repair the hook ran in returned and counted before the failure was answered", st)
		}
	})
	t.Run("a job that is never counted", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(context.Context, RepairSpec) (RepairResult, error) {
				// What t.Fatal does on the pool's worker: the goroutine ends,
				// and the job with it, uncounted.
				runtime.Goexit()
				return RepairResult{}, nil
			}))
		t.Cleanup(func() { stopPool(t, p) })
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		const bound = 200 * time.Millisecond
		start := time.Now()
		st, err := awaitPool(p, start.Add(bound), poolSettled)
		if err == nil {
			t.Fatalf("the wait settled on a job that was never counted: %+v", st)
		}
		if took := time.Since(start); took < bound {
			t.Errorf("the wait gave up after %s, before its bound of %s", took, bound)
		}
		if st.Enqueued != 1 || st.Done+st.Failed+st.Dropped != 0 {
			t.Errorf("pool = %+v, want the one job taken and never counted", st)
		}
	})
	t.Run("a stop while a job does not end", func(t *testing.T) {
		f := newRepairPoolFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		free := func() { releaseOnce.Do(func() { close(release) }) }
		p := NewPool(f.store, 1, 4, WithFsync(noFsync),
			WithRepairRunner(func(context.Context, RepairSpec) (RepairResult, error) {
				// A job the stop's cancel does not end: a hook held on a
				// channel only the test's goroutine closes.
				close(entered)
				<-release
				return RepairResult{}, errors.New("released")
			}))
		// Last registered, first run: the job released, then the pool stopped,
		// before the store closes.
		t.Cleanup(func() { stopPool(t, p) })
		t.Cleanup(free)
		if err := p.EnqueueRepair(f.spec); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-time.After(time.Until(poolWaitEnd(t))):
			t.Fatal("the job never started")
		}
		const bound = 200 * time.Millisecond
		start := time.Now()
		stopped, err := stopWithin(p, start.Add(bound))
		if err == nil {
			t.Fatal("the stop returned while a job that does not end was still running")
		}
		if took := time.Since(start); took < bound {
			t.Errorf("the stop gave up after %s, before its bound of %s", took, bound)
		}
		free()
		select {
		case <-stopped:
		case <-time.After(time.Until(poolWaitEnd(t))):
			t.Error("the stop left running had not returned by the wait's bound once the job was released")
		}
	})
}
