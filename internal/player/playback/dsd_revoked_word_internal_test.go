package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// A DSD entry of a session whose DSD was stopped is refused with the cause
// recorded where it was stopped (backlog B853): the policy's reading that
// failed is dsd_dac_unread, and stands once it comes whatever came first, its
// detail with it; a reconciliation's contained panic is the backend's own
// failure, backend_failed, the fault it records; an ordinary revocation is
// native_revoked, for its reason; and on the harness's path an authorization
// replaced is native_revoked, for reconcilePermission's own reason. Before
// B853 every one of them refused the entry as dsd_dac_unread at its
// admission, and as native_revoked, "DSD permission revoked", at the end of a
// successor's open.

// revocationReason is what the tests' ordinary revocation is given.
const revocationReason = "the test's revocation"

// revocationRig is a backend over the fixture DAC, native at every rate, with
// one session holding a DSD64 occurrence admitted under the set published, and
// the switches the causes are made with: the box's next reading panics
// (broken), or it loses its altsets (gone), so a refresh re-plans the
// occurrence through a hand-off that panics (failedHandoff). Its seams are
// the backend's (Config.Seams): a case adds its own to them, never puts
// others in their place, since the rig's cleanup counts by one of them. The
// session's DSD monitor, which an admitted successor's open starts, is parked:
// its tick never comes, so it never takes the session's lock, and nothing but
// the code a case calls holds that lock (round 4).
type revocationRig struct {
	b            *Backend
	s            *session
	gone, broken *atomic.Bool
	handoff      *failedHandoff
	// ended is the session's DSD monitor's end (Seams.MonitorEnded), which a
	// case starts by an admitted successor's open: one at most a session
	// (startDSDMonitor).
	ended chan struct{}
}

func newRevocationRig(t *testing.T) *revocationRig {
	t.Helper()
	r := &revocationRig{gone: new(atomic.Bool), broken: new(atomic.Bool), handoff: &failedHandoff{}, ended: make(chan struct{}, 1)}
	r.b, _ = fixtureSessionBackend(t, func(p *DSDPolicy) {
		p.Box = fixtureBox{gone: r.gone, broken: r.broken}
		p.Tick = func() (<-chan time.Time, func()) { return nil, func() {} }
	})
	r.b.cfg.Seams = &Seams{MonitorEnded: func(engine.SessionID) { r.ended <- struct{}{} }}
	r.s = r.b.session(1)
	t.Cleanup(func() { r.stop(t) })
	r.s.ev = r.handoff
	r.s.chain = []*occ{openedOcc(r.b, 2)}
	return r
}

// stop is the rig's cleanup: the session ended as a release ends it
// (closeSession: its context, then its end, which ends its DSD monitor,
// closed once under its lock taken within a bound), a monitor a case started
// joined by its end (Seams.MonitorEnded), and then every goroutine of the
// backend's (joinBackend), each within handMadeBound. Until round 3 the rig
// cancelled the context alone, and each monitor a case started refreshed the
// policy every 100 ms for the life of the process; until round 4 it took the
// lock unconditionally, so a lock a containment leaked hung the cleanup.
func (r *revocationRig) stop(t *testing.T) {
	monitor, ok := closeSession(t, r.s)
	if !ok {
		return
	}
	if monitor {
		select {
		case <-r.ended:
		case <-time.After(handMadeBound):
			t.Errorf("the session's DSD monitor did not end within %v of the session's end", handMadeBound)
		}
	}
	joinBackend(t, r.b)
}

// heldAfter is "" where the session's lock is free now, and says it held
// otherwise: the first acquisition after a step that could leak it, at once,
// since nothing else holds it (the rig's session runs nothing, its monitor
// parked). A cause's helper asks it once its containment has returned, so a
// lock the containment kept fails the case there, before a call that would
// wait for it (round 4).
func (r *revocationRig) heldAfter(step string) string {
	if !r.s.mu.TryLock() {
		return "the session's lock is held once " + step + " returned"
	}
	r.s.mu.Unlock()
	return ""
}

// TestTheRevocationRigsCleanupClosesTheSessionsEndOnce (backlog B853, round
// 3): the rig's cleanup (stop) closes the session's end only where nothing
// has: the release of a session that never started closes it itself
// (session.release), and the cleanup leaves it as it is, where a second close
// would panic.
func TestTheRevocationRigsCleanupClosesTheSessionsEndOnce(t *testing.T) {
	r := newRevocationRig(t)
	if err := r.b.Release(context.Background(), r.s.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.s.down:
	default:
		t.Fatal("the release of a session that never started left its end open: the case needs one the cleanup finds closed")
	}
}

// unread is the policy's reading failing: a refresh whose reading panics,
// which stops the DSD of every session that holds it (refreshDSD). It fails
// the case where the reading was not contained, or where the session's lock
// is held once it returned (failReading); it is the case's goroutine's alone.
func (r *revocationRig) unread(t *testing.T) {
	t.Helper()
	if why := r.failReading(); why != "" {
		t.Fatal(why)
	}
}

// failReading is unread, saying what went wrong, "" for nothing: a seam that
// runs on another goroutine than the case's says it with t.Error.
func (r *revocationRig) failReading() string {
	r.broken.Store(true)
	defer r.broken.Store(false)
	if _, err := r.b.refreshDSD("the test's reading"); !errors.Is(err, errDSDUnread) {
		return fmt.Sprintf("the reading answered %v; want it contained", err)
	}
	return r.heldAfter("the contained reading")
}

// reconcilePanic is a reconciliation that panics: the box's altsets taken
// away or given back, so the refresh publishes a set that re-plans the
// session's occurrence, through a hand-off that panics (Backend.reconcileDSD).
// It fails the case where the hand-off was not asked once, or where the
// session's lock is held once the refresh returned (panicReconciliation); it
// is the case's goroutine's alone.
func (r *revocationRig) reconcilePanic(t *testing.T) {
	t.Helper()
	if why := r.panicReconciliation(); why != "" {
		t.Fatal(why)
	}
}

// panicReconciliation is reconcilePanic, saying what went wrong, "" for
// nothing: a seam that runs on another goroutine than the case's says it with
// t.Error.
func (r *revocationRig) panicReconciliation() string {
	calls := r.handoff.calls.Load()
	r.gone.Store(!r.gone.Load())
	r.b.RefreshDSD(true)
	if got := r.handoff.calls.Load(); got != calls+1 {
		return fmt.Sprintf("the refresh asked the hand-off %d times; want the one that panics", got-calls)
	}
	return r.heldAfter("the contained reconciliation")
}

// revoke is an ordinary revocation (Backend.Revoke). It fails the case where
// the session's lock is held once the revocation returned (revoked); it is the
// case's goroutine's alone.
func (r *revocationRig) revoke(t *testing.T) {
	t.Helper()
	if why := r.revoked(); why != "" {
		t.Fatal(why)
	}
}

// revoked is revoke, saying what went wrong, "" for nothing: a seam that runs
// on another goroutine than the case's says it with t.Error (round 5: the
// revocation's lock was not asked after it, and a seam on the admission's
// goroutine called the failing form).
func (r *revocationRig) revoked() string {
	r.b.Revoke(engine.RevokeInput{Session: r.s.id, Reason: revocationReason})
	return r.heldAfter("the revocation")
}

// dsd64Entry is the DSD the tests' entries carry.
var dsd64Entry = format.NativeDSD{BitRateHz: dsdstate.DSD64, Channels: 2}

// refusesDSD64 fails the case unless the set the backend publishes now
// refuses DSD64: the premise of a case about a refusal of the policy's.
func refusesDSD64(t *testing.T, b *Backend) {
	t.Helper()
	if _, _, err := b.dsd.Load().selectFor(dsd64Entry); err == nil {
		t.Fatal("the set the refresh published still plays DSD64: the case needs one that refuses it")
	}
}

// admitted is what admitDSD answers a DSD64 entry of the session, asked on a
// goroutine of its own and awaited within handMadeBound, so an admission that
// a lock the case's causes leaked blocks fails the case, where it waited to
// the package's timeout (round 4). Its seams run on that goroutine: a case's
// seam says a failure with t.Error, never t.Fatal. Once it answered, the
// session's lock is asserted free at once (heldAfter), so a lock the admission
// kept fails the case there, where it went unseen until the rig's cleanup
// (round 5).
func (r *revocationRig) admitted(t *testing.T) engine.Unplayable {
	t.Helper()
	answer := make(chan error, 1)
	go func() {
		_, err := r.b.admitDSD(&occ{owner: r.s, id: 9}, dsd64Entry)
		answer <- err
	}()
	var err error
	select {
	case err = <-answer:
	case <-time.After(handMadeBound):
		t.Fatalf("the admission did not answer within %v: the session's lock is held", handMadeBound)
	}
	if why := r.heldAfter("the admission"); why != "" {
		t.Fatal(why)
	}
	var u *engine.Unplayable
	if !errors.As(err, &u) {
		t.Fatalf("a DSD64 entry of a session whose DSD was stopped was admitted: %v", err)
	}
	return *u
}

// The words and details each cause refuses with.
var (
	unreadRefusal     = engine.Unplayable{Reason: ReasonDACUnread, Detail: errDSDUnread.Error()}
	panicRefusal      = engine.Unplayable{Reason: ReasonFailed, Detail: fmt.Sprintf("player backend: %s panicked; the bridge's log has why", "the effective DSD set's reconciliation ("+dsdStepReplan+")")}
	revocationRefusal = engine.Unplayable{Reason: "native_revoked", Detail: revocationReason}
)

// TestAnEntryAfterTheSessionsDSDStoppedIsRefusedForTheCause (backlog B853):
// admitDSD refuses a DSD entry of the session with the cause recorded, after
// each cause alone and after two in either order: the reading's failure
// stands whatever came first, and otherwise the first cause does.
func TestAnEntryAfterTheSessionsDSDStoppedIsRefusedForTheCause(t *testing.T) {
	type cause = func(*revocationRig, *testing.T)
	unread, panics, revoke := (*revocationRig).unread, (*revocationRig).reconcilePanic, (*revocationRig).revoke
	for _, tc := range []struct {
		name   string
		causes []cause
		want   engine.Unplayable
	}{
		{"the policy's reading failed", []cause{unread}, unreadRefusal},
		{"a reconciliation panicked", []cause{panics}, panicRefusal},
		{"an ordinary revocation", []cause{revoke}, revocationRefusal},
		{"a revocation, then the reading failed", []cause{revoke, unread}, unreadRefusal},
		{"the reading failed, then a revocation", []cause{unread, revoke}, unreadRefusal},
		{"a revocation, then a reconciliation panicked", []cause{revoke, panics}, revocationRefusal},
		{"a reconciliation panicked, then a revocation", []cause{panics, revoke}, panicRefusal},
		{"the reading failed, then a reconciliation panicked", []cause{unread, panics}, unreadRefusal},
		{"a reconciliation panicked, then the reading failed", []cause{panics, unread}, unreadRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRevocationRig(t)
			for _, c := range tc.causes {
				c(r, t)
			}
			if got := r.admitted(t); got != tc.want {
				t.Errorf("a DSD64 entry was refused %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestARevocationDuringAnAdmissionIsItsRefusal (backlog B853): a revocation
// that comes while an entry's admission reads the policy is the second check
// of admitDSD's to find, after the set's plan, and it refuses the entry for
// the revocation's own word and reason.
func TestARevocationDuringAnAdmissionIsItsRefusal(t *testing.T) {
	r := newRevocationRig(t)
	r.b.cfg.Seams.Admitting = func(engine.OccurrenceID) {
		if why := r.revoked(); why != "" {
			t.Error(why)
		}
	}
	if got := r.admitted(t); got != revocationRefusal {
		t.Errorf("an entry whose admission a revocation came beside was refused %+v; want %+v", got, revocationRefusal)
	}
}

// TestAReconciliationThatPanicsDuringAnAdmissionIsItsRefusal (backlog B853,
// round 2): a refresh that comes while an entry's admission is under way,
// before it reads the set (Seams.Admitting), publishes a set in which the DAC
// offers no DSD and stops the session's DSD in a reconciliation that panics:
// the entry is refused for the cause recorded, backend_failed, never for what
// the new set says of its rate, which admitDSD answered first until round 2.
func TestAReconciliationThatPanicsDuringAnAdmissionIsItsRefusal(t *testing.T) {
	r := newRevocationRig(t)
	r.b.cfg.Seams.Admitting = func(engine.OccurrenceID) {
		if why := r.panicReconciliation(); why != "" {
			t.Error(why)
		}
	}
	got := r.admitted(t)
	refusesDSD64(t, r.b)
	if got != panicRefusal {
		t.Errorf("an entry whose admission a reconciliation that panicked came beside was refused %+v; want %+v", got, panicRefusal)
	}
}

// goldenDSD serves every reference as decode's DSD64 golden.
type goldenDSD []byte

func (g goldenDSD) Open(context.Context, engine.Reference) (Content, error) {
	return Content{Data: bytes.NewReader(g), Size: int64(len(g))}, nil
}

func readGoldenDSD(t *testing.T) goldenDSD {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", "lsb_dsd64_partial_block.dsf"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// successorOf is a successor occurrence of s's, of a DSD reference, whose
// open the test runs (openSuccessor).
func successorOf(t *testing.T, s *session, id engine.OccurrenceID) (*occ, context.Context) {
	t.Helper()
	ref, err := engine.NewReference("successor.dsf")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	o := s.newOccLocked(engine.Occurrence{ID: id, Entry: uint32(id), Ref: ref})
	ctx, cancel := context.WithCancel(s.ctx)
	o.cancel = cancel
	s.mu.Unlock()
	return o, ctx
}

// TestASuccessorWhoseOpenEndsAfterARevocationIsRefusedForIt (backlog B853):
// a successor admitted before the session's DSD was revoked, the revocation
// coming before its open ends, is refused at that end for the revocation's
// own word and reason (openSuccessor), as admitDSD refuses one admitted
// after it.
func TestASuccessorWhoseOpenEndsAfterARevocationIsRefusedForIt(t *testing.T) {
	r := newRevocationRig(t)
	r.b.cfg.Resolver = readGoldenDSD(t)
	// The seam runs on the case's goroutine (openSuccessor is called here),
	// before the open's end would wait for a lock the revocation kept: the
	// failing form is safe and stops the case there.
	r.b.cfg.Seams.Opened = func(engine.OccurrenceID, engine.Reference) { r.revoke(t) }
	o, ctx := successorOf(t, r.s, 3)
	r.s.openSuccessor(ctx, o)
	r.s.mu.Lock()
	refused, plan := o.refused, o.plan
	r.s.mu.Unlock()
	if plan == nil {
		t.Fatal("the successor was not admitted before the revocation: the case needs a plan its open's end refuses")
	}
	if refused == nil || *refused != revocationRefusal {
		t.Errorf("the successor was refused %+v; want %+v", refused, revocationRefusal)
	}
}

// TestASuccessorWhoseOpenEndsAfterAPanickedReconciliationIsRefusedForIt
// (backlog B853, round 2): a successor admitted under the set published,
// whose open has read its first chunk (Seams.Opened), meets a refresh that
// publishes a set in which the DAC offers no DSD and stops the session's DSD
// in a reconciliation that panics: at its open's end it is refused for the
// cause recorded, backend_failed, before its readmission under the new set
// could refuse it for that set's own word, as it did until round 2.
func TestASuccessorWhoseOpenEndsAfterAPanickedReconciliationIsRefusedForIt(t *testing.T) {
	r := newRevocationRig(t)
	r.b.cfg.Resolver = readGoldenDSD(t)
	o, ctx := successorOf(t, r.s, 3)
	admittedBefore := false
	r.b.cfg.Seams.Opened = func(engine.OccurrenceID, engine.Reference) {
		r.s.mu.Lock()
		admittedBefore = o.admitted != nil && o.admitted == r.b.dsd.Load() && o.plan != nil && o.refused == nil
		r.s.mu.Unlock()
		r.reconcilePanic(t)
	}
	r.s.openSuccessor(ctx, o)
	if !admittedBefore {
		t.Fatal("the successor did not hold its admission under the set published as its open read its first chunk: the case needs one a readmission would refuse")
	}
	refusesDSD64(t, r.b)
	r.s.mu.Lock()
	refused := o.refused
	r.s.mu.Unlock()
	if refused == nil || *refused != panicRefusal {
		t.Errorf("the successor was refused %+v; want %+v", refused, panicRefusal)
	}
}

// TestARevocationDropsAPreparedEntryForTheCauseRecordedFirst (backlog B853,
// round 2): a revocation drops the session's prepared DSD successors for the
// cause recorded, its own where nothing was recorded before it, and an
// earlier one where that stands: the policy's reading that failed, or a
// reconciliation that panicked, as the entries after it are refused. Until
// round 2 it dropped them for its own reason whatever stood. The prepared
// successor's open starts the session's DSD monitor, which the rig's cleanup
// ends and joins (round 3: each case left one refreshing the policy every
// 100 ms for the life of the process).
func TestARevocationDropsAPreparedEntryForTheCauseRecordedFirst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(*revocationRig, *testing.T)
		want   engine.Unplayable
	}{
		{"nothing recorded before it", nil, revocationRefusal},
		{"the policy's reading failed before it", (*revocationRig).unread, unreadRefusal},
		{"a reconciliation panicked before it", (*revocationRig).reconcilePanic, panicRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRevocationRig(t)
			r.b.cfg.Resolver = readGoldenDSD(t)
			o, ctx := successorOf(t, r.s, 3)
			r.s.openSuccessor(ctx, o)
			// The monitor the open started is parked (the rig's), so nothing
			// holds the session's lock here but a leak: each read below asserts
			// it free at once (round 4), and the cause before the revocation,
			// a containment, asks it as it returns (heldAfter).
			if !r.s.mu.TryLock() {
				t.Fatal("the session's lock is held once the successor's open returned")
			}
			prepared := o.refused == nil && o.plan != nil && !o.dropped && o.out == nil
			monitor := r.s.dsdMonitor
			r.s.mu.Unlock()
			if !prepared {
				t.Fatal("the successor was not prepared: the case needs one a revocation drops")
			}
			if !monitor {
				t.Fatal("the prepared successor's open started no DSD monitor: the case needs the one the rig's cleanup ends")
			}
			if tc.before != nil {
				tc.before(r, t)
			}
			r.revoke(t)
			if !r.s.mu.TryLock() {
				t.Fatal("the session's lock is held once the revocation returned")
			}
			refused, dropped := o.refused, o.dropped
			r.s.mu.Unlock()
			if !dropped || refused == nil || *refused != tc.want {
				t.Errorf("the prepared successor was dropped %v, refused %+v; want %+v", dropped, refused, tc.want)
			}
		})
	}
}

// admissionEvents records the native plans a session reports
// (engine.NativePlanEvents), a first occurrence's its Start's admission.
type admissionEvents struct {
	engine.Events
	plans atomic.Int32
}

func (e *admissionEvents) NativePlan(engine.SessionID, engine.OccurrenceID, engine.NativePlan) {
	e.plans.Add(1)
}

// TestARevocationAsTheFirstOccurrenceOpensIsItsStartsRefusal (backlog B853,
// round 2): a session's first occurrence, admitted as its open began, meets an
// ordinary revocation once its first chunk is read (Seams.Opened), the set
// unchanged or a set published meanwhile that refuses its rate: its Start is
// refused for the revocation's word and reason at the open's end, reports no
// admission (no native plan) and starts no monitor. Until round 2 the
// unchanged set's readmission returned at once, and the Start reported its
// plan and failed at the native open, "DSD permission revoked", a device's
// failure; the replaced set's refused it for the set's own word.
func TestARevocationAsTheFirstOccurrenceOpensIsItsStartsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replaced bool
	}{
		{"the set unchanged", false},
		{"a set published as it opened that refuses its rate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone := new(atomic.Bool)
			b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: gone} })
			b.cfg.Resolver = readGoldenDSD(t)
			const id engine.SessionID = 7
			b.cfg.Seams = &Seams{Opened: func(engine.OccurrenceID, engine.Reference) {
				b.Revoke(engine.RevokeInput{Session: id, Reason: revocationReason})
				if tc.replaced {
					gone.Store(true)
					b.RefreshDSD(true)
				}
			}}
			ref, err := engine.NewReference("first.dsf")
			if err != nil {
				t.Fatal(err)
			}
			ev := &admissionEvents{}
			err = b.Start(context.Background(), engine.StartRequest{Session: id, First: engine.Occurrence{ID: 1, Entry: 1, Ref: ref}, Events: ev})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := b.Release(ctx, id); err != nil {
					t.Errorf("the session's release: %v", err)
				}
				if err := b.Wait(ctx); err != nil {
					t.Errorf("the backend's goroutines: %v", err)
				}
			})
			if tc.replaced {
				refusesDSD64(t, b)
			}
			var u *engine.Unplayable
			if !errors.As(err, &u) || *u != revocationRefusal {
				t.Errorf("the Start answered %v; want the refusal %+v", err, revocationRefusal)
			}
			if n := ev.plans.Load(); n != 0 {
				t.Errorf("the Start reported %d native plans: an admission the revocation had ended", n)
			}
			s := b.known(id)
			if s == nil {
				t.Fatal("the session's record is gone before its Release")
			}
			s.mu.Lock()
			monitor := s.dsdMonitor
			s.mu.Unlock()
			if monitor {
				t.Error("the session started a DSD monitor for a first occurrence the revocation refused")
			}
		})
	}
}

// TestAnAuthorizationReplacedAsASuccessorOpensRefusesItForItsOwnReason
// (backlog B853, the harness's path): the approval record replaced while a
// successor admitted under it opens, no native output active, so
// reconcilePermission revokes the session's DSD with no call to Revoke: the
// successor is refused at its open's end for reconcilePermission's own word
// and reason, which it recorded.
func TestAnAuthorizationReplacedAsASuccessorOpensRefusesItForItsOwnReason(t *testing.T) {
	cfg, _ := approvedBackendFixture(t)
	cfg.Resolver = readGoldenDSD(t)
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b.RefreshAuthorization(ctx)
	s := b.session(1)
	t.Cleanup(func() { endSession(t, b, s) })
	s.ev = plainEvents{}
	p, err := b.readPermission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.permission = p
	replaced := false
	b.cfg.Seams = &Seams{Opened: func(engine.OccurrenceID, engine.Reference) {
		if err := os.Remove(cfg.Authorization.path()); err != nil {
			t.Error(err)
		}
		b.RefreshAuthorization(ctx)
		s.mu.Lock()
		replaced = s.nativeRevoked
		s.mu.Unlock()
	}}
	o, octx := successorOf(t, s, 3)
	s.openSuccessor(octx, o)
	if !replaced {
		t.Fatal("the authorization's replacement did not revoke the session's DSD before the open ended")
	}
	s.mu.Lock()
	refused, plan, outs := o.refused, o.plan, len(s.outs)
	s.mu.Unlock()
	if plan == nil || outs != 0 {
		t.Fatalf("the successor's plan %+v, %d outputs: the case needs one admitted, with no native output active", plan, outs)
	}
	want := engine.Unplayable{Reason: "native_revoked", Detail: "selected DSD policy is no longer eligible"}
	if refused == nil || *refused != want {
		t.Errorf("the successor was refused %+v; want %+v", refused, want)
	}
}
