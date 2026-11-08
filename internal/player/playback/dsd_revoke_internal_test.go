package playback

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// whyLog is a DSD policy's log that keeps the reason of each line saying a
// session's DSD stops: the reason a reconciliation gives Backend.Revoke.
type whyLog struct {
	mu  sync.Mutex
	why []string
}

func (l *whyLog) Info(string, ...any) {}

func (l *whyLog) Warn(msg string, args ...any) {
	if !strings.Contains(msg, "its DSD stops") {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i+1 < len(args); i += 2 {
		if why, ok := args[i+1].(string); ok && args[i] == "why" {
			l.why = append(l.why, why)
		}
	}
}

func (l *whyLog) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.why...)
}

// TestARevocationsFaultIsTheSessionsOneFault (B728 B9 round 6; one fault,
// once): a reconciliation that revokes the session's DSD while an occurrence's
// own sink activates (claimed, its output adopted: here a converted
// occurrence's PCM sink, which Backend.Revoke finds open) records
// native_revoked on the session for the reason it gives Revoke, and neither it
// nor Revoke replaces a fault the session has: a later revocation for another
// reason leaves the first, and a fault the session had before the
// reconciliation stands through both.
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
			b, store := choiceSessionBackend(t, func(p *DSDPolicy) { p.Log = log })
			store.mu.Lock()
			store.st.DACs[0].Choice.Mode = dsdstate.ModeConvert
			st := store.st.Clone()
			store.mu.Unlock()
			b.RefreshDSD(true)
			st.DACs[0].Choice.Mode = dsdstate.ModeOff
			next := setOf(t, b, st)
			s := b.session(1)
			defer s.cancel()
			s.ev = plainEvents{}
			o := openedOcc(b, 2)
			if !o.converts() {
				t.Fatalf("the occurrence's plan %+v under the set published: want it converted", o.policyPlan)
			}
			o.conv = &dsdconv.Converter{}
			o.claimed, o.out = true, &output{sink: choiceSink{}}
			s.chain = []*occ{o}
			s.fault = tc.before
			s.reconcileDSD(next)
			why := log.reasons()
			want := tc.before
			if want == nil && len(why) == 1 {
				want = &engine.Fault{Reason: "native_revoked", Detail: why[0]}
			}
			if !o.stale || s.fault == nil || want == nil || *s.fault != *want {
				t.Fatalf("stale %v, the fault %+v, the revocations' reasons %q; want the occurrence stale and the fault %+v", o.stale, s.fault, why, want)
			}
			b.Revoke(engine.RevokeInput{Session: 1, Reason: "another revocation"})
			if *s.fault != *want {
				t.Errorf("Backend.Revoke for another reason, the occurrence's sink still open, left the fault %+v; want %+v", s.fault, want)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.Wait(ctx); err != nil {
				t.Errorf("the backend's goroutines: %v", err)
			}
		})
	}
}
