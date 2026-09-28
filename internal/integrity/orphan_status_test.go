package integrity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOrphanSidecarSweeperStatusFollowsTheRefusalLatch — the console's
// Jobs card said "on" while every tick refused, and only the journal said
// otherwise. Status reports the latch: which kind of refusal a streak is,
// and since when; a streak of the other kind restarts the clock; a tick
// that decided nothing leaves it; the first tick that proceeds clears it.
func TestOrphanSidecarSweeperStatusFollowsTheRefusalLatch(t *testing.T) {
	skipWhereModesDenyNothing(t)
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	stranded := seedTestSidecarTree(t, dir, "stranded-", 15)
	locked := filepath.Join(dir, "locked")
	hidden := seedTestSidecarTree(t, locked, "stranded-", 1000)
	ageFixtures(t, dir)
	l := &switchableLister{rows: rowsNaming(live, stranded, hidden)}
	s := NewOrphanSidecarSweeper(l, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond

	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Fatalf("before any tick: %+v, want the zero status", got)
	}
	s.tick(context.Background())
	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Fatalf("a tick that proceeds: %+v, want not refusing", got)
	}

	// The rows for the 1,015 stranded files are lost: a lost index.
	l.rows = rowsNaming(live)
	s.tick(context.Background())
	first := s.Status()
	if first.Refusing != OrphanRefusalMassOrphans || first.Since.IsZero() {
		t.Fatalf("a refused tick: %+v, want massOrphans with a start", first)
	}
	s.tick(context.Background())
	if got := s.Status(); got != first {
		t.Errorf("the streak's second tick moved the status: %+v, want %+v", got, first)
	}

	// A tick that decided nothing is evidence of nothing.
	l.rows = nil
	s.tick(context.Background())
	if got := s.Status(); got != first {
		t.Errorf("an empty catalog moved the status: %+v, want %+v", got, first)
	}

	// The locked directory hides 1,000 of them: 15 of 35 against 20 rows
	// passes the mass-orphan check, and the walk is partial. A streak of
	// the other kind starts its own clock.
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	l.rows = rowsNaming(live)
	s.tick(context.Background())
	partial := s.Status()
	if partial.Refusing != OrphanRefusalPartialWalk || !partial.Since.After(first.Since) {
		t.Errorf("after the walk turned partial: %+v, want partialWalk since after %v", partial, first.Since)
	}

	// The rows come back and the directory opens: the tick proceeds.
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	l.rows = rowsNaming(live, stranded, hidden)
	s.tick(context.Background())
	if got := s.Status(); got != (OrphanSweepStatus{}) {
		t.Errorf("a tick that proceeds after the streak: %+v, want not refusing", got)
	}
}

// TestOrphanSidecarSweeperStatusIsReadableBesideTheRunningLoop — the Jobs
// handler reads Status on its own goroutine while the sweep's run goroutine
// moves the latch; under -race this is the test that says the two do not
// share a variable unsynchronised.
func TestOrphanSidecarSweeperStatusIsReadableBesideTheRunningLoop(t *testing.T) {
	dir, live := strandedTree(t)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	ticked := make(chan struct{}, 1)
	s.SetOnTickComplete(func(int) {
		select {
		case ticked <- struct{}{}:
		default:
		}
	})
	stop := s.Start(context.Background())
	defer stop()

	deadline := time.After(5 * time.Second)
	for {
		if s.Status().Refusing == OrphanRefusalMassOrphans {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the boot tick of a stranded tree never showed as refusing: %+v", s.Status())
		case <-time.After(time.Millisecond):
		}
	}
	<-ticked
	if (*OrphanSidecarSweeper)(nil).Status() != (OrphanSweepStatus{}) {
		t.Error("a nil sweeper's status is not the zero status")
	}
}
