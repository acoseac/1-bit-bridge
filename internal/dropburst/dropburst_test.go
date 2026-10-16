package dropburst

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// lines is a slog.Handler that keeps every record's message and count, and
// fails the test if a record arrives while mu, the pool's mutex, is held.
type lines struct {
	t  *testing.T
	mu *sync.Mutex

	keptMu sync.Mutex
	kept   []string
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }

func (l *lines) Handle(_ context.Context, r slog.Record) error {
	if !l.mu.TryLock() {
		l.t.Errorf("%q was written with the pool's mutex held", r.Message)
	} else {
		l.mu.Unlock()
	}
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	})
	l.keptMu.Lock()
	l.kept = append(l.kept, b.String())
	l.keptMu.Unlock()
	return nil
}

func (l *lines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lines) WithGroup(string) slog.Handler      { return l }

func (l *lines) got() []string {
	l.keptMu.Lock()
	defer l.keptMu.Unlock()
	return append([]string(nil), l.kept...)
}

const msg = "pool: dropped the jobs queued before their feature went off"

// newTracker returns a Tracker over a mutex of its own, as a pool's, and
// what it writes.
func newTracker(t *testing.T) (*Tracker, *sync.Mutex, *lines) {
	t.Helper()
	mu := &sync.Mutex{}
	l := &lines{t: t, mu: mu}
	return New(mu, slog.New(l), msg), mu, l
}

func accept(mu *sync.Mutex, tr *Tracker, n int) {
	mu.Lock()
	for i := 0; i < n; i++ {
		tr.AcceptedLocked()
	}
	mu.Unlock()
}

func requireLines(t *testing.T, l *lines, want ...string) {
	t.Helper()
	got := l.got()
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	}
}

// TestARunEndsWhenNothingAcceptedIsLeftUndecided: three accepted jobs
// dropped one after another are one run, and its one line comes with the
// drop that leaves nothing undecided, carrying all three.
func TestARunEndsWhenNothingAcceptedIsLeftUndecided(t *testing.T) {
	tr, mu, l := newTracker(t)
	accept(mu, tr, 3)
	tr.Drop()
	tr.Drop()
	requireLines(t, l)
	tr.Drop()
	requireLines(t, l, msg+" count=3")
}

// TestAJobThatRunsEndsTheRunBeforeIt: a job decided to run means the gate
// opened again, so the drops before it are a run of their own, written then;
// a drop after it starts the next run.
func TestAJobThatRunsEndsTheRunBeforeIt(t *testing.T) {
	tr, mu, l := newTracker(t)
	accept(mu, tr, 4)
	tr.Drop()
	tr.Drop()
	requireLines(t, l)
	tr.Ran()
	requireLines(t, l, msg+" count=2")
	tr.Drop()
	requireLines(t, l, msg+" count=2", msg+" count=1")
}

// TestARunWithNoDropWritesNothing: jobs that all run write no line, and
// neither does a job passed over at shutdown with no run open.
func TestARunWithNoDropWritesNothing(t *testing.T) {
	tr, mu, l := newTracker(t)
	accept(mu, tr, 3)
	tr.Ran()
	tr.Ran()
	mu.Lock()
	n := tr.PassedOverLocked()
	mu.Unlock()
	tr.Write(n)
	if n != 0 {
		t.Errorf("PassedOverLocked = %d with no run open, want 0", n)
	}
	requireLines(t, l)
}

// TestAJobPassedOverEndsTheRunItWasTheLastUndecidedJobOf: a job the pool
// took while stopping leaves the undecided count with no decision, and when
// it is the last one, the run before it is over: its count comes back for
// the caller to write once the pool's mutex is released.
func TestAJobPassedOverEndsTheRunItWasTheLastUndecidedJobOf(t *testing.T) {
	tr, mu, l := newTracker(t)
	accept(mu, tr, 3)
	tr.Drop()
	tr.Drop()
	mu.Lock()
	n := tr.PassedOverLocked()
	mu.Unlock()
	if n != 2 {
		t.Fatalf("PassedOverLocked = %d, want 2: the run ended with the last undecided job", n)
	}
	requireLines(t, l)
	tr.Write(n)
	requireLines(t, l, msg+" count=2")
}

// TestADropThatEndsARunHasWrittenItsLineWhenItReturns: the line of the drop
// that ends a run is written by the time Drop returns, before the pool's next
// section, where it counts the drop. A pool whose caller sees it idle can then
// read the line.
func TestADropThatEndsARunHasWrittenItsLineWhenItReturns(t *testing.T) {
	tr, mu, l := newTracker(t)
	accept(mu, tr, 1)
	tr.Drop()
	requireLines(t, l, msg+" count=1")
}

// TestTheRunCountsEveryDropAcrossGoroutines: drops from several workers at
// once add up to one line, and lose no drop.
func TestTheRunCountsEveryDropAcrossGoroutines(t *testing.T) {
	tr, mu, l := newTracker(t)
	const workers, each = 8, 50
	accept(mu, tr, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				tr.Drop()
			}
		}()
	}
	wg.Wait()
	requireLines(t, l, fmt.Sprintf("%s count=%d", msg, workers*each))
}
