package acoustid

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
)

// The client's own switch (backlog B617, round 3): a client is built with
// enrich.enabled and asks it whatever context a lookup carries, so a caller
// whose context carries no switch sends nothing while it is off, and a
// lookup on the wire when it turns off is cut off with its refusal within
// its interval, never taken for a no-match, which a sweep persists.

// switchAnswer is a lookup AcoustID answers with a match.
const switchAnswer = `{"status":"ok","results":[{"id":"9ff43b6a-4f16-427c-93c2-92307ca505e0","score":0.98}]}`

// TestTheClientRefusesABareContextWhileItsSwitchIsOff: off, nothing is sent
// on a context that carries no switch, and the lookup answers ErrOff, never
// ErrNoMatch; on, the same lookup is answered.
func TestTheClientRefusesABareContextWhileItsSwitchIsOff(t *testing.T) {
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = io.WriteString(w, switchAnswer)
	}))
	t.Cleanup(srv.Close)
	off := NewClient(srv.URL, testKey, "ua", nil, enrichgate.Enrichment(func() bool { return false }))
	_, err := off.Lookup(context.Background(), testFP())
	if !errors.Is(err, enrichgate.ErrOff) || errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrOff", err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("AcoustID was asked %d time(s) with the switch off", n)
	}
	if got := off.Admit(context.Background()); got != enrichgate.ErrOff {
		t.Errorf("Admit with the switch off = %v, want ErrOff", got)
	}
	var none *Client
	if got := none.Admit(context.Background()); got != enrichgate.ErrOff {
		t.Errorf("a nil client's Admit = %v, want ErrOff", got)
	}
	on := NewClient(srv.URL, testKey, "ua", nil, onSwitches)
	if res, err := on.Lookup(context.Background(), testFP()); err != nil || len(res) != 1 {
		t.Errorf("switched on: %d result(s), err %v; want AcoustID's answer", len(res), err)
	}
	if got := on.Admit(context.Background()); got != nil {
		t.Errorf("Admit with the switch on = %v, want nil", got)
	}
}

// TestTheClientCutsOffABareContextsLookupWhenItsSwitchTurnsOff: a lookup on
// a context that carries no switch, held by AcoustID, is cut off once the
// switch turns off, within the interval it is asked at in production
// (enrichgate.Interval, a second; the bound allows a second more for a
// loaded host), with ErrOff: no answer, so no no-match.
func TestTheClientCutsOffABareContextsLookupWhenItsSwitchTurnsOff(t *testing.T) {
	var on atomic.Bool
	on.Store(true)
	holding := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case holding <- struct{}{}:
		default:
		}
		// Held until the client gives up, then nothing is answered
		// (build-ci-tests.md, B526).
		<-r.Context().Done()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, testKey, "ua", nil, enrichgate.Enrichment(on.Load))

	flipped := make(chan time.Time, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-holding:
		case <-done:
			return
		}
		on.Store(false)
		flipped <- time.Now()
	}()
	_, err := c.Lookup(context.Background(), testFP())
	var after time.Duration
	select {
	case at := <-flipped:
		after = time.Since(at)
	default:
		t.Fatalf("the lookup returned (%v) before AcoustID held it", err)
	}
	if !errors.Is(err, enrichgate.ErrOff) || errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrOff", err)
	}
	if IsTransient(err) {
		t.Error("the cut reads as a transient failure, which a sweep retries as one")
	}
	if after > 2*enrichgate.Interval {
		t.Errorf("cut off %v after the switch-off, past its interval (%v)", after, enrichgate.Interval)
	}
}
