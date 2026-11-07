// Package enrichgate is the send gate of enrich.enabled and
// enrich.deezerEnabled (backlog B617): a client of a service the switches
// name sends nothing while they are off, whatever context it is handed.
//
// One convention holds throughout: a switch is a predicate its holder asks,
// and a nil predicate is off. A client holds its own (Switches, given when it
// is built), so a client built with none sends nothing, and its transport asks
// them at the send and again when the answer arrives, and watches each
// request on the wire, cutting it off within the switches' interval
// (Interval, a second). A pass holds one too, on its context (CancelWhenOff),
// which a refusal or a tick ends, so everything running on it stops as at a
// shutdown: a pacing sleep ends, a write the pass reaches records nothing
// (Admit), and an error path that asks the context first takes the refusal for
// no answer at all, never for a miss. The client asks the switches its
// request's context carries as well as its own, and a refusal from either ends
// that context's pass.
//
// enrich.enabled governs every send (the MusicBrainz, Cover Art Archive,
// Deezer, iTunes, Atlas premium-cover and AcoustID clients, a mirror's
// included); enrich.deezerEnabled governs Deezer's sends alone, so turning
// Deezer off refuses a Deezer request and leaves a MusicBrainz one on the same
// pass running. Off, enrich.enabled answers ErrOff whatever Deezer's says.
// This package imports nothing of the bridge's, so the enricher, the AcoustID
// client and serve's adapters all share it.
package enrichgate

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// Interval is how far apart a switch is asked while nothing else asks it: a
// switch-off cancels a pass's context, and a request on the wire, within
// Interval. A send and an answer ask the switches themselves, with no wait.
const Interval = time.Second

// scope says which sends a switch governs.
type scope uint8

const (
	// scopeEvery governs every send: enrich.enabled.
	scopeEvery scope = iota
	// scopeDeezer governs only a Deezer client's sends:
	// enrich.deezerEnabled.
	scopeDeezer
)

// on reports whether sw answers on. A nil switch is off.
func on(sw func() bool) bool { return sw != nil && sw() }

// interval is every, or Interval when it is 0 or less.
func interval(every time.Duration) time.Duration {
	if every <= 0 {
		return Interval
	}
	return every
}

// strongest is the refusal of two that stands: enrich.enabled's over
// Deezer's, either over none.
func strongest(a, b error) error {
	switch {
	case errors.Is(a, ErrOff) || errors.Is(b, ErrOff):
		return ErrOff
	case a != nil:
		return a
	default:
		return b
	}
}

// gate is one switch a pass's context carries, the refusal it answers, and
// the context it cancels.
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
// enrich.enabled's switch on, for a pass to run on: it is cancelled, with
// ErrOff as its cause, once on answers off, asked every `every` (Interval when
// every is 0 or less), or at a send or a write that asks it (a client's
// Switches, Admit). release ends the asking and the context, and waits for the
// goroutine that asks; the caller defers it. A context whose switch is off at
// the start, a nil on among them, is cancelled already.
func CancelWhenOff(parent context.Context, on func() bool, every time.Duration) (ctx context.Context, release context.CancelFunc) {
	return cancelWhenOff(parent, on, ErrOff, scopeEvery, interval(every))
}

// CancelWhenDeezerOff is CancelWhenOff for enrich.deezerEnabled: its switch
// governs a Deezer client's sends alone, and cancels with ErrDeezerOff. The
// enricher derives one from its batch's context for each Deezer request, so
// a Deezer flip cancels that request and nothing else.
func CancelWhenDeezerOff(parent context.Context, on func() bool, every time.Duration) (ctx context.Context, release context.CancelFunc) {
	return cancelWhenOff(parent, on, ErrDeezerOff, scopeDeezer, interval(every))
}

// cancelWhenOff is CancelWhenOff with the refusal, the scope and the asking
// interval spelled out.
func cancelWhenOff(parent context.Context, sw func() bool, refusal error, sc scope, every time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	g := &gate{on: sw, refusal: refusal, scope: sc, cancel: cancel, outer: gateOf(parent)}
	ctx = context.WithValue(ctx, gateKey{}, g)
	if !on(sw) {
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
				if !on(sw) {
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

// admitCtx asks the switches ctx carries that govern a send of sc: the
// refusal of the switch that ended ctx already, or of one that is off now,
// whose context it cancels, so the pass on it stops there. Of two that are
// off, enrich.enabled's answers. Nil when every switch ctx carries is on, or
// it carries none: a context alone never admits anything, since every caller
// asks a switch it holds beside it (Switches.Answer, Admit).
func admitCtx(ctx context.Context, sc scope) error {
	if r := Refusal(ctx); r != nil {
		return r
	}
	var off *gate
	for g := gateOf(ctx); g != nil; g = g.outer {
		if g.scope != scopeEvery && g.scope != sc {
			continue
		}
		if on(g.on) {
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

// refuse ends the switch contexts ctx carries that a refusal reaches, so the
// pass on ctx stops as if its own switch had refused: ErrOff ends every one,
// ErrDeezerOff those of Deezer's switch (that request alone).
func refuse(ctx context.Context, r error) {
	for g := gateOf(ctx); g != nil; g = g.outer {
		switch {
		case errors.Is(r, ErrOff):
			g.cancel(ErrOff)
		case errors.Is(r, ErrDeezerOff) && g.scope == scopeDeezer:
			g.cancel(ErrDeezerOff)
		}
	}
}

// Admit asks enrich.enabled before a write of what a pass learnt: the switch
// the writer holds, own, and those its context carries. A nil own is off. It
// answers ErrOff when one is off, after ending the switch contexts ctx
// carries, so the pass on ctx stops there as at a shutdown, and nil when all
// are on. An answer taken while the switch was on can still meet it off here.
func Admit(ctx context.Context, own func() bool) error {
	return Enrichment(own).Answer(ctx)
}

// Refusal is the refusal of the switch that ended ctx (a pass's, or a
// request's watch), ErrOff or ErrDeezerOff, or nil while ctx is live, or when
// it ended otherwise (a shutdown, a deadline, its release). Only this package
// ends a context with either as its cause.
func Refusal(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrOff):
		return ErrOff
	case errors.Is(cause, ErrDeezerOff):
		return ErrDeezerOff
	}
	return nil
}

// Refused is the refusal err carries, ErrOff or ErrDeezerOff (ErrOff when it
// carries both), or nil. A caller handed an error by a client asks it before
// it takes the error for an answer, a miss or a failure, whatever its own
// context says: a context that carries no switch is not ended by a client's
// refusal, so asking the context alone takes the refusal for a failure.
func Refused(err error) error {
	switch {
	case errors.Is(err, ErrOff):
		return ErrOff
	case errors.Is(err, ErrDeezerOff):
		return ErrDeezerOff
	}
	return nil
}

// Switches are a client's own: the predicates its requests answer to whatever
// their contexts carry, enrich.enabled for every client and
// enrich.deezerEnabled beside it for Deezer's. A nil predicate is off, and so
// is the zero value: a client built without its switches sends nothing.
type Switches struct {
	enrichment func() bool
	deezer     func() bool
	scope      scope
	every      time.Duration
}

// Enrichment is the switches of a client that enrich.enabled alone governs:
// MusicBrainz's, the Cover Art Archive's, iTunes', Atlas premium covers' and
// AcoustID's.
func Enrichment(on func() bool) Switches {
	return Switches{enrichment: on, scope: scopeEvery}
}

// Deezer is the switches of Deezer's client: enrich.enabled, and
// enrich.deezerEnabled beside it.
func Deezer(on, deezer func() bool) Switches {
	return Switches{enrichment: on, deezer: deezer, scope: scopeDeezer}
}

// ForDeezer is s as a Deezer client takes it: s itself when it is Deezer's,
// and otherwise s with no Deezer switch, which is off, so a Deezer client
// built with another client's switches sends nothing.
func (s Switches) ForDeezer() Switches {
	if s.scope == scopeDeezer {
		return s
	}
	return Switches{enrichment: s.enrichment, scope: scopeDeezer, every: s.every}
}

// Every is s asking its switches every d while a request is on the wire, in
// place of Interval: a test's interval, which production leaves alone.
func (s Switches) Every(d time.Duration) Switches {
	s.every = d
	return s
}

// refusal is what s's own switches answer now: ErrOff while enrich.enabled is
// off, else ErrDeezerOff while a Deezer client's switch is off, else nil.
func (s Switches) refusal() error {
	if !on(s.enrichment) {
		return ErrOff
	}
	if s.scope == scopeDeezer && !on(s.deezer) {
		return ErrDeezerOff
	}
	return nil
}

// Answer asks s and the switches ctx carries whether what a client is about
// to take may be taken: a request at its send, an answer as it arrives, and
// whatever a client returns once its answer began (a decoded body, a 404, an
// error status's body), which a caller could otherwise keep as a miss. A
// refusal ends the switch contexts ctx carries that it reaches, so the pass
// on ctx stops, and is returned, ErrOff before ErrDeezerOff; nil when every
// switch is on.
func (s Switches) Answer(ctx context.Context) error {
	r := strongest(admitCtx(ctx, s.scope), s.refusal())
	if r != nil {
		refuse(ctx, r)
	}
	return r
}

// watch derives a request's own context from ctx, which s's switches end,
// with their refusal as its cause, once they answer off, asked every s's
// interval: what cuts a request off on the wire whatever ctx carries. A tick
// that finds them off ends ctx's pass too (refuse). release ends the asking
// and the context, and waits for the goroutine that asks.
func (s Switches) watch(ctx context.Context) (context.Context, func()) {
	rctx, cancel := context.WithCancelCause(ctx)
	released := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(interval(s.every))
		defer tick.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-released:
				return
			case <-tick.C:
				if r := s.refusal(); r != nil {
					refuse(ctx, r)
					cancel(r)
					return
				}
			}
		}
	}()
	var once sync.Once
	return rctx, func() {
		once.Do(func() {
			close(released)
			<-exited
			cancel(nil)
		})
	}
}

// Transport is base (http.DefaultTransport when nil) behind s: a request is
// sent only while s, and every switch its context carries, are on (Answer),
// a context that carries none answering to s alone; its answer is taken only
// while they still are when it arrives; and while it is on the wire, its body
// included, s is asked every interval, and a switch-off cuts it off with the
// switch's refusal rather than a bare cancellation. The request's watch ends
// when its body is closed, or with the error that ends it.
func (s Switches) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, sw: s}
}

type transport struct {
	base http.RoundTripper
	sw   Switches
}

// RoundTrip admits req, sends it through base on a context the client's
// switches watch, and admits its answer.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if r := t.sw.Answer(ctx); r != nil {
		// A RoundTripper closes the body of a request it does not send.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, r
	}
	rctx, release := t.sw.watch(ctx)
	resp, err := t.base.RoundTrip(req.WithContext(rctx))
	if err != nil {
		// A cut the watch or a pass's switch made, whose cause rctx takes
		// from either.
		r := Refusal(rctx)
		release()
		if r != nil {
			return nil, r
		}
		return nil, err
	}
	if r := t.sw.Answer(ctx); r != nil {
		_ = resp.Body.Close()
		release()
		return nil, r
	}
	resp.Body = &watchedBody{ReadCloser: resp.Body, rctx: rctx, release: release}
	return resp, nil
}

// CloseIdleConnections passes http.Client.CloseIdleConnections to base.
func (t *transport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// watchedBody is an answer's body while its request is watched: a read the
// switches cut off fails with their refusal, and Close ends the watch.
type watchedBody struct {
	io.ReadCloser
	rctx    context.Context
	release func()
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		if r := Refusal(b.rctx); r != nil {
			return n, r
		}
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// Client is a copy of c (a zero client when nil) whose transport is
// s.Transport(c.Transport). The caller's client is never changed.
func (s Switches) Client(c *http.Client) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	cp.Transport = s.Transport(cp.Transport)
	return &cp
}
