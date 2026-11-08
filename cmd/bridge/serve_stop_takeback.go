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

// finishTakeBackFloor is how long, at least, a take-back begun once the
// teardown has run asks for the install marker's lock, where that teardown ran
// past the stop's budget with nothing killing the process (backlog B660's
// fourth round: its deadline was the budget's from the stop's arrival, past by
// then, so it gave up at the lock's first refusal, and three such clean stops
// before ready rolled a working install back).
const finishTakeBackFloor = 2 * time.Second

// stopExitMargin is how long before updater.SCMStopWait from the stop's
// arrival a take-back begun once the teardown has run gives up at the latest,
// leaving the process that long to exit inside the Windows updater's wait for
// the service to stop, which it waits out holding the install marker's lock.
const stopExitMargin = time.Second

// finishTakeBackDeadline is the deadline of a take-back begun at now, once the
// teardown has run: the stop's budget from its arrival stopAt (now where no
// arrival was told: a bind the host refused, a close serve began, or a stop
// whose notice had not run), but at least finishTakeBackFloor from now, and
// never past updater.SCMStopWait less stopExitMargin from the arrival.
func finishTakeBackDeadline(stopAt, now time.Time) time.Time {
	if stopAt.IsZero() {
		stopAt = now
	}
	deadline := stopAt.Add(stopBudget)
	if floor := now.Add(finishTakeBackFloor); floor.After(deadline) {
		deadline = floor
	}
	if ceiling := stopAt.Add(updater.SCMStopWait - stopExitMargin); deadline.After(ceiling) {
		deadline = ceiling
	}
	return deadline
}

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
// A panic on serve's goroutine ends a start uncleanly, whatever came before
// it (backlog B760): its end takes nothing back, and a take-back that began as
// the stop arrived is undone (countAgain).
//
// It asks updater.TakeBackFailedStart, which writes only where the marker
// still holds the count this start wrote, and clears the start's token in
// that write, until it has its answer or the stop's budget (stopBudget) ends:
// one budget for the teardown and the take-back together, but that a take-back
// begun once a teardown longer than the budget has run asks for
// finishTakeBackFloor still, inside the SCM's wait (the fourth round). Whether an end is a
// stop is asked when the teardown has run, of serve's caller's context and
// the restart request as they stand then (finish's takeBack), never of the
// notice arrived has had: the context's AfterFunc runs on a goroutine of its
// own, which an error at once after the stop can beat (the third round).
type stopTakeBack struct {
	dataDir string
	counted int
	// save and asked are a boot test's (serveOpts.saveTakeBack and
	// takeBackAsked): nil in production.
	save  func(dataDir string, st updater.State) error
	asked func(reached bool, err error)

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
// this start tried to write in dataDir's install marker, with opts' seams.
func newStopTakeBack(dataDir string, counted int, opts serveOpts) *stopTakeBack {
	return &stopTakeBack{dataDir: dataDir, counted: counted, save: opts.saveTakeBack, asked: opts.takeBackAsked,
		done: make(chan struct{})}
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
		b.beginLocked(b.stopAt.Add(stopBudget))
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
// answer or deadline passes: the budget from the stop's arrival for one begun
// as the stop arrives, finishTakeBackDeadline's for one begun once the
// teardown has run. b.mu is held.
func (b *stopTakeBack) beginLocked(deadline time.Time) {
	b.began = true
	b.deadline = deadline
	go b.run(deadline)
}

// run takes the count back, asking again until it has an answer or deadline
// passes.
func (b *stopTakeBack) run(deadline time.Time) {
	defer close(b.done)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		reached, err := updater.TakeBackFailedStart(ctx, b.dataDir, version.ServerVersion, b.counted, b.save)
		if b.asked != nil {
			b.asked(reached, err)
		}
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
// begun and the end takes the count back (finishTakeBackDeadline: at least
// finishTakeBackFloor from now, below the SCM's wait from the stop's arrival),
// and joins it, so nothing of it writes once serve lets go of its data dir; no
// take-back begins after it. It reports whether the take-back had its answer,
// why not where it began and had none, and the deadline the rest of this
// start's end keeps to: the take-back's, or the budget's from now where none
// began.
func (b *stopTakeBack) finish(takeBack bool) (answered bool, missed error, deadline time.Time) {
	b.mu.Lock()
	b.finished = true
	if takeBack && !b.began {
		b.beginLocked(finishTakeBackDeadline(b.stopAt, time.Now()))
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

// countAgain counts this start again where its take-back began as its stop
// arrived and the start then ended in a panic on serve's goroutine (backlog
// B760): the start had answered as ready, so its count went back as the stop
// arrived, which the panic that ended it after came too late to stop. It is
// asked once finish has joined the take-back, whatever that answered, and
// writes only where the take-back's own write stands
// (updater.CountFailedStartAgain), asking again until it has an answer or its
// deadline passes: finishTakeBackDeadline's, at least finishTakeBackFloor from
// now and below the SCM's wait from the stop's arrival. tried says a take-back
// had begun, so that there was one to undo; counted, that this wrote the count
// back; err, why it had no answer.
func (b *stopTakeBack) countAgain() (tried, counted bool, err error) {
	b.mu.Lock()
	began, stopAt := b.began, b.stopAt
	b.mu.Unlock()
	if !began {
		return false, false, nil
	}
	ctx, cancel := context.WithDeadline(context.Background(), finishTakeBackDeadline(stopAt, time.Now()))
	defer cancel()
	for {
		wrote, err := updater.CountFailedStartAgain(ctx, b.dataDir, version.ServerVersion, b.counted)
		if err == nil {
			return true, wrote, nil
		}
		select {
		case <-ctx.Done():
			return true, false, err
		case <-time.After(stopTakeBackRetry):
		}
	}
}
