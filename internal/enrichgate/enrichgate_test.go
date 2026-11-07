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

// hour is an asking interval no test outlasts: a context or a watch asking
// that rarely is ended by a send's own Answer or not at all.
const hour = time.Hour

// switchOf is a switch a test turns, counting how often it is asked.
type switchOf struct {
	on    atomic.Bool
	asked atomic.Int64
}

func newSwitch(on bool) *switchOf {
	s := &switchOf{}
	s.on.Store(on)
	return s
}

func (s *switchOf) get() bool {
	s.asked.Add(1)
	return s.on.Load()
}

// alwaysOn and off are switches nobody turns.
func alwaysOn() bool { return true }
func off() bool      { return false }

// TestTheSwitchIsAskedOnceASecond pins the bound a switch-off cancels a
// request on the wire within: a second, Interval, when the caller gives no
// interval of its own, as the enricher's batch, the fingerprint sweep, the
// harvest's cover refresh and every client's watch do in production.
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
	if got := Enrichment(alwaysOn).every; got != 0 {
		t.Errorf("a client's switches ask every %v unless a test says otherwise, want 0 (Interval)", got)
	}
}

// TestCancelWhenOffCancelsOnceItsSwitchIsOff: the context ends, with the
// switch's refusal as its cause, once the switch answers false at a tick of
// its interval, a short one here, so the bound is the interval's; one off at
// the start is ended already, and so is one whose switch is nil, which is
// off as everywhere in this package; release ends the context and the
// asking. (The test of the same name in internal/enrich, before the gate
// moved here.)
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
		ctx, release := CancelWhenOff(context.Background(), off, every)
		defer release()
		if ctx.Err() == nil || Refusal(ctx) != ErrOff {
			t.Errorf("a context whose switch is off at the start: err %v, refusal %v", ctx.Err(), Refusal(ctx))
		}
	})
	t.Run("a nil switch is off", func(t *testing.T) {
		ctx, release := CancelWhenOff(context.Background(), nil, every)
		defer release()
		if ctx.Err() == nil || Refusal(ctx) != ErrOff {
			t.Errorf("a nil switch: err %v, refusal %v; want the context ended with ErrOff", ctx.Err(), Refusal(ctx))
		}
		dctx, releaseDeezer := CancelWhenDeezerOff(context.Background(), nil, every)
		defer releaseDeezer()
		if dctx.Err() == nil || Refusal(dctx) != ErrDeezerOff {
			t.Errorf("a nil Deezer switch: err %v, refusal %v; want the context ended with ErrDeezerOff", dctx.Err(), Refusal(dctx))
		}
	})
	t.Run("a parent's cancel is no refusal", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, release := CancelWhenOff(parent, alwaysOn, every)
		defer release()
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

// TestASendAfterTheFlipIsRefusedWithNoTick: Admit asks the switches a
// context carries itself, so a write after the flip is refused at once, with
// no tick of the interval, and the refusal ends the switch's context,
// through any context derived from it, with the refusal as its cause; a
// context the switch ended answers its refusal from then on, even with the
// switch on again.
func TestASendAfterTheFlipIsRefusedWithNoTick(t *testing.T) {
	s := newSwitch(true)
	ctx, release := CancelWhenOff(context.Background(), s.get, hour)
	defer release()
	child, cancel := context.WithTimeout(ctx, hour)
	defer cancel()
	if err := Admit(child, alwaysOn); err != nil {
		t.Fatalf("Admit with the switch on = %v", err)
	}
	s.on.Store(false)
	if err := Admit(child, alwaysOn); err != ErrOff {
		t.Fatalf("Admit after the flip = %v, want ErrOff", err)
	}
	if ctx.Err() == nil || Refusal(ctx) != ErrOff || Refusal(child) != ErrOff {
		t.Errorf("the refusal left the switch's context %v (refusal %v), its child's refusal %v",
			ctx.Err(), Refusal(ctx), Refusal(child))
	}
	s.on.Store(true)
	if err := Admit(child, alwaysOn); err != ErrOff {
		t.Errorf("Admit on a context the switch ended, the switch on again = %v, want ErrOff", err)
	}
}

// TestAdmitAsksTheWritersOwnSwitch: a context that carries no switch
// answers to the writer's own alone, which admits while it is on and
// refuses while it is off or nil; and the writer's switch refusing ends the
// switch contexts its context carries, so the pass on it stops there.
func TestAdmitAsksTheWritersOwnSwitch(t *testing.T) {
	bare := context.Background()
	if err := Admit(bare, alwaysOn); err != nil {
		t.Errorf("Admit(a bare context, on) = %v, want nil", err)
	}
	for name, own := range map[string]func() bool{"off": off, "nil": nil} {
		if err := Admit(bare, own); err != ErrOff {
			t.Errorf("Admit(a bare context, %s) = %v, want ErrOff", name, err)
		}
	}
	pass, release := CancelWhenOff(bare, alwaysOn, hour)
	defer release()
	if err := Admit(pass, off); err != ErrOff {
		t.Fatalf("Admit(a pass's context, off) = %v, want ErrOff", err)
	}
	if Refusal(pass) != ErrOff {
		t.Errorf("the writer's refusal left the pass's context live (refusal %v)", Refusal(pass))
	}
}

// TestDeezersSwitchGovernsDeezersSendsAlone: a Deezer context derived from
// enrich.enabled's answers ErrDeezerOff to a Deezer client when Deezer alone
// is off, and ends itself alone, leaving the outer context, and a send of any
// other client on it, live; with enrich.enabled off a Deezer client answers
// ErrOff, never ErrDeezerOff, and ends the outer context too. A Deezer
// client's own switches answer the same way on a context that carries none,
// and switches that are not Deezer's have no Deezer switch, which is off.
func TestDeezersSwitchGovernsDeezersSendsAlone(t *testing.T) {
	deezerClient := Deezer(alwaysOn, alwaysOn)
	t.Run("Deezer off", func(t *testing.T) {
		enrichment, deezer := newSwitch(true), newSwitch(true)
		outer, release := CancelWhenOff(context.Background(), enrichment.get, hour)
		defer release()
		dctx, releaseDeezer := CancelWhenDeezerOff(outer, deezer.get, hour)
		defer releaseDeezer()
		deezer.on.Store(false)
		if err := Enrichment(alwaysOn).Answer(dctx); err != nil {
			t.Errorf("a send of another client on Deezer's context = %v, want admitted", err)
		}
		if err := deezerClient.Answer(dctx); err != ErrDeezerOff {
			t.Fatalf("a Deezer client's send = %v, want ErrDeezerOff", err)
		}
		if dctx.Err() == nil || Refusal(dctx) != ErrDeezerOff {
			t.Errorf("Deezer's context: err %v, refusal %v", dctx.Err(), Refusal(dctx))
		}
		if outer.Err() != nil || Enrichment(alwaysOn).Answer(outer) != nil {
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
		if err := deezerClient.Answer(dctx); err != ErrOff {
			t.Fatalf("a Deezer client's send with both off = %v, want ErrOff", err)
		}
		if Refusal(outer) != ErrOff || Refusal(dctx) != ErrOff {
			t.Errorf("refusals: outer %v, Deezer's %v; want ErrOff", Refusal(outer), Refusal(dctx))
		}
	})
	t.Run("a Deezer client's own switches", func(t *testing.T) {
		bare := context.Background()
		for _, c := range []struct {
			name string
			sw   Switches
			want error
		}{
			{"both on", Deezer(alwaysOn, alwaysOn), nil},
			{"Deezer off", Deezer(alwaysOn, off), ErrDeezerOff},
			{"enrichment off", Deezer(off, alwaysOn), ErrOff},
			{"both off", Deezer(off, off), ErrOff},
			{"no Deezer switch", Deezer(alwaysOn, nil), ErrDeezerOff},
			{"another client's switches", Enrichment(alwaysOn).ForDeezer(), ErrDeezerOff},
			{"Deezer's as a Deezer client takes them", Deezer(alwaysOn, alwaysOn).ForDeezer(), nil},
		} {
			if err := c.sw.Answer(bare); err != c.want {
				t.Errorf("%s: Answer = %v, want %v", c.name, err, c.want)
			}
		}
		pass, release := CancelWhenOff(bare, alwaysOn, hour)
		defer release()
		dctx, releaseDeezer := CancelWhenDeezerOff(pass, alwaysOn, hour)
		defer releaseDeezer()
		if err := Deezer(alwaysOn, off).Answer(dctx); err != ErrDeezerOff {
			t.Fatalf("a Deezer client whose Deezer switch is off = %v, want ErrDeezerOff", err)
		}
		if Refusal(dctx) != ErrDeezerOff || pass.Err() != nil {
			t.Errorf("its refusal: Deezer's context %v, the pass's %v; want Deezer's ended alone",
				Refusal(dctx), pass.Err())
		}
	})
}

// TestZeroSwitchesAreOff: a client built with no switches, the zero value or
// a nil predicate, sends nothing.
func TestZeroSwitchesAreOff(t *testing.T) {
	for name, sw := range map[string]Switches{
		"the zero value":         {},
		"a nil predicate":        Enrichment(nil),
		"a nil Deezer predicate": Deezer(nil, nil),
	} {
		if err := sw.Answer(context.Background()); err != ErrOff {
			t.Errorf("%s: Answer = %v, want ErrOff", name, err)
		}
	}
}

// countingServer answers every request after hold, counting them.
type countingServer struct {
	srv   *httptest.Server
	asked atomic.Int64
	mu    sync.Mutex
	hold  func(w http.ResponseWriter, r *http.Request)
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
			hold(w, r)
			if r.Context().Err() != nil {
				panic(http.ErrAbortHandler)
			}
		}
		_, _ = io.WriteString(w, "answer")
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *countingServer) setHold(f func(w http.ResponseWriter, r *http.Request)) {
	c.mu.Lock()
	c.hold = f
	c.mu.Unlock()
}

// fresh is a client on a transport of its own, so no request here shares a
// connection with another test's (build-ci-tests.md, B456).
func fresh(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// untimed is fresh with no Timeout. http.Client ends a request's context
// itself once its body is closed, or it fails, only when it has a deadline
// to stop, so a test of what ends a client's watch takes a client with none.
func untimed(t *testing.T) *http.Client {
	t.Helper()
	c := fresh(t)
	c.Timeout = 0
	return c
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

// TestTheClientsOwnSwitchesGovernABareContext: a request whose context
// carries no switch answers to the client's own, which no context can lend
// it: sent while they are on; refused at the send, with nothing asked of the
// server, while they are off, nil or zero; its answer not taken when they
// turn off before it arrives; and cut off on the wire, waiting for its
// answer or reading its body, within the client's interval when they turn
// off meanwhile, with their refusal rather than a bare cancellation.
func TestTheClientsOwnSwitchesGovernABareContext(t *testing.T) {
	srv := newCountingServer(t)
	bare := context.Background()

	t.Run("on", func(t *testing.T) {
		got, err := get(bare, Enrichment(alwaysOn).Client(fresh(t)), srv.srv.URL)
		if err != nil || got != "answer" {
			t.Fatalf("a request with the client's switch on: %q, %v", got, err)
		}
	})
	t.Run("refused at the send", func(t *testing.T) {
		for _, c := range []struct {
			name string
			sw   Switches
			want error
		}{
			{"off", Enrichment(off), ErrOff},
			{"nil", Enrichment(nil), ErrOff},
			{"zero", Switches{}, ErrOff},
			{"Deezer's, Deezer off", Deezer(alwaysOn, off), ErrDeezerOff},
			{"Deezer's, enrichment off", Deezer(off, alwaysOn), ErrOff},
			{"Deezer's, both off", Deezer(off, off), ErrOff},
		} {
			from := srv.asked.Load()
			_, err := get(bare, c.sw.Client(fresh(t)), srv.srv.URL)
			if !errors.Is(err, c.want) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			}
			if c.want == ErrOff && errors.Is(err, ErrDeezerOff) {
				t.Errorf("%s: enrichment off answered Deezer's refusal", c.name)
			}
			if n := srv.asked.Load() - from; n != 0 {
				t.Errorf("%s: the server was asked %d time(s)", c.name, n)
			}
		}
	})
	t.Run("answered after the flip", func(t *testing.T) {
		s := newSwitch(true)
		client := Enrichment(s.get).Every(hour).Client(fresh(t))
		srv.setHold(func(http.ResponseWriter, *http.Request) { s.on.Store(false) })
		defer srv.setHold(nil)
		got, err := get(bare, client, srv.srv.URL)
		if !errors.Is(err, ErrOff) || got != "" {
			t.Errorf("an answer after the flip: %q, %v; want no answer and ErrOff", got, err)
		}
	})
	t.Run("cut off waiting for its answer", func(t *testing.T) {
		s := newSwitch(true)
		const every = 5 * time.Millisecond
		client := Enrichment(s.get).Every(every).Client(fresh(t))
		holding := make(chan struct{})
		srv.setHold(func(_ http.ResponseWriter, r *http.Request) {
			close(holding)
			<-r.Context().Done()
		})
		defer srv.setHold(nil)
		flipped := make(chan time.Time, 1)
		go func() {
			<-holding
			s.on.Store(false)
			flipped <- time.Now()
		}()
		_, err := get(bare, client, srv.srv.URL)
		after := time.Since(<-flipped)
		if !errors.Is(err, ErrOff) {
			t.Errorf("a request cut off on the wire: %v, want ErrOff", err)
		}
		// The interval is 5 ms; a second is the production bound itself.
		if after > Interval {
			t.Errorf("cut off %v after the flip, past the interval's bound", after)
		}
	})
	t.Run("cut off reading its body", func(t *testing.T) {
		s := newSwitch(true)
		client := Enrichment(s.get).Every(5 * time.Millisecond).Client(fresh(t))
		srv.setHold(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "part")
			w.(http.Flusher).Flush()
			s.on.Store(false)
			<-r.Context().Done()
		})
		defer srv.setHold(nil)
		req, err := http.NewRequestWithContext(bare, http.MethodGet, srv.srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			// The switch can be off before the answer is admitted: that is
			// the refusal at the answer, and the same no-answer.
			if !errors.Is(err, ErrOff) {
				t.Fatalf("Do = %v, want an answer or ErrOff", err)
			}
			return
		}
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); !errors.Is(err, ErrOff) {
			t.Errorf("a body cut off on the wire: %v, want ErrOff", err)
		}
	})
}

// TestARefusalByTheClientsSwitchesEndsThePassOnItsContext: a pass whose own
// switch is on stops at a refusal of the client's, as when its own refuses:
// enrich.enabled's ends every switch context the request carries, and a
// Deezer client's Deezer switch the Deezer context alone; a tick of the
// client's watch that finds them off does the same.
func TestARefusalByTheClientsSwitchesEndsThePassOnItsContext(t *testing.T) {
	srv := newCountingServer(t)
	t.Run("at the send", func(t *testing.T) {
		pass, release := CancelWhenOff(context.Background(), alwaysOn, hour)
		defer release()
		if _, err := get(pass, Enrichment(off).Client(fresh(t)), srv.srv.URL); !errors.Is(err, ErrOff) {
			t.Fatalf("err = %v, want ErrOff", err)
		}
		if Refusal(pass) != ErrOff {
			t.Errorf("the client's refusal left the pass's context live (refusal %v)", Refusal(pass))
		}
	})
	t.Run("a Deezer client's Deezer switch", func(t *testing.T) {
		pass, release := CancelWhenOff(context.Background(), alwaysOn, hour)
		defer release()
		dctx, releaseDeezer := CancelWhenDeezerOff(pass, alwaysOn, hour)
		defer releaseDeezer()
		if _, err := get(dctx, Deezer(alwaysOn, off).Client(fresh(t)), srv.srv.URL); !errors.Is(err, ErrDeezerOff) {
			t.Fatalf("err = %v, want ErrDeezerOff", err)
		}
		if Refusal(dctx) != ErrDeezerOff || pass.Err() != nil {
			t.Errorf("Deezer's context %v, the pass's %v; want Deezer's ended alone", Refusal(dctx), pass.Err())
		}
	})
	t.Run("a tick of the watch", func(t *testing.T) {
		pass, release := CancelWhenOff(context.Background(), alwaysOn, hour)
		defer release()
		s := newSwitch(true)
		client := Enrichment(s.get).Every(5 * time.Millisecond).Client(fresh(t))
		holding := make(chan struct{})
		srv.setHold(func(_ http.ResponseWriter, r *http.Request) {
			close(holding)
			<-r.Context().Done()
		})
		defer srv.setHold(nil)
		go func() {
			<-holding
			s.on.Store(false)
		}()
		if _, err := get(pass, client, srv.srv.URL); !errors.Is(err, ErrOff) {
			t.Fatalf("err = %v, want ErrOff", err)
		}
		if Refusal(pass) != ErrOff {
			t.Errorf("the watch's refusal left the pass's context live (refusal %v)", Refusal(pass))
		}
	})
}

// TestTheTransportAdmitsAtTheSendAndAtTheAnswer drives Transport over a real
// server with the client's own switches on and a pass's switch on the
// request's context: one sent after the pass's flip never reaches the
// server; an answer that arrives after it is not taken; a request a tick of
// the pass's switch cut off on the wire fails with its refusal, not a bare
// cancellation; a Deezer switch on the context is no plain client's; and
// Client leaves the caller's client as it was.
func TestTheTransportAdmitsAtTheSendAndAtTheAnswer(t *testing.T) {
	srv := newCountingServer(t)
	plain := &http.Client{Timeout: 10 * time.Second}
	client := Enrichment(alwaysOn).Every(hour).Client(plain)
	if plain.Transport != nil {
		t.Fatal("Client changed the caller's client")
	}

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
		srv.setHold(func(http.ResponseWriter, *http.Request) { s.on.Store(false) })
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
		srv.setHold(func(_ http.ResponseWriter, r *http.Request) {
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
		if _, err := get(dctx, Deezer(alwaysOn, alwaysOn).Client(plain), srv.srv.URL); !errors.Is(err, ErrDeezerOff) {
			t.Errorf("a Deezer client's send with Deezer off: %v, want ErrDeezerOff", err)
		}
	})
}

// TestTheWatchEndsWithTheBodyOrTheError: a client's switches are asked while
// its request is on the wire, and never again once the answer's body is
// closed, or once the request fails: the watch is released there and waited
// for, so nothing asks after Close or the error returns, on a client with no
// Timeout too, whose own context nothing else ends.
func TestTheWatchEndsWithTheBodyOrTheError(t *testing.T) {
	srv := newCountingServer(t)
	const every = time.Millisecond
	// quiet is how long a released watch must go unasked: fifty of its
	// intervals, each of which a running watch would ask in.
	const quiet = 50 * every
	stillAsked := func(t *testing.T, s *switchOf, what string) {
		t.Helper()
		from := s.asked.Load()
		time.Sleep(quiet)
		if n := s.asked.Load() - from; n != 0 {
			t.Errorf("the switch was asked %d time(s) after %s", n, what)
		}
	}
	t.Run("the body's Close", func(t *testing.T) {
		s := newSwitch(true)
		answered := make(chan struct{})
		srv.setHold(func(http.ResponseWriter, *http.Request) {
			// Long enough for the watch to tick while the request waits.
			time.Sleep(10 * every)
			close(answered)
		})
		defer srv.setHold(nil)
		got, err := get(context.Background(), Enrichment(s.get).Every(every).Client(untimed(t)), srv.srv.URL)
		if err != nil || got != "answer" {
			t.Fatalf("get = %q, %v", got, err)
		}
		<-answered
		if s.asked.Load() < 3 {
			t.Fatalf("the switch was asked %d time(s): the watch never ran, and the pin reads nothing", s.asked.Load())
		}
		stillAsked(t, s, "the body was closed")
	})
	t.Run("an error", func(t *testing.T) {
		s := newSwitch(true)
		srv.setHold(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * every)
			// Hang up with no answer: the request fails with a transport
			// error.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			panic(http.ErrAbortHandler)
		})
		defer srv.setHold(nil)
		if _, err := get(context.Background(), Enrichment(s.get).Every(every).Client(untimed(t)), srv.srv.URL); err == nil || errors.Is(err, ErrOff) {
			t.Fatalf("get = %v, want a transport error", err)
		}
		stillAsked(t, s, "the request failed")
	})
}
