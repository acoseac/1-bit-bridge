// Package enrichgate is the send gate of enrich.enabled and
// enrich.deezerEnabled (backlog B617): what runs on a context it derives
// stops when the switch goes off, as at a shutdown.
//
// The switch rides the context (CancelWhenOff, CancelWhenDeezerOff), and
// every client of an upstream the switch names sends through Transport,
// which asks it at the send and again when the answer arrives (Admit). So
// a request that starts after the flip is refused before it leaves, an
// answer that arrives after it is not taken, and a request on the wire is
// cancelled within Interval, when the context's own ticker sees the switch
// off. A refusal cancels the switch's context at once, with the refusal as
// its cause, so everything else running on it stops as a shutdown stops
// it: a pacing sleep ends, a write the pass reaches on the context records
// nothing, and an error handler that asks the context first takes the
// refusal for no answer at all, never for a miss.
//
// enrich.enabled's context governs every send (the MusicBrainz, Cover Art
// Archive, Deezer, Atlas premium-cover and AcoustID clients, a mirror's
// included); enrich.deezerEnabled's governs Deezer's sends alone, so
// turning Deezer off cancels a Deezer request and leaves a MusicBrainz one
// on the same batch running. This package imports nothing of the bridge's,
// so the enricher, the AcoustID client and serve's adapters all share it.
package enrichgate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

var (
	// ErrOff is enrich.enabled's refusal: a request it refused at the send,
	// an answer it did not take, or a request it cut off on the wire. The
	// upstream was not asked, or what it said was not kept: no answer, never
	// a miss. It wraps context.Canceled, as a shutdown's stop does.
	ErrOff = fmt.Errorf("enrichment is switched off (enrich.enabled): %w", context.Canceled)
	// ErrDeezerOff is enrich.deezerEnabled's refusal, of a Deezer request
	// alone.
	ErrDeezerOff = fmt.Errorf("Deezer artist portraits are switched off (enrich.deezerEnabled): %w", context.Canceled)
)

// Interval is how far apart a switch's context asks the switch: a switch-off
// cancels what runs on it, a request on the wire included, within Interval.
// A send asks the switch itself (Admit), with no wait at all.
const Interval = time.Second

// scope says which sends a switch governs.
type scope uint8

const (
	// scopeEvery governs every send through a Transport: enrich.enabled.
	scopeEvery scope = iota
	// scopeDeezer governs only the sends of a DeezerTransport:
	// enrich.deezerEnabled.
	scopeDeezer
)

// gate is one switch a context carries, the refusal it answers, and the
// context it cancels.
type gate struct {
	on      func() bool
	refusal error
	scope   scope
	cancel  context.CancelCauseFunc
	outer   *gate // the switch of the context this one derives from, if any
}

type gateKey struct{}

// gateOf is the innermost switch ctx carries, or nil.
func gateOf(ctx context.Context) *gate {
	g, _ := ctx.Value(gateKey{}).(*gate)
	return g
}

// CancelWhenOff returns a context derived from parent that carries
// enrich.enabled's switch on: it is cancelled, with ErrOff as its cause, once
// on answers false, asked every `every` (Interval when every is 0 or less),
// or at a send through a Transport (Admit). release ends the asking and the
// context, and waits for the goroutine that asks; the caller defers it. A
// context whose on answers false at the start is cancelled already. A nil on
// is never off: the context carries no switch, and only parent ends it.
func CancelWhenOff(parent context.Context, on func() bool, every time.Duration) (ctx context.Context, release context.CancelFunc) {
	return cancelWhenOff(parent, on, ErrOff, scopeEvery, interval(every))
}

// CancelWhenDeezerOff is CancelWhenOff for enrich.deezerEnabled: its switch
// governs a DeezerTransport's sends alone, and cancels with ErrDeezerOff. The
// enricher derives one from its batch's context for each Deezer request, so
// a Deezer flip cancels that request and nothing else.
func CancelWhenDeezerOff(parent context.Context, on func() bool, every time.Duration) (ctx context.Context, release context.CancelFunc) {
	return cancelWhenOff(parent, on, ErrDeezerOff, scopeDeezer, interval(every))
}

// interval is every, or Interval when it is 0 or less.
func interval(every time.Duration) time.Duration {
	if every <= 0 {
		return Interval
	}
	return every
}

// cancelWhenOff is CancelWhenOff with the refusal, the scope and the asking
// interval spelled out.
func cancelWhenOff(parent context.Context, on func() bool, refusal error, sc scope, every time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if on == nil {
		return ctx, func() { cancel(nil) }
	}
	g := &gate{on: on, refusal: refusal, scope: sc, cancel: cancel, outer: gateOf(parent)}
	ctx = context.WithValue(ctx, gateKey{}, g)
	if !on() {
		cancel(refusal)
		return ctx, func() { cancel(nil) }
	}
	released := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-released:
				return
			case <-tick.C:
				if !on() {
					cancel(refusal)
					return
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(released)
			<-exited
			cancel(nil)
		})
	}
}

// Admit asks every enrich.enabled switch ctx carries whether a send may go
// out now, and answers ErrOff when one is off, after cancelling the context
// that switch derived, so the pass on it stops as at a shutdown. Nil when
// every switch is on, or ctx carries none (a caller with no switch, such as
// a CLI that decided its own). A context a switch has ended already answers
// that switch's refusal.
func Admit(ctx context.Context) error { return admit(ctx, scopeEvery) }

// AdmitDeezer is Admit for a Deezer send: it asks enrich.deezerEnabled's
// switch as well, which a Deezer flip cancels alone (ErrDeezerOff), and
// answers ErrOff first when enrich.enabled is off.
func AdmitDeezer(ctx context.Context) error { return admit(ctx, scopeDeezer) }

// admit asks the switches ctx carries that govern a send of sc. Of two that
// are off, enrich.enabled's answers: it ends the pass, where Deezer's ends a
// Deezer request.
func admit(ctx context.Context, sc scope) error {
	if r := Refusal(ctx); r != nil {
		return r
	}
	var off *gate
	for g := gateOf(ctx); g != nil; g = g.outer {
		if g.scope != scopeEvery && g.scope != sc {
			continue
		}
		if g.on() {
			continue
		}
		if off == nil || g.scope == scopeEvery {
			off = g
		}
	}
	if off == nil {
		return nil
	}
	off.cancel(off.refusal)
	return off.refusal
}

// Refusal is the refusal of the switch that ended ctx, or nil while ctx is
// live, or when it ended otherwise (a shutdown, a deadline, its release).
func Refusal(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	for g := gateOf(ctx); g != nil; g = g.outer {
		if errors.Is(cause, g.refusal) {
			return g.refusal
		}
	}
	return nil
}

// Transport is base (http.DefaultTransport when nil) behind the gate of
// enrich.enabled: a request is sent only while every enrich.enabled switch
// its context carries is on (Admit), its answer is taken only while they are
// still on when it arrives, and a request a switch cut off on the wire fails
// with that switch's refusal rather than a bare cancellation. A request whose
// context carries no switch passes as if the gate were not there.
func Transport(base http.RoundTripper) http.RoundTripper {
	return newTransport(base, scopeEvery)
}

// DeezerTransport is Transport for Deezer's requests: it asks
// enrich.deezerEnabled's switch as well (AdmitDeezer).
func DeezerTransport(base http.RoundTripper) http.RoundTripper {
	return newTransport(base, scopeDeezer)
}

func newTransport(base http.RoundTripper, sc scope) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, scope: sc}
}

type transport struct {
	base  http.RoundTripper
	scope scope
}

// RoundTrip admits req, sends it through base, and admits its answer.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := admit(ctx, t.scope); err != nil {
		// A RoundTripper closes the body of a request it does not send.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		if r := Refusal(ctx); r != nil {
			return nil, r
		}
		return nil, err
	}
	if err := admit(ctx, t.scope); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// CloseIdleConnections passes http.Client.CloseIdleConnections to base.
func (t *transport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Client is a copy of c (a zero client when nil) whose transport is
// Transport(c.Transport). The caller's client is never changed.
func Client(c *http.Client) *http.Client { return client(c, scopeEvery) }

// DeezerClient is Client with DeezerTransport.
func DeezerClient(c *http.Client) *http.Client { return client(c, scopeDeezer) }

func client(c *http.Client, sc scope) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	cp.Transport = newTransport(cp.Transport, sc)
	return &cp
}
