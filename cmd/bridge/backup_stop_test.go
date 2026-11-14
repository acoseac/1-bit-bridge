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
	stepReadRecordWords = "its read of the snapshot's record"
	stepVetoWords       = "putting its veto in place"
	stepCheckWords      = "its check"
	stepWriteWords      = "its verdict's write"
	stepReadBackWords   = "its read of the record after it"
	stepPruneWords      = "the prune after it"
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
	// verify is what the case checks besides what the ticker said, once the
	// ticker has returned.
	verify []func()
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

// parkAfterCheck parks the worker once its check has answered or been given
// up, outside every step, so no bound of its own is armed there.
func (r *stopRun) parkAfterCheck() *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
		if filepath.Dir(dir) == r.root {
			p.park()
		}
	}))
	return p
}

// parkAnswered parks the step named once it has answered and published what
// it found, before its worker takes the answer (backup.SetOnStepAnsweredForTest).
func (r *stopRun) parkAnswered(step string) *stopPark {
	p := r.park()
	r.t.Cleanup(backup.SetOnStepAnsweredForTest(func(dir, s string) {
		if s == step && filepath.Dir(dir) == r.root {
			p.park()
		}
	}))
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

// failFirstRead makes the first read of the run's snapshot's manifest fail,
// its bytes cut to "{" just before it.
func (r *stopRun) failFirstRead() {
	var first atomic.Bool
	r.t.Cleanup(backup.SetOnManifestReadForTest(func(path string) {
		if filepath.Dir(filepath.Dir(path)) == r.root && first.CompareAndSwap(false, true) {
			_ = os.WriteFile(path, []byte("{"), 0o600)
		}
	}))
}

// recordReads has the case check, once the ticker has returned, that its
// snapshot's record reads database want.
func (r *stopRun) recordReads(want string) {
	r.verify = append(r.verify, func() {
		dirs := snapshotDirs(r.t, r.root)
		if len(dirs) != 1 {
			r.t.Fatalf("%d snapshots, want the ticker's one", len(dirs))
		}
		rec := recordOf(r.t, dirs[0])
		if got := backup.StateWords(rec.DatabaseState, rec.DatabaseReasons); got != want {
			r.t.Errorf("the record reads database %s, want %s", got, want)
		}
	})
}

// stopOrder is the order a case of the table fires the two bounds a stop arms
// in: the worker's bound on the step it is in, and its caller's expiry.
type stopOrder int

const (
	// workerFirst fires the worker's bound on its step, where the stop armed
	// one, and has the worker answer.
	workerFirst stopOrder = iota
	// expiryFirst fires the caller's expiry while the verification is where
	// the stop came, before the worker's bound.
	expiryFirst
	// boundThenExpiry fires the worker's bound first (or lets the check's own
	// half bound give it up), then, once the worker is parked further on or its
	// answer held, the caller's expiry.
	boundThenExpiry
)

func (o stopOrder) String() string {
	switch o {
	case workerFirst:
		return "the worker's bound first"
	case expiryFirst:
		return "the caller's expiry first"
	}
	return "the worker's bound, then the caller's expiry"
}

// The orders a row runs in.
var (
	bothOrders  = []stopOrder{workerFirst, expiryFirst}
	afterBound  = []stopOrder{workerFirst, boundThenExpiry}
	everyOrder  = []stopOrder{workerFirst, expiryFirst, boundThenExpiry}
	expiryAlone = []stopOrder{expiryFirst}
)

// stopCase is a row of TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause.
type stopCase struct {
	name string
	// said is every piece of what the stopped ticker says; nil: nothing.
	said   []string
	orders []stopOrder
	// arm sets the row's seams before the ticker starts, for the order given,
	// and answers where the test stops the ticker (at, a park the verification
	// reaches), the step whose bound the stop then arms for the worker (step;
	// "" where the stop arms none: the check's own half bound gives it up, or
	// the worker is outside every step), and, for boundThenExpiry, a park the
	// verification reaches once that bound has fired, or its answer held,
	// before the expiry fires (later).
	arm func(r *stopRun, order stopOrder) (at *stopPark, step string, later *stopPark)
}

// TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause is backlog B895's
// table: a verification stopped where each row puts it, with the two bounds
// its stop arms (the worker's on the step it is in, its caller's expiry)
// fired by the test in each order the row names (backup.SetStopBoundsForTest):
// the worker's first, the caller's first while the verification is where the
// stop came, or the worker's and then the caller's once the worker has moved
// on or its answer is held. The ticker says nothing where the stop alone gave
// the verification up (the check, a recheck's check, the verdict's write, the
// read of the record), and says, in every order, a step a bound of its own
// gave up, whether before the stop or after the stop gave another up, a read
// of the record or a write that failed (or is not known durable), a recheck
// refused or not recorded, and a FAILED finding. Round 1 (67cffea1) said
// nothing of each in the expiry's order, and nothing in the worker's of the
// read that failed, the refused or unrecorded recheck, and named the stop's
// give-up, not the own bound's, after it. Round 3's rows hold a step once it
// has answered and before its worker takes the answer, and the worker after
// its check (backup.SetOnStepAnsweredForTest, SetAfterCheckForTest): round 2
// (af338e82) published each outcome only once its worker took it, so it said
// nothing of them there, nor of a recheck's ErrRecheckNotRecorded before the
// steps after its check, and it said a recheck's INCOMPLETE that the stop
// alone kept from being written.
func TestAStoppedTickerSaysOnlyWhatItsStopAloneDidNotCause(t *testing.T) {
	deadline := context.DeadlineExceeded.Error()
	for _, tc := range []stopCase{
		{"the stop alone, at the check", nil, everyOrder, func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
			check := r.parkCheck()
			switch order {
			case expiryFirst:
				// The worker held after the check, which its own half bound may
				// give up before the expiry fires: the expiry answers.
				r.parkAfterCheck()
				return check, "", nil
			case boundThenExpiry:
				r.recordReads("INCOMPLETE: cancelled")
				return check, "", r.parkReadBack()
			}
			r.recordReads("INCOMPLETE: cancelled")
			return check, "", nil
		}},
		{"the stop alone, at the verdict's write", nil, everyOrder, func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
			write := r.parkSync()
			if order == boundThenExpiry {
				return write, stepWriteWords, r.holdAnswer()
			}
			return write, stepWriteWords, nil
		}},
		{"the stop alone, at the read of the record", nil, everyOrder, func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
			read := r.parkReadBack()
			if order == boundThenExpiry {
				return read, stepReadBackWords, r.holdAnswer()
			}
			return read, stepReadBackWords, nil
		}},
		{"a recheck, the stop alone at its check", nil, afterBound, func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
			r.recheck()
			check := r.parkCheck()
			if order == boundThenExpiry {
				return check, "", r.parkReadBack()
			}
			return check, "", nil
		}},
		{"the write's own bound, then the stop at the read of the record",
			[]string{"was given up", stepWriteWords + " did not end within its bound (" + deadline + ")"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.parkSync()
				r.named = stepWriteWords
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"the stop at the write, then the read's own bound",
			[]string{"was given up", stepReadBackWords + " did not end within its bound (" + deadline + ")"}, afterBound,
			func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
				write := r.parkSync()
				r.parkReadBack()
				r.named = stepReadBackWords
				if order == boundThenExpiry {
					return write, stepWriteWords, r.holdAnswer()
				}
				return write, stepWriteWords, nil
			}},
		{"a read of the record that failed, then the prune",
			[]string{"read the snapshot's manifest"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.failFirstRead()
				return r.parkPrune(), stepPruneWords, nil
			}},
		{"a recheck refused, then the read of the record",
			[]string{backup.ErrRecheckRefused.Error()}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.recheck()
				r.failVetoSyncs()
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a recheck not recorded where no directory can be synced, then the read of the record",
			[]string{backup.ErrRecheckNotRecorded.Error()}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.recheck()
				r.t.Cleanup(backup.SetSyncsParentDirForTest(false))
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a write that failed, then the prune",
			[]string{"injected: the staging failed"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetVerdictWriteFailuresForTest(r.failHere("injected: the staging failed"), nil, nil))
				return r.parkPrune(), stepPruneWords, nil
			}},
		{"a write not known durable, then the read of the record",
			[]string{"injected: the verdict's sync failed"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { return errors.New("injected: the verdict's sync failed") }))
				return r.parkReadBack(), stepReadBackWords, nil
			}},
		{"a FAILED finding, then the verdict's write",
			[]string{"database FAILED: schema-newer"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				makeStoreNewer(r.t, r.src.ManifestDB)
				return r.parkSync(), stepWriteWords, nil
			}},
		{"a read of the record that failed once the stop gave the write up",
			[]string{"its record could not be read back", "injected: the read-back failed"}, afterBound,
			func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
				write := r.parkSync()
				r.t.Cleanup(backup.SetReadBackFailureForTest(r.failHere("injected: the read-back failed")))
				if order == boundThenExpiry {
					return write, stepWriteWords, r.holdAnswer()
				}
				return write, stepWriteWords, nil
			}},
		{"an INCOMPLETE whose write failed once the stop gave the check up",
			[]string{"injected: the staging failed"}, afterBound,
			func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
				check := r.parkCheck()
				r.t.Cleanup(backup.SetVerdictWriteFailuresForTest(r.failHere("injected: the staging failed"), nil, nil))
				if order == boundThenExpiry {
					return check, "", r.parkReadBack()
				}
				return check, "", nil
			}},
		// Round 3's rows: each outcome published as its step answers.
		{"a recheck not recorded, the caller's expiry while its worker waits after the check",
			[]string{backup.ErrRecheckNotRecorded.Error()}, expiryAlone,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.recheck()
				r.t.Cleanup(backup.SetSyncsParentDirForTest(false))
				return r.parkAfterCheck(), "", nil
			}},
		{"a read of the record that failed, the stop as that read answers",
			[]string{"read the snapshot's manifest"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				// Every lookup of a veto of the run's snapshots, from the read's
				// failure until the read has answered: none is to be made.
				var reading atomic.Bool
				var looked atomic.Int64
				var first atomic.Bool
				r.t.Cleanup(backup.SetOnManifestReadForTest(func(path string) {
					if filepath.Dir(filepath.Dir(path)) == r.root && first.CompareAndSwap(false, true) {
						_ = os.WriteFile(path, []byte("{"), 0o600)
						reading.Store(true)
					}
				}))
				r.t.Cleanup(backup.SetLookupVetoForTest(func(path string) (os.FileInfo, error) {
					if reading.Load() && strings.HasPrefix(path, r.root+string(filepath.Separator)) {
						looked.Add(1)
					}
					return os.Lstat(path)
				}))
				answered := r.park()
				r.t.Cleanup(backup.SetOnStepAnsweredForTest(func(dir, step string) {
					if step == stepReadRecordWords && filepath.Dir(dir) == r.root {
						reading.Store(false)
						answered.park()
					}
				}))
				r.verify = append(r.verify, func() {
					if n := looked.Load(); n != 0 {
						r.t.Errorf("a veto was looked up %d times once the record's read had failed", n)
					}
				})
				return answered, stepReadRecordWords, nil
			}},
		{"a FAILED finding, the stop as the check answers",
			[]string{"database FAILED: schema-newer"}, bothOrders,
			func(r *stopRun, order stopOrder) (*stopPark, string, *stopPark) {
				makeStoreNewer(r.t, r.src.ManifestDB)
				answered := r.parkAnswered(stepCheckWords)
				if order == expiryFirst {
					// The worker held after the check, which its own half bound
					// may give up before the expiry fires: the expiry answers.
					r.parkAfterCheck()
				} else {
					// The check answered before its half bound passed: what it
					// found is written, never given up for INCOMPLETE.
					r.recordReads("FAILED: schema-newer")
				}
				return answered, "", nil
			}},
		{"a recheck refused, the stop as its veto answers",
			[]string{backup.ErrRecheckRefused.Error()}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.recheck()
				r.failVetoSyncs()
				return r.parkAnswered(stepVetoWords), stepVetoWords, nil
			}},
		{"a write that failed, the stop as the write answers",
			[]string{"injected: the staging failed"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetVerdictWriteFailuresForTest(r.failHere("injected: the staging failed"), nil, nil))
				return r.parkAnswered(stepWriteWords), stepWriteWords, nil
			}},
		{"a read of the record after it that failed, the stop as that read answers",
			[]string{"its record could not be read back", "injected: the read-back failed"}, bothOrders,
			func(r *stopRun, _ stopOrder) (*stopPark, string, *stopPark) {
				r.t.Cleanup(backup.SetReadBackFailureForTest(r.failHere("injected: the read-back failed")))
				return r.parkAnswered(stepReadBackWords), stepReadBackWords, nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range tc.orders {
				t.Run(order.String(), func(t *testing.T) {
					stopTheTicker(t, tc, order)
				})
			}
		})
	}
}

// failVetoSyncs makes neither veto of a recheck, in the bundle or beside it,
// durable.
func (r *stopRun) failVetoSyncs() {
	r.t.Cleanup(backup.SetSyncVetoForTest(func(path string) error {
		if strings.HasPrefix(path, r.root+string(filepath.Separator)) {
			return errors.New("injected: the veto's sync failed")
		}
		return nil
	}))
}

// stopTheTicker runs one case of the table: its row armed, the ticker started
// and stopped where the row puts it, the bounds fired in the order asked, and
// what the ticker said compared with the row's.
func stopTheTicker(t *testing.T, tc stopCase, order stopOrder) {
	r := newStopRun(t)
	at, step, later := tc.arm(r, order)
	if (order == boundThenExpiry) != (later != nil) {
		t.Fatalf("the row names a park after its bound (%v) for the order %q", later != nil, order)
	}
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
	switch order {
	case workerFirst:
		if bound != nil {
			bound.fire()
		}
	case expiryFirst:
		expiry.fire()
	case boundThenExpiry:
		if bound != nil {
			bound.fire()
		}
		awaitGuarded(t, later.entered, "the verification did not reach where its caller's expiry comes")
		expiry.fire()
	}
	awaitGuarded(t, done, "the stopped ticker did not return")
	if order == workerFirst && (!r.answered.Load() || r.expired.Load()) {
		t.Fatalf("the caller did not take its worker's answer: answered %v, expired %v", r.answered.Load(), r.expired.Load())
	}
	if order != workerFirst && !r.expired.Load() {
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
	for _, check := range r.verify {
		check()
	}
}
