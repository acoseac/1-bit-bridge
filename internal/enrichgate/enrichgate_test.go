package enrichgate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hour is an asking interval no test outlasts: a context asking that rarely
// is ended by a send's own Admit or not at all.
const hour = time.Hour

// switchOf is a switch a test turns.
type switchOf struct{ on atomic.Bool }

func newSwitch(on bool) *switchOf {
	s := &switchOf{}
	s.on.Store(on)
	return s
}

func (s *switchOf) get() bool { return s.on.Load() }

// TestTheSwitchIsAskedOnceASecond pins the bound a switch-off cancels a
// request on the wire within: a second, Interval, when the caller gives no
// interval of its own, as the enricher's batch, the fingerprint sweep and the
// harvest's cover refresh do in production.
func TestTheSwitchIsAskedOnceASecond(t *testing.T) {
	if Interval != time.Second {
		t.Errorf("Interval = %v, want a second", Interval)
	}
	for _, every := range []time.Duration{0, -1} {
		if got := interval(every); got != Interval {
			t.Errorf("interval(%v) = %v, want Interval", every, got)
		}
	}
	if got := interval(7 * time.Millisecond); got != 7*time.Millisecond {
		t.Errorf("interval(7ms) = %v, want 7ms", got)
	}
}

// TestCancelWhenOffCancelsOnceItsSwitchIsOff: the context ends, with the
// switch's refusal as its cause, once the switch answers false at a tick of
// its interval, a short one here, so the bound is the interval's; one off at
// the start is ended already; a nil switch is never off, and only the parent
// ends that context; release ends the context and the asking. (The test of
// the same name in internal/enrich, before the gate moved here.)
func TestCancelWhenOffCancelsOnceItsSwitchIsOff(t *testing.T) {
	const every = 2 * time.Millisecond
	t.Run("a switch-off at a tick", func(t *testing.T) {
		s := newSwitch(true)
		ctx, release := cancelWhenOff(context.Background(), s.get, ErrOff, scopeEvery, every)
		defer release()
		if ctx.Err() != nil {
			t.Fatal("ended while the switch is on")
		}
		s.on.Store(false)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("not ended after the switch-off")
		}
		if !errors.Is(context.Cause(ctx), ErrOff) || Refusal(ctx) != ErrOff {
			t.Errorf("cause %v, refusal %v; want ErrOff", context.Cause(ctx), Refusal(ctx))
		}
	})
	t.Run("off at the start", func(t *testing.T) {
		ctx, release := CancelWhenOff(context.Background(), func() bool { return false }, every)
		defer release()
		if ctx.Err() == nil || Refusal(ctx) != ErrOff {
			t.Errorf("a context whose switch is off at the start: err %v, refusal %v", ctx.Err(), Refusal(ctx))
		}
	})
	t.Run("a nil switch", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, release := CancelWhenOff(parent, nil, every)
		defer release()
		if ctx.Err() != nil || Admit(ctx) != nil {
			t.Fatal("a nil switch ended the context or refused a send")
		}
		cancel()
		<-ctx.Done()
		if Refusal(ctx) != nil {
			t.Error("a parent's cancel read as a switch's refusal")
		}
	})
	t.Run("release", func(t *testing.T) {
		ctx, release := CancelWhenOff(context.Background(), newSwitch(true).get, every)
		release()
		if ctx.Err() == nil {
			t.Error("release left the context live")
		}
		if Refusal(ctx) != nil {
			t.Error("a released context read as a switch's refusal")
		}
		release() // twice is no panic
	})
}

// TestASendAfterTheFlipIsRefusedWithNoTick: Admit asks the switch itself, so
// a send after the flip is refused at once, with no tick of the interval,
// and the refusal ends the switch's context, through any context derived
// from it, with the refusal as its cause; a context the switch ended answers
// its refusal from then on, even with the switch on again.
func TestASendAfterTheFlipIsRefusedWithNoTick(t *testing.T) {
	s := newSwitch(true)
	ctx, release := CancelWhenOff(context.Background(), s.get, hour)
	defer release()
	child, cancel := context.WithTimeout(ctx, hour)
	defer cancel()
	if err := Admit(child); err != nil {
		t.Fatalf("Admit with the switch on = %v", err)
	}
	s.on.Store(false)
	if err := Admit(child); err != ErrOff {
		t.Fatalf("Admit after the flip = %v, want ErrOff", err)
	}
	if ctx.Err() == nil || Refusal(ctx) != ErrOff || Refusal(child) != ErrOff {
		t.Errorf("the refusal left the switch's context %v (refusal %v), its child's refusal %v",
			ctx.Err(), Refusal(ctx), Refusal(child))
	}
	s.on.Store(true)
	if err := Admit(child); err != ErrOff {
		t.Errorf("Admit on a context the switch ended, the switch on again = %v, want ErrOff", err)
	}
}

// TestDeezersSwitchGovernsDeezersSendsAlone: a Deezer context derived from
// enrich.enabled's answers ErrDeezerOff to a Deezer send when Deezer alone is
// off, and ends itself alone, leaving the outer context, and a send of any
// other client on it, live; with enrich.enabled off a Deezer send answers
// ErrOff and ends the outer context too.
func TestDeezersSwitchGovernsDeezersSendsAlone(t *testing.T) {
	t.Run("Deezer off", func(t *testing.T) {
		enrichment, deezer := newSwitch(true), newSwitch(true)
		outer, release := CancelWhenOff(context.Background(), enrichment.get, hour)
		defer release()
		dctx, releaseDeezer := CancelWhenDeezerOff(outer, deezer.get, hour)
		defer releaseDeezer()
		deezer.on.Store(false)
		if err := Admit(dctx); err != nil {
			t.Errorf("a send of another client on Deezer's context = %v, want admitted", err)
		}
		if err := AdmitDeezer(dctx); err != ErrDeezerOff {
			t.Fatalf("AdmitDeezer = %v, want ErrDeezerOff", err)
		}
		if dctx.Err() == nil || Refusal(dctx) != ErrDeezerOff {
			t.Errorf("Deezer's context: err %v, refusal %v", dctx.Err(), Refusal(dctx))
		}
		if outer.Err() != nil || Admit(outer) != nil {
			t.Error("a Deezer flip ended enrich.enabled's context")
		}
	})
	t.Run("enrichment off", func(t *testing.T) {
		enrichment, deezer := newSwitch(true), newSwitch(true)
		outer, release := CancelWhenOff(context.Background(), enrichment.get, hour)
		defer release()
		dctx, releaseDeezer := CancelWhenDeezerOff(outer, deezer.get, hour)
		defer releaseDeezer()
		enrichment.on.Store(false)
		deezer.on.Store(false)
		if err := AdmitDeezer(dctx); err != ErrOff {
			t.Fatalf("AdmitDeezer with both off = %v, want ErrOff", err)
		}
		if Refusal(outer) != ErrOff || Refusal(dctx) != ErrOff {
			t.Errorf("refusals: outer %v, Deezer's %v; want ErrOff", Refusal(outer), Refusal(dctx))
		}
	})
}

// countingServer answers every request after hold, counting them.
type countingServer struct {
	srv   *httptest.Server
	asked atomic.Int64
	mu    sync.Mutex
	hold  func(r *http.Request)
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	c := &countingServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.asked.Add(1)
		c.mu.Lock()
		hold := c.hold
		c.mu.Unlock()
		if hold != nil {
			hold(r)
			if r.Context().Err() != nil {
				panic(http.ErrAbortHandler)
			}
		}
		_, _ = io.WriteString(w, "answer")
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *countingServer) setHold(f func(r *http.Request)) {
	c.mu.Lock()
	c.hold = f
	c.mu.Unlock()
}

// get sends a GET on ctx through client and reads the answer.
func get(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// TestTheTransportAdmitsAtTheSendAndAtTheAnswer drives Transport over a real
// server: a request whose context carries no switch passes; one sent after
// the flip never reaches the server; an answer that arrives after the flip is
// not taken; a request a tick of the switch cut off on the wire fails with
// the switch's refusal, not a bare cancellation; and Client leaves the
// caller's client as it was.
func TestTheTransportAdmitsAtTheSendAndAtTheAnswer(t *testing.T) {
	srv := newCountingServer(t)
	plain := &http.Client{Timeout: 10 * time.Second}
	client := Client(plain)
	if plain.Transport != nil {
		t.Fatal("Client changed the caller's client")
	}

	t.Run("no switch", func(t *testing.T) {
		got, err := get(context.Background(), client, srv.srv.URL)
		if err != nil || got != "answer" {
			t.Fatalf("a request with no switch: %q, %v", got, err)
		}
	})
	t.Run("sent after the flip", func(t *testing.T) {
		s := newSwitch(true)
		ctx, release := CancelWhenOff(context.Background(), s.get, hour)
		defer release()
		s.on.Store(false)
		from := srv.asked.Load()
		_, err := get(ctx, client, srv.srv.URL)
		if !errors.Is(err, ErrOff) {
			t.Errorf("err = %v, want ErrOff", err)
		}
		if n := srv.asked.Load() - from; n != 0 {
			t.Errorf("the server was asked %d time(s) after the flip", n)
		}
	})
	t.Run("answered after the flip", func(t *testing.T) {
		s := newSwitch(true)
		ctx, release := CancelWhenOff(context.Background(), s.get, hour)
		defer release()
		srv.setHold(func(*http.Request) { s.on.Store(false) })
		defer srv.setHold(nil)
		got, err := get(ctx, client, srv.srv.URL)
		if !errors.Is(err, ErrOff) || got != "" {
			t.Errorf("an answer after the flip: %q, %v; want no answer and ErrOff", got, err)
		}
		if Refusal(ctx) != ErrOff {
			t.Error("the answer's refusal left the switch's context live")
		}
	})
	t.Run("cut off on the wire", func(t *testing.T) {
		s := newSwitch(true)
		ctx, release := CancelWhenOff(context.Background(), s.get, 2*time.Millisecond)
		defer release()
		holding := make(chan struct{})
		srv.setHold(func(r *http.Request) {
			close(holding)
			<-r.Context().Done()
		})
		defer srv.setHold(nil)
		go func() {
			<-holding
			s.on.Store(false)
		}()
		_, err := get(ctx, client, srv.srv.URL)
		if !errors.Is(err, ErrOff) {
			t.Errorf("a request cut off on the wire: %v, want ErrOff", err)
		}
	})
	t.Run("a Deezer switch is no plain client's", func(t *testing.T) {
		// Off after the context is made: one off at the start is ended
		// already, and nothing is sent on an ended context.
		deezer := newSwitch(true)
		dctx, release := CancelWhenDeezerOff(context.Background(), deezer.get, hour)
		defer release()
		deezer.on.Store(false)
		if _, err := get(dctx, client, srv.srv.URL); err != nil {
			t.Errorf("a plain client refused a send on Deezer's context: %v", err)
		}
		if _, err := get(dctx, DeezerClient(plain), srv.srv.URL); !errors.Is(err, ErrDeezerOff) {
			t.Errorf("a Deezer client's send with Deezer off: %v, want ErrDeezerOff", err)
		}
	})
}
