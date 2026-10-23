package main

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The player's files in the data directory, beside its identity
// (config.PlayerUUIDFile): the engine's queue file and its ID and token
// reservation (engine.Config.QueuePath, ReservePath). bridge output play
// keeps its own elsewhere, and never here.
const (
	playerQueueFile       = "player-queue.json"
	playerReservationFile = "player-ids.json"
)

// The bounds of the player's stop: the engine's shutdown, its release
// (engine.ReleaseTimeout) and its flush with room for both, as bridge output
// play waits for it, and then the backend's goroutines (playback.Backend.Wait).
const (
	playerEngineStopBound  = engine.ReleaseTimeout + 5*time.Second
	playerBackendWaitBound = 5 * time.Second
)

// reasonBindingChanged is the output's reason, to the engine, while the
// live config binds another DAC than the one the player started with.
const reasonBindingChanged = "binding_changed"

// playerDetailBytes bounds the detail a line of the player's carries.
const playerDetailBytes = 256

// playerLog is where serve's player logs: the player component, as the
// engine and the backend do.
var playerLog = logging.Component("player")

// servePlayerPlan is serve's decision about its player, made once, at boot,
// from the boot snapshot of the config: a change of output.player while
// serve runs applies when serve next starts.
type servePlayerPlan struct {
	start bool
	// off is why the player does not start: "" when it starts, and when it
	// is simply off (output.player.enabled is not true).
	off string
	// told is an off reason the config's Validate has logged already:
	// config.PlayerEnabled's, which Load runs (logPlayerOff).
	told bool
}

// planServePlayer decides whether serve starts its player: only when every
// gate holds, each refusal a reason. The player runs on this config
// (config.PlayerEnabled: enabled, a block that reads and is valid, Linux,
// not public); a DAC is bound (output.player.device), which PlayerEnabled
// does not ask; serve holds its data directory's lock, under which alone
// the player's identity and IDs are made (lockHeld); and the pause release
// is one the engine takes, 1 s to 10 min, of the config's 1 s to an hour.
// goos is the host ("" is this one). It reads nothing but the config.
func planServePlayer(cfg *config.Config, goos string, lockHeld bool) servePlayerPlan {
	if goos == "" {
		goos = runtime.GOOS
	}
	player := cfg.Output.Player
	on, why := cfg.PlayerEnabled(goos)
	switch {
	case why != nil:
		return servePlayerPlan{off: why.Error(), told: true}
	case !on:
		return servePlayerPlan{}
	case !player.Device.Configured():
		return servePlayerPlan{off: "output.player.device binds no DAC: bridge output dac bind <card> --binding serial|uniqueModel|port binds one, and the player starts when bridge serve next starts"}
	case !lockHeld:
		return servePlayerPlan{off: "serve holds no lock on its data directory (it said why above), and the player's identity and its IDs are made only under that lock"}
	}
	if r := player.PauseRelease(); r < playMinPauseRelease || r > playMaxPauseRelease {
		return servePlayerPlan{off: fmt.Sprintf("output.player.pauseReleaseSeconds is outside the %d to %d seconds the player's engine takes (the config takes up to an hour): set it to %d or less, and the player starts when bridge serve next starts",
			int(playMinPauseRelease/time.Second), int(playMaxPauseRelease/time.Second), int(playMaxPauseRelease/time.Second))}
	}
	return servePlayerPlan{start: true}
}

// servePlayerWiring is what serve hands its player.
type servePlayerWiring struct {
	// Cfg is the boot snapshot of the config: what the player runs on until
	// serve next starts.
	Cfg *config.Config
	// Hold is serve's hold on its data directory's lock, which the player
	// shares while anything it started may still write there.
	Hold *dataDirHold
	// Store and Paths are the library index and the live path resolver the
	// player's resolver reads (internal/player/library).
	Store library.Store
	Paths library.Paths
	// Feed is the hotplug watcher's (startPlayerHotplug), which the output
	// reports are made from.
	Feed *hotplug.Feed
	// Trees are what the binding locator reads; the zero value is hostTrees.
	Trees outputTrees
	// Backend builds the backend; nil is playback.New. A boot test passes a
	// stand-in: no test opens a PCM node.
	Backend func(playback.Config) (playBackend, error)
	// Boot, when set, is told the player as serve decides it, before
	// anything of it starts.
	Boot func(*bridgePlayer)
	// Seams are a boot test's hold on the player; nil in production.
	Seams *servePlayerSeams
	// Bounds are the stop's bounds; the zero value is production's.
	Bounds servePlayerBounds
}

// servePlayerSeams are where a boot test holds serve's player between two
// of its steps, or makes a step panic. Each runs on the goroutine that
// reaches it.
type servePlayerSeams struct {
	// reports runs before each output report reaches the engine.
	reports func(engine.OutputReport)
	// opens runs as the resolver opens a reference (library.Seams.Opens).
	opens func(engine.Reference)
}

// servePlayerBounds are the stop's bounds: how long it waits for the
// engine's Run to return, and then for the backend's goroutines.
type servePlayerBounds struct {
	engine, backend time.Duration
}

// orProduction is b, or production's bounds where b sets none.
func (b servePlayerBounds) orProduction() servePlayerBounds {
	if b.engine <= 0 {
		b.engine = playerEngineStopBound
	}
	if b.backend <= 0 {
		b.backend = playerBackendWaitBound
	}
	return b
}

// bridgePlayer is serve's player: the engine (internal/player/engine), the
// backend (internal/player/playback) over the library's resolver
// (internal/player/library) and the bound DAC, and the output reports made
// from the hotplug watcher's snapshots. It starts on a goroutine of its
// own, its supervisor, so serve's library serving never waits on the
// player's storage, and every fault stays inside it.
type bridgePlayer struct {
	plan   servePlayerPlan
	bounds servePlayerBounds
	hold   *dataDirHold
	seams  *servePlayerSeams
	// device is the binding the player started with.
	device config.PlayerDevice

	// cancel ends the player; nil when it never started.
	cancel context.CancelFunc
	// ready is closed once the player runs, or once it has failed to;
	// done once its supervisor has returned; settled once nothing it
	// started runs, its share of the data directory's lock let go.
	ready, done, settled chan struct{}
	readyOnce            sync.Once

	// Set before ready closes: the engine and the backend that run, or why
	// the player did not start past its plan.
	eng    *engine.Engine
	be     playBackend
	failed string

	// The supervisor's alone.
	run     *playerRun
	foreign bool

	mu      sync.Mutex
	stopped time.Duration // how long its stop took, once it has
}

// playerRun is the engine as it runs: its Run, ended by stop and over once
// done is closed, and the feed it is reported from.
type playerRun struct {
	eng  *engine.Engine
	be   playBackend
	done chan struct{}
	stop context.CancelFunc
	sub  *hotplug.Subscription
}

// startServePlayer starts the player plan allows, on a goroutine of its
// own, and returns at once; for a plan that refuses it, it logs why (but a
// reason Validate has logged) and starts nothing: no goroutine, no file.
// The caller defers stop.
func startServePlayer(plan servePlayerPlan, w servePlayerWiring) *bridgePlayer {
	p := &bridgePlayer{plan: plan, bounds: w.Bounds.orProduction(), hold: w.Hold, seams: w.Seams,
		ready: make(chan struct{}), done: make(chan struct{}), settled: make(chan struct{})}
	if !plan.start {
		if plan.off != "" && !plan.told {
			playerLog.Warn("player: not started", "reason", plan.off)
		}
		p.markReady()
		close(p.done)
		close(p.settled)
		if w.Boot != nil {
			w.Boot(p)
		}
		return p
	}
	p.device = w.Cfg.Output.Player.Device
	w.Hold.acquire()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	if w.Boot != nil {
		w.Boot(p)
	}
	go p.supervise(ctx, w)
	return p
}

// markReady closes ready, once.
func (p *bridgePlayer) markReady() { p.readyOnce.Do(func() { close(p.ready) }) }

// stop is serve's teardown of the player: it ends it and waits for its
// supervisor, which stops the engine and joins it, and then the backend,
// each within its bound, before serve's watcher and store close (the
// caller's defer order). A player whose start or whose state storage is
// stalled past the bounds is abandoned, said so, and keeps its share of the
// data directory's lock until it ends.
func (p *bridgePlayer) stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	bound := p.bounds.engine + p.bounds.backend + time.Second
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
		playerLog.Warn("player: did not stop within its bound: its start or its state storage is stalled; it is abandoned and ends with the process, and serve keeps its data directory's lock until then",
			"bound", bound)
	}
}

// supervise runs the player: it starts it, reports the output to it and
// logs what it does until ctx ends, and then stops it. A panic anywhere in
// it is recovered and logged, and the player stops.
func (p *bridgePlayer) supervise(ctx context.Context, w servePlayerWiring) {
	defer func() {
		if r := recover(); r != nil {
			logPlayerPanic("the player's supervisor", r)
		}
		p.markReady()
		if p.run != nil {
			p.shutdown()
		}
		close(p.done)
		p.settle()
	}()
	if !p.start(ctx, w) {
		return
	}
	p.watch(ctx)
}

// fail records why the player did not start past its plan, logs it, and
// marks it ready.
func (p *bridgePlayer) fail(why string) bool {
	p.failed = why
	playerLog.Warn("player: not started", "reason", why)
	p.markReady()
	return false
}

// start makes the player's identity under the data directory's lock, builds
// the resolver, the backend and the engine, and runs the engine; it reports
// whether the player runs.
func (p *bridgePlayer) start(ctx context.Context, w servePlayerWiring) bool {
	cfg, player := w.Cfg, w.Cfg.Output.Player
	if w.Feed == nil {
		return p.fail("serve started no hotplug watcher for the player")
	}
	id, made, err := config.EnsurePlayerUUID(cfg.DataDir)
	if err != nil {
		return p.fail("the player's identity: " + err.Error())
	}
	if made {
		playerLog.Info("player: made the player's identity", "uuid", id)
	}
	var seams *library.Seams
	if p.seams != nil && p.seams.opens != nil {
		seams = &library.Seams{Opens: p.seams.opens}
	}
	res, err := library.New(library.Config{Store: w.Store, Paths: w.Paths, Seams: seams})
	if err != nil {
		return p.fail(err.Error())
	}
	newBackend := w.Backend
	if newBackend == nil {
		newBackend = func(c playback.Config) (playBackend, error) { return playback.New(c) }
	}
	be, err := newBackend(playback.Config{
		Resolver: res,
		Locator:  playback.BindingLocator{Device: player.Device, Observer: w.Trees.or(hostTrees).observer()},
		Notify:   p.note,
	})
	if err != nil {
		return p.fail("the player's backend: " + err.Error())
	}
	eng, err := engine.New(engine.Config{
		Backend:      be,
		QueuePath:    filepath.Join(cfg.DataDir, playerQueueFile),
		ReservePath:  filepath.Join(cfg.DataDir, playerReservationFile),
		DataDir:      cfg.DataDir,
		PauseRelease: player.PauseRelease(),
		DSD:          player.EffectiveDSDMode(),
	})
	if err != nil {
		return p.fail(err.Error())
	}
	if ctx.Err() != nil {
		// Serve stopped while the player started: the engine never runs, and
		// an engine that never ran holds nothing.
		p.markReady()
		return false
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	run := &playerRun{eng: eng, be: be, done: make(chan struct{}), stop: stopRun, sub: w.Feed.Subscribe()}
	p.run = run
	go func() {
		defer close(run.done)
		defer func() {
			if r := recover(); r != nil {
				logPlayerPanic("the player's engine", r)
			}
		}()
		_ = eng.Run(runCtx)
	}()
	p.eng, p.be = eng, be
	playerLog.Info("player: started", "uuid", id, "entries", eng.Snapshot().Len(), "pauseRelease", player.PauseRelease())
	p.markReady()
	return true
}

// watch reports the output to the engine as the watcher publishes it, and
// logs what the engine does, until ctx ends or the engine stops on its own,
// which only a recovered panic in it makes it do.
func (p *bridgePlayer) watch(ctx context.Context) {
	run := p.run
	rep := &hotplug.Reporter{}
	var last *engine.Snapshot
	for {
		changed := run.eng.Changed()
		s := run.eng.Snapshot()
		logPlayerChanges(last, s)
		last = s
		select {
		case <-ctx.Done():
			return
		case <-run.done:
			playerLog.Error("player: the engine stopped on its own, which only a panic in it does (logged above): the player is off until bridge serve next starts, and the device may be held until then")
			return
		case snap := <-run.sub.C():
			p.report(ctx, rep, snap)
		case <-changed:
		}
	}
}

// report hands the engine the report snap makes. A snapshot resolved from
// another binding than the one the player started with, the live config
// changed, says nothing about the player's DAC: it is reported unavailable
// (binding_changed) until the binding is put back or serve starts again,
// and a player the live config turns off is unavailable as disabled.
func (p *bridgePlayer) report(ctx context.Context, rep *hotplug.Reporter, snap hotplug.Snapshot) {
	rp, ok := rep.Report(snap)
	if !ok {
		return
	}
	foreign := snap.Resolution().State != binding.StateDisabled && !hotplug.SameBinding(snap.Binding(), p.device)
	if foreign {
		rp.Available, rp.Reason = false, reasonBindingChanged
	}
	if foreign != p.foreign {
		p.foreign = foreign
		if foreign {
			playerLog.Warn("player: output.player.device changed while serve runs: the player keeps the binding it started with until bridge serve next starts, and its output is unavailable until then, or until the binding is put back")
		} else {
			playerLog.Info("player: output.player.device names the binding the player started with again")
		}
	}
	if p.seams != nil && p.seams.reports != nil {
		p.seams.reports(rp)
	}
	if err := p.run.eng.ReportOutput(ctx, rp); err != nil {
		if ctx.Err() == nil && engine.ReasonOf(err) != engine.ReasonClosed {
			playerLog.Warn("player: the engine refused the output's report", "report", rp.Generation, "err", err)
		}
		return
	}
	attrs := []any{"report", rp.Generation}
	if rp.UnitChanged {
		attrs = append(attrs, "unitChanged", true)
	}
	if rp.Available {
		playerLog.Info("player: the output is available", attrs...)
		return
	}
	playerLog.Info("player: the output is unavailable", append(attrs, "reason", rp.Reason)...)
}

// shutdown stops the engine and waits for it, then for the backend's
// goroutines, each within its bound, and says what it left: an engine whose
// state write did not return, a device call held in the kernel, a release
// never confirmed. Then it closes the feed's subscription. It is the
// supervisor's last step, and recovers its own panic.
func (p *bridgePlayer) shutdown() {
	defer func() {
		if r := recover(); r != nil {
			logPlayerPanic("the player's shutdown", r)
		}
	}()
	run := p.run
	began := time.Now()
	run.stop()
	t := time.NewTimer(p.bounds.engine)
	select {
	case <-run.done:
		t.Stop()
	case <-t.C:
		playerLog.Warn("player: the engine did not shut down within its bound: its state storage may be stalled, a write or an fsync that does not return; it is abandoned and ends with the process, and serve keeps its data directory's lock until then",
			"bound", p.bounds.engine)
	}
	wctx, cancel := context.WithTimeout(context.Background(), p.bounds.backend)
	err := run.be.Wait(wctx)
	cancel()
	if err != nil {
		playerLog.Warn("player: the backend did not finish within its bound: a call to the device is held in the kernel, and the device may still be held until the process exits",
			"err", err)
	}
	if u, ok := run.eng.Snapshot().ReleaseUnconfirmed(); ok {
		playerLog.Warn("player: a session's release was never confirmed: the device may still be held until the process exits",
			"session", uint64(u.Session))
	}
	run.sub.Close()
	took := time.Since(began)
	p.mu.Lock()
	p.stopped = took
	p.mu.Unlock()
	playerLog.Info("player: shut down", "took", took.Round(time.Millisecond))
}

// settle lets go of the player's share of the data directory's lock and
// closes settled, once the engine's Run has returned too: at once when it
// has, and when it does for one the stop abandoned.
func (p *bridgePlayer) settle() {
	finish := func() {
		p.hold.done()
		close(p.settled)
	}
	if p.run == nil {
		finish()
		return
	}
	select {
	case <-p.run.done:
		finish()
	default:
		runDone := p.run.done
		go func() {
			<-runDone
			finish()
		}()
	}
}

// note logs what a session's sink did: the DAC opened, with what the kernel
// committed and the status file of its substream, which reads closed once
// the device is let go, and let go. It runs on the backend's goroutine and
// recovers its own panic.
func (p *bridgePlayer) note(n playback.Note) {
	defer func() {
		if r := recover(); r != nil {
			logPlayerPanic("the player's note of a sink", r)
		}
	}()
	pr := n.Params
	attrs := []any{"session", uint64(n.Session), "card", pr.Endpoint.Card, "device", pr.Endpoint.Device, "subdevice", pr.Subdevice, "status", n.StatusPath()}
	switch n.Kind {
	case playback.NoteOpened, playback.NoteReopened:
		playerLog.Info("player: the DAC is "+n.Kind.String(), append(attrs, "unit", n.Unit, "format", string(pr.Format),
			"channels", pr.Channels, "rateHz", pr.RateHz, "validBits", n.Transport.ValidBits)...)
	default:
		playerLog.Info("player: the DAC is let go ("+n.Kind.String()+"), and its status file reads closed", attrs...)
	}
}

// logPlayerChanges logs what s changed since last, at each new generation:
// the transport's state, a stop with its reason, each fault and each entry
// skipped. Nothing logs a position, which moves no generation.
func logPlayerChanges(last, s *engine.Snapshot) {
	if last != nil && last.Generation() == s.Generation() {
		return
	}
	if st := s.State(); (last == nil || last.State() != st) && st != engine.Releasing {
		playerLog.Info("player: the transport is "+st.String(), "selected", s.Selected())
	}
	if stop := s.LastStop(); stop.Reason != "" && (last == nil || last.LastStop() != stop) {
		playerLog.Info("player: playback stopped", "reason", string(stop.Reason), "detail", clipDetail(stop.Detail), "entry", stop.Entry)
	}
	var before []engine.Fault
	if last != nil {
		before = last.Faults()
	}
	for _, f := range s.Faults() {
		if !hasFault(before, f) {
			playerLog.Warn("player: a fault stopped playback", "reason", string(f.Reason), "detail", clipDetail(f.Detail),
				"entry", f.Entry, "frames", f.Frames)
		}
	}
	for id, sk := range s.Skips() {
		if last != nil {
			if was, ok := last.Skipped(id); ok && was == sk {
				continue
			}
		}
		playerLog.Info("player: an entry was skipped", "entry", id, "reason", string(sk.Reason), "detail", clipDetail(sk.Detail))
	}
}

// hasFault reports whether faults holds f.
func hasFault(faults []engine.Fault, f engine.Fault) bool {
	for _, g := range faults {
		if g.Occurrence == f.Occurrence && g.Reason == f.Reason && g.At.Equal(f.At) {
			return true
		}
	}
	return false
}

// clipDetail is d cut to playerDetailBytes, on a rune's boundary.
func clipDetail(d string) string {
	if len(d) <= playerDetailBytes {
		return d
	}
	cut := playerDetailBytes
	for cut > 0 && !utf8.RuneStart(d[cut]) {
		cut--
	}
	return d[:cut] + "…"
}

// logPlayerPanic logs a panic recovered in serve's player: where, its
// stack, and its value only when it is the runtime's own error, which
// carries nothing the player read (a value code panics with could quote a
// path).
func logPlayerPanic(where string, r any) {
	attrs := []any{"where", where, "stack", string(debug.Stack())}
	if rerr, ok := r.(runtime.Error); ok {
		attrs = append(attrs, "error", rerr.Error())
	}
	playerLog.Error("player: a panic was recovered; the library is served as before", attrs...)
}
