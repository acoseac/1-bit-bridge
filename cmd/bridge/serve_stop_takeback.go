package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// stopTakeBackRetry is how long a take-back of a start's count waits before it
// asks for the install marker's lock again, once a wait for it has given up.
const stopTakeBackRetry = 50 * time.Millisecond

// stopBudget is serve's stop's budget, its teardown and the take-back of its
// start's count together, from the moment the stop arrives (backlog B660's
// third round; the second gave the take-back after the teardown a fixed two
// seconds on every path): the take-back asks for the install marker's lock
// until it lands or the budget ends, and so does the clear of the start's
// token after it. It is inside the shortest wait a service manager gives a
// stop before it kills the process or gives up on it: the Windows updater's
// wait for the SCM to stop the service (updater.SCMStopWait, 15 s, waited out
// holding the install marker's lock, so a take-back that meets that lock gives
// up inside it), launchd's ExitTimeOut (20 s by default; the plist `bridge
// init` writes sets none) and systemd's TimeoutStopSec (90 s by default; the
// unit sets none) (TestTheStopBudgetIsInsideEveryServiceManagersWait). An end
// that no stop began (a bind the host refused, a close serve began) has the
// same budget from its teardown's end.
const stopBudget = 10 * time.Second

// stopTakeBack is a start's take-back of its count (backlog B660's rounds 2
// and 3). Once the start has answered as ready (arm, the confirmation's first
// yes), it begins as the start's stop arrives (arrived: serve's caller's
// context, which SIGINT, SIGTERM, the SCM's Stop and the launcher's own stop
// end, or a restart request; never serve's own context, which runServe ends on
// every exit), so a supervisor that kills a stop it finds too slow (launchd
// past ExitTimeOut, systemd past TimeoutStopSec, the SCM past its wait) does
// not leave a clean stop of a ready start counted. Before ready it begins only
// once the teardown has run (finish): a start that hangs before it is ready,
// stopped and killed in a teardown that never ends, stays counted, so the
// third such start's successor rolls the install back (the third round: the
// second began it as any stop arrived, and a version that hung past its
// admission never reached its rollback). A slow but healthy start that a
// supervisor kills in a slow teardown before it was ready counts too, which
// errs toward a rollback that the confirmation of a later start forestalls.
//
// It asks updater.TakeBackFailedStart, which writes only where the marker
// still holds the count this start wrote, and clears the start's token in
// that write, until it has its answer or the stop's budget (stopBudget) ends:
// one budget for the teardown and the take-back together. Whether an end is a
// stop is asked when the teardown has run, of serve's caller's context and
// the restart request as they stand then (finish's takeBack), never of the
// notice arrived has had: the context's AfterFunc runs on a goroutine of its
// own, which an error at once after the stop can beat (the third round).
type stopTakeBack struct {
	dataDir string
	counted int

	mu       sync.Mutex
	armed    bool      // the start answered as ready before any stop
	finished bool      // the teardown has run
	stopAt   time.Time // when the first stop arrived; zero for none yet
	began    bool      // the take-back has begun
	deadline time.Time // the budget's end, once it has begun
	lastErr  error     // why its last ask had no answer

	done    chan struct{}
	settled atomic.Bool
}

// newStopTakeBack is the take-back of the count counted that the admission of
// this start tried to write in dataDir's install marker.
func newStopTakeBack(dataDir string, counted int) *stopTakeBack {
	return &stopTakeBack{dataDir: dataDir, counted: counted, done: make(chan struct{})}
}

// arrived is told that a stop of this start arrived. It records when the first
// did, the budget's start, and begins the take-back where the start had
// answered as ready before it. Safe from any goroutine; after finish it does
// nothing.
func (b *stopTakeBack) arrived() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	if b.stopAt.IsZero() {
		b.stopAt = time.Now()
	}
	if b.armed && !b.began {
		b.beginLocked()
	}
}

// arm is told that this start has answered as ready, ctx being serve's own
// context, which a stop and a restart request both end: where no stop began
// before it (ctx still live, and no arrival told: a restart request tells its
// arrival before it ends ctx), each stop from now on begins the take-back as
// it arrives. It reports whether it armed; a stop that came first leaves it
// unarmed, and that stop's count goes back once the teardown has run.
func (b *stopTakeBack) arm(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished || ctx.Err() != nil || !b.stopAt.IsZero() {
		return false
	}
	b.armed = true
	return true
}

// beginLocked starts the take-back on a goroutine of its own, until it has its
// answer or the budget, from the first stop's arrival, or from now where none
// arrived, ends. b.mu is held.
func (b *stopTakeBack) beginLocked() {
	b.began = true
	from := b.stopAt
	if from.IsZero() {
		from = time.Now()
	}
	b.deadline = from.Add(stopBudget)
	go b.run(b.deadline)
}

// run takes the count back, asking again until it has an answer or deadline
// passes.
func (b *stopTakeBack) run(deadline time.Time) {
	defer close(b.done)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		_, err := updater.TakeBackFailedStart(ctx, b.dataDir, version.ServerVersion, b.counted)
		if err == nil {
			b.settled.Store(true)
			return
		}
		b.mu.Lock()
		b.lastErr = err
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(stopTakeBackRetry):
		}
	}
}

// finish is told that serve's teardown has run. takeBack says whether this end
// takes the start's count back: a stop, asked by the caller of serve's caller's
// context and of the restart request as they stand now, a bind the host
// refused, or a close serve began. It begins the take-back where none has
// begun and the end takes the count back, and joins it, so nothing of it
// writes once serve lets go of its data dir; no take-back begins after it. It
// reports whether the take-back had its answer, why not where it began and
// had none, and the deadline the rest of this start's end keeps to: the
// budget's end, from the first stop's arrival, or from now where none
// arrived.
func (b *stopTakeBack) finish(takeBack bool) (answered bool, missed error, deadline time.Time) {
	b.mu.Lock()
	b.finished = true
	if takeBack && !b.began {
		b.beginLocked()
	}
	began := b.began
	deadline = b.deadline
	b.mu.Unlock()
	if !began {
		return false, nil, time.Now().Add(stopBudget)
	}
	<-b.done
	if b.settled.Load() {
		return true, nil, deadline
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return false, b.lastErr, deadline
}
