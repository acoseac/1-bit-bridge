package playback

import (
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A panic in the effective DSD policy's work is contained and bounded (B728
// B9 round 19). A reconciliation's is caught in each session's
// (session.reconcileDSD), so every other session is judged, and the set is
// tried once more by the next refresh; the retry's failure stops the sessions
// it failed in (reconcileOrStop). A reading's (the binding, the box, the
// controls, the state) or a publish's reaches only a choice's write, which
// asks refreshDSD itself: every other caller asks tryRefreshDSD, which counts
// a run of them and stops what holds DSD at its bound (refreshFailedLocked).
// Neither failure waits on anything: the next refresh is at most a session
// monitor's tick away while a session holds DSD.

// dsdAttempts is the bound of both, as a count of failures in a row: a
// set's reconciliation that fails is tried once more by the next refresh, and
// that retry's failure is terminal; and the second refresh in a row whose
// reading panics stops every session that holds DSD. One failure is tried
// again, never acted on: a panic that does not come back changes nothing.
const dsdAttempts = 2

// dsdFailure is a session's reconciliation that panicked
// (Backend.reconcileDSD), and the step it panicked in: its judgement, the
// engine's hand-off or Revoke (dsdStep*).
type dsdFailure struct {
	s    *session
	step string
}

// The steps of a session's reconciliation (session.reconcileDSD).
const (
	dsdStepJudge  = "its judgement"
	dsdStepReplan = "the engine's hand-off"
	dsdStepRevoke = "the revocation"
)

// reconcileOrStop reconciles every session against d (reconcileDSD) and
// records d judged (dsdJudged) once each session's reconciliation has
// completed. One that failed leaves d unjudged, for the next refresh, which
// finds d the same set, to try once more; that retry's failure is terminal
// (dsdAttempts): each session it failed in is stopped (stopDSD), Revoke never
// asked again of one whose Revoke is what panicked, and d is recorded judged,
// so no refresh tries it again. Nothing here hands an occurrence off. A
// failure's panic was logged where it was recovered. dsdMu is held.
func (b *Backend) reconcileOrStop(d *dsdSet) {
	fails := b.reconcileDSD(d)
	if len(fails) == 0 {
		b.dsdJudged = d
		return
	}
	if b.dsdTried != d {
		b.dsdTried, b.dsdTries = d, 0
	}
	if b.dsdTries++; b.dsdTries < dsdAttempts {
		return
	}
	for _, f := range fails {
		b.dsdLog().Warn("player: the effective DSD set's reconciliation panicked again when tried once more: the session stops", "session", uint64(f.s.id), "generation", d.gen, "step", f.step)
		f.s.stopDSD("the effective DSD set's reconciliation", f.step != dsdStepRevoke)
	}
	b.dsdJudged = d
}

// errDSDUnread answers an admission and an activation whose reading of the
// effective DSD policy panicked once such readings in a row have reached
// their bound (refreshFailedLocked): DSD is refused until a reading
// completes.
var errDSDUnread = errors.New("player backend: the effective DSD policy did not read; the bridge's log has why")

// tryRefreshDSD is refreshDSD for every caller but a choice's write: a
// session's admission and activation, its monitor, the supervisor's refresh
// and the silent probe's (B728 B9 round 19). A panic in the refresh outside
// its reconciliation, which contains its own (reconcileOrStop), reaches none
// of them: it is recovered under dsdMu and counted (refreshFailedLocked), and
// the set published answered. err is nil but where such failures in a row
// have reached their bound: errDSDUnread, by which an admission refuses its
// occurrence's DSD and an activation refuses. where names the caller for the
// log.
func (b *Backend) tryRefreshDSD(where string) (d *dsdSet, err error) {
	b.dsdMu.Lock()
	defer b.dsdMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			d, err = b.dsd.Load(), b.refreshFailedLocked(where, r)
		}
	}()
	return b.refreshDSDLocked(), nil
}

// refreshFailedLocked counts a refresh that panicked outside its
// reconciliation (tryRefreshDSD) in the run of such refreshes that the next
// one to complete ends (refreshDSDLocked), and logs the run's first, with its
// stack: once a run, never at every tick. The run's second (dsdAttempts) and
// each after it stop every session that holds DSD (holdsDSD, stopDSD), its
// fault recorded and its DSD revoked: a reading that does not complete judges
// nothing, so a set published whose reconciliation did not complete is never
// tried again while the run lasts, and what plays is no longer read against
// the policy. From the bound on it answers errDSDUnread. dsdMu is held: the
// stops take each session's lock and Revoke's under it, as a reconciliation
// does.
func (b *Backend) refreshFailedLocked(where string, r any) error {
	b.dsdFails++
	if b.dsdFails == 1 {
		b.logPanic("the effective DSD policy's reading ("+where+")", r)
	}
	if b.dsdFails < dsdAttempts {
		return nil
	}
	if b.dsdFails == dsdAttempts {
		b.dsdLog().Warn("player: the effective DSD policy's reading panicked twice in a row: every session holding DSD stops, and DSD is refused until a reading completes", "where", where)
	}
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
	return errDSDUnread
}

// holdsDSD reports whether s holds a DSD occurrence a reconciliation would
// judge: opened, admitted under the policy, not dropped and not played out
// (judgeDSDLocked's own test, against no set). A panic in it (a sink's
// position read) is logged and answers true: a session that cannot be read
// is stopped.
func (s *session) holdsDSD() (holds bool) {
	l := s.hold()
	defer l.release()
	defer func() {
		if r := recover(); r != nil {
			s.b.logPanic("the effective DSD policy's look at what a session holds", r)
			holds = true
		}
	}()
	l.lock()
	for _, o := range s.chain {
		if o.opened() && o.admitted != nil && !o.dropped && !o.playedOutLocked() {
			l.unlock()
			return true
		}
	}
	l.unlock()
	return false
}

// stopDSD stops s, which the effective DSD policy could not judge: its
// reconciliation failed again when tried once more (reconcileOrStop), or the
// policy's reading panicked twice in a row while s holds DSD
// (refreshFailedLocked). Its fault is recorded, backend_failed, as for any
// panic of the backend's, unless it has one (faulted): the producer writes no
// more, the reporter reports the fault, and the engine releases the session.
// Its DSD is revoked at once (Backend.Revoke, contained) where revoke says so;
// where Revoke is what panicked, the fault alone stops it. Nothing here hands
// an occurrence off.
func (s *session) stopDSD(where string, revoke bool) {
	err := s.faulted(where)
	if !revoke {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.b.logPanic("the revocation of a session the effective DSD policy stops", r)
		}
	}()
	s.b.Revoke(engine.RevokeInput{Session: s.id, Reason: err.Error()})
}
