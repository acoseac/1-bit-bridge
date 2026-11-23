package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The publisher of the console's control (B634): one goroutine that orders
// every state the player bar takes. It wakes on the engine's Changed (taken
// before each read, so a publish after the read wakes it), its own
// subscription to the hotplug feed, the live config's change, a change of an
// operation's record, a tick for what has no signal of its own (the
// backend's facts and its test tone), and a handler's ask; each wake
// captures, on this one goroutine, one engine snapshot, the backend's facts
// and tone, the live config's withholding, the records and the clock, and
// builds the state from that capture alone. It gives a publication its
// revision in the same step as the capture, and nothing else assigns one.
//
// A capture of its own wakes is published only where its state differs from
// the last publication's. A capture asked for an operation's answer
// evaluates the operation's outcome on that capture, sets its record final
// within it, and is published under the next revision whatever it found (J1,
// K10), so an answer is never a state from before its command. A capture
// asked by a read (GET now, GET queue) is published where it differs, and the
// read otherwise answers the latest publication, at once.

// publisherTick is how often the publisher captures for what has no signal of
// its own: the backend's facts and its test tone.
const publisherTick = 500 * time.Millisecond

// anchorDrift is how far the measured position may depart from the anchor's
// extrapolation before the anchor is replaced (a seek, a stall).
const anchorDrift = time.Second

// recentRecords is how many records, whatever their tab, every publication
// carries.
const recentRecords = 8

// The kinds of a capture's ask.
type askKind int

const (
	// askRead is a read's: published where it differs, answered with the
	// latest otherwise.
	askRead askKind = iota
	// askFinal is an operation's answer: evaluated on the capture, its record
	// set final within it, published under the next revision whatever it
	// found.
	askFinal
)

// captureAsk is a handler's ask for a capture.
type captureAsk struct {
	kind askKind
	// eval runs on the publisher's goroutine on the capture, before the state
	// is built: it sets the asking operation's record final, under the
	// control's lock. Nil for a read.
	eval  func(*capture)
	reply chan publication
}

// publication is one publication: what the bar reads (admin's), and what the
// control reads of it, the engine snapshot it was built from.
type publication struct {
	admin.PlayerPublication
	snap  *engine.Snapshot
	state admin.PlayerState
}

// capture is what one wake of the publisher read: the clock, the player as
// it runs, the engine snapshot (nil while no engine runs), the backend's
// facts and tone, the live config's withholding and the closed engine's
// facts.
type capture struct {
	at       time.Time
	running  bool
	why      string
	snap     *engine.Snapshot
	tone     bool
	withheld string
	closed   playerClosedFacts
	closedOK bool
	live     *playback.PlaybackFacts
}

// publisher is the control's one publisher.
type publisher struct {
	c     *consoleControl
	asks  chan captureAsk
	nudge chan struct{}
	quit  chan struct{}
	done  chan struct{}

	// The publisher's goroutine's alone.
	lastCmp []byte
	lastIDs []uint32
	anchor  *anchorState

	mu       sync.Mutex
	rev      uint64
	queueRev uint64
	latest   *publication
	wake     chan struct{}
	// lastQueue is the latest publication's snapshot that held a queue, which
	// a switch-out of a closed engine hands over.
	lastQueue *engine.Snapshot
}

// anchorState is the anchor as the publisher keeps it: the published anchor,
// and the measurement it was set from, on the backend's clock.
type anchorState struct {
	a   admin.PlayerAnchor
	occ engine.OccurrenceID
	at  time.Time
}

// newPublisher is c's publisher, not running yet.
func newPublisher(c *consoleControl) *publisher {
	return &publisher{c: c, asks: make(chan captureAsk), nudge: make(chan struct{}, 1), quit: make(chan struct{}),
		done: make(chan struct{}), wake: make(chan struct{})}
}

// start runs the publisher on a goroutine of its own.
func (p *publisher) start() { go p.run() }

// stop ends the publisher and waits for it within bound, reporting whether it
// ended.
func (p *publisher) stop(bound time.Duration) bool {
	select {
	case <-p.quit:
	default:
		close(p.quit)
	}
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

// recordChanged wakes the publisher: a change of an operation's record.
func (p *publisher) recordChanged() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

// run is the publisher's loop.
func (p *publisher) run() {
	defer close(p.done)
	defer func() {
		if r := recover(); r != nil {
			logPlayerPanic("the console control's publisher", r)
		}
	}()
	tick := time.NewTicker(publisherTick)
	defer tick.Stop()
	var sub *hotplug.Subscription
	if f := p.c.p.feedOf(); f != nil {
		sub = f.Subscribe()
		defer sub.Close()
	}
	for {
		engChanged, ready, closedKnown := p.c.engineSignals()
		cfgChanged := p.c.cfgChanged()
		p.publishOwn(p.capture())
		var feedC <-chan hotplug.Snapshot
		if sub != nil {
			feedC = sub.C()
		}
		select {
		case <-p.quit:
			return
		case a := <-p.asks:
			p.serve(a)
		case <-engChanged:
		case <-ready:
		case <-closedKnown:
		case <-feedC:
		case <-cfgChanged:
		case <-p.nudge:
		case <-tick.C:
		}
	}
}

// serve answers a, on a capture of its own.
func (p *publisher) serve(a captureAsk) {
	cap := p.capture()
	var pub publication
	if a.kind == askFinal {
		if a.eval != nil {
			a.eval(cap)
		}
		pub = p.publish(cap, true)
	} else {
		pub = p.publishOwn(cap)
	}
	a.reply <- pub
}

// ask hands the publisher kind's ask, and waits for its publication, or for
// ctx's end or the publisher's, answering the latest publication then, and
// whether the ask's own publication was made. An ask the publisher ended
// before taking is not evaluated, and one it ended while serving (between its
// evaluation and its publication) is not published: no capture of the
// caller's stands in for the publisher's, so a final ask that reports none
// made is the publisher unavailable, never an outcome its eval decided.
func (p *publisher) ask(ctx context.Context, kind askKind, eval func(*capture)) (publication, bool) {
	a := captureAsk{kind: kind, eval: eval, reply: make(chan publication, 1)}
	select {
	case p.asks <- a:
	case <-p.done:
		return p.latestPub(), false
	case <-ctx.Done():
		return p.latestPub(), false
	}
	select {
	case pub := <-a.reply:
		return pub, true
	case <-p.done:
		select {
		case pub := <-a.reply:
			return pub, true
		default:
		}
		return p.latestPub(), false
	}
}

// latestPub is the latest publication, the zero one before the first.
func (p *publisher) latestPub() publication {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest == nil {
		return publication{}
	}
	return *p.latest
}

// nextRevision is the revision the publication being made now takes: asked
// from an operation's evaluation, on the publisher's goroutine, whose
// publication follows it under that revision.
func (p *publisher) nextRevision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rev + 1
}

// lastQueueSnap is the latest publication's snapshot that held a queue, nil
// for none: what a switch-out of a closed engine hands over.
func (p *publisher) lastQueueSnap() *engine.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastQueue
}

// wakeCh is closed at the next publication.
func (p *publisher) wakeCh() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wake
}

// capture reads, once, what a publication is built from.
func (p *publisher) capture() *capture {
	c := p.c
	cap := &capture{at: c.now()}
	eng, why := c.engine()
	cap.why = why
	if eng != nil {
		cap.snap = eng.Snapshot()
		if signalled(eng.Done()) || cap.snap.Closed() {
			cap.why = reasonClosed
		} else {
			cap.running = true
		}
	}
	if cap.why == reasonClosed {
		cap.closed, cap.closedOK = c.p.closedEngine()
	}
	if be := c.backend(); be != nil {
		if tb := tonesOf(be); tb != nil {
			cap.tone = tb.ToneStatus().State != playback.ToneIdle
		}
		if fb, ok := be.(factsBackend); ok {
			_, cap.live = fb.Facts()
		}
	}
	if live := c.liveConfig(); live != nil && c.view != nil {
		cap.withheld = c.view.withheld(live)
	}
	return cap
}

// publishOwn publishes cap where its state differs from the last
// publication's, and answers the latest publication.
func (p *publisher) publishOwn(cap *capture) publication {
	return p.publish(cap, false)
}

// publish builds cap's state and publishes it under the next revision when
// force is set or the state differs from the last publication's; it answers
// the publication it made, or the latest.
func (p *publisher) publish(cap *capture, force bool) publication {
	st, queueChanged, anchorNew := p.build(cap)
	cmp := st
	cmp.Revision, cmp.At, cmp.QueueRev = 0, "", 0
	if cmp.Anchor != nil {
		a := *cmp.Anchor
		a.ID = 0
		cmp.Anchor = &a
	}
	cmpBytes, _ := json.Marshal(cmp)
	p.mu.Lock()
	first := p.latest == nil
	p.mu.Unlock()
	if !force && !first && !queueChanged && !anchorNew && bytes.Equal(cmpBytes, p.lastCmp) {
		return p.latestPub()
	}
	if s := p.c.seams; s != nil && s.beforePublish != nil {
		s.beforePublish()
	}
	p.mu.Lock()
	p.rev++
	st.Revision = p.rev
	if queueChanged || first {
		p.queueRev = p.rev
	}
	st.QueueRev = p.queueRev
	if anchorNew && st.Anchor != nil {
		st.Anchor.ID = p.rev
		p.anchor.a.ID = p.rev
	} else if st.Anchor != nil && p.anchor != nil {
		st.Anchor.ID = p.anchor.a.ID
	}
	b, err := json.Marshal(st)
	if err != nil {
		p.mu.Unlock()
		playerLog.Error("player control: a publication could not be encoded", "err", err)
		return p.latestPub()
	}
	pub := &publication{PlayerPublication: admin.PlayerPublication{Epoch: p.c.epoch, Revision: st.Revision, QueueRev: st.QueueRev, Bytes: b},
		snap: cap.snap, state: st}
	p.latest = pub
	if cap.snap != nil && cap.snap.Len() > 0 {
		p.lastQueue = cap.snap
	}
	close(p.wake)
	p.wake = make(chan struct{})
	p.mu.Unlock()
	p.lastCmp = cmpBytes
	return *pub
}

// build is cap's state, whether its queue differs from the last capture's
// (its IDs: an ID is never handed out twice, so its reference is fixed, and
// the token, which is 0 while the engine's reservation is not durable, is
// not asked), and whether its anchor is a new one.
func (p *publisher) build(cap *capture) (admin.PlayerState, bool, bool) {
	c := p.c
	st := admin.PlayerState{Epoch: c.epoch, At: cap.at.UTC().Format(time.RFC3339Nano), Running: cap.running, Hold: holdNone,
		Release: admin.PlayerReleaseState{State: releaseConfirmed}, Commands: []string{}, Operations: c.recentRecords()}
	if !cap.running {
		st.Why = cap.why
	}
	s := cap.snap
	var ids []uint32
	if s != nil {
		ids = s.IDs()
	}
	queueChanged := !slices.Equal(ids, p.lastIDs)
	p.lastIDs = ids
	st.Offered, st.OfferedWhy = offeredOf(cap)
	st.Silent = silentOf(cap)
	st.Tone = cap.tone
	if s == nil {
		return st, queueChanged, p.setAnchor(nil)
	}
	st.Transport = s.State().String()
	st.Starting = s.Starting()
	if s.State() == engine.Releasing {
		st.Pending = s.Pending().String()
	}
	st.Hold = holdOf(s)
	if u, ok := s.ReleaseUnconfirmed(); ok {
		st.Release = admin.PlayerReleaseState{State: releaseUnconfirmed, Since: u.Since.UTC().Format(time.RFC3339Nano)}
	}
	st.Generation = s.Generation().String()
	st.Token, st.Length = s.Token(), s.Len()
	st.Selected, st.Repeat = s.Selected(), s.Repeat()
	st.LastStop = admin.PlayerLastStop{Reason: string(s.LastStop().Reason), Entry: s.LastStop().Entry}
	a := s.LastAttempt()
	st.LastAttempt = admin.PlayerLastAttempt{Seq: a.Seq, Failed: a.Failed, Busy: a.Busy, Denied: a.Denied}
	t, heard := s.Track()
	st.Seek = cap.running && heard && seekableTrack(t)
	if cap.running {
		st.Commands = commandsOf(s, st.Seek)
	}
	if heard {
		st.Track = c.trackOf(t)
	}
	if lf := cap.live; lf != nil && cap.running {
		st.Signal = &admin.PlayerSignal{Transport: lf.Transport, Format: lf.Format, RateHz: lf.RateHz, Channels: lf.Channels,
			ValidBits: lf.ValidBits, State: lf.State}
	}
	var anchorNew bool
	if heard {
		anchorNew = p.setAnchor(&anchorInput{t: t, pos: s.Position(), advancing: s.State() == engine.Playing && !s.Starting()})
	} else {
		anchorNew = p.setAnchor(nil)
	}
	if p.anchor != nil {
		an := p.anchor.a
		st.Anchor = &an
	}
	return st, queueChanged, anchorNew
}

// anchorInput is what an anchor is judged from: the track heard, the
// position measured, and whether it advances.
type anchorInput struct {
	t         engine.Track
	pos       engine.Position
	advancing bool
}

// setAnchor replaces the anchor only where its occurrence changes, whether
// it advances changes, or the measured position departs from the anchor's
// extrapolation by more than anchorDrift (§4.4), and reports whether it did:
// otherwise every publication carries the same anchor, its id and time
// included, so steady playback publishes nothing.
func (p *publisher) setAnchor(in *anchorInput) bool {
	if in == nil {
		changed := p.anchor != nil
		p.anchor = nil
		return changed
	}
	seconds, at := 0.0, time.Time{}
	if in.pos.Occurrence == in.t.Occurrence {
		seconds, at = in.pos.Seconds(), in.pos.Measured
	}
	if old := p.anchor; old != nil && old.occ == in.t.Occurrence && old.a.Advancing == in.advancing {
		expect := old.a.Seconds
		if old.a.Advancing && !at.IsZero() && !old.at.IsZero() {
			expect += at.Sub(old.at).Seconds()
		}
		if math.Abs(seconds-expect) <= anchorDrift.Seconds() {
			return false
		}
	}
	p.anchor = &anchorState{occ: in.t.Occurrence, at: at, a: admin.PlayerAnchor{Occurrence: uint64(in.t.Occurrence), Entry: in.t.Entry,
		Seconds: seconds, At: anchorTime(at), Advancing: in.advancing}}
	return true
}

// anchorTime is t as an anchor carries it, "" for none.
func anchorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// The hold and release words of the state.
const (
	holdNone           = "none"
	holdCheck          = "check"
	holdMaintenance    = "maintenance"
	holdConfiguring    = "configuring"
	releaseConfirmed   = "confirmed"
	releaseUnconfirmed = "unconfirmed"
)

// holdOf is the hold s shows: the DSD check, maintenance, or a configuration
// reservation (a DAC's DSD choice being written).
func holdOf(s *engine.Snapshot) string {
	if m, ok := s.Maintenance(); ok {
		if m.Reason == holdCheck {
			return holdCheck
		}
		return holdMaintenance
	}
	if s.ConfigurationReserved() {
		return holdConfiguring
	}
	return holdNone
}

// offeredOf is whether the bridge's player can be an output on cap, or why
// not: it does not run, its output is not known or not available (the
// report's word), or the live config withholds it.
func offeredOf(cap *capture) (bool, string) {
	if !cap.running {
		return false, cap.why
	}
	switch out := cap.snap.Output(); {
	case !out.Known:
		return false, reasonOutputUnknown
	case !out.Available:
		if out.Reason == "" {
			return false, reasonOutputUnavailable
		}
		return false, out.Reason
	}
	if cap.withheld != "" {
		return false, cap.withheld
	}
	return true, ""
}

// silentOf is the barrier's predicate on cap (§3.1): an engine that runs,
// stopped or paused, nothing starting or releasing, no release unconfirmed,
// no hold and no test tone in the device slot; a player that never ran an
// engine, which holds no device; and a closed engine only once serve's
// player has said no session holds the device (playerClosedFacts.Silent).
func silentOf(cap *capture) bool {
	if cap.tone {
		return false
	}
	switch {
	case cap.running:
		s := cap.snap
		if _, ok := s.ReleaseUnconfirmed(); ok {
			return false
		}
		if _, held := s.Maintenance(); held {
			return false
		}
		st := s.State()
		return (st == engine.Stopped || st == engine.Paused) && !s.Starting()
	case cap.why == reasonClosed:
		return cap.closedOK && cap.closed.Silent()
	}
	return true
}

// seekableTrack reports whether the engine seeks within t: PCM of known
// length and rate (engine.TimeSeek).
func seekableTrack(t engine.Track) bool {
	if !t.HasDetails {
		return false
	}
	d := t.Details
	dsd := d.Codec == "DSD" || d.Bits == 1
	return engine.TimeSeek(dsd) && d.Frames > 0 && d.RateHz > 0
}

// The words of the commands the state offers.
const (
	cmdWordReplace = "replace"
	cmdWordAppend  = "append"
	cmdWordDelete  = "delete"
)

// commandsOf is what the engine takes now, by its table (doc.go) read off s:
// stop while the engine runs; under a hold stop alone; pause and repeat;
// the edits while the engine takes token-advancing edits; and whatever
// starts a session (play, next, previous, index, and the seek in a track it
// can seek) while the output is known and available, no configuration is
// being written, no release is unconfirmed and the queue holds an entry.
func commandsOf(s *engine.Snapshot, seekable bool) []string {
	out := []string{admin.PlayerCmdStop}
	if _, held := s.Maintenance(); held {
		return out
	}
	out = append(out, admin.PlayerCmdPause, admin.PlayerCmdRepeat)
	if s.Edits().Refused == "" {
		out = append(out, cmdWordReplace, cmdWordAppend, cmdWordDelete)
	}
	out2 := s.Output()
	_, unconfirmed := s.ReleaseUnconfirmed()
	if !out2.Known || !out2.Available || s.ConfigurationReserved() || unconfirmed || s.Len() == 0 {
		return out
	}
	out = append(out, admin.PlayerCmdPlay, admin.PlayerCmdNext, admin.PlayerCmdPrevious, admin.PlayerCmdIndex)
	if st := s.State(); seekable && (st == engine.Playing || st == engine.Paused) {
		out = append(out, admin.PlayerCmdSeek)
	}
	return out
}
