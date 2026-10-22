package engine

import (
	"errors"
	"fmt"
	"io/fs"
)

// insert is InsertAfter.
func (e *Engine) insert(c *command) {
	o := &e.o
	name := c.op.String()
	switch {
	case c.ref.IsZero():
		e.answer(c, reply{err: refuse(name, ReasonBadReference, "the zero Reference names nothing")})
	case c.id != 0 && !o.q.has(c.id):
		e.answer(c, reply{err: refuse(name, ReasonIDNotFound, fmt.Sprintf("no entry %d to insert after", c.id))})
	case c.metaLen > MaxMetadataBytes:
		e.answer(c, reply{err: refuse(name, ReasonMetadataTooLarge, fmt.Sprintf("%d bytes of metadata, more than %d", c.metaLen, MaxMetadataBytes))})
	case o.q.len() >= MaxEntries:
		e.answer(c, reply{err: refuse(name, ReasonFull, fmt.Sprintf("the queue holds %d entries", MaxEntries))})
	case o.q.meta+len(c.meta) > MaxTotalMetadataBytes:
		e.answer(c, reply{err: refuse(name, ReasonFull, fmt.Sprintf("the queue holds %d bytes of metadata and takes at most %d", o.q.meta, MaxTotalMetadataBytes))})
	default:
		if !e.values(c, true) {
			return
		}
		id := o.res.ids.take()
		o.token = o.res.tokens.take()
		empty := o.q.len() == 0
		o.q = o.q.insertAfter(c.id, entry{id: id, ref: c.ref, meta: c.meta})
		o.pass = nil
		e.saved()
		if empty {
			e.selectEntry(id)
		}
		e.ahead()
		e.reconcile(o.sess)
		e.answer(c, reply{id: id})
	}
}

// delete is Delete. Deleting the selection selects the entry that followed
// it: while playing it plays, while paused the transport stays paused at
// its start with the paused content dropped, and with nothing after it
// playback stops and nothing is selected.
func (e *Engine) delete(c *command) {
	o := &e.o
	if !o.q.has(c.id) {
		e.answer(c, reply{err: refuse(c.op.String(), ReasonIDNotFound, fmt.Sprintf("no entry %d", c.id))})
		return
	}
	if !e.values(c, false) {
		return
	}
	o.token = o.res.tokens.take()
	selected := o.selected == c.id
	var next uint32
	if selected {
		if next = o.q.after(c.id, o.repeat); next == c.id {
			next = 0
		}
	}
	o.q = o.q.remove(c.id)
	o.pass = nil
	if _, ok := o.skips[c.id]; ok {
		e.setSkips(func(m map[uint32]Skip) { delete(m, c.id) })
	}
	e.saved()
	e.ahead()
	if selected {
		e.selectionDeleted(next)
	} else {
		e.reconcile(o.sess)
	}
	e.answer(c, reply{})
}

// selectionDeleted moves the selection to next, the entry that followed a
// deleted selection, and the transport as delete says.
func (e *Engine) selectionDeleted(next uint32) {
	o := &e.o
	e.selectEntry(next)
	s := o.sess
	live := s != nil && (s.phase == sessStarting || s.phase == sessLive)
	switch {
	case o.state == Playing && live:
		if next != 0 {
			e.releaseSession(s, IntentPlay, StopInfo{})
		} else {
			e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "the entry playing was deleted, and nothing follows it"})
		}
	case o.state == Paused && live:
		if next != 0 {
			e.releaseSession(s, IntentPause, StopInfo{})
		} else {
			e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "the paused entry was deleted, and nothing follows it"})
		}
	case o.state == Paused && next == 0:
		o.state = Stopped
		e.stopped(StopInfo{Reason: ReasonStopped, Detail: "the paused entry was deleted, and nothing follows it"})
	case o.state == Releasing && next == 0 && o.then != IntentStop:
		e.setIntent(IntentStop, "the entry to play was deleted")
	}
}

// deleteAll is DeleteAll: the queue emptied, nothing selected, playback
// stopped.
func (e *Engine) deleteAll(c *command) {
	o := &e.o
	if o.q.len() > 0 {
		if !e.values(c, false) {
			return
		}
		o.token = o.res.tokens.take()
		o.q, o.pass = emptyQueue, nil
		if o.skips != nil {
			e.setSkips(func(m map[uint32]Skip) { clear(m) })
		}
		e.saved()
		e.ahead()
	}
	e.selectEntry(0)
	s := o.sess
	switch {
	case s != nil && (s.phase == sessStarting || s.phase == sessLive):
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "the queue was emptied"})
	case o.state == Paused:
		o.state = Stopped
		e.stopped(StopInfo{Reason: ReasonStopped, Detail: "the queue was emptied"})
	case o.state == Releasing:
		e.setIntent(IntentStop, "the queue was emptied")
	}
	e.answer(c, reply{})
}

// setRepeat is SetRepeat.
func (e *Engine) setRepeat(c *command) {
	o := &e.o
	if o.repeat != c.on {
		o.repeat = c.on
		e.saved()
		e.reconcile(o.sess)
	}
	e.answer(c, reply{})
}

// setShuffle is SetShuffle.
func (e *Engine) setShuffle(c *command) {
	if c.on {
		e.answer(c, reply{err: refuse(c.op.String(), ReasonNotInRelease, "shuffle is release 7a's")})
		return
	}
	e.answer(c, reply{})
}

// values reports whether c may take the next ID (when id is set) and the
// next token now. Otherwise c is refused (a file that could not be moved
// aside, values exhausted, a reservation that failed or has not landed
// within reserveTimeout) or parked until a reservation write lands, at most
// reserveTimeout; a write is started for it when none is under way.
func (e *Engine) values(c *command, id bool) bool {
	r := &e.o.res
	name := c.op.String()
	if (r.stuckQueue != "" || r.stuckReserve != "") && !e.unstick() {
		e.answer(c, reply{err: refuse(name, ReasonFileStuck, e.editState().Detail)})
		return false
	}
	if (id && r.ids.exhausted()) || r.tokens.exhausted() {
		if !r.exhaustedLogged {
			r.exhaustedLogged = true
			e.log.Error("player: IDs exhausted: every ID or token a uint32 holds has been handed out, none is handed out twice, and every insert and every other edit that advances the token is refused")
		}
		e.answer(c, reply{err: refuse(name, ReasonIDsExhausted, e.editState().Detail)})
		return false
	}
	if (id && r.ids.needs()) || r.tokens.needs() {
		if r.overdue || (r.refuseParked && r.failed != nil) {
			e.answer(c, reply{err: &Error{Reason: ReasonReservation, Op: name, Detail: "the reservation could not be written", Err: r.failed}})
			return false
		}
		e.reserveWrite()
		if !r.awaiting {
			r.awaiting = true
			r.waits++
			seq := r.waits
			r.bound = e.clk.AfterFunc(reserveTimeout, func() { e.post(msgReserveOverdue{seq: seq}) })
		}
		e.park(c)
		return false
	}
	return true
}

// reserveOverdue is the wait for a reservation write past reserveTimeout:
// the edits parked on it are refused, and so is every edit that needs a
// value until it lands, rather than waiting on a write that may never.
func (e *Engine) reserveOverdue(seq uint64) {
	r := &e.o.res
	if !r.awaiting || seq != r.waits {
		return
	}
	r.awaiting, r.overdue, r.refuseParked, r.bound = false, true, true, nil
	if r.failed == nil {
		r.failed = fmt.Errorf("the reservation write has not landed within %v", reserveTimeout)
		e.log.Warn("player: the reservation write has not landed by its bound; inserts and the other edits that advance the token are refused until it does",
			"path", e.cfg.ReservePath, "bound", reserveTimeout)
	}
	e.touched()
}

// unstick reports whether the bad files that could not be moved aside are
// gone, the operator having moved them; the engine then writes again. It is
// one Lstat a file, on the owner, only while an edit is refused for them.
func (e *Engine) unstick() bool {
	r := &e.o.res
	for _, p := range []*string{&r.stuckQueue, &r.stuckReserve} {
		if *p == "" {
			continue
		}
		if _, err := e.sm.lstat(*p); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		e.log.Info("player: the bad file is gone; the engine writes it again", "path", *p)
		if p == &r.stuckQueue {
			e.w.setOff(false)
		}
		*p = ""
		e.touched()
	}
	return true
}

// reserveWrite writes a reservation a block ahead of each counter that is
// low, on a goroutine of its own; the owner learns of it from
// msgReserveDone.
func (e *Engine) reserveWrite() {
	r := &e.o.res
	if r.inflight || r.stuckReserve != "" {
		return
	}
	r.ids.want, r.tokens.want = r.ids.durable, r.tokens.durable
	if r.ids.low() || r.ids.needs() {
		r.ids.want = r.ids.target()
	}
	if r.tokens.low() || r.tokens.needs() {
		r.tokens.want = r.tokens.target()
	}
	r.inflight = true
	data := encodeReserve(r.ids.want, r.tokens.want)
	e.spawn(func() { e.post(msgReserveDone{err: e.sm.writeReserve(e.cfg.ReservePath, data)}) })
}

// ahead reserves the next block in the background once less than a block
// is left, so a command waits on a write only when the next block is not
// durable by the time the current one runs out. A failed reservation is
// never retried here, as if it were optional: the next command that needs
// one writes it.
func (e *Engine) ahead() {
	r := &e.o.res
	if r.failed == nil && (r.ids.low() || r.tokens.low()) {
		e.reserveWrite()
	}
}

// reserveDone is a reservation write's result: durable, or a failure that
// refuses the commands that waited on it.
func (e *Engine) reserveDone(err error) {
	r := &e.o.res
	r.inflight, r.awaiting, r.overdue = false, false, false
	if r.bound != nil {
		r.bound.Stop()
		r.bound = nil
	}
	if err == nil {
		r.ids.durable = max(r.ids.durable, r.ids.want)
		r.tokens.durable = max(r.tokens.durable, r.tokens.want)
		if r.failed != nil {
			r.failed = nil
			e.log.Info("player: the reservation was written again", "path", e.cfg.ReservePath)
		}
	} else {
		if r.failed == nil {
			e.log.Warn("player: the reservation could not be written; inserts and the other edits that advance the token are refused until one is",
				"path", e.cfg.ReservePath, "err", err)
		}
		r.failed = err
		r.refuseParked = true
	}
	e.touched()
}

// reportOutput is ReportOutput: a report not newer than the last applied is
// discarded. The output going away, or its unit changing, stops playback
// and freezes the position; while nothing plays it drops what the backend
// prepared. Its return changes nothing: never autoplay.
func (e *Engine) reportOutput(c *command) {
	o := &e.o
	r := c.report
	if r.Generation <= o.output.Generation {
		e.answer(c, reply{})
		return
	}
	was := o.output
	o.output = OutputState{Known: true, Available: r.Available, Reason: r.Reason, Generation: r.Generation}
	if was.Known != o.output.Known || was.Available != o.output.Available || was.Reason != o.output.Reason {
		e.visible()
	} else {
		e.touched()
	}
	if !r.Available || r.UnitChanged {
		why := ReasonOutputUnavailable
		if r.Available {
			why = ReasonUnitChanged
		}
		e.outputGone(why, r.Reason)
	}
	e.answer(c, reply{})
}

// outputGone stops what plays, drops a pending play and the transport
// commands parked before it, and asks the backend to drop what it prepared
// when nothing played.
func (e *Engine) outputGone(why Reason, detail string) {
	o := &e.o
	stop := StopInfo{Reason: why, Detail: detail}
	err := refuse("play", why, detail)
	e.supersedeParked(func(c *command) error { return refuse(c.op.String(), why, detail) })
	s := o.sess
	switch {
	case s != nil && (s.phase == sessStarting || s.phase == sessLive):
		e.playFailed(err)
		e.releaseSession(s, IntentStop, stop)
		return
	case o.state == Releasing && o.then != IntentStop:
		e.playFailed(err)
		e.setIntent(IntentStop, detail)
	case o.state == Paused:
		o.state = Stopped
		e.stopped(stop)
	}
	e.discard()
}
