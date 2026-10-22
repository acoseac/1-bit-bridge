package hotplug

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// watch is one run of the watcher: its event source, and the goroutine
// (run) that owns everything below and takes every reading, whatever asked
// for it: a burst's window, the periodic tick, an overflow, a follow-up,
// a retry, the start, a changed binding. One reading at a time, so an older
// reading is never published after a newer one.
type watch struct {
	svc     *Service
	src     Source
	session uint64
	cancel  context.CancelFunc
	done    chan struct{}
	poke    chan struct{}

	clock  Clock
	timing Timing

	// device is the binding the last reading resolved.
	device config.PlayerDevice
	// window is the burst of events being waited out.
	window burst
	// pending is a reading due with no window: the start's, an overflow's,
	// a changed binding's, or a retry after a failure (at its backoff).
	pending *trigger
	// nextPeriodic is the safety net's next reading.
	nextPeriodic time.Time
	// followLeft are the follow-ups a transient card may still have,
	// followAt when the next is due (zero for none), and transient the
	// transient cards of the last reading.
	followLeft int
	followAt   time.Time
	transient  []int
	// failures are the failed readings in a row, and retryAt the earliest
	// the next may be taken.
	failures int
	retryAt  time.Time
	// removed are the card directories a removal reached since the last
	// publish; dirs the card directories the last reading and the last
	// snapshot hold, which a removal is matched against.
	removed map[string]bool
	dirs    []string
	// mode is the source's mode as last seen, and lostLogged says the
	// current run of losses has been logged.
	mode       SourceStatus
	lostLogged bool
}

// burst is a window of events: open from the first, read once they have
// been quiet for Timing.Quiet, or Timing.MaxDelay after the first.
type burst struct {
	open        bool
	first, last time.Time
}

// trigger is why a reading is taken. renew grants a transient card its
// follow-ups again.
type trigger struct {
	why   string
	renew bool
	at    time.Time
}

func newWatch(svc *Service, src Source, session uint64, cancel context.CancelFunc) *watch {
	return &watch{svc: svc, src: src, session: session, cancel: cancel, done: make(chan struct{}), poke: make(chan struct{}, 1),
		clock: svc.cfg.Clock, timing: svc.cfg.Timing}
}

// pokeIt tells the watch the config changed, never waiting.
func (w *watch) pokeIt() {
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

// run is the watch's goroutine: it reads the box once at start, and then
// whenever a deadline comes, until ctx ends.
func (w *watch) run(ctx context.Context) {
	now := w.clock.Now()
	w.mode = w.src.Status()
	w.pending = &trigger{why: "start", renew: true, at: now}
	w.nextPeriodic = now.Add(w.timing.Periodic)
	w.dirs = w.svc.feed.lastDirs()
	timer := w.clock.NewTimer()
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			// A stopped watch, abandoned or not, writes nothing more.
			return
		}
		due := w.next()
		timer.Reset(due)
		w.svc.update(func() { w.svc.next = due })
		select {
		case <-ctx.Done():
			return
		case u := <-w.src.Events():
			w.onEvent(u)
		case <-w.src.Notes():
			w.onNote()
		case <-w.poke:
			w.onPoke()
		case <-timer.C():
			w.onTimer(ctx)
		}
	}
}

// next is when the next reading is due: the window's end while a burst is
// open (everything else folds into it), else a pending reading, else the
// earlier of the periodic tick and a follow-up; never before a retry's
// backoff.
func (w *watch) next() time.Time {
	var due time.Time
	switch {
	case w.window.open:
		due = w.window.last.Add(w.timing.Quiet)
		if cap := w.window.first.Add(w.timing.MaxDelay); cap.Before(due) {
			due = cap
		}
	case w.pending != nil:
		due = w.pending.at
	default:
		due = w.nextPeriodic
		if !w.followAt.IsZero() && w.followAt.Before(due) {
			due = w.followAt
		}
	}
	if w.failures > 0 && due.Before(w.retryAt) {
		due = w.retryAt
	}
	return due
}

// onEvent opens or extends the burst's window. A pending reading folds into
// the window: the window's reading reads everything.
func (w *watch) onEvent(u Uevent) {
	now := w.clock.Now()
	if u.removes() {
		w.noteRemoval(u.DevPath)
	}
	if !w.window.open {
		w.window = burst{open: true, first: now}
		w.pending = nil
	}
	w.window.last = now
	w.svc.update(func() { w.svc.stats.Events++ })
}

// noteRemoval records the card directories a removal reached: the card
// itself, a node below it, or the interface, device, hub or controller
// above it. The path is only compared, as a string, with directories the
// readers found; nothing is opened by it.
func (w *watch) noteRemoval(devpath string) {
	p := strings.TrimPrefix(devpath, "/")
	for _, d := range w.dirs {
		if p == d || strings.HasPrefix(d, p+"/") || strings.HasPrefix(p, d+"/") {
			if w.removed == nil {
				w.removed = map[string]bool{}
			}
			w.removed[d] = true
		}
	}
}

// onNote takes the source's news: events lost (read everything, folded
// into an open window, at once otherwise, logged once a run), or a new mode
// (republished at the snapshot's generation).
func (w *watch) onNote() {
	if w.src.TakeLost() {
		if !w.lostLogged {
			w.svc.log.Warn("player hotplug: events were lost (the kernel dropped some, or the watcher fell behind); reading the whole inventory")
			w.lostLogged = true
		}
		if !w.window.open && w.pending == nil {
			w.pending = &trigger{why: "lost", renew: true, at: w.clock.Now()}
		}
		w.svc.update(func() { w.svc.stats.Lost++ })
	}
	if st := w.src.Status(); st.Mode != w.mode.Mode || st.Reason != w.mode.Reason {
		w.mode = st
		w.svc.log.Info("player hotplug: events: " + st.String())
		if w.svc.feed.setSource(w.session, st) {
			w.svc.update(func() { w.svc.stats.Publishes++ })
		}
	}
}

// onPoke takes a change of the config: a binding that changed is read at
// once, or with the window when one is open.
func (w *watch) onPoke() {
	set := w.svc.cfg.Settings()
	if set.Enabled && !sameDevice(set.Device, w.device) && !w.window.open && w.pending == nil {
		w.pending = &trigger{why: "binding", renew: true, at: w.clock.Now()}
	}
	w.svc.update(func() { w.svc.stats.ConfigChanges++ })
}

// onTimer takes the reading that is due.
func (w *watch) onTimer(ctx context.Context) {
	now := w.clock.Now()
	if now.Before(w.next()) {
		return
	}
	var t trigger
	switch {
	case w.window.open:
		t = trigger{why: "events", renew: true}
		w.window = burst{}
	case w.pending != nil:
		t = *w.pending
		w.pending = nil
	case !w.followAt.IsZero() && !now.Before(w.followAt):
		t = trigger{why: "follow-up"}
	default:
		t = trigger{why: "periodic"}
	}
	w.reconcile(ctx, now, t)
}

// reconcile reads the box, resolves the binding, and publishes what
// changed. A reading that fails, or is partial, publishes no answer: the
// last snapshot stands, marked stale, and the next attempt backs off. A
// transient card is followed up, and the binding's answer held back while
// a transient card may be the bound unit, until the follow-ups run out.
func (w *watch) reconcile(ctx context.Context, now time.Time, t trigger) {
	defer w.svc.update(func() { w.svc.stats.Reconciles++ })
	w.nextPeriodic = now.Add(w.timing.Periodic)
	w.followAt = time.Time{}
	w.lostLogged = false
	set := w.svc.cfg.Settings()
	if !set.Enabled {
		// The config turned the player off; the service stops this watch.
		return
	}
	w.device = set.Device
	inv, err := w.svc.cfg.Observe(ctx)
	if ctx.Err() != nil {
		if err == nil {
			w.svc.update(func() { w.svc.stats.Discarded++ })
		}
		return
	}
	if err == nil && inv.Partial {
		err = fmt.Errorf("the reading was partial: %s", strings.Join(inv.Problems, "; "))
	}
	if err != nil {
		w.fail(now, err)
		return
	}
	w.recovered()
	res, settled := binding.Settled(set.Device, inv)
	transient := transientCards(inv)
	if t.renew || (t.why == "periodic" && !slices.Equal(transient, w.transient)) {
		w.followLeft = w.timing.FollowUps
	}
	w.transient = transient
	w.dirs = nil
	for _, c := range inv.Cards {
		if c.Dir != "" {
			w.dirs = append(w.dirs, c.Dir)
		}
	}
	if len(transient) > 0 && w.followLeft > 0 {
		w.followLeft--
		w.followAt = now.Add(w.timing.FollowUpEvery)
		if !settled {
			return
		}
	}
	switch w.svc.feed.offer(w.session, reading{observed: now, inv: inv, res: res, device: set.Device, source: w.mode, removed: w.removed}, w.svc.log) {
	case published, republished:
		w.svc.update(func() { w.svc.stats.Publishes++ })
	case discarded:
		w.svc.update(func() { w.svc.stats.Discarded++ })
	}
	w.removed = nil
	w.dirs = append(w.dirs, w.svc.feed.lastDirs()...)
}

// fail is a reading that failed: counted, the run of failures logged once,
// the last snapshot marked stale, and the next attempt due after a backoff
// that doubles with each failure in a row.
func (w *watch) fail(now time.Time, err error) {
	w.failures++
	backoff := w.timing.backoff(w.failures)
	w.retryAt = now.Add(backoff)
	w.pending = &trigger{why: "retry", renew: true, at: w.retryAt}
	var streak Failure
	first := false
	w.svc.update(func() {
		w.svc.stats.Failures++
		if w.svc.failure == nil {
			w.svc.failure, first = &Failure{Since: now, Err: err.Error()}, true
		}
		streak = *w.svc.failure
	})
	if first {
		w.svc.log.Warn("player hotplug: the box could not be read; the last snapshot stands, marked stale", "err", err.Error(), "retryIn", backoff)
	} else {
		w.svc.log.Debug("player hotplug: the box could not be read again", "err", err.Error(), "failures", w.failures, "retryIn", backoff)
	}
	if w.svc.feed.markStale(w.session, streak) {
		w.svc.update(func() { w.svc.stats.Publishes++ })
	}
}

// recovered ends a run of failures once a reading works.
func (w *watch) recovered() {
	if w.failures == 0 {
		return
	}
	w.svc.log.Info("player hotplug: the box reads again", "failures", w.failures)
	w.failures, w.retryAt = 0, time.Time{}
	if w.pending != nil && w.pending.why == "retry" {
		w.pending = nil
	}
	w.svc.update(func() { w.svc.failure = nil })
}

// transientCards are the indexes of a reading's transient cards.
func transientCards(inv binding.Inventory) []int {
	var out []int
	for _, c := range inv.Cards {
		if c.State == binding.CardTransient {
			out = append(out, c.Index)
		}
	}
	return out
}
