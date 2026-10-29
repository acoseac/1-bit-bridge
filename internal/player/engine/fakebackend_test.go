package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeBackend is a Backend that plays nothing and does what the test says.
// By default Start opens at once (an entry whose reference is in
// unplayable is refused before anything opens), every other call answers
// at once with nil (Cut true, Resume continued or, after DeviceReleased,
// reopened), and Release waits for its session's Start to return, closes
// what it opened and acknowledges; if its context ends first it answers
// with the context's error and acknowledges later through Events.Released,
// as the contract asks. It enforces the contract's first term: a Start
// while another session's release is unacknowledged is a violation, which
// fails the test.
type fakeBackend struct {
	t *testing.T

	mu         sync.Mutex
	sessions   map[SessionID]*fakeSession
	order      []*fakeSession
	calls      []fakeCall
	violations []string
	discards   int
	changed    chan struct{}
	cleanup    chan struct{}
	// wg counts the fake's own goroutines; closing, under mu, stops new
	// ones once close has begun to wait for them.
	wg      sync.WaitGroup
	closing bool

	unplayable map[string]Unplayable
	onStart    func(ctx context.Context, s *fakeSession) error
	onPrepare  func(ctx context.Context, s *fakeSession, after OccurrenceID, next *Occurrence) error
	onCut      func(ctx context.Context, s *fakeSession, from OccurrenceID) (bool, error)
	onPause    func(ctx context.Context, s *fakeSession) error
	onResume   func(ctx context.Context, s *fakeSession) (ResumeResult, error)
	onRelease  func(ctx context.Context, s *fakeSession) error
	onDiscard  func(ctx context.Context) error

	// beforeStart runs on a Start's goroutine before the fake records any of
	// it, so a test can hold a Start that has not reached the backend yet:
	// the engine makes each call on a goroutine of its own, and a Release of
	// the session can get there first (B576).
	beforeStart func(req StartRequest)
	// beforePrepare runs on a Prepare's goroutine before the fake records any
	// of it, so a test can hold a Prepare the engine has made that has not
	// reached the backend yet (B579).
	beforePrepare func(id SessionID, after OccurrenceID, next *Occurrence)
	// waiting runs each time waitFor finds its condition false, before it
	// waits for the fake to change: a test that holds a call short of the
	// fake until a reader waits for it, the latest a scheduler could leave
	// it, lets it go there (B579).
	waiting func()
}

// fakeSession is one session the engine started on a fakeBackend.
type fakeSession struct {
	b     *fakeBackend
	id    SessionID
	first Occurrence
	opts  SessionOptions
	ev    Events

	// Under b.mu.
	started               bool
	opened, closed, wrote bool
	releaseAsked          bool
	acked                 bool
	devReleased           bool
	prepares              []fakePrepare
	startDone             chan struct{}
}

// fakePrepare is one Prepare a session was given.
type fakePrepare struct {
	after OccurrenceID
	next  *Occurrence
}

// fakeCall is one call the engine made.
type fakeCall struct {
	kind    string
	session SessionID
	// entry is a start's first entry or a prepare's next one (0: none
	// follows); occ that occurrence, or a cut's from.
	entry uint32
	occ   OccurrenceID
	after OccurrenceID
}

func (c fakeCall) String() string {
	switch c.kind {
	case "start":
		return fmt.Sprintf("start %d", c.entry)
	case "prepare":
		if c.entry == 0 {
			return "prepare end"
		}
		return fmt.Sprintf("prepare %d", c.entry)
	case "cut":
		return fmt.Sprintf("cut %d", c.occ)
	}
	return c.kind
}

// newFakeBackend is a fake for t. Its contract check, verify, is t's cleanup,
// registered here before any engine is built over the fake, so it runs after
// the cleanups that shut that engine down and join every goroutine it
// started.
func newFakeBackend(t *testing.T) *fakeBackend {
	b := &fakeBackend{
		t:          t,
		sessions:   map[SessionID]*fakeSession{},
		changed:    make(chan struct{}),
		cleanup:    make(chan struct{}),
		unplayable: map[string]Unplayable{},
	}
	t.Cleanup(b.verify)
	return b
}

// close unblocks every call a test held and waits for the fake's own
// goroutines. It reads no session: a call the engine made may still be on
// its way here (verify).
func (b *fakeBackend) close() {
	b.mu.Lock()
	b.closing = true
	b.mu.Unlock()
	select {
	case <-b.cleanup:
	default:
		close(b.cleanup)
	}
	b.wg.Wait()
}

// verify fails the test on any violation of the contract, a session that had
// calls and never a Start among them. It runs once every goroutine the engine
// started has ended, never sooner: the engine makes each call on a goroutine
// of its own, and a Release can reach the backend before its session's Start,
// which still comes (Backend's contract). Read before the engine's goroutines
// were joined, the sessions took such a Start for one never made (B576,
// TestTheContractCheckWaitsForAStartStillOnItsWay).
func (b *fakeBackend) verify() {
	b.close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sessions {
		if !s.started {
			b.violations = append(b.violations, fmt.Sprintf("session %d had calls and never a Start", s.id))
		}
	}
	for _, v := range b.violations {
		b.t.Errorf("backend contract: %s", v)
	}
}

// signal wakes every waiter; b.mu is held.
func (b *fakeBackend) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *fakeBackend) record(c fakeCall) {
	b.calls = append(b.calls, c)
	b.signal()
}

// session is id's record, made by whichever of its calls comes first: the
// engine runs each call on a goroutine of its own, so a Release asked for
// during a slow Start may reach the backend before that Start does, as the
// contract says.
func (b *fakeBackend) session(id SessionID) *fakeSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionLocked(id)
}

func (b *fakeBackend) sessionLocked(id SessionID) *fakeSession {
	s := b.sessions[id]
	if s == nil {
		s = &fakeSession{b: b, id: id, startDone: make(chan struct{})}
		b.sessions[id] = s
	}
	return s
}

func (b *fakeBackend) Start(ctx context.Context, req StartRequest) error {
	b.mu.Lock()
	before := b.beforeStart
	b.mu.Unlock()
	if before != nil {
		before(req)
	}
	b.mu.Lock()
	for _, o := range b.order {
		if !o.acked {
			b.violations = append(b.violations, fmt.Sprintf("session %d opened while session %d's release was unacknowledged", req.Session, o.id))
		}
	}
	s := b.sessionLocked(req.Session)
	if s.started {
		b.violations = append(b.violations, fmt.Sprintf("session %d started twice", s.id))
	}
	s.started, s.first, s.opts, s.ev = true, req.First, req.Options, req.Events
	b.order = append(b.order, s)
	b.record(fakeCall{kind: "start", session: s.id, entry: req.First.Entry, occ: req.First.ID})
	hook := b.onStart
	b.mu.Unlock()
	var err error
	if hook != nil {
		err = hook(ctx, s)
	} else {
		err = s.open()
	}
	close(s.startDone)
	return err
}

// script runs f, which sets the fake's script, under the fake's lock, so a
// test may change it while the engine runs.
func (b *fakeBackend) script(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f()
}

// open is the session's device open, the default Start: an entry scripted
// unplayable is refused before anything opens, and a session asked to
// release while it opened closes at once and writes nothing.
func (s *fakeSession) open() error {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.signal()
	if u, ok := b.unplayable[s.first.Ref.String()]; ok {
		return &u
	}
	s.opened = true
	if s.releaseAsked {
		s.closed = true
		return nil
	}
	s.wrote = true
	return nil
}

func (b *fakeBackend) Prepare(ctx context.Context, id SessionID, after OccurrenceID, next *Occurrence) error {
	b.mu.Lock()
	before := b.beforePrepare
	b.mu.Unlock()
	if before != nil {
		before(id, after, next)
	}
	s := b.session(id)
	b.mu.Lock()
	c := fakeCall{kind: "prepare", session: id, after: after}
	var n *Occurrence
	if next != nil {
		cp := *next
		n = &cp
		c.entry, c.occ = next.Entry, next.ID
	}
	if s != nil {
		s.prepares = append(s.prepares, fakePrepare{after: after, next: n})
	}
	b.record(c)
	var u Unplayable
	refused := false
	if n != nil {
		u, refused = b.unplayable[n.Ref.String()]
	}
	hook := b.onPrepare
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, s, after, n)
	}
	if refused && s != nil {
		s.ev.Refused(id, n.ID, u)
	}
	return nil
}

func (b *fakeBackend) Cut(ctx context.Context, id SessionID, from OccurrenceID) (bool, error) {
	s := b.session(id)
	b.mu.Lock()
	b.record(fakeCall{kind: "cut", session: id, occ: from})
	hook := b.onCut
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, s, from)
	}
	return true, nil
}

func (b *fakeBackend) Pause(ctx context.Context, id SessionID) error {
	s := b.session(id)
	b.mu.Lock()
	b.record(fakeCall{kind: "pause", session: id})
	hook := b.onPause
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, s)
	}
	return nil
}

func (b *fakeBackend) Resume(ctx context.Context, id SessionID) (ResumeResult, error) {
	s := b.session(id)
	b.mu.Lock()
	b.record(fakeCall{kind: "resume", session: id})
	released := s != nil && s.devReleased
	if s != nil {
		s.devReleased = false
	}
	hook := b.onResume
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, s)
	}
	if released {
		return ResumeReopened, nil
	}
	return ResumeContinued, nil
}

func (b *fakeBackend) Release(ctx context.Context, id SessionID) error {
	s := b.session(id)
	b.mu.Lock()
	b.record(fakeCall{kind: "release", session: id})
	if s != nil {
		s.releaseAsked = true
	}
	hook := b.onRelease
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, s)
	}
	return s.release(ctx)
}

// release is the default Release: it waits for Start to return, closes
// what it opened and acknowledges. A context that ends first answers with
// its error, and the acknowledgement follows through Events.Released once
// Start has returned.
func (s *fakeSession) release(ctx context.Context) error {
	select {
	case <-s.startDone:
		s.ack()
		return nil
	default:
	}
	select {
	case <-s.startDone:
		s.ack()
		return nil
	case <-ctx.Done():
	}
	b := s.b
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return ctx.Err()
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		select {
		case <-s.startDone:
		case <-b.cleanup:
			return
		}
		s.ack()
		s.ev.Released(s.id)
	}()
	return ctx.Err()
}

// releasedLater acknowledges s's release through Events.Released on a
// goroutine of the fake's own, as a Release that returned its context's
// error does once it can.
func (s *fakeSession) releasedLater() {
	b := s.b
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		s.ack()
		s.ev.Released(s.id)
	}()
}

// ack closes what the session opened and records the release as
// acknowledged.
func (s *fakeSession) ack() {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.opened {
		s.closed = true
	}
	s.acked = true
	b.signal()
}

func (b *fakeBackend) Discard(ctx context.Context) error {
	b.mu.Lock()
	b.discards++
	b.record(fakeCall{kind: "discard"})
	hook := b.onDiscard
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return nil
}

// waitFor waits until cond, called with b.mu held, holds. Each time it does
// not, the waiting hook runs, when one is set, before the wait.
func (b *fakeBackend) waitFor(what string, cond func() bool) {
	b.t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	for {
		b.mu.Lock()
		ok, ch, waiting := cond(), b.changed, b.waiting
		b.mu.Unlock()
		if ok {
			return
		}
		if waiting != nil {
			waiting()
		}
		select {
		case <-ch:
		case <-timer.C:
			b.t.Fatalf("waited %v for the backend: %s; calls: %v", *watchdog, what, b.callList())
		}
	}
}

// nth waits for the n-th session (from 1) to start and returns it.
func (b *fakeBackend) nth(n int) *fakeSession {
	b.t.Helper()
	b.waitFor(fmt.Sprintf("session %d to start", n), func() bool { return len(b.order) >= n })
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.order[n-1]
}

// starts is the number of sessions started.
func (b *fakeBackend) starts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.order)
}

// count is the number of calls of kind.
func (b *fakeBackend) count(kind string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.countLocked(kind)
}

// countLocked is count with b.mu held, as waitFor's condition holds it.
func (b *fakeBackend) countLocked(kind string) int {
	n := 0
	for _, c := range b.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// callList is every call so far, as strings.
func (b *fakeBackend) callList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		out = append(out, c.String())
	}
	return out
}

// callsSince is the calls made after the first n.
func (b *fakeBackend) callsSince(n int) []string {
	all := b.callList()
	if n > len(all) {
		return nil
	}
	return all[n:]
}

// mark is the number of calls so far, for callsSince.
func (b *fakeBackend) mark() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// prepared waits until s has had n Prepares after the occurrence after and
// returns the last one's successor (nil: none follows).
func (s *fakeSession) prepared(after OccurrenceID, n int) *Occurrence {
	s.b.t.Helper()
	var last *Occurrence
	s.b.waitFor(fmt.Sprintf("prepare %d after occurrence %d in session %d", n, after, s.id), func() bool {
		k := 0
		for _, p := range s.prepares {
			if p.after == after {
				k++
				last = p.next
			}
		}
		return k >= n
	})
	return last
}

// successor waits until s has had a Prepare after the occurrence after that
// names a successor, and returns that successor, the last one's when several
// do (B579). One that names the end can come first, as after an occurrence
// handed off while repeat was off when repeat comes on, and a count of the
// Prepares cannot tell whether the one that names the successor has arrived.
func (s *fakeSession) successor(after OccurrenceID) *Occurrence {
	s.b.t.Helper()
	var next *Occurrence
	s.b.waitFor(fmt.Sprintf("a successor named after occurrence %d in session %d", after, s.id), func() bool {
		next = nil
		for _, p := range s.prepares {
			if p.after == after && p.next != nil {
				next = p.next
			}
		}
		return next != nil
	})
	return next
}

// preparesAfter is the Prepares s had after the occurrence after.
func (s *fakeSession) preparesAfter(after OccurrenceID) []fakePrepare {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	var out []fakePrepare
	for _, p := range s.prepares {
		if p.after == after {
			out = append(out, p)
		}
	}
	return out
}

// isAcked reports whether s's release was acknowledged.
func (s *fakeSession) isAcked() bool {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.acked
}

// state is what s's device went through: opened, closed, written to.
func (s *fakeSession) state() (opened, closed, wrote bool) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.opened, s.closed, s.wrote
}

func (s *fakeSession) submitted(o OccurrenceID) { s.ev.Submitted(s.id, o) }
func (s *fakeSession) audible(o OccurrenceID)   { s.ev.Audible(s.id, o) }
func (s *fakeSession) finished()                { s.ev.Finished(s.id) }
func (s *fakeSession) fault(f Fault)            { s.ev.Fault(s.id, f) }

func (s *fakeSession) preparedEvent(o OccurrenceID, j Join) { s.ev.Prepared(s.id, o, j) }

func (s *fakeSession) position(o OccurrenceID, frames int64) {
	s.ev.Position(s.id, Position{Occurrence: o, Frames: frames, RateHz: 44100})
}

// deviceReleased reports the paused session's own release of the device.
func (s *fakeSession) deviceReleased() {
	s.b.mu.Lock()
	s.devReleased = true
	s.b.mu.Unlock()
	s.ev.DeviceReleased(s.id)
}

// gate is something a scripted call waits on until the test opens it.
type gate chan struct{}

func newGate() gate { return make(gate) }

func (g gate) open() { close(g) }

// wait waits for the gate, the context (when honour is set) or the end of
// the test.
func (g gate) wait(ctx context.Context, b *fakeBackend, honour bool) error {
	var done <-chan struct{}
	if honour {
		done = ctx.Done()
	}
	select {
	case <-g:
		return nil
	case <-done:
		return ctx.Err()
	case <-b.cleanup:
		return context.Canceled
	}
}

// entriesOf is the entries' IDs in order.
func entriesOf(es []Entry) []uint32 {
	out := make([]uint32, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
