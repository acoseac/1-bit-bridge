package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// stopTakeBackRetry is how long the stop's take-back waits before it asks for
// the install marker's lock again, once a wait for it has given up.
const stopTakeBackRetry = 50 * time.Millisecond

// stopTakeBack is a start's take-back of its count (backlog B660's second
// round), begun as soon as the start's stop arrives: serve's stop (the
// context its caller gave it, which SIGINT, SIGTERM, the SCM's Stop and the
// launcher's own stop cancel) or a restart request, never the context serve
// cancels itself on every exit. Until then the count went back only after the
// whole teardown, which a supervisor that kills a stop it finds too slow
// (launchd past ExitTimeOut, systemd past TimeoutStopSec, the SCM past its
// wait) cut short, so a clean stop stayed counted. A crash in the teardown
// after a stop is not counted, by the same rule: the stop came first.
//
// It asks updater.TakeBackFailedStart, which writes only where the marker
// still holds the count this start wrote, so the take-back after the teardown
// (takeBackFailedStart), kept for the ends that are no stop, a bind the host
// refused and a failure of the tailnet's network, writes nothing once this
// one has landed. A wait for the install marker's lock that gives up is asked
// again until the teardown ends, and the take-back after it is the last try
// (what a holder past both leaves is backlog B760).
type stopTakeBack struct {
	dataDir string
	counted int

	once    sync.Once
	started atomic.Bool
	settled atomic.Bool
	ctx     context.Context
	end     context.CancelFunc
	done    chan struct{}
}

// newStopTakeBack is the stop's take-back of the count counted that the
// admission of this start tried to write in dataDir's install marker.
func newStopTakeBack(dataDir string, counted int) *stopTakeBack {
	ctx, end := context.WithCancel(context.Background())
	return &stopTakeBack{dataDir: dataDir, counted: counted, ctx: ctx, end: end, done: make(chan struct{})}
}

// arrived begins the take-back, once: the start's stop has arrived. Safe from
// any goroutine; after finish it does nothing.
func (b *stopTakeBack) arrived() {
	b.once.Do(func() {
		b.started.Store(true)
		go b.run()
	})
}

// run takes the count back, asking again until it has an answer or the
// teardown ends.
func (b *stopTakeBack) run() {
	defer close(b.done)
	for {
		if _, err := updater.TakeBackFailedStart(b.ctx, b.dataDir, version.ServerVersion, b.counted); err == nil {
			b.settled.Store(true)
			return
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(stopTakeBackRetry):
		}
	}
}

// finish ends the take-back once serve's teardown has run, and joins it, so
// that nothing of it writes once serve lets go of its data dir: it reports
// whether a stop arrived, and whether the take-back had its answer. No
// take-back begins after it.
func (b *stopTakeBack) finish() (arrived, settled bool) {
	b.once.Do(func() {
		// The teardown is over: a stop that arrives now is no stop of this
		// start.
	})
	b.end()
	if !b.started.Load() {
		return false, false
	}
	<-b.done
	return true, b.settled.Load()
}
