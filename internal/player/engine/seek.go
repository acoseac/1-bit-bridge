package engine

import (
	"fmt"
	"math"
)

// A seek within a track (B674) is a session of its own: the engine releases
// the session that holds the track, and once that release is acknowledged
// starts another whose first occurrence continues the one the seek took,
// under its ID, from the target's frame (StartRequest.From). So the backend's
// contract for a release and a start holds unchanged (the device closed with
// the frames the DAC had not played, the successors handed off with it, and
// opened again where the binding resolves now, checked before a frame), and
// the session is the seek's fence: every event names its session, the engine
// takes only the current one's, and a seek's session is a new one, so a
// position, a boundary or an end the released session still reports is not
// the track's. The track heard (Snapshot.Track), its count and its details
// stay as they were: the occurrence goes on, and the position reads the
// target from the seek's step until the new session measures it.

// seekTarget is where a seek takes the track: its entry, the occurrence the
// session started at the target continues, the frame, and what the backend
// reported of the content, its rate and length among it.
type seekTarget struct {
	entry   uint32
	occ     OccurrenceID
	frame   int64
	details Details
}

// seek reports whether o is a seek within a track.
func (o op) seek() bool { return o == opSeekAbsolute || o == opSeekRelative }

// seek runs a seek by its rule (policy): refused with no track to seek in;
// otherwise its target is checked (seekPoint) and the seek either releases
// the session that holds the track, to play or pause at the target as the
// transport was, or, for a target no session plays yet, moves it. A refusal
// changes nothing. The position reads the target from this step. A seek that
// plays answers as PlayID does, once the session at its target has started,
// and its playback is carried there (as carry carries a play command), so a
// command that moves the pending target first, or a Stop, supersedes it; a
// seek that leaves the transport paused answers once its release is
// acknowledged.
func (e *Engine) seek(c *command) {
	o := &e.o
	name := c.op.String()
	live := o.sess != nil && (o.sess.phase == sessStarting || o.sess.phase == sessLive)
	r := policy(c.op, o.state, live, false, o.seek != nil)
	if r.act == actRefuse {
		e.answer(c, reply{err: refuse(name, r.refuse, fmt.Sprintf("the player is %s with no track to seek in", o.state))})
		return
	}
	if r.output {
		if u := o.unconfirmed; u != nil {
			e.answer(c, reply{err: refuse(name, ReasonReleaseUnconfirmed,
				fmt.Sprintf("session %d's release is not confirmed: nothing opens the device until it is", u.Session))})
			return
		}
		if why := e.outputRefusal(name); why != nil {
			e.answer(c, reply{err: why})
			return
		}
	}
	at, err := e.seekPoint(c)
	if err != nil {
		e.answer(c, reply{err: err})
		return
	}
	o.seek = &at
	o.pos = Position{Occurrence: at.occ, Frames: at.frame, RateHz: at.details.RateHz}
	e.touched()
	switch r.act {
	case actRetarget:
		if o.state == Releasing && o.then == IntentPlay {
			e.waitCarried(c)
			return
		}
		e.wait(c, false)
	case actSeek:
		s := o.sess
		if r.then == IntentPlay {
			// The plays its session was started for, not heard yet, go on at
			// the target: their playback is the same occurrence's.
			e.carry(s)
		}
		e.releaseSession(s, r.then, StopInfo{})
		if r.then == IntentPlay {
			e.waitCarried(c)
			return
		}
		e.wait(c, false)
	}
}

// waitCarried answers c, a seek that plays, as a play command carried to the
// session that starts after the release under way (carry): with that
// session's playback, unless a command that moves the pending target, or a
// stop, replaces it first.
func (e *Engine) waitCarried(c *command) {
	e.o.waiters = append(e.o.waiters, waiter{c: c, play: true, carried: true})
}

// seekPoint is where c, a seek, takes the track: the target of a seek no
// session plays yet, or the session's first occurrence, the one heard or the
// one starting. It is refused for DSD, native or DoP, by the occurrence's own
// report (a native plan, or Details of DSD) or the session's transport, before
// anything else is asked; for a track the backend has reported nothing of;
// and for a target before the start (absolute), at or past the end of the
// content, the exact end included, or beyond the arithmetic, and in content of
// no known length or rate. A relative target counts from the occurrence's
// measured position, or the target of a seek under way, or where the
// occurrence's hand-off started while nothing of it is measured, and one
// before the start is the start.
func (e *Engine) seekPoint(c *command) (seekTarget, error) {
	o := &e.o
	name := c.op.String()
	var at seekTarget
	var pos int64
	switch s := o.sess; {
	case o.seek != nil:
		at, pos = *o.seek, o.seek.frame
	case s != nil && len(s.chain) > 0:
		oc := s.chain[0]
		if dsd := oc.plan != nil || s.native() || (oc.details != nil && isDSD(*oc.details)); !TimeSeek(dsd) {
			return at, refuse(name, ReasonSeekUnsupported, "the track is DSD, which the player does not position in time")
		}
		if oc.details == nil {
			return at, refuse(name, ReasonSeekUnavailable, "the backend has reported nothing of the track's content yet")
		}
		at = seekTarget{entry: oc.entry, occ: oc.id, details: *oc.details}
		pos = oc.from
		if o.pos.Occurrence == oc.id {
			pos = o.pos.Frames
		}
	default:
		return at, refuse(name, ReasonSeekUnavailable, "no track to seek in")
	}
	length, rate := at.details.Frames, at.details.RateHz
	if length <= 0 || rate <= 0 {
		return at, refuse(name, ReasonSeekOutOfRange, "the track's length is not known")
	}
	frame, ok := seekFrame(c.op == opSeekRelative, c.seconds, pos, rate)
	if !ok || frame >= length {
		return at, refuse(name, ReasonSeekOutOfRange, fmt.Sprintf("%d s takes the track to no frame before the end of its %d frames", c.seconds, length))
	}
	at.frame = frame
	return at, nil
}

// isDSD reports content a backend reported as DSD: its codec, or a sample of
// one bit.
func isDSD(d Details) bool { return d.Codec == "DSD" || d.Bits == 1 }

// seekFrame is the frame seconds take the content to at rate, every product
// and sum checked: from its start, absolute, or from pos, the position,
// relative, a relative target before the start being the start. It is false
// for an absolute target before the start, and for a target the arithmetic
// cannot hold, which is past the end of any content.
func seekFrame(relative bool, seconds, pos int64, rate int) (int64, bool) {
	if rate <= 0 || pos < 0 {
		return 0, false
	}
	r := int64(rate)
	if seconds > math.MaxInt64/r || seconds < math.MinInt64/r {
		return 0, relative && seconds < 0
	}
	d := seconds * r
	switch {
	case !relative:
		return d, seconds >= 0
	case d > 0 && pos > math.MaxInt64-d:
		return 0, false
	}
	return max(pos+d, 0), true
}

// seekPending reports whether a seek's target is still to be played by a
// session to come: the release the seek made is under way, to play or pause
// at it, or the transport is paused at it with no session; and its entry is
// still the queue's and selected, with no maintenance and no shutdown. Any
// other step (a Stop, a stopping report of the output, a deletion of its
// entry, a DeleteAll, a release past its deadline, maintenance, the shutdown)
// leaves it with nothing to play.
func (e *Engine) seekPending() bool {
	o := &e.o
	sk := o.seek
	if sk == nil || o.maint != nil || o.closing || o.selected != sk.entry || !o.q.has(sk.entry) {
		return false
	}
	switch o.state {
	case Releasing:
		return o.then != IntentStop
	case Paused:
		return o.sess == nil
	}
	return false
}

// settleSeek drops, after every step, the target of a seek nothing is to
// play any more (seekPending).
func (e *Engine) settleSeek() {
	if e.o.seek != nil && !e.seekPending() {
		e.dropSeek()
	}
}

// dropSeek forgets a seek's target, and the track it kept heard: no session
// plays it.
func (e *Engine) dropSeek() {
	o := &e.o
	sk := o.seek
	if sk == nil {
		return
	}
	o.seek = nil
	if o.track != nil && o.track.Occurrence == sk.occ {
		e.hear(nil)
	}
	e.touched()
}
