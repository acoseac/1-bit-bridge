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
// ownership; abandonPending, a claim and a prune hold the lock alone, a claim
// the ownership it took until its caller has it. What a holder lets go of at
// its end is bounded too (release): the snapshot's ownership, a step of the
// hold before the lock goes (alsoLetGo), then the lock itself.
type hold struct {
	// ctx is the holder's caller's, ended at a give-up and at the hold's end
	// too, so a step still running that asks it stops at its next ask (the
	// prune, between its removals); stop is the caller's own stop.
	ctx    context.Context
	cancel context.CancelFunc
	stop   <-chan struct{}

	mu        sync.Mutex
	spellings []string
	key       string
	root      string
	snapshot  string
	at        string
	// step is the give-up of the step the holder is in (stepOf), which a
	// give-up of the hold ends too.
	step      *stepSignal
	locked    time.Time
	unlock    func()
	waits     []<-chan struct{}
	markWaits []<-chan struct{}
	given     bool
	// markless: a give-up of the hold by its caller (abandon) raises no mark,
	// since the holder waits for a write it handed to a hold of its own
	// (handOff), or has ended its wait; a step of the hold that overruns marks
	// all the same.
	markless bool
	// releasing: the hold has begun to let its lock go (release); a give-up
	// after it marks the folder in this process alone, writing no marker.
	releasing bool
	// ends is what the hold lets go of before its lock (alsoLetGo).
	ends  []func()
	mark  *stuckMark
	ended bool
}

// stepSignal is the give-up of one step under a hold: ended once that step
// is given up, by its own bound or by its holder's caller while it runs, and,
// for a verdict's write alone (publishOf), at its admission where the hold was
// given up before it: never by its caller's stop, so the write of an
// INCOMPLETE (stepIncomplete), begun after another step's give-up, still
// lands, and the record is still read back (stepReadBack), while a write that
// asks it (publishVerdict) lands nothing once it is given up itself.
type stepSignal struct {
	ctx    context.Context
	cancel context.CancelFunc
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
	stepPruning    = "its removals"
	stepOwnership  = "letting go of its snapshot's ownership"
	stepRelease    = "letting go of the snapshots' lock"
)

// releaseBound is how long letting a hold's lock go may take (release)
// before the hold is given up, a test's shorter one aside
// (SetReleaseBoundForTest).
var releaseBound = StopWriteBound

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

// handOff records that the holder now waits for a write it handed to a hold
// of its own (abandonPending): a give-up of this hold by its caller from then
// on raises no mark, since nothing this hold took overran (its caller's
// give-up may come as the holder answers, and find it letting go), and the
// other hold marks the folder itself once its own step overruns. A step of
// this hold's release that overruns marks the folder all the same.
func (h *hold) handOff() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markless = true
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
// that step's goroutine ends, which the hold then waits for: the step it is
// in given up with it (its stepSignal ended), the hold's context ended, and
// the folder marked, once, with no filesystem asked (markStuck), unless every
// hold is let go already; once the hold has begun to let its lock go, the
// mark is this process's alone, with no marker written.
func (h *hold) giveUp(done <-chan struct{}, step string) {
	h.giveUpAs(done, step, false)
}

// giveUpAs is giveUp, byCaller saying the give-up is the holder's caller's
// (abandon), which raises no mark where the holder handed its write off
// (handOff).
func (h *hold) giveUpAs(done <-chan struct{}, step string, byCaller bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if done != nil {
		h.waits = append(h.waits, done)
	}
	if h.step != nil {
		h.step.cancel()
	}
	if !h.given {
		h.given = true
		h.cancel()
	}
	if h.mark != nil || h.ended || (byCaller && h.markless) {
		return
	}
	keys := append([]string(nil), h.spellings...)
	if h.key != "" {
		keys = append(keys, h.key, h.root)
	}
	root := h.root
	if h.releasing {
		root = ""
	}
	h.mark = markStuck(keys, root, StuckCheck{Snapshot: h.snapshot, Step: step, Since: time.Now().UTC()})
}

// abandon is the caller's give-up of a holder it stopped waiting for, at the
// step it is in.
func (h *hold) abandon() {
	h.mu.Lock()
	step := h.at
	h.mu.Unlock()
	h.giveUpAs(nil, step, true)
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

// alsoLetGo adds end, what letting go of another of the holder's holds takes
// (a capture's ownership: Capture.Release), to what the hold lets go of
// before its lock (release); a nil end adds nothing.
func (h *hold) alsoLetGo(end func()) {
	if end == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, end)
}

// waitTracked waits for every goroutine the hold tracks to end, those it
// comes to track while it waits included.
func (h *hold) waitTracked() {
	for n := 0; ; {
		h.mu.Lock()
		waits := h.waits[n:]
		h.mu.Unlock()
		if len(waits) == 0 {
			return
		}
		for _, w := range waits {
			<-w
		}
		n += len(waits)
	}
}

// release lets go of what the hold holds, once each, in this order: it waits
// for every goroutine the hold tracks to end, so the lock is held until the
// last step given up has ended, whenever that is; it lets go of what
// alsoLetGo added, each a step of the hold bounded releaseBound from its
// start, past which the hold is given up (stepOwnership) and keeps its lock,
// marked, until that step has ended; and then it removes its marker and lets
// the folder's lock go. Its start of that last part is recorded in the
// section that takes the mark and the lock's release, so no give-up after it
// writes a marker; and the removal and the lock's let-go are bounded
// (releaseBound) too, past which the hold is given up (stepRelease), its mark
// standing in this process until they have ended (keepMarkUntil).
func (h *hold) release() {
	h.waitTracked()
	h.mu.Lock()
	ends := h.ends
	h.ends = nil
	h.mu.Unlock()
	for _, end := range ends {
		stepOf(h, time.Now().Add(releaseBound), stepOwnership, func(context.Context, context.Context) struct{} {
			end()
			return struct{}{}
		}, nil)
	}
	h.waitTracked()
	h.mu.Lock()
	mark, unlock := h.mark, h.unlock
	h.unlock = nil
	h.releasing = true
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mark.removeMarker()
		if unlock != nil {
			unlock()
		}
	}()
	t := time.NewTimer(releaseBound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		h.keepMarkUntil(done)
		h.giveUp(nil, stepRelease)
	}
}

// keepMarkUntil keeps a mark the hold raises standing until done closes,
// though the hold itself is let go: a lock's let-go that overran its bound
// (release).
func (h *hold) keepMarkUntil(done <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markWaits = append(h.markWaits, done)
}

// drop takes the hold's mark out of this process's record, once whatever
// keepMarkUntil names has ended, keeps one from being raised after it, and
// ends the hold's context: the last thing its holder does, once every hold,
// the slot and the ownership included, is let go.
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
	h.cancel()
}

// stepOf runs step under h on a goroutine of its own, the step named words,
// and waits for its answer until by, or StopWriteBound past the caller's stop
// where that comes first. The step is handed the hold's context and its own
// give-up (stepSignal), which only a give-up of this step ends. A step that
// has not answered then is given up (giveUp) and answered false, left to end
// on its own while h waits for it before it lets the lock go; what it answers
// late is dropped, or handed to late where that is set, which h waits for
// too. A panic in the step is raised again on the caller's goroutine.
func stepOf[T any](h *hold, by time.Time, words string, step func(ctx, given context.Context) T, late func(T)) (T, bool) {
	v, _, answered := runStep(h, by, words, false, step, late)
	return v, answered
}

// publishOf runs a verdict's write, write, as a step of h (stepWrite), as
// stepOf does, admitted only where h was not given up as the step is
// registered (backlog B653's sixth round): the check of the give-up and the
// registration of the step's signal are one section of the hold's lock, with
// no I/O in it, so a give-up comes either before it, and the write is not
// admitted (admitted false: write never runs, and its holder writes its
// INCOMPLETE in a step of its own), or after it, and ends the write's signal
// (giveUpAs), which write asks before its rename and before each removal of a
// veto.
func publishOf[T any](h *hold, by time.Time, write func(given context.Context) T) (v T, admitted, answered bool) {
	return runStep(h, by, stepWrite, true, func(_, given context.Context) T { return write(given) }, nil)
}

// onAdmission, nil in production, runs while a verdict's write is admitted,
// inside the section of the hold's lock that decides it, between the check of
// the give-up and the registration of the step: a test parks a give-up
// against it, which must wait for the section to end.
var onAdmission func()

// runStep is stepOf, and publishOf where publish is set: a verdict's write,
// admitted only where the hold is not given up as its step is registered.
func runStep[T any](h *hold, by time.Time, words string, publish bool, step func(ctx, given context.Context) T, late func(T)) (v T, admitted, answered bool) {
	type answer struct {
		v        T
		panicked any
		ok       bool
	}
	sig := &stepSignal{}
	sig.ctx, sig.cancel = context.WithCancel(context.Background())
	h.mu.Lock()
	if publish && h.given {
		h.mu.Unlock()
		sig.cancel()
		return v, false, true
	}
	if publish && onAdmission != nil {
		onAdmission()
	}
	h.at, h.step = words, sig
	h.mu.Unlock()
	out := make(chan answer, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				out <- answer{panicked: r}
			}
		}()
		out <- answer{v: step(h.ctx, sig.ctx), ok: true}
	}()
	timer := time.NewTimer(time.Until(by))
	defer timer.Stop()
	stop := h.stop
	for {
		select {
		case a := <-out:
			h.mu.Lock()
			if h.step == sig {
				h.step = nil
			}
			h.mu.Unlock()
			sig.cancel()
			if !a.ok {
				panic(a.panicked)
			}
			return a.v, true, true
		case <-stop:
			stop = nil
			if time.Until(by) > StopWriteBound {
				timer.Reset(StopWriteBound)
			}
		case <-timer.C:
			if late != nil {
				lateDone := make(chan struct{})
				go func() {
					defer close(lateDone)
					if a := <-out; a.ok {
						late(a.v)
					}
				}()
				h.track(lateDone)
			}
			h.giveUp(done, words)
			return v, true, false
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
