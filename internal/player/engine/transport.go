package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// errCallDeadline is the cause a backend call's context ends with at its
// deadline.
var errCallDeadline = errors.New("player: the call's deadline passed")

// sessPhase is where a session is.
type sessPhase int

const (
	// sessStarting has Start under way.
	sessStarting sessPhase = iota
	// sessLive has had Start return nil: it plays, or is paused.
	sessLive
	// sessReleasing has had its release asked for and not acknowledged.
	sessReleasing
	// sessUnconfirmed passed its release deadline unacknowledged.
	sessUnconfirmed
	// sessAcked has had its release acknowledged: finished, so a deadline
	// or an acknowledgement still on its way to the owner changes nothing.
	sessAcked
)

// stage is how far an occurrence has got.
type stage int

const (
	stageRequested stage = iota // named in a Start or a Prepare
	stagePrepared               // its decoder open
	stageSubmitted              // its first frames in the sink
	stageAudible                // heard
)

// occ is one hand-off of an entry.
type occ struct {
	reservation *OccurrenceReservation
	id          OccurrenceID
	entry       uint32
	ref         Reference
	// meta is the entry's metadata as it was handed off, shared with the
	// queue, which never writes to it; details is what the backend reported
	// of its content (DetailsEvents), nil until it does.
	meta    []byte
	details *Details
	plan    *NativePlan
	stage   stage
	// unwanted is why it must not be heard: an edit removed it, or made it
	// no longer follow the occurrence before it. reconcile sets it in the
	// edit's own step, even while a Prepare or a Cut is under way, and clears
	// it when an edit makes it follow again; heard while it is set, it stops
	// playback.
	unwanted string
	// late is a Cut that answered it could not: some of its frames have
	// been or will be played, so no Cut is asked of it again.
	late bool
	// skipped is the entries its hand-off passed over between the
	// occurrence before it and its own, refused in the pass it was named
	// in. An edit starts a new pass, which offers them again further on,
	// but this hand-off keeps them passed over: it follows the occurrence
	// before it while the queue still holds nothing else between them.
	skipped []uint32
}

func (oc *occ) public() Occurrence {
	return Occurrence{ID: oc.id, Entry: oc.entry, Ref: oc.ref, Reservation: oc.reservation}
}

// session is one playback session as the owner tracks it.
type session struct {
	id    SessionID
	phase sessPhase
	// chain is what was handed off, in play order: chain[0] is the audible
	// occurrence, or the first while nothing is.
	chain []*occ
	// endSent is the backend told nothing follows the last occurrence;
	// noMore, that the backend took no further successor, so what follows
	// plays in a session of its own once this one has finished.
	endSent, noMore bool
	// succCall is the Prepare or Cut under way, one at a time;
	// transportCall the Pause or Resume.
	succCall, transportCall   *call
	paused, devReleased       bool
	pauseTimer                timer
	pauseSeq                  uint64
	hasCurrent, currentNative bool
	retiring                  *retiringLedger
	releaseCall               *call
	releaseTimer              timer
}

// index is where occurrence id is in the chain, or -1.
func (s *session) index(id OccurrenceID) int {
	for i, oc := range s.chain {
		if oc.id == id {
			return i
		}
	}
	return -1
}

// stopPause stops the pause's release deadline.
func (s *session) stopPause() {
	if s.pauseTimer != nil {
		s.pauseTimer.Stop()
		s.pauseTimer = nil
	}
}

// stopTimers stops every timer the session armed.
func (s *session) stopTimers() {
	s.stopPause()
	if s.releaseTimer != nil {
		s.releaseTimer.Stop()
	}
}

// move is where a transport command takes the selection.
type move int

const (
	moveKeep move = iota
	moveNext
	movePrevious
	moveTo // the command's ID, or the ID at its index
)

// act is what a transport command does to the transport.
type act int

const (
	actNothing act = iota
	actStart       // start a session at the selection, the first entry when none
	actResume      // resume the paused session
	actPause       // pause the session
	actRelease     // release the session; the rule's then follows
	actIntent      // set the pending intent while releasing
	actStop        // stop a transport that holds no session
)

// rule is what one transport command does in one state.
type rule struct {
	move move
	act  act
	then Intent
	// output is a rule that starts a session, or will: refused while the
	// output is unknown or unavailable.
	output bool
}

// policy is the package documentation's table, in one place: what op does
// in state st, where live is a session held (false for paused at the start
// of the selection, with none) and atEnd a Next whose selection is the last
// entry with repeat off. Next and Previous select and play in every state,
// as PlayID and PlayIndex do, as their OpenHome actions (Next, Previous,
// SeekId, SeekIndex) are used to: OpenHome's reference Playlist source
// (ohPipeline's SourcePlaylist) plays after Next and Previous, paused and
// stopped included. A Next past the end follows the end of the queue's
// rule. Release 5a may adjust it once named controllers have been tested.
func policy(op op, st State, live, atEnd bool) rule {
	held := st == Playing || (st == Paused && live)
	switch op {
	case opPlay:
		switch {
		case st == Stopped, st == Paused && !live:
			return rule{act: actStart, output: true}
		case st == Paused:
			return rule{act: actResume, output: true}
		case st == Releasing:
			return rule{act: actIntent, then: IntentPlay, output: true}
		}
	case opPause:
		if st == Playing {
			return rule{act: actPause}
		}
	case opStop:
		switch {
		case held:
			return rule{act: actRelease, then: IntentStop}
		case st == Paused:
			return rule{act: actStop}
		case st == Releasing:
			return rule{act: actIntent, then: IntentStop}
		}
	case opNext, opPrevious, opPlayID, opPlayIndex:
		if atEnd {
			switch {
			case held:
				return rule{act: actRelease, then: IntentStop}
			case st == Paused:
				return rule{act: actStop}
			case st == Releasing:
				return rule{act: actIntent, then: IntentStop}
			}
			return rule{}
		}
		mv := moveTo
		switch op {
		case opNext:
			mv = moveNext
		case opPrevious:
			mv = movePrevious
		}
		switch {
		case st == Stopped, st == Paused && !live:
			return rule{move: mv, act: actStart, output: true}
		case held:
			return rule{move: mv, act: actRelease, then: IntentPlay, output: true}
		case st == Releasing:
			return rule{move: mv, act: actIntent, then: IntentPlay, output: true}
		}
	}
	return rule{}
}

// transport runs a transport command by its rule. A Stop, which waits for
// nothing, overtakes the transport commands parked before it, and answers
// them (supersedeParked): none of them ran (ReasonOvertaken).
func (e *Engine) transport(c *command) {
	o := &e.o
	name := c.op.String()
	switch c.op {
	case opStop:
		e.supersedeParked(func(p *command) error {
			return refuse(p.op.String(), ReasonOvertaken, "a stop ran before it, so it did not run")
		})
	case opPlay, opPlayID, opPlayIndex, opNext, opPrevious:
		if u := o.unconfirmed; u != nil {
			e.answer(c, reply{err: refuse(name, ReasonReleaseUnconfirmed,
				fmt.Sprintf("session %d's release is not confirmed: nothing opens the device until it is", u.Session))})
			return
		}
	}
	var target uint32
	switch c.op {
	case opPlayID:
		if !o.q.has(c.id) {
			e.answer(c, reply{err: refuse(name, ReasonIDNotFound, fmt.Sprintf("no entry %d", c.id))})
			return
		}
		target = c.id
	case opPlayIndex:
		if c.index < 0 || c.index >= o.q.len() {
			e.answer(c, reply{err: refuse(name, ReasonIndexNotFound, fmt.Sprintf("no index %d in %d entries", c.index, o.q.len()))})
			return
		}
		target = o.q.entries[c.index].id
	}
	live := o.sess != nil && (o.sess.phase == sessStarting || o.sess.phase == sessLive)
	atEnd := c.op == opNext && o.selected != 0 && o.q.after(o.selected, o.repeat) == 0
	r := policy(c.op, o.state, live, atEnd)
	if r.output {
		if why := e.outputRefusal(name); why != nil {
			e.answer(c, reply{err: why})
			return
		}
	}
	sel := o.selected
	switch r.move {
	case moveNext:
		sel = o.q.after(o.selected, o.repeat)
	case movePrevious:
		sel = o.q.before(o.selected)
	case moveTo:
		sel = target
	}
	if r.act == actStart || (r.act == actIntent && r.then == IntentPlay) {
		if sel == 0 {
			sel = o.q.first()
		}
		if sel == 0 {
			e.answer(c, reply{err: refuse(name, ReasonEmptyQueue, "the queue is empty")})
			return
		}
	}
	moved := sel != o.selected
	e.selectEntry(sel)
	switch r.act {
	case actNothing:
		e.answer(c, reply{})
	case actStart:
		e.wait(c, true)
		e.startSession(sel)
	case actResume:
		e.resume(c)
	case actPause:
		e.pause(c)
	case actRelease:
		stop := StopInfo{Reason: ReasonStopped, Detail: name}
		if atEnd {
			stop = StopInfo{Reason: ReasonEndOfQueue, Detail: "next past the last entry"}
		}
		if r.then == IntentPlay {
			e.replaced(o.sess, name)
		}
		e.releaseSession(o.sess, r.then, stop)
		e.wait(c, r.then == IntentPlay)
	case actIntent:
		if r.then == IntentPlay && moved {
			e.replacedCarry(name)
		}
		e.setIntent(r.then, name)
		e.wait(c, r.then == IntentPlay)
	case actStop:
		o.state = Stopped
		stop := StopInfo{Reason: ReasonStopped, Detail: name}
		if atEnd {
			stop = StopInfo{Reason: ReasonEndOfQueue, Detail: "next past the last entry"}
		}
		e.stopped(stop)
		e.answer(c, reply{})
	}
}

// outputRefusal is why nothing may start while the output is as it is, or
// nil.
func (e *Engine) outputRefusal(op string) error {
	switch out := e.o.output; {
	case !out.Known:
		return refuse(op, ReasonOutputUnknown, "the output has not been reported yet")
	case !out.Available:
		return refuse(op, ReasonOutputUnavailable, out.Reason)
	}
	return nil
}

// selectEntry moves the selection.
func (e *Engine) selectEntry(id uint32) {
	if e.o.selected != id {
		e.o.selected = id
		e.saved()
	}
}

// setIntent replaces the pending intent: the latest command's wins. A
// pending play that by replaces is superseded.
func (e *Engine) setIntent(then Intent, by string) {
	o := &e.o
	if then == IntentStop {
		e.escalateRetiring()
	}
	if then != IntentPlay && o.then == IntentPlay {
		e.superseded(by)
	}
	if o.then != then {
		o.then = then
		e.visible()
	}
}

// playFailed records why the playback the waiting play commands asked for
// will not happen, on each that has no outcome yet, neither a reason nor a
// playback that began (playBegan), and on a Play whose Resume is under way.
// The first outcome a command gets is its answer, whatever later commands
// do: a later Play that plays never answers for an earlier one whose
// playback was replaced or refused, and nothing that stops a playback after
// it began is the answer of the command that asked for it.
func (e *Engine) playFailed(err error) {
	o := &e.o
	for i := range o.waiters {
		if w := &o.waiters[i]; w.play && !w.decided {
			w.decided, w.err = true, err
		}
	}
	if s := o.sess; s != nil {
		if tc := s.transportCall; tc != nil && tc.kind == callResume && tc.waiter != nil && tc.waitErr == nil {
			tc.waitErr = err
		}
	}
}

// superseded records, on the waiting play commands, that by, a later
// command or edit, replaced their playback before it began. A reason
// already recorded (the output, a fault, the backend's refusal) stands, and
// a command whose playback began is passed over (playFailed).
func (e *Engine) superseded(by string) {
	e.playFailed(refuse("play", ReasonSuperseded, "replaced before playback began: "+by))
}

// replaced records that by, a later command, replaced s's playback with
// another before it was heard: Next, Previous, PlayID or PlayIndex while s
// starts or holds the device, which releases s to play the command's own
// target. The play commands s was started for answer ReasonSuperseded, and
// so does a Play whose Resume is under way on s, whose paused content never
// plays on; one whose playback began (playBegan), or that was given a reason
// already, keeps its answer. The commands that waited behind a release are
// not s's: the session that starts once that release is acknowledged is
// started for them, as for the command by.
func (e *Engine) replaced(s *session, by string) {
	if s == nil {
		return
	}
	e.supersedeWaiters(by, func(w *waiter) bool { return w.sess == s.id })
	if tc := s.transportCall; tc != nil && tc.kind == callResume && tc.waiter != nil && tc.waitErr == nil {
		tc.waitErr = refuse("play", ReasonSuperseded, "replaced before playback resumed: "+by)
	}
}

// replacedCarry records that by, a Next, Previous, PlayID or PlayIndex sent
// while a release to play is under way, moved the pending target off the
// entry the engine itself was moving on to: the play commands carried past an
// entry refused at its start (carry) answer ReasonSuperseded, naming by, as
// they would have had by come while their session started (replaced). One
// that names the entry already pending replaces nothing, the same session
// playing it (Codex on round 8). The commands that set the pending intent
// themselves, by among them, share the session after the release, as before;
// one whose playback began keeps its answer.
func (e *Engine) replacedCarry(by string) {
	e.supersedeWaiters(by, func(w *waiter) bool { return w.carried })
}

// supersedeWaiters answers ReasonSuperseded, naming by, on the play waiters
// with no outcome yet that which picks.
func (e *Engine) supersedeWaiters(by string, which func(w *waiter) bool) {
	err := refuse("play", ReasonSuperseded, "replaced before playback began: "+by)
	for i := range e.o.waiters {
		if w := &e.o.waiters[i]; w.play && !w.decided && which(w) {
			w.decided, w.err = true, err
		}
	}
}

// carry hands the play commands s was started for, with no outcome yet, on
// to the session that follows s's release: the engine itself moved on, past
// s's first entry refused before any of its content played, and their
// playback goes on in that session (startSession), unless a command replaces
// it first (replacedCarry), which the carried mark lets it tell. The other
// release the engine makes to play on, onFinished's when s took no
// successor, has no such command: it needs s live, and the settle answers
// every waiter once s's Start has.
func (e *Engine) carry(s *session) {
	for i := range e.o.waiters {
		if w := &e.o.waiters[i]; w.play && !w.decided && w.sess == s.id {
			w.sess, w.carried = 0, true
		}
	}
}

// playBegan records, on the play commands s was started for that have no
// outcome yet, that their playback began: s was heard. Only s's Audible
// decides them, and onAudible takes one only from the current session,
// starting or live (current): a session released, its release unconfirmed,
// finished or replaced reports in vain. Each answers nil from then on,
// whatever stops playback after (playFailed passes it over); one already
// given a reason keeps it, since deciding again changes nothing recorded. A
// Play whose Resume is under way is not one of them: an Audible while a
// session is paused can be content a pause by zeros was still playing out,
// so the Resume's own answer stands.
func (e *Engine) playBegan(s *session) {
	for i := range e.o.waiters {
		if w := &e.o.waiters[i]; w.play && w.sess == s.id {
			w.decided = true
		}
	}
}

// stopped records why playback stopped.
func (e *Engine) stopped(why StopInfo) {
	why.Entry, why.At = e.o.selected, e.clk.Now()
	e.o.lastStop = why
	e.visible()
}

// newOcc is a new hand-off of entry id.
func (e *Engine) newOcc(id uint32) *occ {
	o := &e.o
	var reservation *OccurrenceReservation
	if e.occurrenceBudget != nil {
		reservation = e.occurrenceBudget.Reserve()
		if reservation == nil {
			return nil
		}
	}
	o.nextOcc++
	en, _ := o.q.get(id)
	return &occ{reservation: reservation, id: OccurrenceID(o.nextOcc), entry: id, ref: en.ref, meta: en.meta}
}

// hear makes oc the occurrence heard, or nothing (nil): the audible
// occurrence and its track (Snapshot.Track), built from the occurrence as it
// was handed off, never looked up in the queue; one more track counted for
// each occurrence heard, the one heard already counting no more, and one
// more change of the details whenever what is known of the content heard
// changes (B470 PR 5a3). A published track is never written to: each is a
// new one.
func (e *Engine) hear(oc *occ) {
	o := &e.o
	if oc != nil && o.track != nil && o.track.Occurrence == oc.id {
		return
	}
	before, had := o.track.details()
	if oc == nil {
		o.audible, o.track = Audible{}, nil
	} else {
		o.audible = Audible{Occurrence: oc.id, Entry: oc.entry}
		t := &Track{Occurrence: oc.id, Entry: oc.entry, Ref: oc.ref, Meta: oc.meta}
		if oc.details != nil {
			t.Details, t.HasDetails = *oc.details, true
		}
		o.track = t
		o.tracks++
	}
	if after, has := o.track.details(); has != had || after != before {
		o.detailsN++
	}
}

// startSession starts a session at entry target, for the play commands
// waiting with none started for them: the one that starts it, and those
// that waited behind the release before it. The owner starts one only with
// no other session unacknowledged.
func (e *Engine) startSession(target uint32) {
	o := &e.o
	o.nextSess++
	s := &session{id: SessionID(o.nextSess), phase: sessStarting}
	for i := range o.waiters {
		if w := &o.waiters[i]; w.play && w.sess == 0 {
			w.sess, w.carried = s.id, false
		}
	}
	first := e.newOcc(target)
	if first == nil {
		e.playFailed(refuse("play", Reason("occurrence_budget_exceeded"), "outstanding occurrence bound"))
		o.state = Stopped
		e.visible()
		return
	}
	s.chain = []*occ{first}
	o.sess = s
	o.sessions[s.id] = s
	o.state, o.starting, o.then = Playing, true, IntentStop
	e.hear(nil)
	o.join = nil
	e.selectEntry(target)
	e.visible()
	req := StartRequest{
		Session: s.id,
		First:   first.public(),
		Options: SessionOptions{PauseRelease: e.cfg.PauseRelease, DSD: e.cfg.DSD},
		Events:  e.evts,
	}
	e.call(callStart, s, StartTimeout, func(ctx context.Context) callResult { return callResult{err: e.be.Start(ctx, req)} })
}

// releaseSession asks s to release; then is what follows the
// acknowledgement, and why, when playback stops, is recorded. Released to
// anything but play, a session still starting supersedes the plays waiting
// for it, but for those it was heard for (playBegan), and so does one whose
// Resume is under way: that Play is answered at once with the reason
// already recorded for it (the output, a fault, a command that replaced
// its playback) or as superseded, a Pause under way with nil. Released to
// play, the caller has already said whose playback that is: a command's
// that replaced s's (replaced), or the one s's own commands asked for, going
// on past an entry refused at its start (carry).
func (e *Engine) releaseSession(s *session, then Intent, why StopInfo) {
	o := &e.o
	if s == nil || s.phase == sessReleasing || s.phase == sessUnconfirmed {
		e.setIntent(then, why.Detail)
		return
	}
	native := s.native()
	if native {
		s.retiring = &retiringLedger{token: ReleaseToken{Session: s.id, Generation: 1}, chain: make([]*occ, len(s.chain))}
		for i, o := range s.chain {
			cp := *o
			s.retiring.chain[i] = &cp
		}
	}
	s.phase = sessReleasing
	s.stopPause()
	if tc := s.transportCall; tc != nil && tc.waiter != nil {
		var err error
		if tc.kind == callResume {
			if err = tc.waitErr; err == nil && then != IntentPlay {
				err = refuse(tc.waiter.op.String(), ReasonSuperseded, "replaced before playback resumed: "+why.Detail)
			}
		}
		e.answer(tc.waiter, reply{err: err})
		tc.waiter = nil
	}
	s.transportCall = nil
	o.then = then
	if then != IntentPlay {
		e.superseded(why.Detail)
	}
	if then != IntentPlay && why.Reason != "" {
		e.stopped(why)
	}
	if o.maint == nil {
		o.state = Releasing
	}
	o.starting, o.join = false, nil
	if !native {
		e.hear(nil)
	}
	e.visible()
	req := ReleaseRequest{Mode: ReleaseImmediate, Cause: ReleaseStop}
	if native {
		req.Token = s.retiring.token
		if then == IntentPlay {
			req.Mode = ReleaseTrackChange
			req.Cause = ReleaseNavigation
		}
	}
	rc := e.call(callRelease, s, ReleaseTimeout, func(ctx context.Context) callResult {
		if b, ok := e.be.(NativeLifecycle); native && ok {
			return callResult{err: b.ReleaseNative(ctx, req)}
		}
		return callResult{err: e.be.Release(ctx, s.id)}
	})
	s.releaseCall = rc
	s.releaseTimer = e.clk.At(rc.deadline, func() { e.post(msgReleaseDeadline{s: s}) })
}

// releaseDeadline is s's release past ReleaseTimeout: unconfirmed, the
// pending intent dropped, and nothing opens the device until the
// acknowledgement comes. A deadline that reaches the owner after the
// acknowledgement, its timer having fired first, finds s finished and does
// nothing.
func (e *Engine) releaseDeadline(s *session) {
	o := &e.o
	if s.phase != sessReleasing || o.sessions[s.id] != s {
		return
	}
	s.phase = sessUnconfirmed
	if s.retiring != nil {
		e.recordRetiringFault(s, Fault{Reason: ReasonReleaseUnconfirmed, Detail: ErrReleaseUnconfirmed.Error()})
	}
	if rc := s.releaseCall; rc != nil {
		rc.cancel(errCallDeadline)
	}
	if o.sess == s {
		o.sess = nil
	}
	o.unconfirmed = &ReleaseInfo{Session: s.id, Since: e.clk.Now()}
	e.log.Warn("player: a session's release was not confirmed by its deadline; nothing opens the device until it is", "session", uint64(s.id), "deadline", ReleaseTimeout)
	if o.then == IntentPlay {
		e.playFailed(refuse("play", ReasonReleaseUnconfirmed, fmt.Sprintf("session %d's release is not confirmed", s.id)))
	}
	o.then = IntentStop
	if o.maint == nil {
		o.state = Stopped
	} else if m := o.maint; m.waiter != nil {
		e.answer(m.waiter, reply{hold: Hold{Lease: m.lease, Expires: m.expires, ReleaseConfirmed: false}})
		m.waiter = nil
	}
	e.visible()
}

// ackAt is s's release acknowledged at at, on the engine's clock: by Release
// returning nil, or by Events.Released. An acknowledgement at or past the
// release's deadline is late whichever reaches the owner first, it or the
// deadline: the deadline's policy applies first (releaseDeadline: the
// release unconfirmed, the pending intent dropped), and the acknowledgement
// then clears the unconfirmed release without running the intent it
// dropped.
func (e *Engine) ackAt(s *session, at time.Time) {
	if rc := s.releaseCall; s.phase == sessReleasing && rc != nil && !at.Before(rc.deadline) {
		e.releaseDeadline(s)
	}
	e.acked(s)
}

// acked is s's release acknowledged (ackAt). s is finished from then on
// (sessAcked).
func (e *Engine) acked(s *session) {
	o := &e.o
	if s.retiring != nil {
		e.hear(nil)
		s.retiring = nil
		e.touched()
	}
	switch s.phase {
	case sessReleasing:
		s.phase = sessAcked
		releaseOccurrences(s.chain)
		s.stopTimers()
		delete(o.sessions, s.id)
		if o.sess == s {
			o.sess = nil
		}
		if u := o.unconfirmed; u != nil && u.Session == s.id {
			o.unconfirmed = nil
		}
		if m := o.maint; m != nil {
			m.confirmed = o.unconfirmed == nil
			if m.waiter != nil {
				e.answer(m.waiter, reply{hold: Hold{Lease: m.lease, Expires: m.expires, ReleaseConfirmed: m.confirmed}})
				m.waiter = nil
			}
		}
		e.afterRelease()
	case sessUnconfirmed:
		s.phase = sessAcked
		releaseOccurrences(s.chain)
		s.stopTimers()
		delete(o.sessions, s.id)
		if u := o.unconfirmed; u != nil && u.Session == s.id {
			o.unconfirmed = nil
			e.log.Info("player: the unconfirmed release was acknowledged late", "session", uint64(s.id))
			e.visible()
		}
		if m := o.maint; m != nil && o.unconfirmed == nil {
			m.confirmed = true
			e.touched()
		}
	}
}

// afterRelease applies the pending intent once the release is
// acknowledged.
func (e *Engine) afterRelease() {
	o := &e.o
	then := o.then
	o.then = IntentStop
	e.visible()
	switch {
	case o.maint != nil:
	case o.closing:
		o.state = Stopped
	case then == IntentPlay:
		if why := e.outputRefusal("play"); why != nil {
			e.playFailed(why)
			o.state = Stopped
			return
		}
		target := o.selected
		if target == 0 {
			target = o.q.first()
		}
		if target == 0 {
			e.playFailed(refuse("play", ReasonEmptyQueue, "the queue is empty"))
			o.state = Stopped
			return
		}
		e.startSession(target)
	case then == IntentPause:
		o.state = Paused
	default:
		o.state = Stopped
	}
}

// pause asks the session to pause; c is answered with the backend's
// answer.
func (e *Engine) pause(c *command) {
	s := e.o.sess
	tc := e.call(callPause, s, CallTimeout, func(ctx context.Context) callResult { return callResult{err: e.be.Pause(ctx, s.id)} })
	tc.waiter = c
	s.transportCall = tc
}

// resume asks the paused session to play on; c is answered with the
// backend's answer.
func (e *Engine) resume(c *command) {
	s := e.o.sess
	tc := e.call(callResume, s, CallTimeout, func(ctx context.Context) callResult {
		how, err := e.be.Resume(ctx, s.id)
		return callResult{err: err, resume: how}
	})
	tc.waiter = c
	s.transportCall = tc
}

// current is session id when it is the one playing or starting.
func (e *Engine) current(id SessionID) *session {
	s := e.o.sess
	if s == nil || s.id != id || (s.phase != sessStarting && s.phase != sessLive) {
		return nil
	}
	return s
}

// following is the entry that follows id in play order, passing over the
// entries refused in this pass, or 0.
func (e *Engine) following(id uint32) uint32 {
	next, _ := e.walk(id, nil)
	return next
}

// walk is the entry that follows id in play order, passing over the entries
// refused in this pass and those in past (a hand-off's own skips), or 0, and
// the entries it passed over.
func (e *Engine) walk(id uint32, past []uint32) (uint32, []uint32) {
	o := &e.o
	var passed []uint32
	cur := id
	for i := 0; i < o.q.len(); i++ {
		next := o.q.after(cur, o.repeat)
		if next == 0 {
			return 0, passed
		}
		if !o.pass[next] && !slices.Contains(past, next) {
			return next, passed
		}
		passed = append(passed, next)
		cur = next
	}
	return 0, passed
}

// reconcile holds the session's hand-offs to what the queue now says
// follows the audible occurrence. First, in the step of the edit that
// changed it and whatever call is under way, every occurrence whose entry
// no longer follows the one before it is marked unwanted, and one that
// follows again is cleared: an unwanted occurrence that is heard stops
// playback at once (onAudible), whatever a Prepare or a Cut answers after.
// An occurrence still follows when the walk from the one before it, passing
// over this pass's refusals and the entries its own hand-off passed over
// (occ.skipped), reaches it: an edit elsewhere starts a new pass, and
// changes nothing between the two. Then, with no call under way, the first
// unwanted occurrence not yet submitted is replaced by a new Prepare, and a
// submitted one is cut at its boundary; one whose cut came too late is left
// to stop playback if it is heard. One Prepare or Cut is under way at a
// time, and the tail's successor is named once its frames are in the sink.
func (e *Engine) reconcile(s *session) {
	o := &e.o
	if s == nil || s != o.sess || s.phase != sessLive || o.closing || len(s.chain) == 0 {
		return
	}
	for i := 1; i < len(s.chain); i++ {
		prev, cur := s.chain[i-1], s.chain[i]
		switch next, _ := e.walk(prev.entry, cur.skipped); {
		case cur.entry == next:
			cur.unwanted = ""
		case !o.q.has(cur.entry):
			cur.unwanted = fmt.Sprintf("entry %d was deleted", cur.entry)
		default:
			cur.unwanted = fmt.Sprintf("entry %d is no longer next", cur.entry)
		}
	}
	if s.succCall != nil {
		return
	}
	for i := 1; i < len(s.chain); i++ {
		prev, cur := s.chain[i-1], s.chain[i]
		switch {
		case cur.unwanted == "":
			continue
		case cur.late:
		case cur.stage < stageSubmitted:
			releaseOccurrences(s.chain[i:])
			s.chain = s.chain[:i]
			e.prepare(s, prev)
		default:
			e.cut(s, cur)
		}
		return
	}
	last := s.chain[len(s.chain)-1]
	if last.stage < stageSubmitted || s.noMore {
		return
	}
	if e.following(last.entry) != 0 || !s.endSent {
		e.prepare(s, last)
	}
}

// prepare names what follows after now (nothing, when the walk ends), with
// the entries the walk passed over as the new occurrence's skips.
func (e *Engine) prepare(s *session, after *occ) {
	entry, passed := e.walk(after.entry, nil)
	var next *occ
	var pub *Occurrence
	if entry != 0 {
		next = e.newOcc(entry)
		if next == nil {
			e.onFault(s.id, Fault{Reason: Reason("occurrence_budget_exceeded"), Detail: "outstanding occurrence bound"})
			return
		}
		next.skipped = passed
		s.chain = append(s.chain, next)
		p := next.public()
		pub = &p
		s.endSent = false
	} else {
		s.endSent = true
	}
	if e.o.join != nil {
		e.o.join = nil
		e.touched()
	}
	pc := e.call(callPrepare, s, CallTimeout, func(ctx context.Context) callResult {
		return callResult{err: e.be.Prepare(ctx, s.id, after.id, pub)}
	})
	pc.after, pc.next = after, next
	s.succCall = pc
}

// cut asks the backend to end the content before from at its boundary and
// to discard from and what follows it.
func (e *Engine) cut(s *session, from *occ) {
	cc := e.call(callCut, s, CallTimeout, func(ctx context.Context) callResult {
		ok, err := e.be.Cut(ctx, s.id, from.id)
		return callResult{err: err, cut: ok && err == nil}
	})
	cc.from = from
	s.succCall = cc
}

// skip records entry as refused before any of its content played.
func (e *Engine) skip(entry uint32, u Unplayable) {
	o := &e.o
	if o.pass == nil {
		o.pass = map[uint32]bool{}
	}
	o.pass[entry] = true
	e.setSkips(func(m map[uint32]Skip) { m[entry] = Skip{Reason: u.Reason, Detail: u.Detail, At: e.clk.Now()} })
}

// allFailed reports a pass in which every entry was refused.
func (e *Engine) allFailed() bool {
	o := &e.o
	for _, en := range o.q.entries {
		if !o.pass[en.id] {
			return false
		}
	}
	return o.q.len() > 0
}

// callKind is a backend call's kind.
type callKind int

const (
	callStart callKind = iota
	callPrepare
	callCut
	callPause
	callResume
	callRelease
	callDiscard
)

var callNames = [...]string{"start", "prepare", "cut", "pause", "resume", "release", "discard"}

func (k callKind) String() string { return callNames[k] }

// call is one backend call under way.
type call struct {
	id      uint64
	kind    callKind
	sess    *session
	cancel  context.CancelCauseFunc
	timer   timer
	expired bool
	waiter  *command
	// waitErr is why a Resume's waiting Play will not get its playback,
	// recorded as it was decided (playFailed); releaseSession answers it.
	waitErr error
	// deadline is the call's, on the engine's clock (zero: none): an answer
	// returned at or past it is late, whichever of the answer and the
	// expiry reaches the owner first.
	deadline time.Time
	// after and next are a Prepare's; from a Cut's.
	after, next, from *occ
}

// callResult is what a backend call returned, and when, on the engine's
// clock.
type callResult struct {
	err    error
	cut    bool
	resume ResumeResult
	at     time.Time
}

// callCtx is a backend call's context: the engine's life, ended at the
// call's deadline, which Deadline reports, both on the engine's clock.
//
// Its Done is a channel of its own, closed once its base has ended, so a
// context derived from it (context.WithCancel, an errgroup) cannot attach
// to the base, which would end it with the base's context.Canceled: it
// follows Done and takes its Err from callCtx, context.DeadlineExceeded
// once the deadline has ended the call. The same on either clock: a
// standard library deadline context would race the engine's own deadline
// handling, whichever ended the call first deciding the error, and would
// leave the tests, on the fake clock, a path production never takes.
type callCtx struct {
	context.Context // the base, which the engine ends with a cause
	deadline        time.Time
	done            chan struct{}
}

// newCallCtx is a call's context over base, ended at deadline.
func newCallCtx(base context.Context, deadline time.Time) *callCtx {
	c := &callCtx{Context: base, deadline: deadline, done: make(chan struct{})}
	context.AfterFunc(base, func() { close(c.done) })
	return c
}

// Deadline is the call's deadline.
func (c *callCtx) Deadline() (time.Time, bool) { return c.deadline, true }

// Done is closed once the base has ended.
func (c *callCtx) Done() <-chan struct{} { return c.done }

// Err is nil until Done is closed, then context.DeadlineExceeded when the
// deadline ended the call (its cause is errCallDeadline), as
// context.Context's contract says of a context past its deadline, and
// otherwise the base's: Canceled when the engine's life ended, or the call
// was answered.
func (c *callCtx) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	if errors.Is(context.Cause(c.Context), errCallDeadline) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// call runs fn on a goroutine of its own under a context derived from the
// engine's life that carries the call's deadline, timeout from now (none
// for 0), and ends there; it hands the owner what fn returns and when. At
// the deadline a call is expired, but a Release's deadline is the
// confirmed-release rule's (releaseDeadline), and its late acknowledgement
// is still taken.
func (e *Engine) call(kind callKind, s *session, timeout time.Duration, fn func(ctx context.Context) callResult) *call {
	o := &e.o
	o.nextCall++
	c := &call{id: o.nextCall, kind: kind, sess: s}
	base, cancel := context.WithCancelCause(e.life)
	c.cancel = cancel
	ctx := context.Context(base)
	if timeout > 0 {
		c.deadline = e.clk.Now().Add(timeout)
		ctx = newCallCtx(base, c.deadline)
		expires := kind != callRelease
		c.timer = e.clk.At(c.deadline, func() {
			cancel(errCallDeadline)
			if expires {
				e.post(msgCallExpired{c: c})
			}
		})
	}
	o.calls[c.id] = c
	e.spawn(func() any {
		r := e.callBackend(kind, ctx, fn)
		r.at = e.clk.Now()
		return msgCallDone{c: c, r: r}
	})
	return c
}

// callBackend is fn, on its call's goroutine. A panic in the backend is
// recovered there, logged, and is the call's failure (ReasonBackend), which
// the owner takes through the path an error the call returned takes: a
// Start's stops playback and releases the session, a Prepare's leaves what
// follows to a session of its own, a Cut's is one that could not cut, a
// Pause's and a Resume's answer their command, a Release's leaves the
// release unconfirmed until it is acknowledged, and a Discard's changes
// nothing. The error names the call and nothing the panic carried.
func (e *Engine) callBackend(kind callKind, ctx context.Context, fn func(ctx context.Context) callResult) (r callResult) {
	defer func() {
		if p := recover(); p != nil {
			where := "the backend's " + kind.String() + " call"
			e.logPanic(where, p)
			r = callResult{err: &Error{Reason: ReasonBackend, Op: kind.String(), Detail: where + " panicked; the bridge's log has why"}}
		}
	}()
	return fn(ctx)
}

// callDone applies what a call returned. An answer at or past the call's
// deadline is late whichever reaches the owner first, it or the expiry: its
// context ends as the deadline ends it, and the call is treated as failed
// (callExpired) and what it returned is not taken. A Release's late answer
// is an acknowledgement, taken after its deadline's policy (ackAt).
func (e *Engine) callDone(c *call, r callResult) {
	o := &e.o
	if !c.deadline.IsZero() && !r.at.Before(c.deadline) {
		c.cancel(errCallDeadline)
		if c.kind != callRelease && !c.expired {
			e.callExpired(c)
		}
	}
	delete(o.calls, c.id)
	if c.timer != nil {
		c.timer.Stop()
	}
	c.cancel(nil)
	if c.expired {
		return
	}
	s := c.sess
	switch c.kind {
	case callStart:
		e.startDone(s, r)
	case callPrepare:
		if s.succCall == c {
			s.succCall = nil
		}
		if e.current(s.id) != s {
			return
		}
		if r.err != nil {
			// The chain keeps its audible occurrence: one the backend reported
			// heard is not taken back by its Prepare's refusal.
			if c.next != nil {
				if i := s.index(c.next.id); i > 0 {
					releaseOccurrences(s.chain[i:])
					s.chain = s.chain[:i]
				}
			}
			// A successor heard before its Prepare answered was taken: the
			// hand-off the refusal is about happened, so the refusal is stale
			// and the session goes on naming successors, unless the backend
			// said the session is ending.
			if c.next != nil && s.index(c.next.id) == 0 && !errors.Is(r.err, ErrSessionEnding) {
				e.log.Info("player: the backend refused a successor it had already played; the session plays on", "session", uint64(s.id), "err", r.err)
				e.reconcile(s)
				return
			}
			s.noMore = true
			if !errors.Is(r.err, ErrSessionEnding) {
				e.log.Info("player: the backend took no successor; what follows plays in a session of its own", "session", uint64(s.id), "err", r.err)
			}
			return
		}
		e.reconcile(s)
	case callCut:
		if s.succCall == c {
			s.succCall = nil
		}
		if e.current(s.id) != s {
			return
		}
		switch i := s.index(c.from.id); {
		case i < 0:
		case !r.cut:
			c.from.late = true
		case i == 0:
			// A cut of what was heard: the backend answered it could after
			// reporting the occurrence audible, which a backend that keeps the
			// contract never does, and the chain would be left with nothing.
			e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonRemovedAudible,
				Detail: fmt.Sprintf("occurrence %d was cut after it became audible", c.from.id)})
			return
		default:
			releaseOccurrences(s.chain[i:])
			s.chain = s.chain[:i]
			s.endSent = false
		}
		e.reconcile(s)
	case callPause:
		e.pauseDone(c, r)
	case callResume:
		e.resumeDone(c, r)
	case callRelease:
		if r.err == nil {
			e.ackAt(s, r.at)
		} else if errors.Is(r.err, ErrReleaseUnconfirmed) {
			e.releaseDeadline(s)
		}
	}
}

// startDone applies Start's answer: the session plays; or its first entry
// was refused and is skipped; or it failed, and playback stops with why.
func (e *Engine) startDone(s *session, r callResult) {
	o := &e.o
	if s.phase != sessStarting || o.sess != s {
		return
	}
	e.attempted(r.err)
	if r.err == nil {
		s.phase = sessLive
		if s.chain[0].stage < stageSubmitted {
			s.chain[0].stage = stageSubmitted
		}
		// A start is the end of a pass: what was refused before it is
		// offered again, once, by the entries that follow it.
		o.pass = nil
		e.touched()
		e.reconcile(s)
		return
	}
	if s.native() {
		e.onFault(s.id, Fault{Reason: ReasonBackend, Detail: r.err.Error(), Occurrence: s.chain[0].id})
		return
	}
	var u *Unplayable
	if errors.As(r.err, &u) {
		e.skipAtStart(s, *u)
		return
	}
	e.playFailed(&Error{Reason: ReasonBackend, Op: "start", Detail: "the backend could not start the session", Err: r.err})
	e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonBackend, Detail: r.err.Error()})
}

// skipAtStart skips a session's first entry, refused before any of its
// content played: the selection moves to the entry that follows and a new
// session starts there, until every entry of the pass has been refused.
func (e *Engine) skipAtStart(s *session, u Unplayable) {
	entry := s.chain[0].entry
	e.skip(entry, u)
	if e.allFailed() {
		e.playFailed(refuse("play", ReasonAllUnplayable, "every entry was refused before any of its content played"))
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonAllUnplayable, Detail: "every entry was refused before any of its content played"})
		return
	}
	next := e.following(entry)
	if next == 0 {
		e.playFailed(&Error{Reason: ReasonUnplayable, Op: "play", Detail: fmt.Sprintf("entry %d: %s", entry, u.Reason)})
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonUnplayable, Detail: fmt.Sprintf("entry %d, the last, was refused: %s", entry, u.Reason)})
		return
	}
	e.selectEntry(next)
	e.carry(s)
	e.releaseSession(s, IntentPlay, StopInfo{})
}

// pauseDone applies Pause's answer.
func (e *Engine) pauseDone(c *call, r callResult) {
	o := &e.o
	s := c.sess
	if s.transportCall == c {
		s.transportCall = nil
	}
	w := c.waiter
	if e.current(s.id) != s || s.phase != sessLive {
		if w != nil {
			e.answer(w, reply{})
		}
		return
	}
	if r.err != nil {
		if w != nil {
			e.answer(w, reply{err: &Error{Reason: ReasonBackend, Op: "pause", Detail: "the backend could not pause", Err: r.err}})
		}
		return
	}
	s.paused = true
	o.state = Paused
	e.visible()
	e.armPauseDeadline(s)
	if w != nil {
		e.answer(w, reply{})
	}
}

// resumeDone applies Resume's answer; a refusal leaves the session paused
// with the reason.
func (e *Engine) resumeDone(c *call, r callResult) {
	o := &e.o
	s := c.sess
	if s.transportCall == c {
		s.transportCall = nil
	}
	w := c.waiter
	if e.current(s.id) != s || s.phase != sessLive {
		if w != nil {
			e.answer(w, reply{})
		}
		return
	}
	e.attempted(r.err)
	if r.err != nil {
		o.resumeErr = r.err.Error()
		e.touched()
		if w != nil {
			e.answer(w, reply{err: &Error{Reason: ReasonBackend, Op: "play", Detail: "the backend could not resume", Err: r.err}})
		}
		return
	}
	s.paused, s.devReleased = false, false
	s.stopPause()
	o.state, o.resumed, o.resumeErr = Playing, r.resume, ""
	e.visible()
	if w != nil {
		e.answer(w, reply{})
	}
}

// callExpired is a call that did not answer by its deadline: its context
// ends, its waiter is answered, and its session, when it is the current
// one, is released.
func (e *Engine) callExpired(c *call) {
	o := &e.o
	if _, ok := o.calls[c.id]; !ok {
		return
	}
	c.expired = true
	c.cancel(errCallDeadline)
	why := fmt.Sprintf("%s did not answer within its deadline", c.kind)
	if w := c.waiter; w != nil {
		e.answer(w, reply{err: refuse(w.op.String(), ReasonTimeout, why)})
		c.waiter = nil
	}
	s := c.sess
	if s == nil {
		return
	}
	if s.transportCall == c {
		s.transportCall = nil
	}
	if s.succCall == c {
		s.succCall = nil
	}
	if e.current(s.id) == s {
		if c.kind == callStart || c.kind == callResume {
			e.attempted(refuse("play", ReasonTimeout, why))
		}
		e.playFailed(refuse("play", ReasonTimeout, why))
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonTimeout, Detail: why})
	}
}

// attempted records the outcome of a play attempt whose answer the owner
// took (Snapshot.LastAttempt, B470 PR 5b): a session's Start or a Resume,
// err its failure or nil. The attempt is busy when err wraps ErrDeviceBusy,
// the backend's mapping of its device layer's busy refusal, and never by
// the error's text. It replaces the last attempt, so a later one that
// succeeds, or fails otherwise, ends a busy record. It moves no generation.
func (e *Engine) attempted(err error) {
	o := &e.o
	o.attempt = Attempt{Seq: o.attempt.Seq + 1, Failed: err != nil, Busy: errors.Is(err, ErrDeviceBusy)}
	e.touched()
}

// onAudible is the played content crossing an occurrence's first frame:
// the selection moves to its entry, and the playback the waiting play
// commands asked for has begun (playBegan). An occurrence an edit removed,
// or made no longer next, stops playback at once, and so does one crossed on
// the way to it; the selection never moves to an entry the queue does not
// hold.
func (e *Engine) onAudible(id SessionID, oid OccurrenceID) {
	o := &e.o
	s := e.current(id)
	if s == nil {
		return
	}
	i := s.index(oid)
	if i < 0 {
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonRemovedAudible, Detail: fmt.Sprintf("occurrence %d, which the engine did not hand off, became audible", oid)})
		return
	}
	for _, crossed := range s.chain[1 : i+1] {
		if crossed.unwanted != "" {
			e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonRemovedAudible, Detail: crossed.unwanted + ", and it became audible"})
			return
		}
	}
	oc := s.chain[i]
	if !o.q.has(oc.entry) {
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonRemovedAudible, Detail: fmt.Sprintf("entry %d is not in the queue, and it became audible", oc.entry)})
		return
	}
	releaseOccurrences(s.chain[:i])
	s.chain = s.chain[i:]
	oc.stage = stageAudible
	e.hear(oc)
	o.pos = Position{Occurrence: oc.id}
	o.starting = false
	e.playBegan(s)
	if _, ok := o.skips[oc.entry]; ok {
		e.setSkips(func(m map[uint32]Skip) { delete(m, oc.entry) })
	}
	e.selectEntry(oc.entry)
	e.touched()
	e.reconcile(s)
}

// onDetails is what the backend reports of an occurrence's content
// (DetailsEvents): kept with the occurrence, so it is the track's when the
// occurrence is heard, and the track's at once when it is heard already. A
// report from a session that is not current, or of an occurrence the
// session does not hold, changes nothing; none moves the generation.
func (e *Engine) onDetails(id SessionID, oid OccurrenceID, d Details) {
	o := &e.o
	s := e.current(id)
	if s == nil {
		return
	}
	i := s.index(oid)
	if i < 0 {
		return
	}
	s.chain[i].details = &d
	if o.track == nil || o.track.Occurrence != oid {
		return
	}
	before, had := o.track.details()
	t := *o.track
	t.Details, t.HasDetails = d, true
	o.track = &t
	if !had || before != d {
		o.detailsN++
	}
	e.touched()
}

// onFinished is everything handed off played out: playback stops on the
// last entry, or, when the backend took no successor the queue now has,
// that successor plays in a session of its own.
func (e *Engine) onFinished(id SessionID) {
	s := e.current(id)
	if s == nil {
		return
	}
	if next := e.following(e.o.selected); s.noMore && next != 0 {
		e.selectEntry(next)
		e.releaseSession(s, IntentPlay, StopInfo{})
		return
	}
	e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonEndOfQueue, Detail: "the last entry played out"})
}

// onFault is a failure after content played: recorded, playback stopped,
// never restarted.
func (e *Engine) onFault(id SessionID, f Fault) {
	o := &e.o
	s := e.current(id)
	if s == nil {
		return
	}
	f.Entry = o.selected
	if i := s.index(f.Occurrence); i >= 0 {
		f.Entry = s.chain[i].entry
	}
	if f.At.IsZero() {
		f.At = e.clk.Now()
	}
	faults := append(slices.Clone(o.faults), f)
	if len(faults) > MaxFaults {
		faults = faults[len(faults)-MaxFaults:]
	}
	o.faults = faults
	e.playFailed(refuse("play", ReasonFault, string(f.Reason)))
	e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonFault, Detail: string(f.Reason) + ": " + f.Detail})
}

// onRefused is a successor refused before any of its content played: it
// is skipped, and what follows it is named instead.
func (e *Engine) onRefused(id SessionID, oid OccurrenceID, u Unplayable) {
	s := e.current(id)
	if s == nil {
		return
	}
	i := s.index(oid)
	if i <= 0 {
		return
	}
	e.skip(s.chain[i].entry, u)
	releaseOccurrences(s.chain[i:])
	s.chain = s.chain[:i]
	s.endSent = false
	e.reconcile(s)
}

// onDeviceReleased is a paused session's own release of the device.
func (e *Engine) onDeviceReleased(id SessionID) {
	o := &e.o
	s := e.current(id)
	if s == nil || !s.paused {
		return
	}
	s.devReleased = true
	s.stopPause()
	if u := o.unconfirmed; u != nil && u.Session == s.id && u.Paused {
		o.unconfirmed = nil
		e.log.Info("player: the paused session's release of the device was reported late", "session", uint64(s.id))
		e.visible()
		return
	}
	e.touched()
}

// pauseOverdue is a pause whose release of the device was not reported by
// PauseRelease plus ReleaseTimeout: unconfirmed, under the same rule as a
// release.
func (e *Engine) pauseOverdue(s *session, seq uint64) {
	o := &e.o
	if e.current(s.id) != s || !s.paused || s.devReleased || seq != s.pauseSeq || o.unconfirmed != nil {
		return
	}
	o.unconfirmed = &ReleaseInfo{Session: s.id, Since: e.clk.Now(), Paused: true}
	e.log.Warn("player: a paused session's release of the device was not reported by its deadline; nothing opens the device until it is", "session", uint64(s.id))
	e.visible()
}

// discard asks the backend to drop what it prepared outside a session.
func (e *Engine) discard() {
	e.call(callDiscard, nil, CallTimeout, func(ctx context.Context) callResult { return callResult{err: e.be.Discard(ctx)} })
}

// effectivePauseRelease belongs to the occurrence holding the device. DSD's
// fixed native deadline overrides the configured PCM pause, in either direction.
func (e *Engine) effectivePauseRelease(s *session) time.Duration {
	if s.hasCurrent {
		if s.currentNative {
			return 30 * time.Second
		}
		return e.cfg.PauseRelease
	}
	for _, o := range s.chain {
		if o.plan != nil {
			return 30 * time.Second
		}
		if o.details != nil {
			if o.details.Codec == "DSD" && o.details.Bits == 1 {
				return 30 * time.Second
			}
			return e.cfg.PauseRelease
		}
	}
	return e.cfg.PauseRelease
}
