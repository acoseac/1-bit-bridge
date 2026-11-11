package playback

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

type whyLog struct {
	mu   sync.Mutex
	why  []string
	held chan struct{}
	gate chan struct{}
}

func (l *whyLog) arm() (held <-chan struct{}, letGo func()) {
	h, g := make(chan struct{}), make(chan struct{})
	l.mu.Lock()
	l.held, l.gate = h, g
	l.mu.Unlock()
	var once sync.Once
	return h, func() { once.Do(func() { close(g) }) }
}

func (l *whyLog) Info(string, ...any) {}

func (l *whyLog) Warn(msg string, args ...any) {
	if !strings.Contains(msg, "its DSD stops") {
		return
	}
	l.mu.Lock()
	for i := 0; i+1 < len(args); i += 2 {
		if why, ok := args[i+1].(string); ok && args[i] == "why" {
			l.why = append(l.why, why)
		}
	}
	held, gate := l.held, l.gate
	l.held, l.gate = nil, nil
	l.mu.Unlock()
	if held != nil {
		close(held)
		<-gate
	}
}

func (l *whyLog) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.why...)
}

func TestARevocationsFaultIsTheSessionsOneFault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before *engine.Fault
	}{
		{"none before the reconciliation", nil},
		{"an xrun before the reconciliation", &engine.Fault{Reason: engine.ReasonXrun, Detail: "the DAC ran dry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &whyLog{}
			var gone atomic.Bool
			b, store := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Log = log; p.Box = fixtureBox{gone: &gone} })
			store.mu.Lock()
			store.st.DefaultMode = dsdstate.ModeConvert
			store.mu.Unlock()
			b.RefreshDSD(true)
			s := b.session(1)
			defer s.cancel()
			s.ev = plainEvents{}
			o := openedOcc(b, 2)
			if !o.converts() {
				t.Fatalf("the occurrence's plan %+v under the set published: want it converted", o.policyPlan)
			}
			o.conv = &dsdconv.Converter{}
			o.claimed, o.out = true, &output{sink: fixtureSink{}}
			s.chain = []*occ{o}
			s.fault = tc.before
			held, letGo := log.arm()
			defer letGo()
			done := make(chan struct{})
			go func() {
				defer close(done)
				gone.Store(true)
				b.RefreshDSD(true)
			}()
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("the reconciliation said nothing of the session's DSD stopping within 10 s")
			}
			why := log.reasons()
			want := tc.before
			if want == nil && len(why) == 1 {
				want = &engine.Fault{Reason: "native_revoked", Detail: why[0]}
			}
			fault := func() (engine.Fault, bool, bool) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.fault == nil {
					return engine.Fault{}, false, o.stale
				}
				return *s.fault, true, o.stale
			}
			if f, ok, stale := fault(); !stale || !ok || want == nil || f != *want {
				t.Errorf("held before Backend.Revoke: stale %v, the fault %+v (%v), the revocations' reasons %q; want the occurrence stale and the fault %+v, the reconciliation's own record", stale, f, ok, why, want)
			}
			letGo()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the reconciliation did not return within 10 s")
			}
			if f, ok, _ := fault(); !ok || want == nil || f != *want {
				t.Errorf("once Backend.Revoke ran, the fault %+v (%v); want %+v", f, ok, want)
			}
			b.Revoke(engine.RevokeInput{Session: 1, Reason: "another revocation"})
			if f, ok, _ := fault(); !ok || want == nil || f != *want {
				t.Errorf("Backend.Revoke for another reason, the occurrence's sink still open, left the fault %+v (%v); want %+v", f, ok, want)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.Wait(ctx); err != nil {
				t.Errorf("the backend's goroutines: %v", err)
			}
		})
	}
}
