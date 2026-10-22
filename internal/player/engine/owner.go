package engine

import (
	"context"
	"maps"
	"slices"
)

// owner is the state the owner goroutine alone reads and writes, and New
// before it starts.
type owner struct {
	gen          uint64
	genDirty     bool // a controller-visible change since the last publish
	dirty        bool // any change since the last publish
	persistDirty bool // a change of what the queue file holds

	q        *queue
	token    uint32
	selected uint32
	repeat   bool
	shuffle  bool
	res      reservation

	state       State
	then        Intent
	starting    bool
	sess        *session               // the session starting, live or releasing
	sessions    map[SessionID]*session // every session whose release is not yet acknowledged
	unconfirmed *ReleaseInfo
	audible     Audible
	pos         Position
	join        *Join
	resumed     ResumeResult
	resumeErr   string
	lastStop    StopInfo
	skips       map[uint32]Skip // replaced, never written to, once published
	pass        map[uint32]bool // the entries refused since the last start or queue change
	faults      []Fault         // replaced, never written to, once published
	output      OutputState
	maint       *maintHold
	persist     PersistState

	waiters []waiter
	outbox  []answered
	notes   []note
	parked  []*command
	// drainAt is where drainParked took the command it runs from, so a
	// command parked again keeps its place in its lane; -1 otherwise.
	drainAt  int
	calls    map[uint64]*call
	nextCall uint64
	nextSess uint64
	nextOcc  uint64

	closing    bool
	phase      shutdownPhase
	writerDone bool
	graceOver  bool
	finished   bool
	grace      timer
}

// waiter is a command answered when the transport settles: no start and no
// release under way. play is a command that asked for playback, and its
// answer is the first outcome recorded for that playback, whatever later
// commands do: decided is set by that outcome, and err is it, nil once the
// playback began, the session started for it heard (playBegan), and
// otherwise why it did not happen (playFailed): a later command that
// replaced the session started for it before it was heard (replaced)
// among them. So a later Play that plays never answers for an earlier one,
// and nothing that stops a playback after it began is the answer of the
// command that asked for it.
//
// sess is the session started for a play command (startSession), and 0
// while it waits behind a release or a pending intent: the session that
// starts once the release is acknowledged is started for it, and for every
// other command waiting so. Only sess's Audible decides it, and the settle
// answers it nil only while sess is the session playing. The engine's own
// move past an entry refused at its start hands it on to the session that
// follows (carry), and carried marks it until that session starts: a Next,
// Previous, PlayID or PlayIndex sent meanwhile that moves the pending target
// replaces the playback the engine was moving on to, and supersedes it
// (replacedCarry), where the commands that set the pending intent
// themselves, and one that names the entry already pending, share the
// session after the release.
type waiter struct {
	c       *command
	play    bool
	sess    SessionID
	carried bool
	decided bool
	err     error
}

// shutdownPhase is how far shutdown has got.
type shutdownPhase int

const (
	phaseRunning shutdownPhase = iota
	// phaseReleasing waits for the session's release to settle and for a
	// reservation write under way.
	phaseReleasing
	// phaseFlushing waits for the writer to write the newest revision and
	// exit.
	phaseFlushing
	// phaseJoining waits shutdownGrace for backend calls still under way.
	phaseJoining
)

// start loads what the files hold and publishes the first snapshot. It
// runs in New: the owner has not started, so it may wait on the files.
func (e *Engine) start() {
	o := &e.o
	o.q = emptyQueue
	o.sessions = map[SessionID]*session{}
	o.calls = map[uint64]*call{}
	o.gen = 1
	o.drainAt = -1

	rf := e.loadReserve()
	idsBase, tokBase := uint64(rf.IDs), uint64(rf.Tokens)
	if qf, entries := e.loadQueue(); qf != nil {
		o.q = newQueue(entries)
		o.selected, o.repeat = qf.Selected, qf.Repeat
		if qf.dangling != 0 {
			e.log.Warn("player: the queue file's selection is not in its queue; the queue loads with nothing selected",
				"path", e.cfg.QueuePath, "selected", qf.dangling)
		}
		idsBase = max(idsBase, qf.IDs.Reserved)
		if qf.IDs.Next > 0 {
			idsBase = max(idsBase, qf.IDs.Next-1)
		}
		tokBase = max(tokBase, qf.Tokens.Reserved, uint64(qf.Tokens.Current))
		for _, en := range entries {
			idsBase = max(idsBase, uint64(en.id))
		}
	}
	o.res.ids, o.res.tokens = startCounter(idsBase), startCounter(tokBase)
	if o.res.stuckReserve == "" {
		ids, tokens := o.res.ids.target(), o.res.tokens.target()
		if err := e.sm.writeReserve(e.cfg.ReservePath, encodeReserve(ids, tokens)); err != nil {
			o.res.failed = err
			e.log.Warn("player: the reservation of this run could not be written; inserts and the other edits that advance the token are refused until one is",
				"path", e.cfg.ReservePath, "err", err)
		} else {
			o.res.ids.durable, o.res.tokens.durable = ids, tokens
		}
	}
	// The run's token is published only once a durable reservation covers
	// it: after a reservation write that failed, or with a bad reservation
	// file that stays, there is none (0) until an edit's reservation lands,
	// so two such starts never publish one token twice.
	if !o.res.tokens.exhausted() && !o.res.tokens.needs() {
		o.token = o.res.tokens.take()
	}
	if o.res.stuckQueue != "" {
		e.w.off = true
	}
	o.state = Stopped
	o.dirty = true
	e.publishIfDirty()
}

// loadReserve reads the reservation file: none is a reservation of
// nothing, and a bad one is moved aside and counts as none.
func (e *Engine) loadReserve() reserveFile {
	path := e.cfg.ReservePath
	data, err := e.readBounded(path, maxReserveFileBytes)
	if err == nil && data == nil {
		return reserveFile{}
	}
	var f reserveFile
	if err == nil {
		f, err = decodeReserve(data)
	}
	if err != nil {
		if !e.moveAside("reservation file", path, err.Error()) {
			e.o.res.stuckReserve = path
		}
		return reserveFile{}
	}
	return f
}

// loadQueue reads the queue file: none is an empty queue, and a bad one is
// moved aside and the queue starts empty.
func (e *Engine) loadQueue() (*queueFile, []entry) {
	path := e.cfg.QueuePath
	data, err := e.readBounded(path, maxQueueFileBytes)
	if err == nil && data == nil {
		return nil, nil
	}
	var f *queueFile
	var entries []entry
	if err == nil {
		f, entries, err = decodeQueue(data)
	}
	if err != nil {
		if !e.moveAside("queue file", path, err.Error()) {
			e.o.res.stuckQueue = path
		}
		return nil, nil
	}
	return f, entries
}

// loop is the owner: one command, message or shutdown step at a time,
// and a snapshot after each that changed anything.
func (e *Engine) loop(ctx context.Context) {
	e.spawn(e.w.run)
	stop := ctx.Done()
	for !e.o.finished {
		select {
		case c := <-e.cmds:
			e.admit(c)
		case m := <-e.msgs:
			e.handle(m)
		case <-stop:
			stop = nil
			e.beginShutdown()
		}
		e.advance()
	}
	e.finish()
}

// advance does what the last step made possible: parked commands, waiters
// whose transport has settled, and the next steps of a shutdown.
func (e *Engine) advance() {
	e.drainParked()
	e.settleWaiters()
	e.publishIfDirty()
	if e.o.closing {
		for {
			phase := e.o.phase
			e.shutdownStep()
			if e.o.phase == phase || e.o.finished {
				break
			}
		}
		e.publishIfDirty()
	}
	e.sendReplies()
}

// beginShutdown refuses every mutation from now on, answers every command
// still waiting, and releases the session.
func (e *Engine) beginShutdown() {
	o := &e.o
	o.closing = true
	o.phase = phaseReleasing
	e.touched()
	for _, c := range o.parked {
		e.answer(c, reply{err: closing(c)})
	}
	o.parked = nil
	for _, w := range o.waiters {
		e.answer(w.c, reply{err: closing(w.c)})
	}
	o.waiters = nil
	for _, c := range o.calls {
		if c.waiter != nil {
			e.answer(c.waiter, reply{err: closing(c.waiter)})
			c.waiter = nil
		}
	}
	if o.maint != nil {
		if o.maint.waiter != nil {
			e.answer(o.maint.waiter, reply{err: closing(o.maint.waiter)})
		}
		o.maint.timer.Stop()
		o.maint = nil
		e.visible()
	}
	o.then = IntentStop
	if s := o.sess; s != nil && (s.phase == sessStarting || s.phase == sessLive) {
		e.releaseSession(s, IntentStop, StopInfo{Reason: ReasonStopped, Detail: "the engine is shutting down"})
	}
}

// closing is c's refusal once shutdown has begun.
func closing(c *command) error {
	return refuse(c.op.String(), ReasonClosed, "the engine is shutting down")
}

// shutdownStep moves shutdown on by one phase when the last one is done.
func (e *Engine) shutdownStep() {
	o := &e.o
	switch o.phase {
	case phaseReleasing:
		if (o.sess != nil && o.sess.phase == sessReleasing) || o.res.inflight {
			return
		}
		o.phase = phaseFlushing
		close(e.w.stop)
	case phaseFlushing:
		if !o.writerDone {
			return
		}
		o.phase = phaseJoining
		e.lifeCancel()
		if len(o.calls) > 0 {
			o.grace = e.clk.AfterFunc(shutdownGrace, func() { e.post(msgGraceOver{}) })
		}
	case phaseJoining:
		if len(o.calls) > 0 && !o.graceOver {
			return
		}
		o.finished = true
	}
}

// finish abandons the backend calls still under way, logging each, and
// ends Run.
func (e *Engine) finish() {
	o := &e.o
	for _, c := range o.calls {
		var id SessionID
		if c.sess != nil {
			id = c.sess.id
		}
		e.log.Warn("player: abandoned a backend call that did not return; its goroutine outlives Run", "call", c.kind.String(), "session", uint64(id))
	}
	e.abandoned.Store(int64(len(o.calls)))
	if o.grace != nil {
		o.grace.Stop()
	}
	for _, s := range o.sessions {
		s.stopTimers()
	}
	if b := o.res.bound; b != nil {
		b.Stop()
	}
	e.lifeCancel()
	e.publishIfDirty()
	e.sendReplies()
	close(e.done)
}

// lane is which parked commands a command keeps its order among: a command
// waits behind the parked commands of its lane, never another lane's.
type lane int

const (
	// laneNone never waits: Stop, the output's reports, the maintenance
	// commands and the modes.
	laneNone lane = iota
	// laneValues is the edits that take new IDs or tokens: they wait for a
	// reservation write that has not landed, at most reserveTimeout.
	laneValues
	// laneTransport is the transport commands but Stop: Play and Pause wait
	// for a Pause or a Resume under way, and Pause for a start.
	laneTransport
	lanes
)

// lane is o's lane.
func (o op) lane() lane {
	switch o {
	case opInsert, opDelete, opDeleteAll:
		return laneValues
	case opPlay, opPause, opNext, opPrevious, opPlayID, opPlayIndex:
		return laneTransport
	}
	return laneNone
}

// admit takes c from its caller: a context that has ended means it does
// not run; blocked, or behind a parked command of its lane, it waits its
// turn.
func (e *Engine) admit(c *command) {
	if e.sm.admitted != nil {
		e.sm.admitted(c)
	}
	if err := c.ctx.Err(); err != nil {
		e.answer(c, reply{err: notAdmitted(c.op.String(), err)})
		return
	}
	if !e.o.closing && e.waits(c) {
		e.o.parked = append(e.o.parked, c)
		return
	}
	e.dispatch(c)
}

// waits reports whether c must wait its turn: blocked, or behind a parked
// command of its lane. A command of no lane never waits, and nothing waits
// while maintenance is held, which refuses it at once.
func (e *Engine) waits(c *command) bool {
	l := c.op.lane()
	if l == laneNone || e.o.maint != nil {
		return false
	}
	for _, p := range e.o.parked {
		if p.op.lane() == l {
			return true
		}
	}
	return e.blocked(c)
}

// blocked reports a command that must wait now: an edit that takes new
// values while a reservation write is awaited, and Play and Pause while a
// pause or a resume is under way (or, for Pause, while the session is still
// starting).
func (e *Engine) blocked(c *command) bool {
	o := &e.o
	switch c.op.lane() {
	case laneValues:
		return o.res.awaiting
	case laneTransport:
		if s := o.sess; s != nil && (c.op == opPlay || c.op == opPause) {
			return s.transportCall != nil || (c.op == opPause && s.phase == sessStarting)
		}
	}
	return false
}

// park puts c back to wait: where drainParked took it from when it was
// being run from there, so it keeps its place in its lane.
func (e *Engine) park(c *command) {
	o := &e.o
	c.parked = true
	if i := o.drainAt; i >= 0 {
		o.parked = slices.Insert(o.parked, i, c)
		return
	}
	o.parked = append(o.parked, c)
}

// drainParked runs the parked commands that nothing blocks now, in order
// within each lane: a blocked command holds its lane, never another.
func (e *Engine) drainParked() {
	o := &e.o
	for {
		i := e.runnable()
		if i < 0 {
			break
		}
		c := o.parked[i]
		o.parked = slices.Delete(o.parked, i, i+1)
		o.drainAt = i
		e.dispatch(c)
		o.drainAt = -1
	}
	if !o.res.awaiting {
		o.res.refuseParked = false
	}
}

// runnable is the index of the first parked command that may run, nothing
// blocking it nor any command of its lane before it, or -1.
func (e *Engine) runnable() int {
	o := &e.o
	var stalled [lanes]bool
	for i, c := range o.parked {
		l := c.op.lane()
		if stalled[l] {
			continue
		}
		if !o.closing && e.blocked(c) {
			stalled[l] = true
			continue
		}
		return i
	}
	return -1
}

// supersedeParked answers the parked transport commands that a command
// run before them has overtaken (a Stop, the output going away): a Pause
// with nil, playback being stopped as it asked, and the rest with why
// (ReasonOvertaken, or the output's reason). None of them ran, so none
// changed anything.
func (e *Engine) supersedeParked(why func(c *command) error) {
	o := &e.o
	kept := o.parked[:0]
	for _, p := range o.parked {
		if p.op.lane() != laneTransport {
			kept = append(kept, p)
			continue
		}
		var err error
		if p.op != opPause {
			err = why(p)
		}
		e.answer(p, reply{err: err})
	}
	clear(o.parked[len(kept):])
	o.parked = kept
}

// dispatch runs c.
func (e *Engine) dispatch(c *command) {
	o := &e.o
	c.parked = false
	name := c.op.String()
	switch {
	case o.closing:
		e.answer(c, reply{err: closing(c)})
	case c.op == opOutput:
		e.reportOutput(c)
	case c.op == opEnterMaint:
		e.enterMaintenance(c)
	case c.op == opRenewMaint:
		e.renewMaintenance(c)
	case c.op == opExitMaint:
		e.exitMaintenance(c)
	case o.maint != nil:
		if c.op == opStop {
			e.answer(c, reply{})
		} else {
			e.answer(c, reply{err: refuse(name, ReasonMaintenance, "maintenance is held: "+o.maint.reason)})
		}
	case !e.condsHold(c.conds):
		e.answer(c, reply{err: &Error{Reason: ReasonConflict, Op: name, Detail: "the state changed since the generation the command expected", Current: e.generation()}})
	default:
		switch c.op {
		case opInsert:
			e.insert(c)
		case opDelete:
			e.delete(c)
		case opDeleteAll:
			e.deleteAll(c)
		case opSetRepeat:
			e.setRepeat(c)
		case opSetShuffle:
			e.setShuffle(c)
		default:
			e.transport(c)
		}
	}
	if !c.parked && e.sm.applied != nil {
		e.o.notes = append(e.o.notes, note{what: name, applied: true})
	}
}

// generation is the generation as published, with the changes of this step
// counted.
func (e *Engine) generation() Generation {
	n := e.o.gen
	if e.o.genDirty {
		n++
	}
	return Generation{run: e.run, n: n}
}

// condsHold reports whether every condition holds now.
func (e *Engine) condsHold(conds []Cond) bool {
	for _, c := range conds {
		if c.gen != e.generation() {
			return false
		}
	}
	return true
}

// settleWaiters answers the waiters once no start and no release is under
// way. A play command with no outcome recorded answers nil only while the
// session started for it plays.
func (e *Engine) settleWaiters() {
	o := &e.o
	if len(o.waiters) == 0 || e.busy() {
		return
	}
	for _, w := range o.waiters {
		var err error
		if w.play {
			err = w.err
			if !w.decided && (o.state != Playing || o.sess == nil || o.sess.id != w.sess) {
				// Every path that ends without the playback records why
				// (playFailed, replaced); this keeps the answer from being nil
				// if one is ever missed.
				err = refuse(w.c.op.String(), ReasonSuperseded, "playback did not begin")
			}
		}
		e.answer(w.c, reply{err: err})
	}
	o.waiters = nil
}

// busy reports a start or a release under way.
func (e *Engine) busy() bool {
	s := e.o.sess
	return s != nil && (s.phase == sessStarting || s.phase == sessReleasing)
}

// wait answers c when the transport settles.
func (e *Engine) wait(c *command, play bool) {
	e.o.waiters = append(e.o.waiters, waiter{c: c, play: play})
}

// visible marks a controller-visible change: the generation advances.
func (e *Engine) visible() {
	e.o.genDirty = true
	e.o.dirty = true
}

// touched marks a change only the snapshot shows: the generation stays.
func (e *Engine) touched() { e.o.dirty = true }

// saved marks a visible change the queue file holds.
func (e *Engine) saved() {
	e.o.persistDirty = true
	e.visible()
}

// publishIfDirty publishes the step's changes: a new generation for a
// visible one, a revision for the writer when the queue file's content
// changed, and the snapshot.
func (e *Engine) publishIfDirty() {
	o := &e.o
	if !o.dirty {
		return
	}
	if o.genDirty {
		o.gen++
		o.genDirty = false
	}
	if o.persistDirty {
		o.persist.Revision++
		e.w.update(e.payload())
		o.persistDirty = false
	}
	o.dirty = false
	e.publish(e.snapshot())
}

// payload is the queue file's content now.
func (e *Engine) payload() *payload {
	o := &e.o
	return &payload{
		rev:         o.persist.Revision,
		q:           o.q,
		idsNext:     o.res.ids.next,
		idsReserved: o.res.ids.durable,
		tokCurrent:  o.token,
		tokReserved: o.res.tokens.durable,
		repeat:      o.repeat,
		shuffle:     o.shuffle,
		selected:    o.selected,
	}
}

// snapshot is the state as a reader sees it.
func (e *Engine) snapshot() *Snapshot {
	o := &e.o
	sn := &Snapshot{
		gen:       Generation{run: e.run, n: o.gen},
		state:     o.state,
		starting:  o.starting,
		q:         o.q,
		token:     o.token,
		selected:  o.selected,
		audible:   o.audible,
		pos:       o.pos,
		repeat:    o.repeat,
		shuffle:   o.shuffle,
		output:    o.output,
		resumed:   o.resumed,
		resumeErr: o.resumeErr,
		lastStop:  o.lastStop,
		skips:     o.skips,
		faults:    o.faults,
		edits:     e.editState(),
		persist:   o.persist,
		closed:    o.closing,
	}
	if o.state == Releasing {
		sn.pending = o.then
	}
	if o.maint != nil {
		sn.maint = &MaintenanceInfo{Reason: o.maint.reason, Since: o.maint.since, Expires: o.maint.expires, ReleaseConfirmed: o.maint.confirmed}
	}
	if o.unconfirmed != nil {
		u := *o.unconfirmed
		sn.unconfirmed = &u
	}
	if s := o.sess; s != nil && s.phase == sessLive {
		sn.devReleased = s.devReleased
	}
	if o.join != nil {
		sn.join = &Join{Gapless: o.join.Gapless, Differs: slices.Clone(o.join.Differs)}
	}
	return sn
}

// editState is why inserts and the other token-advancing edits are
// refused now, or nothing.
func (e *Engine) editState() EditState {
	r := &e.o.res
	switch {
	case r.stuckQueue != "":
		return EditState{Refused: ReasonFileStuck, Detail: "the queue file " + r.stuckQueue + " is bad and could not be moved aside"}
	case r.stuckReserve != "":
		return EditState{Refused: ReasonFileStuck, Detail: "the reservation file " + r.stuckReserve + " is bad and could not be moved aside"}
	case r.ids.exhausted():
		return EditState{Refused: ReasonIDsExhausted, Detail: "every ID a uint32 holds has been handed out"}
	case r.tokens.exhausted():
		return EditState{Refused: ReasonIDsExhausted, Detail: "every token a uint32 holds has been handed out"}
	case r.failed != nil && (r.ids.needs() || r.tokens.needs()):
		return EditState{Refused: ReasonReservation, Detail: r.failed.Error()}
	}
	return EditState{}
}

// setSkips replaces the skip record with one changed by f.
func (e *Engine) setSkips(f func(m map[uint32]Skip)) {
	m := maps.Clone(e.o.skips)
	if m == nil {
		m = map[uint32]Skip{}
	}
	f(m)
	if len(m) == 0 {
		m = nil
	}
	e.o.skips = m
	e.touched()
}
