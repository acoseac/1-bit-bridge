package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// stopGuard bounds each wait of a test of a stopped ticker and catches only a
// hang: no bound a stop arms fires there but by the test's hand
// (heldStopBounds), so nothing those tests wait for is a clock's, which a busy
// runner moves (backlog B895).
const stopGuard = 60 * time.Second

// The steps of a verification the tests of a stopped ticker hold, in the
// words the backup package names them by.
const (
	stepWriteWords    = "its verdict's write"
	stepReadBackWords = "its read of the record after it"
	stepPruneWords    = "the prune after it"
)

// heldStopBounds stands in for every bound a stop arms on the verification of
// a snapshot in one backups folder (backup.SetStopBoundsForTest), its caller's
// expiry and the bound on its worker's step: each is held until the test
// fires it, and every one is fired at the test's end (end), from when a bound
// armed fires as it is armed.
type heldStopBounds struct {
	mu      sync.Mutex
	armed   []*heldBound
	changed chan struct{}
	ended   bool
}

// heldBound is one bound a stop armed: on the snapshot dir, at step ("" for
// its caller's expiry), its length as armed (after); fire fires it.
type heldBound struct {
	dir, step string
	after     time.Duration
	fire      func()
}

// holdStopBounds stands a heldStopBounds in for the bounds a stop arms on the
// verifications of the snapshots in root, put back once every cleanup
// registered after it has run.
func holdStopBounds(t *testing.T, root string) *heldStopBounds {
	t.Helper()
	b := &heldStopBounds{changed: make(chan struct{})}
	t.Cleanup(backup.SetStopBoundsForTest(func(dir, step string, after time.Duration) <-chan time.Time {
		if filepath.Dir(dir) != root {
			return nil
		}
		c := make(chan time.Time)
		bound := &heldBound{dir: dir, step: step, after: after, fire: sync.OnceFunc(func() { close(c) })}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.armed = append(b.armed, bound)
		close(b.changed)
		b.changed = make(chan struct{})
		if b.ended {
			bound.fire()
		}
		return c
	}))
	return b
}

// await answers the first bound armed at step ("" for a caller's expiry),
// failing the test where none is within stopGuard.
func (b *heldStopBounds) await(t *testing.T, step string) *heldBound {
	t.Helper()
	guard := time.After(stopGuard)
	for {
		b.mu.Lock()
		changed := b.changed
		for _, bound := range b.armed {
			if bound.step == step {
				b.mu.Unlock()
				return bound
			}
		}
		b.mu.Unlock()
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("no bound at %q was armed within %v", step, stopGuard)
		}
	}
}

// end fires every bound armed, and each armed from now on as it is armed.
func (b *heldStopBounds) end() {
	b.mu.Lock()
	b.ended = true
	armed := append([]*heldBound(nil), b.armed...)
	b.mu.Unlock()
	for _, bound := range armed {
		bound.fire()
	}
}

// stopPark is a hook's park: the hook's first call closes entered and waits
// until letGo, which the test's end calls too; every later call passes.
type stopPark struct {
	entered chan struct{}
	out     chan struct{}
	first   atomic.Bool
	letGo   func()
}

// newStopPark is a stopPark not yet entered.
func newStopPark() *stopPark {
	p := &stopPark{entered: make(chan struct{}), out: make(chan struct{})}
	p.letGo = sync.OnceFunc(func() { close(p.out) })
	return p
}

// park parks its first caller until letGo.
func (p *stopPark) park() {
	if p.first.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.out
	}
}

// awaitGuarded waits for c to close, failing the test with what where it does
// not within stopGuard.
func awaitGuarded(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(stopGuard):
		t.Fatalf("%s within %v", what, stopGuard)
	}
}

// runStoppableTicker runs the backup ticker over src, writing to stdout and
// stderr, and answers its stop and its end. Its drain and its cleanup run
// before any seam its caller set first is put back: the cleanup lets go of
// what the test holds (release), fires every bound the stop armed, stops the
// ticker and waits for it, and for every verification of a snapshot in root,
// to end.
func runStoppableTicker(t *testing.T, src backup.Sources, root string, bounds *heldStopBounds, stdout, stderr io.Writer, release func()) (stop context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	drainLoopOnCleanup(t, cancel, ended, "the backup ticker")
	t.Cleanup(func() {
		release()
		bounds.end()
		cancel()
		waitClosed(t, ended, "the backup ticker")
		awaitVerificationsEnd(t, root)
	})
	go func() {
		defer close(ended)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, stdout, stderr, nil)
	}()
	return cancel, ended
}

// stopRun is one case of TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause:
// a ticker over a fresh install, the bounds its stop arms held (heldStopBounds),
// its verification's handoffs watched, and the parks its case sets let go at
// its end.
type stopRun struct {
	t       *testing.T
	dataDir string
	root    string
	src     backup.Sources
	bounds  *heldStopBounds
	parks   []*stopPark
	// answer, where it is set, holds the worker's answer, for its caller's
	// expiry to come after the worker's last step.
	answer            *stopPark
	answered, expired atomic.Bool
	// named, where it is set, is the step whose own bound is set short once the
	// ticker has named its snapshot (backup.SetStepBoundForTest).
	named        string
	restoreBound func()
	stderr       safeBuffer
}

// newStopRun is a stopRun over a fresh install, its seams on the snapshots of
// its backups folder.
func newStopRun(t *testing.T) *stopRun {
	t.Helper()
	_, dataDir, root := backupInstall(t)
	r := &stopRun{t: t, dataDir: dataDir, root: root,
		src: backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}}
	r.bounds = holdStopBounds(t, root)
	t.Cleanup(backup.SetVerifyHandoffForTest(func(dir string) {
		if filepath.Dir(dir) != root {
			return
		}
		r.answered.Store(true)
		if r.answer != nil {
			r.answer.park()
		}
	}, func(dir string) {
		if filepath.Dir(dir) == root {
			r.expired.Store(true)
		}
	}))
	// Registered before the ticker's cleanup, so run after it, as a seam's
	// restore is.
	t.Cleanup(func() {
		if r.restoreBound != nil {
			r.restoreBound()
		}
	})
	return r
}

// park is a stopPark the run lets go at its end.
func (r *stopRun) park() *stopPark {
	p := newStopPark()
	r.parks = append(r.parks, p)
	return p
}

// holdAnswer holds the worker's answer until the run's end, and answers its
// park.
func (r *stopRun) holdAnswer() *stopPark {
	r.answer = r.park()
	return r.answer
}

// parkCheck parks the check where every check comes (its thread, before it
// reads), the stop then giving it up half of backup.StopWriteBound later on
// its own clock (B886 stands a seam in for it).
func (r *stopRun) parkCheck() *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetOnLowPriorityForTest(p.park))
	return p
}

// parkSync parks the verdict's write in its sync, its rename landed.
func (r *stopRun) parkSync() *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { p.park(); return nil }))
	return p
}

// parkReadBack parks the read of the record after the check.
func (r *stopRun) parkReadBack() *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetReadBackFailureForTest(func(dir string) error {
		if filepath.Dir(dir) == r.root {
			p.park()
		}
		return nil
	}))
	return p
}

// parkPrune parks the prune after the verification.
func (r *stopRun) parkPrune() *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetOnPruneForTest(p.park))
	return p
}

// recheck leaves the install's one snapshot owing a recheck its ticker's start
// finishes in place: verified, then its record's database put back to a check
// that did not end, its files' VERIFIED kept (holdsVerified).
func (r *stopRun) recheck() {
	r.t.Helper()
	res := backup.SnapshotAndVerify(r.t.Context(), r.src, backup.SnapshotOptions{})
	if !res.Kept() || res.VerifyErr != nil || !res.Verify.Record.Passes() {
		r.t.Fatalf("the snapshot to check again: kept %v, %v, %+v", res.Kept(), res.VerifyErr, res.Verify.Record)
	}
	awaitVerificationsEnd(r.t, r.root)
	path := filepath.Join(res.Dir, backup.ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		r.t.Fatal(err)
	}
	m.Verification.DatabaseState, m.Verification.DatabaseReasons = backup.StateIncomplete, []backup.Reason{backup.ReasonDeadline}
	if raw, err = json.Marshal(m); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
	// That verification's answer was not the ticker's.
	r.answered.Store(false)
}

// failHere answers a hook that fails, with what, for the snapshots of the run's
// backups folder.
func (r *stopRun) failHere(what string) func(dir string) error {
	return func(dir string) error {
		if filepath.Dir(dir) == r.root {
			return errors.New(what)
		}
		return nil
	}
}

// stopCase is a row of TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause.
type stopCase struct {
	name string
	// said is every piece of what the stopped ticker says; nil: nothing.
	said []string
	// arm sets the row's seams before the ticker starts, for its worker's bound
	// first or its caller's expiry first, and answers where the test stops the
	// ticker (at, a park the verification reaches), the step whose bound the
	// stop then arms for the worker (step; "" where the check's own half bound
	// gives it up), and, for the expiry first, a park the verification reaches
	// once that step was given up, before the expiry fires (later; nil: the
	// expiry fires at once).
	arm func(r *stopRun, workerFirst bool) (at *stopPark, step string, later *stopPark)
}

// TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause is backlog B895's
// table: a verification stopped where each row puts it, with the two bounds
// its stop arms (the worker's on the step it is in, its caller's expiry)
// fired by the test in either order (backup.SetStopBoundsForTest), the
// expiry where a row says so after the worker's last step (its answer held).
// The ticker says nothing where the stop alone gave the verification up (the
// check, the verdict's write, the read of the record), and says, whichever
// order, a step a bound of its own gave up, whether before the stop or after
// the stop gave another up, a read of the record or a write that failed (or
// is not known durable), a recheck refused or not recorded, and a FAILED
// finding. Round 1 (67cffea1) said nothing of each in the expiry's order, and
// nothing in the worker's of the read that failed, the refused or unrecorded
// recheck, and named the stop's give-up, not the own bound's, after it.
func TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause(t *testing.T) {
	deadline := context.DeadlineExceeded.Error()
	for _, tc := range []stopCase{
		{"the stop alone, at the check", nil, func(r *stopRun, workerFirst bool) (*stopPark, string, *stopPark) {
			check := r.parkCheck()
			if workerFirst {
				return check, "", nil
			}
			return check, "", r.parkReadBack()
		}},
		{"the stop alone, at the verdict's write", nil, func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
			return r.parkSync(), stepWriteWords, nil
		}},
		{"the stop alone, at the read of the record", nil, func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
			return r.parkReadBack(), stepReadBackWords, nil
		}},
		{"the write's own bound, then the stop at the read of the record",
			[]string{"was given up", stepWriteWords + " did not end within its bound (" + deadline + ")"},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				r.parkSync()
				r.named = stepWriteWords
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"the stop at the write, then the read's own bound",
			[]string{"was given up", stepReadBackWords + " did not end within its bound (" + deadline + ")"},
			func(r *stopRun, workerFirst bool) (*stopPark, string, *stopPark) {
				write := r.parkSync()
				r.parkReadBack()
				r.named = stepReadBackWords
				if workerFirst {
					return write, stepWriteWords, nil
				}
				return write, stepWriteWords, r.holdAnswer()
			}},
		{"a read of the record that failed, then the prune",
			[]string{"read the snapshot's manifest"},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				var first atomic.Bool
				r.t.Cleanup(backup.SetOnManifestReadForTest(func(path string) {
					if filepath.Dir(filepath.Dir(path)) == r.root && first.CompareAndSwap(false, true) {
						_ = os.WriteFile(path, []byte("{"), 0o600)
					}
				}))
				return r.parkPrune(), stepPruneWords, nil
			}},
		{"a recheck refused, then the read of the record",
			[]string{backup.ErrRecheckRefused.Error()},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				r.recheck()
				// Neither veto, in the bundle or beside it, can be made durable.
				r.t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
					if strings.HasPrefix(path, r.root+string(filepath.Separator)) {
						return errors.New("injected: the veto's sync failed")
					}
					return nil
				}))
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a recheck not recorded where no directory can be synced, then the read of the record",
			[]string{backup.ErrRecheckNotRecorded.Error()},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				r.recheck()
				r.t.Cleanup(backup.SetSyncsParentDirForTest(false))
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a write that failed, then the prune",
			[]string{"injected: the staging failed"},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetVerdictWriteFailuresForTest(r.failHere("injected: the staging failed"), nil, nil))
				return r.parkPrune(), stepPruneWords, nil
			}},
		{"a write not known durable, then the read of the record",
			[]string{"injected: the verdict's sync failed"},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { return errors.New("injected: the verdict's sync failed") }))
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a FAILED finding, then the verdict's write",
			[]string{"database FAILED: schema-newer"},
			func(r *stopRun, _ bool) (*stopPark, string, *stopPark) {
				makeStoreNewer(r.t, r.src.ManifestDB)
				return r.parkSync(), stepWriteWords, nil
			}},
		{"a read of the record that failed once the stop gave the write up",
			[]string{"its record could not be read back", "injected: the read-back failed"},
			func(r *stopRun, workerFirst bool) (*stopPark, string, *stopPark) {
				write := r.parkSync()
				r.t.Cleanup(backup.SetReadBackFailureForTest(r.failHere("injected: the read-back failed")))
				if workerFirst {
					return write, stepWriteWords, nil
				}
				return write, stepWriteWords, r.holdAnswer()
			}},
		{"an INCOMPLETE whose write failed once the stop gave the check up",
			[]string{"injected: the staging failed"},
			func(r *stopRun, workerFirst bool) (*stopPark, string, *stopPark) {
				check := r.parkCheck()
				r.t.Cleanup(backup.SetVerdictWriteFailuresForTest(r.failHere("injected: the staging failed"), nil, nil))
				if workerFirst {
					return check, "", nil
				}
				return check, "", r.parkReadBack()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, workerFirst := range []bool{true, false} {
				order := "the caller's expiry first"
				if workerFirst {
					order = "the worker's bound first"
				}
				t.Run(order, func(t *testing.T) {
					stopTheTicker(t, tc, workerFirst)
				})
			}
		})
	}
}

// stopTheTicker runs one case of the table: its row armed, the ticker started
// and stopped where the row puts it, the bounds fired in the order asked, and
// what the ticker said compared with the row's.
func stopTheTicker(t *testing.T, tc stopCase, workerFirst bool) {
	r := newStopRun(t)
	at, step, later := tc.arm(r, workerFirst)
	var stdout io.Writer = &safeBuffer{}
	var named *heldWriter
	if r.named != "" {
		named = holdOn("wrote ")
		stdout = named
	}
	stop, done := runStoppableTicker(t, r.src, r.root, r.bounds, stdout, &r.stderr, func() {
		if named != nil {
			named.letGo()
		}
		for _, p := range r.parks {
			p.letGo()
		}
	})
	if named != nil {
		// The step's own bound, set short once its snapshot is named: its step
		// cannot answer (it is parked), so that bound gives it up.
		awaitGuarded(t, named.held, "the ticker did not say it wrote its snapshot")
		dirs := snapshotDirs(t, r.root)
		if len(dirs) != 1 {
			t.Fatalf("%d snapshots, want the ticker's one", len(dirs))
		}
		r.restoreBound = backup.SetStepBoundForTest(dirs[0], time.Millisecond, r.named)
		named.letGo()
	}
	awaitGuarded(t, at.entered, "the verification did not reach where the stop comes")
	stop()
	expiry := r.bounds.await(t, "")
	var bound *heldBound
	if step != "" {
		bound = r.bounds.await(t, step)
	}
	switch {
	case workerFirst && bound != nil:
		bound.fire()
	case !workerFirst && later != nil:
		if bound != nil {
			bound.fire()
		}
		awaitGuarded(t, later.entered, "the verification did not reach where its caller's expiry comes")
		expiry.fire()
	case !workerFirst:
		expiry.fire()
	}
	awaitGuarded(t, done, "the stopped ticker did not return")
	if workerFirst && (!r.answered.Load() || r.expired.Load()) {
		t.Fatalf("the caller did not take its worker's answer: answered %v, expired %v", r.answered.Load(), r.expired.Load())
	}
	if !workerFirst && !r.expired.Load() {
		t.Fatalf("the caller's expiry did not answer it (the worker answered: %v)", r.answered.Load())
	}
	said := r.stderr.String()
	if tc.said == nil && said != "" {
		t.Errorf("a stopped verification was reported: %s", said)
	}
	for _, want := range tc.said {
		if !strings.Contains(said, want) {
			t.Errorf("the stopped ticker did not say %q:\n%s", want, said)
		}
	}
}
