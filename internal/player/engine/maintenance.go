package engine

import (
	"fmt"
	"time"
)

// maintHold is a maintenance hold. Its lease is the holder's alone: the
// snapshot shows the reason, the expiry and whether the release was
// confirmed, never the lease.
type maintHold struct {
	lease          Lease
	reason         string
	since, expires time.Time
	confirmed      bool
	timer          timer
	seq            uint64
	// waiter is the enter, answered once its release settles (deliver).
	waiter *command
	// check is what the DSD check's hold adds (EnterCheck), nil for
	// EnterMaintenance's.
	check *checkHold
}

// checkReason is the reason of the DSD check's hold, which every reader sees
// (MaintenanceInfo.Reason): a snapshot reads maintenance during a check.
const checkReason = "check"

// stoppedBeforeGrant is why a check's grant a Stop came before is refused.
const stoppedBeforeGrant = "a Stop came before the hold was handed over"

// checkHold is what the DSD check's hold adds to a maintenance hold (B728
// B12): its stop signal (Hold.Stop), closed at the hold's first end of any
// kind or its first Stop; the Stops it parks until it ends, in a list of its
// own, never as the hold's one waiter; their deadline, armed as the first
// parks, and whether it has passed; and whether a Stop came before the grant
// was answered.
type checkHold struct {
	stop     chan struct{}
	stops    []*command
	deadline timer
	passed   bool
	stopped  bool
}

// signal closes the hold's stop signal, once.
func (k *checkHold) signal() {
	select {
	case <-k.stop:
	default:
		close(k.stop)
	}
}

// enterMaintenance is EnterMaintenance: the generation checked and every
// mutation closed in this one step (grantHold).
func (e *Engine) enterMaintenance(c *command) {
	o := &e.o
	name := c.op.String()
	switch {
	case o.maint != nil:
		e.answer(c, reply{err: refuse(name, ReasonMaintenance, "maintenance is held already: "+o.maint.reason)})
		return
	case c.want != e.generation():
		e.answer(c, reply{err: &Error{Reason: ReasonConflict, Op: name, Detail: "the state changed since the generation the enter expected", Current: e.generation()}})
		return
	}
	e.grantHold(c, c.reason, nil)
}

// enterCheck is EnterCheck, decided in this one step: refused while a hold is
// held, a configuration reservation is held, a release is unconfirmed or the
// output is unknown or unavailable, and, without interrupt, while the
// transport is anything but stopped with nothing under way (unsettled, the
// reservation's predicate); otherwise granted as EnterMaintenance's hold is.
func (e *Engine) enterCheck(c *command) {
	o := &e.o
	name := c.op.String()
	var err error
	switch u := o.unconfirmed; {
	case o.maint != nil:
		err = refuse(name, ReasonMaintenance, "maintenance is held already: "+o.maint.reason)
	case len(o.configs) > 0:
		err = e.configRefusal(name)
	case u != nil:
		err = refuse(name, ReasonReleaseUnconfirmed, fmt.Sprintf("session %d's release is not confirmed: nothing opens the device until it is", u.Session))
	default:
		err = e.outputRefusal(name)
		if why := e.unsettled(); err == nil && why != "" && !c.on {
			err = refuse(name, ReasonNotStopped, why+": a check interrupts playback only when asked to")
		}
	}
	if err != nil {
		e.answer(c, reply{err: err})
		return
	}
	e.grantHold(c, checkReason, &checkHold{stop: make(chan struct{})})
}

// grantHold grants c a hold for reason, the check's with k: every mutation
// closed in this one step, those parked included; then the session released
// and pending backend work dropped, the grant answered once that release
// settles (deliver).
func (e *Engine) grantHold(c *command, reason string, k *checkHold) {
	o := &e.o
	var l Lease
	for l.IsZero() {
		e.sm.random(l.b[:])
	}
	now := e.clk.Now()
	m := &maintHold{lease: l, reason: reason, since: now, expires: now.Add(e.cfg.MaintenanceTTL), check: k}
	o.maint = m
	e.armMaintenance()
	for _, w := range o.waiters {
		var err error
		if w.play {
			// The outcome already recorded for its playback stands: a reason
			// (playFailed), or nil, the playback having begun (playBegan).
			if err = w.err; !w.decided {
				err = refuse(w.c.op.String(), ReasonMaintenance, "maintenance began: "+reason)
			}
		}
		e.answer(w.c, reply{err: err})
	}
	o.waiters = nil
	for _, p := range o.parked {
		e.answer(p, reply{err: refuse(p.op.String(), ReasonMaintenance, "maintenance began: "+reason)})
	}
	clear(o.parked)
	o.parked = o.parked[:0]
	o.then = IntentStop
	o.state = Maintenance
	e.visible()
	if s := o.sess; s != nil && (s.phase == sessStarting || s.phase == sessLive) {
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "maintenance: " + reason})
	}
	e.discard()
	m.waiter = c
	if o.sess == nil {
		m.confirmed = o.unconfirmed == nil
		e.deliver(m)
	}
}

// deliver answers the enter m holds for, its release settled or past its
// deadline: a plain hold's caller its Hold, confirmed or not; a check's its
// Hold only with that release confirmed and no Stop come first, and otherwise
// why, the hold ended first, so an error leaves its caller nothing to let go:
// ReasonReleaseUnconfirmed, which outranks a Stop, or ReasonStopped.
func (e *Engine) deliver(m *maintHold) {
	c := m.waiter
	m.waiter = nil
	h := Hold{Lease: m.lease, Expires: m.expires, ReleaseConfirmed: m.confirmed}
	switch k := m.check; {
	case k == nil:
	case !m.confirmed:
		e.answer(c, reply{err: refuse(c.op.String(), ReasonReleaseUnconfirmed,
			"the release of what the check interrupted was not confirmed by its deadline: nothing opens the device until it is")})
		e.endMaintenance("", "")
		return
	case k.stopped:
		e.answer(c, reply{err: refuse(c.op.String(), ReasonStopped, stoppedBeforeGrant)})
		e.endMaintenance("", "")
		return
	default:
		h.Stop = k.stop
	}
	e.answer(c, reply{hold: h})
}

// armMaintenance arms the hold's expiry, replacing the timer before it.
func (e *Engine) armMaintenance() {
	m := e.o.maint
	if m.timer != nil {
		m.timer.Stop()
	}
	m.seq++
	seq := m.seq
	m.timer = e.clk.At(m.expires, func() { e.post(msgMaintExpiry{seq: seq}) })
}

// renewMaintenance is RenewMaintenance.
func (e *Engine) renewMaintenance(c *command) {
	m := e.o.maint
	if m == nil || c.lease.IsZero() || c.lease != m.lease {
		e.answer(c, reply{err: refuse(c.op.String(), ReasonLease, "not the lease of the hold")})
		return
	}
	m.expires = e.clk.Now().Add(e.cfg.MaintenanceTTL)
	e.armMaintenance()
	e.touched()
	e.answer(c, reply{expires: m.expires})
}

// exitMaintenance is ExitMaintenance. Its lease is the delivered grant's, so
// no grant is waiting.
func (e *Engine) exitMaintenance(c *command) {
	m := e.o.maint
	if m == nil || c.lease.IsZero() || c.lease != m.lease {
		e.answer(c, reply{err: refuse(c.op.String(), ReasonLease, "not the lease of the hold")})
		return
	}
	e.endMaintenance("", "")
	e.answer(c, reply{})
}

// maintenanceExpiry ends a hold whose expiry has come.
func (e *Engine) maintenanceExpiry(seq uint64) {
	m := e.o.maint
	if m == nil || seq != m.seq || e.clk.Now().Before(m.expires) {
		return
	}
	e.log.Warn("player: maintenance hold expired; the transport is stopped", "reason", m.reason,
		"held_for", fmt.Sprint(e.clk.Now().Sub(m.since)))
	e.endMaintenance(ReasonLease, "the hold expired before it was handed over")
}

// endMaintenance ends the hold: the transport stopped, never resumed. A grant
// not answered yet is answered: a plain hold's entered, its release
// unconfirmed; a check's with why (its expiry, its output gone), or
// ReasonStopped once a Stop came first, never with a lease already gone. A
// check's hold lets its Stops go (releaseStops).
func (e *Engine) endMaintenance(why Reason, detail string) {
	o := &e.o
	m := o.maint
	m.timer.Stop()
	if c := m.waiter; c != nil {
		m.waiter = nil
		r := reply{hold: Hold{Lease: m.lease, Expires: m.expires, ReleaseConfirmed: false}}
		if k := m.check; k != nil {
			if k.stopped {
				why, detail = ReasonStopped, stoppedBeforeGrant
			}
			r = reply{err: refuse(c.op.String(), why, detail)}
		}
		e.answer(c, r)
	}
	if k := m.check; k != nil {
		e.releaseStops(k, nil)
	}
	o.maint = nil
	if o.sess != nil && o.sess.phase == sessReleasing {
		o.state = Releasing
	} else {
		o.state = Stopped
	}
	e.visible()
}

// checkStop is a Stop under a check's hold, which it ends: it closes the
// hold's stop signal and waits for the check to let go, the hold's end, at
// most ReleaseTimeout from the first Stop parked (the hold's own timer, on the
// engine's clock), as a Stop waits for its release; past that deadline a Stop
// is answered at once. One that comes before the grant is answered marks the
// hold stopped, which its release's settling then ends (deliver).
func (e *Engine) checkStop(c *command) {
	m := e.o.maint
	k := m.check
	k.signal()
	if m.waiter != nil {
		k.stopped = true
	}
	if k.passed {
		e.answer(c, reply{})
		return
	}
	k.stops = append(k.stops, c)
	if k.deadline == nil {
		k.deadline = e.clk.AfterFunc(ReleaseTimeout, func() { e.post(msgCheckStops{m: m}) })
	}
}

// checkStopsDue is the deadline of the Stops m's check parked: each is
// answered success, and every later Stop under the hold at once. The deadline
// of a hold that has ended, which a timer that fired before that end can
// bring, changes nothing.
func (e *Engine) checkStopsDue(m *maintHold) {
	if e.o.maint != m {
		return
	}
	m.check.passed = true
	e.releaseStops(m.check, nil)
}

// releaseStops lets go of the Stops k parked, at the hold's end or their
// deadline: its stop signal closed, their deadline stopped, and each answered
// success, as a Stop answers once its release is confirmed or past its
// deadline, or, at the shutdown, closed's refusal.
func (e *Engine) releaseStops(k *checkHold, closed func(c *command) error) {
	k.signal()
	if k.deadline != nil {
		k.deadline.Stop()
	}
	for _, s := range k.stops {
		var err error
		if closed != nil {
			err = closed(s)
		}
		e.answer(s, reply{err: err})
	}
	k.stops = nil
}
