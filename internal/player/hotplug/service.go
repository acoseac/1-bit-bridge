// Package hotplug watches the box for DACs arriving and leaving, for the
// bridge's player (B470 PR 4a2). It hears the kernel's uevents on a netlink
// socket, waits for a burst of them to settle, reads the whole inventory
// with 4a's readers (binding.Observer), resolves the configured binding
// (binding.Settled), and publishes a snapshot when anything changed, with
// an inventory generation that belongs to the serve.
//
// It plays nothing and opens nothing under /dev/snd, ever. What playback
// does about a change (stop, keep the position, never play again by
// itself) is the engine's (PR 4b), which subscribes to the Feed; the check
// before audio (binding.Checker) stays the authority on what an open node
// is.
package hotplug

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

var logger = logging.Component("hotplug")

// Config is what a Service runs on. Settings is the one field without a
// working default.
type Config struct {
	// Settings reads the live config's player block: at every change of
	// the config and on every reading, never once at start.
	Settings func() Settings
	// Changed returns a channel the config closes at its next change
	// (config.RuntimeConfig.Changed); nil, a config that never changes.
	Changed func() <-chan struct{}
	// Observe reads the box: binding.Observer.ObserveContext over the
	// trees the caller names. Nil reads the host's own /proc and /sys.
	Observe func(ctx context.Context) (binding.Inventory, error)
	// OpenSource opens the event source for one watch, which ends with
	// ctx; nil is the kernel's netlink socket (a stub off Linux).
	OpenSource func(ctx context.Context) Source
	// Clock is the watcher's time, SystemClock when nil.
	Clock Clock
	// Timing is the watcher's timing, DefaultTiming when zero.
	Timing Timing
	// Logger is where the watcher logs, the hotplug component when nil.
	Logger *slog.Logger
	// Feed is where it publishes; nil makes one.
	Feed *Feed
}

// Service runs the watcher for one serve: a watch of the box while the live
// config enables the player, nothing but a wait for the config to change
// while it does not. A watch is one goroutine that owns its state and takes
// every reading, one at a time, whatever asked for it, so an older reading
// is never published after a newer one; and its event source's.
type Service struct {
	cfg  Config
	feed *Feed
	log  *slog.Logger

	mu       sync.Mutex
	stats    Stats
	running  bool
	stopping bool
	source   Source
	next     time.Time
	failure  *Failure
	progCh   chan struct{}
}

// Stats count what the service has done, for diagnostics and tests.
type Stats struct {
	// Sessions are the watches started: one per enable.
	Sessions uint64
	// Events are the kernel's events the watch took, Lost the losses its
	// source reported (the kernel's ENOBUFS, or a watch behind), and
	// ConfigChanges the changes of the config it took.
	Events, Lost, ConfigChanges uint64
	// Reconciles are the readings taken, Failures those that failed,
	// Discarded those a stopped watch finished and nothing published, and
	// SetAside those not published because a removal reaching a card had
	// queued behind them (round 2).
	Reconciles, Failures, Discarded, SetAside uint64
	// Publishes are the snapshots published, a generation's republishing
	// (its health changed) included.
	Publishes uint64
}

// Status is the service as it stands.
type Status struct {
	// Running is a watch running: the player is enabled. Stopping is a
	// stop waiting for a watch's goroutines.
	Running, Stopping bool
	// Source is the running watch's event source, mode off with none.
	Source SourceStatus
	Stats  Stats
	// Next is when the watch reads the box next, as things stand.
	Next time.Time
	// Failure is the run of failed readings going on, nil when the last
	// reading worked.
	Failure *Failure
}

// NewService is a service on cfg, not yet running.
func NewService(cfg Config) *Service {
	if cfg.Settings == nil {
		cfg.Settings = func() Settings { return Settings{} }
	}
	if cfg.Changed == nil {
		cfg.Changed = func() <-chan struct{} { return nil }
	}
	if cfg.Observe == nil {
		cfg.Observe = binding.Observer{}.ObserveContext
	}
	if cfg.OpenSource == nil {
		cfg.OpenSource = openKernelSource
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock
	}
	if cfg.Timing == (Timing{}) {
		cfg.Timing = DefaultTiming()
	}
	if cfg.Logger == nil {
		cfg.Logger = logger
	}
	if cfg.Feed == nil {
		cfg.Feed = NewFeed()
	}
	return &Service{cfg: cfg, feed: cfg.Feed, log: cfg.Logger, progCh: make(chan struct{})}
}

// Feed is where the snapshots are published.
func (s *Service) Feed() *Feed { return s.feed }

// Status is the service now.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Running: s.running, Stopping: s.stopping, Stats: s.stats, Next: s.next, Source: SourceStatus{Mode: ModeOff}}
	if s.failure != nil {
		f := *s.failure
		st.Failure = &f
	}
	if s.running && s.source != nil {
		st.Source = s.source.Status()
	}
	return st
}

// Updates returns a channel closed at the service's next change of Status:
// a reader takes the channel, reads Status, and waits on it, never on a
// sleep or a poll (the tests, and serve's).
func (s *Service) Updates() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progCh
}

// update changes the status under its lock and signals progress.
func (s *Service) update(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	close(s.progCh)
	s.progCh = make(chan struct{})
}

// Run runs until ctx ends: a watch while the live config enables the
// player, started and stopped as the config changes. It returns once the
// running watch has stopped, or its stop has given up on it (Timing.StopWait).
func (s *Service) Run(ctx context.Context) {
	var w *watch
	for {
		changed := s.cfg.Changed()
		set := s.cfg.Settings()
		switch {
		case set.Enabled && w == nil:
			w = s.start(ctx)
		case set.Enabled:
			w.pokeIt()
		case w != nil:
			s.stop(w)
			w = nil
			if s.feed.publishDisabled(s.cfg.Clock.Now(), set, s.log) {
				s.update(func() { s.stats.Publishes++ })
			}
		}
		select {
		case <-ctx.Done():
			if w != nil {
				s.stop(w)
			}
			return
		case <-changed:
		}
	}
}

// start starts a watch: its event source, and the goroutine that owns it.
func (s *Service) start(ctx context.Context) *watch {
	wctx, cancel := context.WithCancel(ctx)
	src := s.cfg.OpenSource(wctx)
	w := newWatch(s, src, s.feed.begin(), cancel)
	s.update(func() {
		s.running, s.source = true, src
		s.stats.Sessions++
	})
	s.log.Info("player hotplug: watching for DACs", "events", src.Status().String())
	go func() {
		defer close(w.done)
		w.run(wctx)
	}()
	return w
}

// stop ends a watch and waits for its goroutine and its source's, no
// longer than Timing.StopWait: a read of sysfs cannot be interrupted, and a
// USB device's string attributes take its lock, which a disconnect can
// hold, so a read stuck there is abandoned, never waited for, and what it
// finishes is never published.
func (s *Service) stop(w *watch) {
	w.cancel()
	// The deadline is armed before the stop says it is stopping, so it
	// runs from the moment the stop began, whatever the clock does after.
	timer := s.cfg.Clock.NewTimer()
	timer.Reset(s.cfg.Clock.Now().Add(s.cfg.Timing.StopWait))
	defer timer.Stop()
	s.update(func() { s.stopping = true })
	owner, src := (<-chan struct{})(w.done), w.src.Done()
	for owner != nil || src != nil {
		select {
		case <-owner:
			owner = nil
		case <-src:
			src = nil
		case <-timer.C():
			why := "its event source did not end"
			if owner != nil {
				why = "a read of /sys or /proc is stuck in the kernel, as a USB device's lock held by a disconnect makes it"
			}
			s.log.Warn("player hotplug: the watcher did not stop within its deadline and is abandoned: "+why,
				"waited", s.cfg.Timing.StopWait, "watcherStopped", owner == nil, "eventsStopped", src == nil)
			owner, src = nil, nil
		}
	}
	s.feed.end(w.session)
	s.update(func() {
		s.running, s.stopping, s.source, s.next, s.failure = false, false, nil, time.Time{}, nil
	})
	s.log.Info("player hotplug: stopped watching")
}
