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
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The player's files in the data directory, beside its identity
// (config.PlayerUUIDFile): the engine's queue file and its ID and token
// reservation (engine.Config.QueuePath, ReservePath). bridge output play
// keeps its own elsewhere, and never here.
const (
	playerQueueFile       = "player-queue.json"
	playerReservationFile = engine.ReservationFileName
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
	// openhome is a player that starts its OpenHome surface with it
	// (output.player.openhome, B470 PR 5a2a); openhomeOff is why the opt-in
	// is on and nothing of it starts, for a player that is simply off,
	// which boot says once.
	openhome    bool
	openhomeOff string
}

// planServePlayer decides whether serve starts its player: only when every
// gate holds, each refusal a reason. The player runs on this config
// (config.PlayerEnabled: enabled, a block that reads and is valid, Linux,
// not public); a DAC is bound (output.player.device), which PlayerEnabled
// does not ask; serve holds its data directory's lock, under which alone
// the player's identity and IDs are made (lockHeld); and the pause release
// is one the engine takes, 1 s to 10 min, of the config's 1 s to an hour.
// A player that starts starts its OpenHome surface too when
// output.player.openhome is on (B470 PR 5a2a); with the opt-in on and the
// player simply off, nothing of the surface starts, and boot says so once.
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
	case !on && player.OpenHome:
		return servePlayerPlan{openhomeOff: "output.player.openhome is on and the player is off (output.player.enabled is not true): " +
			"nothing of the OpenHome surface starts, no listener and no goroutine"}
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
	return servePlayerPlan{start: true, openhome: player.OpenHome}
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
	// Live reads the live config's player block as the watcher does
	// (livePlayerSettings), and Changed is the holder's channel closed at
	// its next change: the player follows them itself, so the engine hears
	// at once that the live config turned it off or names another DAC,
	// whatever the watcher's readings of the box do. Nil Live is a config
	// that never changes.
	Live    func() hotplug.Settings
	Changed func() <-chan struct{}
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
	// OpenHome is a boot test's hold on the player's OpenHome surface; the
	// zero value is production's.
	OpenHome openhomeWiring
	// SSDP is the one subsystem shared with the MediaServer, even with DLNA off.
	SSDP *dlna.SSDPSubsystem
}

// servePlayerSeams are where a boot test holds serve's player between two
// of its steps, or makes a step panic. Each runs on the goroutine that
// reaches it.
type servePlayerSeams struct {
	// reports runs before each output report reaches the engine.
	reports func(engine.OutputReport)
	// opens runs as the resolver opens a reference (library.Seams.Opens).
	opens func(engine.Reference)
	// runs runs on the goroutine that runs the engine, before its Run: a
	// panic there is one inside Run, which never reaches Run's shutdown.
	runs func()
	// engineConfig changes the engine's config before engine.New: a boot
	// test's way to have New refuse the player's storage.
	engineConfig func(*engine.Config)
}

// servePlayerBounds are the stop's bounds: how long it waits for the
// engine's Run to return, and then for the backend's goroutines; and what
// the OpenHome surface's own stop takes at most, which comes first.
type servePlayerBounds struct {
	engine, backend, surface, discovery time.Duration
}

// orProduction is b, or production's bounds where b sets none.
func (b servePlayerBounds) orProduction() servePlayerBounds {
	if b.engine <= 0 {
		b.engine = playerEngineStopBound
	}
	if b.backend <= 0 {
		b.backend = playerBackendWaitBound
	}
	if b.surface <= 0 {
		b.surface = openhome.DefaultLimits.StopBound()
	}
	if b.discovery <= 0 {
		b.discovery = openhome.DiscoveryStopBound
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
	// started runs, its share of the data directory's lock let go: never,
	// for an engine whose Run a panic ended (settle).
	ready, done, settled chan struct{}
	readyOnce            sync.Once

	// Set before ready closes: the engine and the backend that run, or why
	// the player did not start past its plan.
	eng    *engine.Engine
	be     playBackend
	failed string

	// The supervisor's alone: the engine as it runs, the live config it
	// follows, what it tells the engine of the output, and the OpenHome
	// surface, when the opt-in starts it.
	run     *playerRun
	live    func() hotplug.Settings
	changed func() <-chan struct{}
	out     playerOutput
	surface *openhome.Surface

	mu            sync.Mutex
	closing       bool
	discoveryStop context.Context
	stopped       time.Duration // how long its stop took, once it has
}

// playerRun is the engine as it runs: its Run, ended by stop and over once
// done is closed, and the feed it is reported from.
type playerRun struct {
	eng  *engine.Engine
	be   playBackend
	done chan struct{}
	stop context.CancelFunc
	sub  *hotplug.Subscription
	// calls is what the supervisor's calls to the engine run on: it ends with
	// serve's stop and with the goroutine that runs the engine, so an engine
	// whose Run a panic ended, whose owner answers nothing, never holds the
	// supervisor in a call.
	calls context.Context
	// panicked is a Run a panic ended, set before done closes.
	panicked bool
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
		if plan.openhomeOff != "" {
			playerLog.Warn("player: the OpenHome surface does not start", "reason", plan.openhomeOff)
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
	p.live, p.changed = w.Live, w.Changed
	if p.live == nil {
		boot := hotplug.Settings{Enabled: true, Device: p.device}
		p.live = func() hotplug.Settings { return boot }
	}
	if p.changed == nil {
		p.changed = func() <-chan struct{} { return nil }
	}
	p.out = playerOutput{rep: &hotplug.Reporter{}, boot: p.device}
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
// supervisor, which stops the OpenHome surface first, then the engine and
// joins it, and then the backend, each within its bound, before serve's
// watcher and store close (the caller's defer order). A player whose start or whose state storage is
// stalled past the bounds is abandoned, said so, and keeps its share of the
// data directory's lock until it ends; one whose engine's Run a panic ended
// keeps it until the process exits (settle).
func (p *bridgePlayer) stop() {
	if p.cancel == nil {
		return
	}
	p.refuseOpenHome(context.Background())
	p.cancel()
	bound := p.bounds.discovery + p.bounds.surface + p.bounds.engine + p.bounds.backend + time.Second
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
		// done closes last, so a caller that sees it sees what settle
		// did: the lock's share let go, or why it is kept.
		p.settle()
		close(p.done)
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
// whether the player runs. A player refused once its identity is made
// leaves none this start made: the engine is what refuses storage it cannot
// keep IDs on (an overlay that does not outlive a reboot, a directory that
// cannot be synced), where an identity made now would outlive the refusal.
func (p *bridgePlayer) start(ctx context.Context, w servePlayerWiring) bool {
	cfg, player := w.Cfg, w.Cfg.Output.Player
	if w.Feed == nil {
		return p.fail("serve started no hotplug watcher for the player")
	}
	id, made, err := config.EnsurePlayerUUID(cfg.DataDir)
	if err != nil {
		return p.fail("the player's identity: " + err.Error())
	}
	refuse := func(why string) bool {
		if made {
			if err := config.ForgetPlayerUUID(cfg.DataDir, id); err != nil {
				playerLog.Warn("player: the identity this start made, for a player that does not start, could not be removed", "err", err)
			}
		}
		return p.fail(why)
	}
	var seams *library.Seams
	if p.seams != nil && p.seams.opens != nil {
		seams = &library.Seams{Opens: p.seams.opens}
	}
	res, err := library.New(library.Config{Store: w.Store, Paths: w.Paths, Seams: seams})
	if err != nil {
		return refuse(err.Error())
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
		return refuse("the player's backend: " + err.Error())
	}
	ecfg := engine.Config{
		PlayerUUID:   id,
		Backend:      be,
		QueuePath:    filepath.Join(cfg.DataDir, playerQueueFile),
		ReservePath:  filepath.Join(cfg.DataDir, playerReservationFile),
		DataDir:      cfg.DataDir,
		PauseRelease: player.PauseRelease(),
		DSD:          player.EffectiveDSDMode(),
	}
	if p.seams != nil && p.seams.engineConfig != nil {
		p.seams.engineConfig(&ecfg)
	}
	eng, err := engine.New(ecfg)
	if err != nil {
		return refuse(err.Error())
	}
	if made {
		playerLog.Info("player: made the player's identity", "uuid", id)
	}
	if ctx.Err() != nil {
		// Serve stopped while the player started: the engine never runs, and
		// an engine that never ran holds nothing.
		p.markReady()
		return false
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	calls, endCalls := context.WithCancel(ctx)
	run := &playerRun{eng: eng, be: be, done: make(chan struct{}), stop: stopRun, sub: w.Feed.Subscribe(), calls: calls}
	p.run = run
	go func() {
		defer close(run.done)
		defer endCalls()
		defer func() {
			if r := recover(); r != nil {
				run.panicked = true
				logPlayerPanic("the player's engine", r)
			}
		}()
		if p.seams != nil && p.seams.runs != nil {
			p.seams.runs()
		}
		_ = eng.Run(runCtx)
	}()
	p.eng, p.be = eng, be
	playerLog.Info("player: started", "uuid", id, "entries", eng.Snapshot().Len(), "pauseRelease", player.PauseRelease())
	if p.plan.openhome {
		p.startOpenHome(id, w)
	}
	p.markReady()
	return true
}

// watch reports the output to the engine as the watcher publishes it and as
// the live config changes, and logs what the engine does, until ctx ends or
// the engine stops on its own, which only a recovered panic in it makes it
// do.
func (p *bridgePlayer) watch(ctx context.Context) {
	run := p.run
	// The holder's channel is taken before the config is read, so a change
	// in between still wakes the loop.
	cfgChanged := p.changed()
	p.follow()
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
			playerLog.Error("player: the engine stopped on its own, which only a panic in it does (logged above): the player is off until bridge serve next starts, and the backend is asked to let go of the device; the release outcome follows")
			return
		case snap := <-run.sub.C():
			p.report(snap)
		case <-cfgChanged:
			cfgChanged = p.changed()
			p.follow()
		case <-changed:
		}
	}
}

// follow reads the live config and tells the engine what it calls for, at
// once, whatever the watcher has read of the box (playerOutput).
func (p *bridgePlayer) follow() {
	p.out.readLive(p.live())
	if rp, ok := p.out.fromConfig(); ok {
		p.send(rp)
	}
}

// report hands the engine the report snap makes, held to the live config,
// read again first.
func (p *bridgePlayer) report(snap hotplug.Snapshot) {
	p.out.readLive(p.live())
	if rp, ok := p.out.fromSnapshot(snap); ok {
		p.send(rp)
	}
}

// playerOutput is what serve's player tells its engine of the output. The
// engine keeps the boot snapshot of output.player; the watcher follows the
// live config, but only as fast as it reads the box, and a reading can stall
// on a USB device's lock or fail, so the player reads the live config
// itself, at each of its changes. While the live config turns the player off
// (disabled), or names another binding than the one the player started with
// (binding_changed), the output is reported unavailable, at once and
// whatever the watcher found; so is a snapshot of another binding. A player
// that is off is disabled, whatever it would bind. The output comes back
// only from a snapshot of the boot binding: the latest the watcher
// published, once the live config names that binding again and runs the
// player, or the next one. Every report is numbered from the Reporter's one
// count (Report for a snapshot, Withhold and Restate for the live config),
// so the engine's input is monotonic whichever source made a report, and no
// generation carries two. It is the supervisor's alone.
type playerOutput struct {
	rep  *hotplug.Reporter
	boot config.PlayerDevice
	// live is the live config's verdict as last read: the player turned off,
	// and another binding named, which follows the binding alone, whatever
	// the player's state.
	live struct{ off, foreign bool }
	// snap is the last snapshot reported: of the player turned off, or of
	// another binding, and the Reporter's own report of it.
	snap struct {
		ok, off, foreign bool
		own              engine.OutputReport
	}
	// sent is the last report the engine took, if told.
	sent engine.OutputReport
	told bool
}

// readLive takes the live config's verdict, set, and logs a change of the
// binding, once each way: it follows the binding alone, so a player turned
// off and on again on the other binding logs nothing more.
func (o *playerOutput) readLive(set hotplug.Settings) {
	o.live.off = !set.Enabled
	foreign := !hotplug.SameBinding(set.Device, o.boot)
	if foreign == o.live.foreign {
		return
	}
	o.live.foreign = foreign
	if foreign {
		playerLog.Warn("player: output.player.device changed while serve runs: the player keeps the binding it started with until bridge serve next starts, and its output is unavailable until then, or until the binding is put back")
		return
	}
	playerLog.Info("player: output.player.device names the binding the player started with again")
}

// withheld is why the output is unavailable whatever the last snapshot says
// of it, "" when nothing withholds it.
func (o *playerOutput) withheld() string {
	switch {
	case o.live.off:
		return string(binding.StateDisabled)
	case o.live.foreign, o.snap.foreign && !o.snap.off:
		return reasonBindingChanged
	}
	return ""
}

// fromSnapshot is the report snap makes, held to the live config; false
// for a generation reported already.
func (o *playerOutput) fromSnapshot(snap hotplug.Snapshot) (engine.OutputReport, bool) {
	rp, ok := o.rep.Report(snap)
	if !ok {
		return rp, false
	}
	o.snap.ok, o.snap.own = true, rp
	o.snap.off = snap.Resolution().State == binding.StateDisabled
	o.snap.foreign = !hotplug.SameBinding(snap.Binding(), o.boot)
	if why := o.withheld(); why != "" {
		rp.Available, rp.Reason = false, why
	}
	return rp, true
}

// fromConfig is the report the live config calls for now: the output
// withheld, or the last snapshot's own report restated; false when the
// engine has it already, or when no snapshot has been reported yet (the
// engine holds the output unknown until then, which refuses Play).
func (o *playerOutput) fromConfig() (engine.OutputReport, bool) {
	if !o.snap.ok {
		return engine.OutputReport{}, false
	}
	if why := o.withheld(); why != "" {
		if o.told && !o.sent.Available && o.sent.Reason == why {
			return engine.OutputReport{}, false
		}
		return o.rep.Withhold(why)
	}
	if o.told && o.sent.Available == o.snap.own.Available && o.sent.Reason == o.snap.own.Reason {
		return engine.OutputReport{}, false
	}
	return o.rep.Restate()
}

// send hands the engine rp and logs what it says of the output. The call
// ends with serve's stop and with the engine's Run (playerRun.calls).
func (p *bridgePlayer) send(rp engine.OutputReport) {
	if p.seams != nil && p.seams.reports != nil {
		p.seams.reports(rp)
	}
	calls := p.run.calls
	if err := p.run.eng.ReportOutput(calls, rp); err != nil {
		if calls.Err() == nil && engine.ReasonOf(err) != engine.ReasonClosed {
			playerLog.Warn("player: the engine refused the output's report", "report", rp.Generation, "err", err)
		}
		return
	}
	p.out.sent, p.out.told = rp, true
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

// shutdown stops the OpenHome surface, when the opt-in started it, so no
// request reaches the engine as it stops (it refuses, shuts down within its
// grace, and closes what is open), then stops the engine and waits for it,
// then for the backend's goroutines, each within its bound, and says what it
// left: an engine whose state write did not return, a device call held in
// the kernel, a release never confirmed. Then it closes the feed's
// subscription. It is the supervisor's last step, and recovers its own
// panic.
// shutdown stops the engine and waits for it, then for the backend's
// goroutines, each within its bound, and says what it left: an engine whose
// state write did not return, a device call held in the kernel, a release
// never confirmed. An engine whose Run a panic ended never ran its own
// shutdown, which releases the session it plays, so the backend lets go of
// every session it holds first, within the backend's bound (releaseAll).
// Then it closes the feed's subscription. It is the supervisor's last step,
// and recovers its own panic.
func (p *bridgePlayer) shutdown() {
	defer func() {
		if r := recover(); r != nil {
			logPlayerPanic("the player's shutdown", r)
		}
	}()
	run := p.run
	began := time.Now()
	// The surface first, so no request reaches the engine as it stops.
	p.stopOpenHome()
	run.stop()
	t := time.NewTimer(p.bounds.engine)
	ended := false
	select {
	case <-run.done:
		t.Stop()
		ended = true
	case <-t.C:
		playerLog.Warn("player: the engine did not shut down within its bound: its state storage may be stalled, a write or an fsync that does not return; it is abandoned and ends with the process, and serve keeps its data directory's lock until then",
			"bound", p.bounds.engine)
	}
	wctx, cancel := context.WithTimeout(context.Background(), p.bounds.backend)
	if ended && run.panicked {
		releaseAll(wctx, run.be)
	}
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

// releaseAll has the backend let go of every session it holds, for an
// engine whose Run a panic ended: the engine's owner, the backend's only
// caller, is gone, and the session it played would hold the DAC until the
// process exits. It waits at most until ctx ends, and says how it went.
func releaseAll(ctx context.Context, be playBackend) {
	if err := be.ReleaseAll(ctx); err != nil {
		playerLog.Warn("player: the engine's Run ended in a panic, and the backend did not let go of every session it held within its bound: the device may still be held until the process exits",
			"err", err)
		return
	}
	playerLog.Info("player: the engine's Run ended in a panic, so the backend let go of every session it held: the device is free")
}

// settle lets go of the player's share of the data directory's lock and
// closes settled, once the engine's Run has returned too: at once when it
// has, and when it does for one the stop abandoned. A Run a panic ended is
// abandoned for good: the panic skipped Run's own shutdown, which waits for
// the engine's reservation writes and its queue writer, so they may still
// write in the data directory, and the player keeps its share of the lock
// until the process exits, saying so; settled never closes.
func (p *bridgePlayer) settle() {
	finish := func() {
		if p.run != nil && p.run.panicked {
			playerLog.Error("player: the engine's Run ended in a panic, before its own shutdown, which waits for its state writes: they may still run, so the player keeps its share of the data directory's lock until the process exits")
			return
		}
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
