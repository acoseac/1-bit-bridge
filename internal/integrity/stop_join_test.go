package integrity

import (
	"context"
	"testing"
	"time"
)

// TestStopJoinsTheRunGoroutine pins the wait that stopFn gained.
//
// It used to close `done` and return immediately. cmd/bridge defers that stop
// ahead of manifestStore.Close(), which only means something if the stop
// actually waits — otherwise a tick could still be inside DeleteVariant when
// the store closes, which is the "database is closed" / corruption class
// runServe's bgWriters drain exists to prevent.
//
// The assertion is that stopFn does not return before the tick does. A tick
// that blocks until released, plus a stopFn called while it is blocked, makes
// that ordering observable rather than timing-dependent.
func TestStopJoinsTheRunGoroutine(t *testing.T) {
	release := make(chan struct{})
	inTick := make(chan struct{}, 1)
	tickDone := make(chan struct{})

	w := &VariantWatcher{
		interval: time.Hour, // only the boot tick runs
		lister:   blockingLister{inTick: inTick, release: release, done: tickDone},
	}
	stop := w.Start(context.Background())

	select {
	case <-inTick:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot tick never started")
	}

	stopped := make(chan struct{})
	go func() { defer close(stopped); stop() }()

	// stopFn must still be waiting while the tick is blocked.
	select {
	case <-stopped:
		t.Fatal("stopFn returned while a tick was still running — it signals but does not join, " +
			"so Store.Close() can land on a live DeleteVariant")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopFn did not return after the tick finished")
	}
}

// TestStopIsGraceBoundedNotUnconditional pins the other direction: a wedged
// tick must degrade to a delayed exit, never a hung one. Same discipline as
// every other wait in this tree.
func TestStopIsGraceBoundedNotUnconditional(t *testing.T) {
	old := stopGrace
	stopGrace = 50 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })

	wedged := make(chan struct{}) // never closed
	inTick := make(chan struct{}, 1)

	w := &VariantWatcher{
		interval: time.Hour,
		lister:   blockingLister{inTick: inTick, release: wedged, done: make(chan struct{})},
	}
	stop := w.Start(context.Background())
	select {
	case <-inTick:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot tick never started")
	}

	returned := make(chan struct{})
	go func() { defer close(returned); stop() }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stopFn hung on a wedged tick; the wait must be grace-bounded")
	}
}

// blockingLister parks inside AllVariants until released, so a tick's lifetime
// is controllable from the test.
type blockingLister struct {
	inTick  chan struct{}
	release chan struct{}
	done    chan struct{}
}

func (b blockingLister) AllVariants() ([]VariantSnapshot, error) {
	select {
	case b.inTick <- struct{}{}:
	default:
	}
	<-b.release
	return nil, nil
}
