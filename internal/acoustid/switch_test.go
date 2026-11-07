package acoustid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
)

// TestALookupTheSwitchRefusesIsNotAMiss: the client asks enrich.enabled,
// carried on the lookup's context, at the send and at the answer (backlog
// B617). A lookup that starts after the switch-off is never sent, and a clean
// no-match that arrives after it is not taken: both are enrichgate.ErrOff,
// never ErrNoMatch, which the sweeper would cache and persist for 30 days.
// The context asks the switch once an hour here, so only the gate refuses.
func TestALookupTheSwitchRefusesIsNotAMiss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flipSent bool // the switch goes off before the send, else before the answer
		wantSent int64
	}{
		{name: "sent after the switch-off", flipSent: true, wantSent: 0},
		{name: "answered after the switch-off", wantSent: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var on atomic.Bool
			on.Store(true)
			var sent atomic.Int64
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				on.Store(false)
				fmt.Fprint(w, `{"status":"ok","results":[]}`)
			})
			ctx, release := enrichgate.CancelWhenOff(context.Background(), on.Load, time.Hour)
			defer release()
			if tc.flipSent {
				on.Store(false)
			}
			_, err := c.Lookup(ctx, testFP())
			if !errors.Is(err, enrichgate.ErrOff) {
				t.Errorf("Lookup = %v, want enrichgate.ErrOff", err)
			}
			if errors.Is(err, ErrNoMatch) {
				t.Errorf("Lookup = %v, a no-match the switch-off should have kept from being taken", err)
			}
			if got := sent.Load(); got != tc.wantSent {
				t.Errorf("AcoustID saw %d request(s), want %d", got, tc.wantSent)
			}
		})
	}
}
