package playback

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// On the harness's path (an Authorization, no effective DSD policy), the
// cause recorded where a session's DSD stopped (backlog B853) is what its
// later DSD entries carry downstream too (round 2): the selectors, which
// answer a revoked permission as no approval; the producer's gapless check,
// which revokes on a permission that fails it; and the authorization's
// reconciliation, which drops what a replaced record no longer admits and
// whose panic is contained and recorded as a policy reconciliation's is.
// With nothing recorded each answers as it did.

// harnessRig is a backend on the harness's path over the approved fixture,
// its record published, with one session (1) holding a permission of that
// record, whose every entry is decode's DSD64 golden.
type harnessRig struct {
	b   *Backend
	s   *session
	cfg Config
}

func newHarnessRig(t *testing.T, construct func(Config) (*Backend, error)) *harnessRig {
	t.Helper()
	cfg, _ := approvedBackendFixture(t)
	cfg.Resolver = readGoldenDSD(t)
	b, err := construct(cfg)
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
	s.mu.Lock()
	s.permission = p
	s.mu.Unlock()
	return &harnessRig{b: b, s: s, cfg: cfg}
}

// entry opens a successor of the session as id and answers its refusal, nil
// for one admitted.
func (r *harnessRig) entry(t *testing.T, id engine.OccurrenceID) *engine.Unplayable {
	t.Helper()
	o, ctx := successorOf(t, r.s, id)
	r.s.openSuccessor(ctx, o)
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if o.refused == nil {
		return nil
	}
	u := *o.refused
	return &u
}

// noApproval is the selectors' answer to a permission revoked or missing.
var noApproval = engine.Unplayable{Reason: ReasonDSD, Detail: "native DSD here needs an owner-approved plan, and this backend holds none"}

// TestOnTheHarnesssPathAnEntryAfterARevocationIsRefusedForIt (backlog B853,
// round 2): an ordinary revocation records its cause and revokes the
// session's permission, which the selectors (selectNative, selectTransition)
// answer as no approval: a DSD entry after it is refused for the revocation's
// word and reason, the cause asked before the selector is (openNativeContent),
// as admitDSD asks it first. With nothing recorded, a session with no
// permission, the selector's answer stands as it was.
func TestOnTheHarnesssPathAnEntryAfterARevocationIsRefusedForIt(t *testing.T) {
	transition := func(cfg Config) (*Backend, error) {
		ref, err := engine.NewReference("successor.dsf")
		if err != nil {
			return nil, err
		}
		return NewTransitionQualificationBackend(cfg, TransitionPlans{Policies: map[string]string{ref.String(): "p64"}})
	}
	for _, tc := range []struct {
		name      string
		construct func(Config) (*Backend, error)
	}{
		{"the record's selection", New},
		{"the transition scenario's selection", transition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHarnessRig(t, tc.construct)
			if u := r.entry(t, 2); u != nil {
				t.Fatalf("an entry before the revocation was refused %+v: the case needs one the selector admits", *u)
			}
			r.b.Revoke(engine.RevokeInput{Session: 1, Reason: revocationReason})
			if u := r.entry(t, 3); u == nil || *u != revocationRefusal {
				t.Errorf("an entry after the revocation was refused %+v; want %+v", u, revocationRefusal)
			}
		})
	}
	t.Run("nothing recorded: a session with no permission", func(t *testing.T) {
		r := newHarnessRig(t, New)
		r.s.mu.Lock()
		r.s.permission = nil
		r.s.mu.Unlock()
		if u := r.entry(t, 2); u == nil || *u != noApproval {
			t.Errorf("an entry of a session with no permission was refused %+v; want %+v", u, noApproval)
		}
	})
}

// TestAGaplessSuccessorsRefusalIsTheCauseRecordedBeforeItsCheckFailed
// (backlog B853, round 2): the producer checks a gapless DSD successor's
// permission with the session's lock let go (checkPermission), and a
// permission that fails it is revoked ("gapless DSD authorization changed").
// An ordinary revocation that comes while the check reads the record drops
// the successor for its cause, which stands: the producer's own revocation
// replaces neither the record nor the successor's refusal, which it rebuilt
// from the check's error until round 2. With nothing recorded before the
// check failed, the successor is refused for the check's own error, as it
// was.
func TestAGaplessSuccessorsRefusalIsTheCauseRecordedBeforeItsCheckFailed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		revokeDuring bool
	}{
		{"a revocation as its permission is checked", true},
		{"nothing recorded before the check failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := approvedBackendFixture(t)
			b := &Backend{cfg: cfg, sessions: map[engine.SessionID]*session{}}
			ctx := context.Background()
			p, err := b.readPermission(ctx)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := p.selectNative(dsd64Entry)
			if err != nil {
				t.Fatal(err)
			}
			tr := nativeTransport(plan)
			s := newSession(b, 1)
			b.sessions[1] = s
			t.Cleanup(s.cancel)
			s.permission = p
			s.cur = &output{transport: tr, policyID: plan.PolicyID}
			done := make(chan struct{})
			close(done)
			o := &occ{id: 2, owner: s, plan: &plan, permission: p, transport: tr, done: done}
			s.chain = []*occ{o}
			b.rememberEdge(tr, &plan, p)
			want := revocationRefusal
			if tc.revokeDuring {
				var once sync.Once
				cfg.Authorization.Healthy = func() bool {
					once.Do(func() { b.Revoke(engine.RevokeInput{Session: 1, Reason: revocationReason}) })
					return true
				}
			} else {
				if err := os.Remove(cfg.Authorization.path()); err != nil {
					t.Fatal(err)
				}
				check := b.checkPermission(ctx, p, nil, edgeKey(tr, &plan))
				if check == nil {
					t.Fatal("the permission passed its check with its record gone: the case needs one that fails")
				}
				want = engine.Unplayable{Reason: "native_revoked", Detail: check.Error()}
			}
			st := s.next()
			s.mu.Lock()
			refused, dropped := o.refused, o.dropped
			s.mu.Unlock()
			if st.kind == stepWrite || !dropped {
				t.Fatalf("the producer's step was %v and the successor dropped %v: a gapless successor whose permission failed was not stopped", st.kind, dropped)
			}
			if refused == nil || *refused != want {
				t.Errorf("the successor was refused %+v; want %+v", refused, want)
			}
		})
	}
}

// TestAnEarlierRevocationsCauseStandsWhenTheAuthorizationIsReplaced
// (backlog B853, round 2): an ordinary revocation drops the session's
// prepared successor for its cause, and the authorization's reconciliation
// that comes after it (the record gone: reconcilePermission, replaced) leaves
// that successor's refusal and the session's record as the revocation made
// them, the earlier cause standing, where it dropped the successor again for
// a word of its own until round 2; a DSD entry after both is refused for the
// revocation's cause. With nothing recorded before it, the reconciliation
// drops a prepared successor for its own word, as it did.
func TestAnEarlierRevocationsCauseStandsWhenTheAuthorizationIsReplaced(t *testing.T) {
	ineligible := engine.Unplayable{Reason: "native_revoked", Detail: "selected DSD policy is no longer eligible"}
	for _, tc := range []struct {
		name          string
		revokedBefore bool
		want          engine.Unplayable
	}{
		{"an ordinary revocation before it", true, revocationRefusal},
		{"nothing recorded before it", false, ineligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHarnessRig(t, New)
			o, ctx := successorOf(t, r.s, 2)
			r.s.openSuccessor(ctx, o)
			r.s.mu.Lock()
			prepared := o.refused == nil && o.plan != nil && !o.dropped
			r.s.mu.Unlock()
			if !prepared {
				t.Fatal("the successor was not prepared under the record: the case needs one the reconciliation would drop")
			}
			if tc.revokedBefore {
				r.b.Revoke(engine.RevokeInput{Session: 1, Reason: revocationReason})
			}
			if err := os.Remove(r.cfg.Authorization.path()); err != nil {
				t.Fatal(err)
			}
			r.b.RefreshAuthorization(context.Background())
			r.s.mu.Lock()
			refused, dropped, stopped, record := o.refused, o.dropped, r.s.nativeRevoked, *r.s.dsdRevokedLocked()
			r.s.mu.Unlock()
			if !dropped || refused == nil || *refused != tc.want {
				t.Errorf("the prepared successor was dropped %v, refused %+v; want %+v", dropped, refused, tc.want)
			}
			if !stopped || record != tc.want {
				t.Errorf("the session's record is %+v (stopped %v); want %+v", record, stopped, tc.want)
			}
			if u := r.entry(t, 3); u == nil || *u != tc.want {
				t.Errorf("an entry after the reconciliation was refused %+v; want %+v", u, tc.want)
			}
		})
	}
}

// TestAPanicInTheAuthorizationsReconciliationIsContainedAndRecorded (backlog
// B853, round 2): with no effective policy (cfg.DSD nil) no policy
// reconciliation runs, and the authorization's (reconcilePermission, which
// RefreshAuthorization and the session's monitor run) contains a panic as a
// policy reconciliation's is: the caller goes on, the session's DSD is
// stopped with the backend's own failure recorded as its cause and as its
// fault (stopDSD), and a DSD entry after it is refused for that cause. Until
// round 2 the panic reached the caller, the monitor's goroutine included, and
// nothing was recorded.
func TestAPanicInTheAuthorizationsReconciliationIsContainedAndRecorded(t *testing.T) {
	r := newHarnessRig(t, New)
	r.b.cfg.Seams = &Seams{ReconcilingPermission: func(engine.SessionID) { panic("the test's reconciliation") }}
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		next, err := r.b.readPermission(context.Background())
		r.s.reconcilePermission(next, err)
	}()
	r.b.cfg.Seams = nil
	if escaped != nil {
		t.Fatalf("the reconciliation's panic reached its caller: %v", escaped)
	}
	want := engine.Unplayable{Reason: ReasonFailed, Detail: "player backend: the DSD authorization's reconciliation panicked; the bridge's log has why"}
	r.s.mu.Lock()
	stopped, record, fault := r.s.nativeRevoked, *r.s.dsdRevokedLocked(), r.s.fault
	r.s.mu.Unlock()
	if !stopped || record != want {
		t.Errorf("the session's record is %+v (stopped %v); want %+v", record, stopped, want)
	}
	if fault == nil || fault.Reason != ReasonFailed || fault.Detail != want.Detail {
		t.Errorf("the session's fault is %+v; want the backend's own failure, %q", fault, want.Detail)
	}
	if u := r.entry(t, 2); u == nil || *u != want {
		t.Errorf("an entry after the contained panic was refused %+v; want %+v", u, want)
	}
}
