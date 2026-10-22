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
	// waiter is the enter, answered once its release settles.
	waiter *command
}

// enterMaintenance is EnterMaintenance: the generation checked and every
// mutation closed in this one step, those parked included; then the session
// released and pending backend work dropped.
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
	var l Lease
	for l.IsZero() {
		e.sm.random(l.b[:])
	}
	now := e.clk.Now()
	m := &maintHold{lease: l, reason: c.reason, since: now, expires: now.Add(e.cfg.MaintenanceTTL)}
	o.maint = m
	e.armMaintenance()
	for _, w := range o.waiters {
		var err error
		if w.play && !w.played {
			// A reason already recorded for its playback stands (playFailed);
			// a playback that began answers nil (playBegan).
			if err = w.err; err == nil {
				err = refuse(w.c.op.String(), ReasonMaintenance, "maintenance began: "+c.reason)
			}
		}
		e.answer(w.c, reply{err: err})
	}
	o.waiters = nil
	for _, p := range o.parked {
		e.answer(p, reply{err: refuse(p.op.String(), ReasonMaintenance, "maintenance began: "+c.reason)})
	}
	clear(o.parked)
	o.parked = o.parked[:0]
	o.then = IntentStop
	o.state = Maintenance
	e.visible()
	if s := o.sess; s != nil && (s.phase == sessStarting || s.phase == sessLive) {
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "maintenance: " + c.reason})
	}
	e.discard()
	if o.sess != nil {
		m.waiter = c
		return
	}
	m.confirmed = o.unconfirmed == nil
	e.answer(c, reply{hold: Hold{Lease: l, Expires: m.expires, ReleaseConfirmed: m.confirmed}})
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

// exitMaintenance is ExitMaintenance.
func (e *Engine) exitMaintenance(c *command) {
	m := e.o.maint
	if m == nil || c.lease.IsZero() || c.lease != m.lease {
		e.answer(c, reply{err: refuse(c.op.String(), ReasonLease, "not the lease of the hold")})
		return
	}
	e.endMaintenance()
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
	e.endMaintenance()
}

// endMaintenance ends the hold: the transport stopped, never resumed.
func (e *Engine) endMaintenance() {
	o := &e.o
	m := o.maint
	m.timer.Stop()
	if m.waiter != nil {
		e.answer(m.waiter, reply{hold: Hold{Lease: m.lease, Expires: m.expires, ReleaseConfirmed: false}})
		m.waiter = nil
	}
	o.maint = nil
	if o.sess != nil && o.sess.phase == sessReleasing {
		o.state = Releasing
	} else {
		o.state = Stopped
	}
	e.visible()
}
