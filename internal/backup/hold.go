package backup

import (
	"context"
	"sync"
	"time"
)

// A hold is one tenure of a backups folder's lock (stuck.go): its holder runs
// each step under the lock through stepOf, waited for until its bound, and a
// step that overruns it is given up (giveUp) and the folder marked, the mark
// standing until every hold is let go. A verification's holder is its worker
// (verifyHeld), which also holds this process's slot and the snapshot's
// ownership; abandonPending and a claim hold the lock alone.
type hold struct {
	// ctx is the holder's caller's, ended at a give-up too, so a step still
	// running that asks it stops at its next ask (the prune, between its
	// removals); stop is the caller's own stop.
	ctx    context.Context
	cancel context.CancelFunc
	stop   <-chan struct{}

	mu        sync.Mutex
	spellings []string
	key       string
	root      string
	snapshot  string
	at        string
	locked    time.Time
	unlock    func()
	waits     []<-chan struct{}
	markWaits []<-chan struct{}
	given     bool
	mark      *stuckMark
	ended     bool
}

// The steps a hold names when one is given up, in the doctor's and the
// errors' words.
const (
	stepLockWait   = "its wait for the snapshots' lock"
	stepReadRecord = "its read of the snapshot's record"
	stepVeto       = "putting its veto in place"
	stepCheck      = "its check"
	stepWrite      = "its verdict's write"
	stepPrune      = "the prune after it"
	stepReadBack   = "its read of the record after it"
	stepIncomplete = "its write of INCOMPLETE"
	stepClaim      = "its claim"
)

// newHold is a hold for the snapshot named, on the folder reached by the
// spellings given (spellingKey's), its caller's context ctx; no lock is held
// yet (held).
func newHold(ctx context.Context, snapshot string, spellings ...string) *hold {
	h := &hold{snapshot: snapshot, at: stepLockWait, stop: ctx.Done()}
	h.ctx, h.cancel = context.WithCancel(ctx)
	for _, s := range spellings {
		if s != "" {
			h.spellings = append(h.spellings, spellingKey(s))
		}
	}
	return h
}

// held records the folder's lock taken now, on root, resolved, whose key is
// key, for the snapshot named.
func (h *hold) held(unlock func(), root, key, snapshot string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unlock, h.root, h.key, h.snapshot, h.locked = unlock, root, key, snapshot, time.Now()
}

// in records the step the holder is in, outside stepOf, which a give-up by
// its caller names (abandon).
func (h *hold) in(step string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.at = step
}

// lockedAt is when the hold took the folder's lock.
func (h *hold) lockedAt() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.locked
}

// track adds done, a goroutine's end, to what the hold waits for before it
// lets the lock go.
func (h *hold) track(done <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waits = append(h.waits, done)
}

// giveUp gives the hold up at the step named, done (when not nil) closing as
// that step's goroutine ends, which the hold then waits for: the hold's
// context ended, and the folder marked, once, with no filesystem asked
// (markStuck), unless every hold is let go already.
func (h *hold) giveUp(done <-chan struct{}, step string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if done != nil {
		h.waits = append(h.waits, done)
	}
	if h.given {
		return
	}
	h.given = true
	h.cancel()
	if h.ended {
		return
	}
	keys := append([]string(nil), h.spellings...)
	if h.key != "" {
		keys = append(keys, h.key, h.root)
	}
	h.mark = markStuck(keys, h.root, StuckCheck{Snapshot: h.snapshot, Step: step, Since: time.Now().UTC()})
}

// abandon is the caller's give-up of a holder it stopped waiting for, at the
// step it is in.
func (h *hold) abandon() {
	h.mu.Lock()
	step := h.at
	h.mu.Unlock()
	h.giveUp(nil, step)
}

// gaveUp reports whether the hold was given up.
func (h *hold) gaveUp() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.given
}

// marked is the hold's mark, if one was raised.
func (h *hold) marked() (StuckCheck, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mark == nil {
		return StuckCheck{}, false
	}
	return h.mark.check, true
}

// release waits for every goroutine the hold tracks to end, then removes its
// marker and lets the folder's lock go, once: the lock is held until the last
// step given up has ended, whenever that is.
func (h *hold) release() {
	h.mu.Lock()
	waits := h.waits
	h.mu.Unlock()
	for _, w := range waits {
		<-w
	}
	h.mu.Lock()
	mark, unlock := h.mark, h.unlock
	h.unlock = nil
	h.mu.Unlock()
	mark.removeMarker()
	if unlock != nil {
		unlock()
	}
}

// keepMarkUntil keeps a mark the hold raises standing until done closes,
// though the hold itself is let go: a write its holder started and left to
// end on its own (abandonPending) holds the folder past it.
func (h *hold) keepMarkUntil(done <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markWaits = append(h.markWaits, done)
}

// drop takes the hold's mark out of this process's record, once whatever
// keepMarkUntil names has ended, and keeps one from being raised after it:
// the last thing its holder does, once every hold, the slot and the
// ownership included, is let go.
func (h *hold) drop() {
	h.mu.Lock()
	waits := h.markWaits
	h.mu.Unlock()
	for _, w := range waits {
		<-w
	}
	h.mu.Lock()
	h.ended = true
	mark := h.mark
	h.mu.Unlock()
	mark.drop()
}

// stepOf runs step under h on a goroutine of its own, the step named words,
// and waits for its answer until by, or StopWriteBound past the caller's stop
// where that comes first. A step that has not answered then is given up
// (giveUp) and answered false, left to end on its own while h waits for it
// before it lets the lock go; what it answers late is dropped, or handed to
// late where that is set. A panic in the step is raised again on the caller's
// goroutine.
func stepOf[T any](h *hold, by time.Time, words string, step func(context.Context) T, late func(T)) (T, bool) {
	type answer struct {
		v        T
		panicked any
		ok       bool
	}
	h.in(words)
	out := make(chan answer, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				out <- answer{panicked: r}
			}
		}()
		out <- answer{v: step(h.ctx), ok: true}
	}()
	timer := time.NewTimer(time.Until(by))
	defer timer.Stop()
	stop := h.stop
	for {
		select {
		case a := <-out:
			if !a.ok {
				panic(a.panicked)
			}
			return a.v, true
		case <-stop:
			stop = nil
			if time.Until(by) > StopWriteBound {
				timer.Reset(StopWriteBound)
			}
		case <-timer.C:
			h.giveUp(done, words)
			if late != nil {
				go func() {
					if a := <-out; a.ok {
						late(a.v)
					}
				}()
			}
			var zero T
			return zero, false
		}
	}
}

// laterBound is the bound of a step after a check: half of StopWriteBound
// past the later of the hold's deadline and now, so a step begun once the
// deadline has come still has that much.
func laterBound(deadline time.Time) time.Time {
	from := time.Now()
	if deadline.After(from) {
		from = deadline
	}
	return from.Add(StopWriteBound / 2)
}
