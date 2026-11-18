package playback

import (
	"errors"
	"reflect"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

var errDSDUnread = errors.New("player backend: the effective DSD policy did not read; the bridge's log has why")

const (
	dsdStepJudge  = "its judgement"
	dsdStepReplan = "the engine's hand-off"
	dsdStepRevoke = "the revocation"
)

// refreshDSD reads and reconciles under one lock. The first failed reading
// stops every session holding DSD and refuses admission; no failed operation
// is retried. A subsequent independent reading may establish a fresh policy.
// While a choice's write holds its reading (takeChoiceReading) it reads and
// publishes nothing: it is owed (refreshOwed), asked once the write has
// published (publishChoice), and answers the set published now. Only the
// supervisor's and the probe's refreshes meet one: admission, a monitor and
// an activation are a session's, and while the write holds the configuration
// lease no Start registers, so none is admitted or acquires the device.
func (b *Backend) refreshDSD(where string) (d *dsdSet, err error) {
	b.dsdMu.Lock()
	defer b.dsdMu.Unlock()
	if b.choiceReading {
		b.refreshOwed = true
		return b.dsd.Load(), nil
	}
	defer func() {
		if r := recover(); r != nil {
			b.containDSD("the effective DSD policy's reading ("+where+")", r)
			d, err = b.dsd.Load(), errDSDUnread
		}
	}()
	return b.refreshDSDLocked(), nil
}

// containDSD contains a panic in a reading of the policy or a publication of
// it, under the refreshes' lock: logged without its value (logPanic), the next
// reading made fresh (dsdRead), every session holding DSD stopped, and the set
// published judged as it stands.
func (b *Backend) containDSD(where string, r any) {
	b.dsdRead = time.Time{}
	b.logPanic(where, r)
	b.mu.Lock()
	ss := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		ss = append(ss, s)
	}
	b.mu.Unlock()
	for _, s := range ss {
		if s.holdsDSD() {
			s.stopDSD("the effective DSD policy's reading", true)
		}
	}
	b.dsdJudged = b.dsd.Load()
}

func (b *Backend) refreshDSDLocked() *dsdSet {
	read := b.dsdNow()
	next, view := b.readDSD()
	return b.publishLocked(read, next, view, nil)
}

// publishLocked publishes next and its read side view, read at read, the one
// step a refresh and a choice's write publish by (publishChoice): a set that
// plays what the published one does keeps its pointer and its generation, and
// a read side that says another thing of the DAC is published beside it alone;
// another set is published at the next generation, said once (logDSDSet), and
// reconciled. Each pair stored takes the next publication (dsdPublished.seq);
// a reading that changes nothing stores nothing, and moves nothing. stored,
// when set, runs right after the pair is stored, or with the pair standing
// where nothing changed, before the log, the Published seam and the
// reconciliation. b.dsdMu is held.
func (b *Backend) publishLocked(read time.Time, next *dsdSet, view DSDView, stored func(*dsdPublished)) *dsdSet {
	b.dsdRead = read
	cur := b.dsd.Load()
	pub := b.dsdPub.Load()
	if cur.same(next) {
		if !reflect.DeepEqual(pub.view, view) {
			pub = &dsdPublished{set: cur, view: view, seq: pub.seq + 1}
			b.dsdPub.Store(pub)
		}
		if stored != nil {
			stored(pub)
		}
		if b.dsdJudged != cur {
			b.reconcileDSD(cur)
		}
		return cur
	}
	next.gen = cur.gen + 1
	b.dsd.Store(next)
	pub = &dsdPublished{set: next, view: view, seq: pub.seq + 1}
	b.dsdPub.Store(pub)
	if stored != nil {
		stored(pub)
	}
	b.logDSDSet(next)
	b.published()
	b.reconcileDSD(next)
	return next
}

// holdsDSD includes retiring outputs, without consulting a potentially
// failed position callback. A closed output needs no further containment.
func (s *session) holdsDSD() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.chain {
		if o.admitting || o.admitted != nil && (!o.dropped || o.out != nil && !o.out.closed) {
			return true
		}
	}
	for _, out := range s.outs {
		if out.native != nil && !out.closed {
			return true
		}
	}
	return false
}

// stopDSD records one fault and stops the actual outputs, including retiring
// tails. It does not re-enter Revoke or read a sink: either may be the callback
// that failed. The ordinary teardown still joins the writers and their close.
// It records why the session's DSD stopped (revokeDSDLocked, backlog B853):
// unread, the policy's reading failed, dsd_dac_unread; otherwise a
// reconciliation's contained panic, which judged no occurrence against the
// set that did read, and is the backend's own failure, as the fault recorded
// here is: backend_failed, with that fault's words (checkDSDActivation
// answers an activation from the same fault).
func (s *session) stopDSD(where string, unread bool) {
	err := s.faulted(where)
	why := engine.Unplayable{Reason: ReasonFailed, Detail: err.Error()}
	if unread {
		why = engine.Unplayable{Reason: ReasonDACUnread, Detail: errDSDUnread.Error()}
	}
	s.mu.Lock()
	s.revokeDSDLocked(why)
	var outs []*output
	for _, out := range s.outs {
		if out.native == nil {
			if out.containDrop {
				continue
			}
			out.containDrop = true
		}
		outs = append(outs, out)
	}
	for _, o := range s.chain {
		if o.admitting || o.admitted != nil {
			o.stale = true
		}
	}
	close(s.judged)
	s.judged = make(chan struct{})
	s.mu.Unlock()
	for _, out := range outs {
		if out.native != nil {
			out.native.BeginRelease(alsapcm.Immediate)
		} else {
			s.b.spawn(func() {
				// Deferred first, so it runs after all else here.
				defer s.b.containCalled()
				_ = s.call(s.ctx, func() error {
					// call may return on cancellation before Drop does.
					defer func() {
						s.mu.Lock()
						out.containDrop = false
						s.mu.Unlock()
					}()
					return out.sink.Drop()
				})
			})
		}
	}
	wake(s.wakeP)
	wake(s.wakeR)
}

// position calls a sink's observer at most until its first panic. The last
// measured prefix remains available to teardown, but it cannot become new
// playback evidence. Reconciliation's recoverer stops the real sink; a
// later reporter or cleanup never invokes the failed observer again.
func (o *output) position() (p alsapcm.Position) {
	o.posMu.Lock()
	defer o.posMu.Unlock()
	if o.positionFailed {
		return o.lastPosition
	}
	defer func() {
		if r := recover(); r != nil {
			o.positionFailed = true
			o.lastPosition.State = alsapcm.SinkEnded
			o.lastPosition.Err = errors.New("player backend: the sink's position callback panicked")
			panic(r)
		}
	}()
	p = o.sink.Position()
	o.lastPosition = p
	return p
}
