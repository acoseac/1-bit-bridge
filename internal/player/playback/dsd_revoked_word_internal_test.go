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
// occurrence through a hand-off that panics (failedHandoff).
type revocationRig struct {
	b            *Backend
	s            *session
	gone, broken *atomic.Bool
	handoff      *failedHandoff
}

func newRevocationRig(t *testing.T) *revocationRig {
	t.Helper()
	r := &revocationRig{gone: new(atomic.Bool), broken: new(atomic.Bool), handoff: &failedHandoff{}}
	r.b, _ = fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: r.gone, broken: r.broken} })
	r.s = r.b.session(1)
	t.Cleanup(r.s.cancel)
	r.s.ev = r.handoff
	r.s.chain = []*occ{openedOcc(r.b, 2)}
	return r
}

// unread is the policy's reading failing: a refresh whose reading panics,
// which stops the DSD of every session that holds it (refreshDSD).
func (r *revocationRig) unread(t *testing.T) {
	t.Helper()
	r.broken.Store(true)
	defer r.broken.Store(false)
	if _, err := r.b.refreshDSD("the test's reading"); !errors.Is(err, errDSDUnread) {
		t.Fatalf("the reading answered %v; want it contained", err)
	}
}

// reconcilePanic is a reconciliation that panics: the box's altsets taken
// away or given back, so the refresh publishes a set that re-plans the
// session's occurrence, through a hand-off that panics (Backend.reconcileDSD).
func (r *revocationRig) reconcilePanic(t *testing.T) {
	t.Helper()
	calls := r.handoff.calls.Load()
	r.gone.Store(!r.gone.Load())
	r.b.RefreshDSD(true)
	if got := r.handoff.calls.Load(); got != calls+1 {
		t.Fatalf("the refresh asked the hand-off %d times; want the one that panics", got-calls)
	}
}

// revoke is an ordinary revocation (Backend.Revoke).
func (r *revocationRig) revoke(*testing.T) {
	r.b.Revoke(engine.RevokeInput{Session: r.s.id, Reason: revocationReason})
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

// admitted is what admitDSD answers a DSD64 entry of the session.
func (r *revocationRig) admitted(t *testing.T) engine.Unplayable {
	t.Helper()
	_, err := r.b.admitDSD(&occ{owner: r.s, id: 9}, dsd64Entry)
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
	r.b.cfg.Seams = &Seams{Admitting: func(engine.OccurrenceID) { r.revoke(t) }}
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
	r.b.cfg.Seams = &Seams{Admitting: func(engine.OccurrenceID) { r.reconcilePanic(t) }}
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
	r.b.cfg.Seams = &Seams{Opened: func(engine.OccurrenceID, engine.Reference) { r.revoke(t) }}
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
	r.b.cfg.Seams = &Seams{Opened: func(engine.OccurrenceID, engine.Reference) {
		r.s.mu.Lock()
		admittedBefore = o.admitted != nil && o.admitted == r.b.dsd.Load() && o.plan != nil && o.refused == nil
		r.s.mu.Unlock()
		r.reconcilePanic(t)
	}}
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
	t.Cleanup(s.cancel)
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
