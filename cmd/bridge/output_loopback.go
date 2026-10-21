package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
)

// outputLoopback is the loopback track route's lifecycle in serve (backlog
// B473, decision 2): this bridge's own tracks over plain HTTP on 127.0.0.1
// and ::1, for the MPD beside it to fetch when its database misses a
// track. It starts the first time the localOutputs gate answers true, at
// boot or later through api.LocalOutputsConfig.OnActive, so it runs while
// output.mpd resolves on and never otherwise, and it stops with the server.
//
// One attempt to bind: a port that is taken is reported once, and the
// checklist's Music line says what to do, rather than a bind and a warning
// on every /v1/health poll.
type outputLoopback struct {
	srv  *dlna.LoopbackServer
	log  *slog.Logger
	port int

	mu      sync.Mutex
	tried   bool
	stopped bool
	err     error
}

// outputLoopbackWiring is what the route needs from serve.
type outputLoopbackWiring struct {
	// Library is the index the DLNA listener reads: a track id to its row.
	Library dlna.TrackLookup
	// Roots are the live library roots (the resolver's).
	Roots  func() []string
	Port   int
	Logger *slog.Logger
}

func newOutputLoopback(w outputLoopbackWiring) *outputLoopback {
	log := w.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "output"))
	return &outputLoopback{
		srv:  dlna.NewLoopbackServer(dlna.LoopbackConfig{Library: w.Library, Roots: w.Roots, Port: w.Port, Logger: log}),
		log:  log,
		port: w.Port,
	}
}

// ensureStarted starts the route once. It is the gate's OnActive, called
// on every true answer, so after the first call it costs a lock.
func (o *outputLoopback) ensureStarted() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.tried || o.stopped {
		return
	}
	o.tried = true
	if err := o.srv.Start(); err != nil {
		o.err = err
		o.log.Warn("output: the loopback track route did not start, so MPD cannot fetch this bridge's tracks over HTTP",
			slog.Int("port", o.port), slog.String("err", err.Error()))
		return
	}
	o.log.Info("output: serving this bridge's tracks to the MPD on this box", slog.String("url", o.srv.BaseURL()))
}

// BaseURL is the route's base URL while it serves, "" otherwise.
func (o *outputLoopback) BaseURL() string { return o.srv.BaseURL() }

// StartErr is why the route is not serving after it tried to start, nil
// when it serves or never tried.
func (o *outputLoopback) StartErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// Stop stops the route; it does not start again.
func (o *outputLoopback) Stop(ctx context.Context) {
	o.mu.Lock()
	o.stopped = true
	o.mu.Unlock()
	if err := o.srv.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		o.log.Warn("output: loopback track route shutdown", slog.String("err", err.Error()))
	}
}
