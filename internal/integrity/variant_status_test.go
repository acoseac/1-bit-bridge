package integrity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestVariantWatcherStatusFollowsTheRefusalLatch — the console's Jobs card
// said "on" for the variant watcher while every tick refused a relocation
// or skipped a variants directory that read as unmounted, and only the
// journal said otherwise (backlog B131). Status reports the latch: which
// kind of refusal a streak is, and since when; a streak of the other kind
// restarts the clock; a tick that decided nothing leaves it; a tick that
// passes both guards, or an empty catalog, clears it.
func TestVariantWatcherStatusFollowsTheRefusalLatch(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	current := dir
	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
	w := NewVariantWatcher(lister, &fakeDeleter{}, nil, func() string { return current }, time.Hour, 20)
	tick := func() SweepReport { return w.tick(context.Background()) }

	if got := w.Status(); got != (VariantSweepStatus{}) {
		t.Fatalf("before any tick: %+v, want the zero status", got)
	}
	tick()
	first := w.Status()
	if first.Refusing != VariantRefusalRelocation || first.Since.IsZero() {
		t.Fatalf("a refused relocation: %+v, want relocation with a start", first)
	}
	tick()
	if got := w.Status(); got != first {
		t.Errorf("the streak's second tick moved the status: %+v, want %+v", got, first)
	}

	// A tick that decided nothing is evidence of nothing.
	lister.mu.Lock()
	lister.err = errors.New("database is locked")
	lister.mu.Unlock()
	tick()
	lister.mu.Lock()
	lister.err = nil
	lister.mu.Unlock()
	if got := w.Status(); got != first {
		t.Errorf("a failed listing moved the status: %+v, want %+v", got, first)
	}

	// The volume unmounts: a streak of the other kind, with its own start.
	current = t.TempDir()
	tick()
	unmounted := w.Status()
	if unmounted.Refusing != VariantRefusalVariantsDir || !unmounted.Since.After(first.Since) {
		t.Errorf("an unmounted variants directory: %+v, want variantsDirUnavailable since after %v", unmounted, first.Since)
	}
	tick()
	if got := w.Status(); got != unmounted {
		t.Errorf("the unmounted streak's second tick moved the status: %+v, want %+v", got, unmounted)
	}

	// It comes back, and the files are put where the rows' layout says, but
	// for five whose files really went: the tick passes both guards, adopts
	// the 25 and deletes the five. (Removing the stray sidecar ended the
	// streak until backlog B223; a tree holding no sidecar is what an
	// unmounted volume looks like, which the mount-loss probe skips now.)
	current = dir
	putBack(t, dir, rows[:25])
	if r := tick(); r.Adopted != 25 || r.Deleted != 5 {
		t.Fatalf("the tick after both refusals ended: report %+v, want 25 adopted and 5 deleted", r)
	}
	if got := w.Status(); got != (VariantSweepStatus{}) {
		t.Errorf("a tick that passed both guards: %+v, want not refusing", got)
	}

	// An empty catalog ends a streak too: the rows it withheld are gone.
	w2 := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows, nil}}, &fakeDeleter{}, nil,
		staticDir(t.TempDir()), time.Hour, 20)
	w2.tick(context.Background())
	if got := w2.Status(); got.Refusing != VariantRefusalVariantsDir {
		t.Fatalf("rows over an empty directory: %+v, want variantsDirUnavailable", got)
	}
	w2.tick(context.Background())
	if got := w2.Status(); got != (VariantSweepStatus{}) {
		t.Errorf("an empty catalog after the streak: %+v, want not refusing", got)
	}
}

// TestVariantWatcherStatusIsReadableBesideTheRunningLoop — the Jobs handler
// reads Status on its own goroutine while the watcher's run goroutine moves
// the latch; under -race this is the test that says the two do not share a
// variable unsynchronised.
func TestVariantWatcherStatusIsReadableBesideTheRunningLoop(t *testing.T) {
	_, unmounted, rows := unmountedShape(t, 4)
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, &fakeDeleter{}, nil,
		staticDir(unmounted), time.Hour, 20)
	ticked := make(chan struct{}, 1)
	w.SetOnTickComplete(func(SweepReport) {
		select {
		case ticked <- struct{}{}:
		default:
		}
	})
	stop := w.Start(context.Background())
	defer stop()

	deadline := time.After(5 * time.Second)
	for w.Status().Refusing != VariantRefusalVariantsDir {
		select {
		case <-deadline:
			t.Fatalf("the boot tick over an unmounted variants directory never showed as refusing: %+v", w.Status())
		case <-time.After(time.Millisecond):
		}
	}
	<-ticked
	if (*VariantWatcher)(nil).Status() != (VariantSweepStatus{}) {
		t.Error("a nil watcher's status is not the zero status")
	}
}

// TestEveryVariantRefusalKindIsListed — VariantRefusalKinds is the list the
// console's wording test runs over (TestEveryVariantRefusalKindIsWorded, in
// internal/admin), so a kind the watcher can report and the list leaves out
// would reach the Jobs card as its bare key with every test green.
func TestEveryVariantRefusalKindIsListed(t *testing.T) {
	requireEveryKindIsListed(t, "VariantRefusalKind", "VariantRefusalKinds", 2, VariantRefusalKinds())
}
