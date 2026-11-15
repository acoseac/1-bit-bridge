package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/ssdptest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// toneEntry is one step of the supervisor's, in its order: a report as it
// reaches the engine (servePlayerSeams.reports), or a call of the tone's gate,
// with the engine's output generation as the call was made.
type toneEntry struct {
	report  bool
	gen     uint64
	open    bool
	word    string
	avail   bool
	changed bool
}

func (e toneEntry) String() string {
	switch {
	case e.report && e.avail && e.changed:
		return fmt.Sprintf("report %d available, unit changed", e.gen)
	case e.report && e.avail:
		return fmt.Sprintf("report %d available", e.gen)
	case e.report:
		return fmt.Sprintf("report %d unavailable %s", e.gen, e.word)
	case e.open:
		return fmt.Sprintf("open at %d", e.gen)
	}
	return fmt.Sprintf("shut %s at %d", e.word, e.gen)
}

// toneLog is what the supervisor did to the engine and the tone's gate, in
// its order; at is how far a test has read it.
type toneLog struct {
	mu      sync.Mutex
	entries []toneEntry
	changed chan struct{}
	at      int
}

func newToneLog() *toneLog { return &toneLog{changed: make(chan struct{})} }

func (l *toneLog) add(e toneEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	close(l.changed)
	l.changed = make(chan struct{})
}

// report is the reports seam: rp as it reaches the engine.
func (l *toneLog) report(rp engine.OutputReport) {
	l.add(toneEntry{report: true, gen: rp.Generation, avail: rp.Available, word: rp.Reason, changed: rp.UnitChanged})
}

// expect requires the log's next entries, from where the test last read it,
// to be want, exactly and in order, waiting for them until the test's
// deadline; an entry the test did not expect fails here or at the next
// expect.
func (l *toneLog) expect(t *testing.T, what string, want ...string) {
	t.Helper()
	for {
		l.mu.Lock()
		n, ch := len(l.entries), l.changed
		var got []string
		for _, e := range l.entries[l.at:min(n, l.at+len(want))] {
			got = append(got, e.String())
		}
		l.mu.Unlock()
		if len(got) == len(want) {
			if !slices.Equal(got, want) {
				t.Fatalf("%s: the supervisor did %q, want %q", what, got, want)
			}
			l.at += len(want)
			return
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("%s: the supervisor did %q of %q", what, got, want)
		}
	}
}

// toneStandIn is playerStandIn with the console's test tone (toneBackend),
// standing in for the backend's: a gate the supervisor sets, each call logged
// with the engine's output generation as it is made; a Tone that holds, while
// the gate is open and no tone plays, until the gate shuts, CloseTones runs or
// its context ends, answering the word that ended it, or answers at once
// (answer); a ToneStatus the test sets (status); and Facts, an observation of
// a stream (stream, "" for none). player is the bridgePlayer serve decided,
// told by its boot hook before the supervisor starts, whose engine the
// supervisor's goroutine set before it sets the gate.
type toneStandIn struct {
	*playerStandIn
	log    *toneLog
	player *bridgePlayer
	// onClose, when set, runs at the start of each CloseTones.
	onClose func()

	tmu       sync.Mutex
	open      bool
	shut      playback.ToneOutcome
	closed    bool
	ends      chan playback.ToneOutcome
	tones     int
	admitted  int
	closes    int
	answer    *playback.ToneResult
	status    playback.ToneStatus
	stream    string
	holdStart chan struct{}
	tchanged  chan struct{}
}

func newToneStandIn(log *toneLog) *toneStandIn {
	return &toneStandIn{playerStandIn: newPlayerStandIn(), log: log, shut: playback.ToneOutputUnknown, tchanged: make(chan struct{})}
}

// build is serve's playerBackend: the stand-in, with its tone.
func (s *toneStandIn) build(cfg playback.Config) (playBackend, error) {
	if _, err := s.playerStandIn.build(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// signalT wakes whoever waits on the tone's state; the caller holds tmu.
func (s *toneStandIn) signalT() {
	close(s.tchanged)
	s.tchanged = make(chan struct{})
}

// awaitTone waits until cond holds of the stand-in's tone, read under its
// lock, until the test's deadline.
func (s *toneStandIn) awaitTone(t *testing.T, what string, cond func(s *toneStandIn) bool) {
	t.Helper()
	for {
		s.tmu.Lock()
		ok, ch := cond(s), s.tchanged
		s.tmu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("waiting for the stand-in's tone: %s", what)
		}
	}
}

// Start holds a session's Start while holdStart is set, as a DAC that takes
// its time to open, then starts it as playerStandIn does.
func (s *toneStandIn) Start(ctx context.Context, req engine.StartRequest) error {
	s.tmu.Lock()
	hold := s.holdStart
	s.tmu.Unlock()
	if hold != nil {
		<-hold
	}
	return s.playerStandIn.Start(ctx, req)
}

func (s *toneStandIn) Tone(ctx context.Context, req playback.ToneRequest) playback.ToneResult {
	s.enter()
	defer s.leave()
	s.tmu.Lock()
	s.tones++
	s.signalT()
	if s.answer != nil {
		res := *s.answer
		s.tmu.Unlock()
		return res
	}
	var why playback.ToneOutcome
	switch {
	case s.closed:
		why = playback.ToneServeStopping
	case !s.open:
		why = s.shut
	case s.ends != nil:
		why = playback.TonePlaying
	}
	if why != "" {
		s.tmu.Unlock()
		return playback.ToneResult{Outcome: why}
	}
	ends := make(chan playback.ToneOutcome, 1)
	s.ends = ends
	s.admitted++
	s.signalT()
	s.tmu.Unlock()
	select {
	case why = <-ends:
	case <-ctx.Done():
		why = playback.ToneRequestEnded
	}
	s.tmu.Lock()
	if s.ends == ends {
		s.ends = nil
	}
	s.signalT()
	s.tmu.Unlock()
	return playback.ToneResult{Outcome: why, Admitted: true}
}

// endLocked ends the tone that plays with why; the first cause stands. The
// caller holds tmu.
func (s *toneStandIn) endLocked(why playback.ToneOutcome) {
	if s.ends != nil {
		select {
		case s.ends <- why:
		default:
		}
	}
}

func (s *toneStandIn) ToneOutput(open bool, why playback.ToneOutcome) {
	gen := s.player.eng.Snapshot().Output().Generation
	s.tmu.Lock()
	if open {
		s.open = !s.closed
	} else {
		s.open, s.shut = false, why
		s.endLocked(why)
	}
	s.signalT()
	s.tmu.Unlock()
	if s.log != nil {
		s.log.add(toneEntry{gen: gen, open: open, word: string(why)})
	}
}

func (s *toneStandIn) CloseTones() {
	if s.onClose != nil {
		s.onClose()
	}
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.closed, s.open, s.shut = true, false, playback.ToneServeStopping
	s.closes++
	s.endLocked(playback.ToneServeStopping)
	s.signalT()
}

func (s *toneStandIn) ToneStatus() playback.ToneStatus {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	return s.status
}

func (s *toneStandIn) Facts() (playback.PlayerPolicy, *playback.PlaybackFacts) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.stream == "" {
		return playback.PlayerPolicy{}, nil
	}
	return playback.PlayerPolicy{}, &playback.PlaybackFacts{State: s.stream}
}

// set changes the stand-in's tone under its lock.
func (s *toneStandIn) set(change func(s *toneStandIn)) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	change(s)
	s.signalT()
}

// holdOpen opens the stand-in's gate for a test whose subject is not the
// gate (TestTheSupervisorShutsTheToneGateBeforeTheEngineHears pins the
// supervisor's opening it), once the engine has the output available: the
// supervisor opens it too, and shuts it as the output goes.
func (s *toneStandIn) holdOpen(t *testing.T, eng *engine.Engine) {
	t.Helper()
	availableOutput(t, eng)
	s.set(func(s *toneStandIn) { s.open = !s.closed })
}

// toneRig is serve's player as runServe starts it (startServePlayer) over the
// tone stand-in, the hotplug watcher reading a box the test changes (the GO
// bar's unit, card 5, on consoleBox), woken by the test, whose readings the
// test can make fail and which retries one at once; a live config the test
// changes as the console's holder would; and the console's view of it.
type toneRig struct {
	root   string
	trees  outputTrees
	cfg    *config.Config
	tone   *toneStandIn
	log    *toneLog
	p      *bridgePlayer
	view   *consolePlayer
	source *dacEventSource
	fail   atomic.Bool

	lmu  sync.Mutex
	live hotplug.Settings
	ch   chan struct{}
}

// toneRigOptions are a rig's departures: a watcher that never reads the box
// (the output stays unknown), and a backend that plays no tone.
type toneRigOptions struct {
	noReadings, noTones bool
}

func newToneRig(t *testing.T, o toneRigOptions) *toneRig {
	t.Helper()
	needsDirectorySync(t)
	persistentMedium(t)
	r := &toneRig{log: newToneLog(), ch: make(chan struct{})}
	r.root = sysfstest.BuildText(t, consoleBox+consoleUnit(1, "20b1:3009", consoleSerialA, 2, 5, "0.0"))
	r.trees = outputTrees{proc: filepath.Join(r.root, "proc"), sysfs: filepath.Join(r.root, "sys")}
	r.cfg = playerOutputConfig(t.TempDir(), nil)
	r.live = hotplug.Settings{Enabled: true, Device: r.cfg.Output.Player.Device}
	feed := hotplug.NewFeed()
	if !o.noReadings {
		sub := feed.Subscribe()
		t.Cleanup(sub.Close)
		r.source = &dacEventSource{notes: make(chan struct{}, 1), done: make(chan struct{})}
		close(r.source.done)
		timing := hotplug.DefaultTiming()
		timing.Periodic, timing.BackoffMin, timing.BackoffMax = time.Hour, 5*time.Millisecond, 5*time.Millisecond
		boot := hotplug.Settings{Enabled: true, Device: r.cfg.Output.Player.Device}
		svc := hotplug.NewService(hotplug.Config{Feed: feed, Settings: func() hotplug.Settings { return boot },
			Observe: func(ctx context.Context) (binding.Inventory, error) {
				if r.fail.Load() {
					return binding.Inventory{}, errors.New("the test made the box unreadable")
				}
				return r.trees.observer().ObserveContext(ctx)
			},
			OpenSource: func(context.Context) hotplug.Source { return r.source }, Timing: timing,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); svc.Run(ctx) }()
		drainLoopOnCleanup(t, cancel, done, "the tone rig's hotplug watcher")
		select {
		case <-sub.C():
		case <-serveGiveUp(t):
			t.Fatal("the watcher published no first snapshot")
		}
	}
	r.tone = newToneStandIn(r.log)
	backend := r.tone.build
	if o.noTones {
		backend = newPlayerStandIn().build
	}
	plan := planServePlayer(r.cfg, "linux", true)
	r.p = startServePlayer(plan, servePlayerWiring{Cfg: r.cfg, Hold: newDataDirHold(func() {}), Store: emptyLibrary{}, Paths: emptyLibrary{},
		Feed: feed, Trees: r.trees, Backend: backend, Live: r.liveSettings, Changed: r.liveChanged,
		Seams: &servePlayerSeams{reports: r.log.report}, Boot: func(p *bridgePlayer) { r.tone.player = p }})
	t.Cleanup(r.p.stop)
	awaitReady(t, r.p)
	if r.p.eng == nil {
		t.Fatalf("the player did not start: %s", r.p.failed)
	}
	r.view = newConsolePlayer(consolePlayerWiring{Plan: plan, Player: r.p, View: newBridgeOutput(plan, r.p, r.cfg), Boot: r.cfg,
		GOOS: "linux", LockHeld: true, Trees: r.trees})
	return r
}

func (r *toneRig) liveSettings() hotplug.Settings {
	r.lmu.Lock()
	defer r.lmu.Unlock()
	return r.live
}

func (r *toneRig) liveChanged() <-chan struct{} {
	r.lmu.Lock()
	defer r.lmu.Unlock()
	return r.ch
}

// setLive changes the live config's player as the console's holder would,
// waking the supervisor.
func (r *toneRig) setLive(change func(*hotplug.Settings)) {
	r.lmu.Lock()
	defer r.lmu.Unlock()
	change(&r.live)
	close(r.ch)
	r.ch = make(chan struct{})
}

// read has the watcher read the box at once.
func (r *toneRig) read() {
	r.source.lost.Store(true)
	r.source.notes <- struct{}{}
}

// liveConfig is the boot config with change made to its player: what the
// console's route loads and hands the view.
func (r *toneRig) liveConfig(change func(*config.PlayerConfig)) *config.Config {
	return playerOutputConfig(r.cfg.DataDir, change)
}

// TestTheSupervisorShutsTheToneGateBeforeTheEngineHears pins the order of
// the tone's gate against the engine's output reports (B601): each gate call
// recorded with the engine's output generation as it is made, so a shut's is
// the report before the one it precedes, and an open's the report it follows.
// The first report, then the gate opened; the player turned off and turned on
// again, another DAC chosen and chosen back (a shut withheld before each
// withholding report, an open after each restated one); an unplug (a shut
// output_unavailable before its report); the unit plugged in again and,
// available, re-enumerated (a shut output_changed before each report that
// names a unit change, an open after it); a stale republish (a shut, no
// report) and its recovery at the same generation (an open, no report); and a
// stale snapshot whose recovery is a new generation, unavailable or
// re-enumerated, which opens nothing before its report's shut.
func TestTheSupervisorShutsTheToneGateBeforeTheEngineHears(t *testing.T) {
	r := newToneRig(t, toneRigOptions{})
	l := r.log
	l.expect(t, "the first report", "report 1 available", "open at 1")
	r.setLive(func(s *hotplug.Settings) { s.Enabled = false })
	l.expect(t, "the player turned off", "shut withheld at 1", "report 2 unavailable disabled")
	r.setLive(func(s *hotplug.Settings) { s.Enabled = true })
	l.expect(t, "the player turned on again", "report 3 available", "open at 3")
	r.setLive(func(s *hotplug.Settings) { s.Device = goBarByModel })
	l.expect(t, "another DAC chosen", "shut withheld at 3", "report 4 unavailable binding_changed")
	r.setLive(func(s *hotplug.Settings) { s.Device = r.cfg.Output.Player.Device })
	l.expect(t, "the DAC it started with chosen again", "report 5 available", "open at 5")

	sysfstest.Apply(t, r.root, consoleUnplug(1))
	r.read()
	l.expect(t, "the DAC unplugged", "shut output_unavailable at 5", "report 6 unavailable device_missing")
	sysfstest.Apply(t, r.root, consoleUnit(1, "20b1:3009", consoleSerialA, 3, 5, "0.0"))
	r.read()
	l.expect(t, "the DAC plugged in again", "shut output_changed at 6", "report 7 available, unit changed", "open at 7")
	sysfstest.Apply(t, r.root, consoleUnplug(1)+consoleUnit(1, "20b1:3009", consoleSerialA, 4, 5, "0.0"))
	r.read()
	l.expect(t, "the DAC re-enumerated while available", "shut output_changed at 7", "report 8 available, unit changed", "open at 8")

	r.fail.Store(true)
	r.read()
	l.expect(t, "a stale republish", "shut snapshot_stale at 8")
	r.fail.Store(false)
	l.expect(t, "its recovery at the same generation", "open at 8")

	r.fail.Store(true)
	r.read()
	l.expect(t, "stale again", "shut snapshot_stale at 8")
	sysfstest.Apply(t, r.root, consoleUnplug(1))
	r.fail.Store(false)
	l.expect(t, "a recovery that finds the DAC gone", "shut output_unavailable at 8", "report 9 unavailable device_missing")
	sysfstest.Apply(t, r.root, consoleUnit(1, "20b1:3009", consoleSerialA, 5, 5, "0.0"))
	r.read()
	l.expect(t, "the DAC back", "shut output_changed at 9", "report 10 available, unit changed", "open at 10")

	r.fail.Store(true)
	r.read()
	l.expect(t, "stale once more", "shut snapshot_stale at 10")
	sysfstest.Apply(t, r.root, consoleUnplug(1)+consoleUnit(1, "20b1:3009", consoleSerialA, 6, 5, "0.0"))
	r.fail.Store(false)
	l.expect(t, "a recovery that finds the DAC re-enumerated", "shut output_changed at 10", "report 11 available, unit changed", "open at 11")
}

// toneResult is a console view's tone, run on a goroutine of its own: its
// answer arrives on done.
type toneResult struct {
	done chan admin.PlayerToneResult
}

// startTone asks the view for a tone of -40 dBFS for 10 seconds on a
// goroutine of its own, on ctx.
func startTone(ctx context.Context, view *consolePlayer, live *config.Config) toneResult {
	r := toneResult{done: make(chan admin.PlayerToneResult, 1)}
	go func() { r.done <- view.Tone(ctx, live, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}) }()
	return r
}

// await is the tone's answer, until the test's deadline.
func (r toneResult) await(t *testing.T, what string) admin.PlayerToneResult {
	t.Helper()
	select {
	case res := <-r.done:
		return res
	case <-serveGiveUp(t):
		t.Fatalf("%s: the tone never answered", what)
		return admin.PlayerToneResult{}
	}
}

// TestTheConsoleViewAllowsAToneInOneStateAlone pins the console's view of a
// tone (B601) against the real engine, each refusal by its word with the
// backend never asked: a tone is asked of the backend only while the running
// player is stopped with nothing pending, no release unconfirmed and no
// maintenance, its output known and available, nothing withheld on the
// config the route loaded, and no stream open; the backend's own refusals (a
// probe in the slot, a tone's close that did not answer cleanly) are passed
// on as they are.
func TestTheConsoleViewAllowsAToneInOneStateAlone(t *testing.T) {
	ctx := context.Background()
	r := newToneRig(t, toneRigOptions{})
	eng := r.p.eng
	r.tone.holdOpen(t, eng)
	on := r.cfg
	asked := func() int {
		r.tone.tmu.Lock()
		defer r.tone.tmu.Unlock()
		return r.tone.tones
	}
	// A refusal answers at once: its call is bounded, so a view that asked
	// the backend for a tone it holds answers within the bound, and fails by
	// what it answered.
	refused := func(what, want string, live *config.Config) {
		t.Helper()
		before := asked()
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		res := r.view.Tone(bounded, live, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10})
		if res.Outcome != want || res.Admitted {
			t.Errorf("%s: the view answered %+v, want %s", what, res, want)
		}
		if n := asked(); n != before {
			t.Errorf("%s: the view asked the backend for a tone", what)
		}
	}

	// Stopped, its release confirmed, its output available, nothing withheld:
	// the backend is asked, and its answer is the view's.
	r.tone.set(func(s *toneStandIn) {
		s.answer = &playback.ToneResult{Outcome: playback.TonePlayed, Admitted: true, Format: "S24_3LE", RateHz: 48000, ValidBits: 24, Frames: 480000}
	})
	if res := r.view.Tone(ctx, on, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); res.Outcome != "played" || !res.Admitted ||
		res.Format != "S24_3LE" || res.RateHz != 48000 || res.ValidBits != 24 || res.Frames != 480000 || asked() != 1 {
		t.Fatalf("a stopped player with its output available: %+v, the backend asked %d times; want the backend's tone", res, asked())
	}
	for _, word := range []playback.ToneOutcome{playback.ToneProbeRunning, playback.ToneReleaseUnconfirmed, playback.ToneSessionHolds} {
		r.tone.set(func(s *toneStandIn) { s.answer = &playback.ToneResult{Outcome: word} })
		if res := r.view.Tone(ctx, on, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); res.Outcome != string(word) || res.Admitted {
			t.Errorf("the backend's refusal %s: the view answered %+v", word, res)
		}
	}
	r.tone.set(func(s *toneStandIn) { s.answer = nil })

	// The config the route loaded withholds the output: the view refuses on
	// it, whatever the supervisor has heard.
	refused("the player turned off", "withheld", r.liveConfig(func(p *config.PlayerConfig) { p.Enabled = false }))
	refused("another DAC chosen", "withheld", r.liveConfig(func(p *config.PlayerConfig) { p.Device = goBarByModel }))

	// A stream still open in the backend's observation.
	r.tone.set(func(s *toneStandIn) { s.stream = "open" })
	refused("a stream open", "player_busy", on)
	r.tone.set(func(s *toneStandIn) { s.stream = "released" })
	r.tone.set(func(s *toneStandIn) { s.answer = &playback.ToneResult{Outcome: playback.TonePlayed, Admitted: true} })
	if res := r.view.Tone(ctx, on, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); !res.Admitted {
		t.Errorf("a released stream: %+v, want the backend's tone", res)
	}
	r.tone.set(func(s *toneStandIn) { s.answer, s.stream = nil, "" })

	// Playing, and a pause it holds.
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing && !s.Starting() })
	refused("playing", "player_sounding", on)
	if err := eng.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	refused("paused", "player_sounding", on)

	// Releasing: a Stop whose release the DAC holds.
	r.playerHoldRelease(t)
	stopped := make(chan error, 1)
	go func() { stopped <- eng.Stop(ctx) }()
	awaitEngine(t, eng, "releasing", func(s *engine.Snapshot) bool { return s.State() == engine.Releasing })
	refused("releasing", "player_busy", on)
	// Its release past ReleaseTimeout: unconfirmed until it answers.
	awaitEngine(t, eng, "the release unconfirmed", func(s *engine.Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return ok })
	refused("a release unconfirmed", "release_unconfirmed", on)
	r.playerLetRelease()
	if err := <-stopped; err != nil && engine.ReasonOf(err) != engine.ReasonReleaseUnconfirmed {
		t.Fatalf("the held Stop: %v", err)
	}
	awaitEngine(t, eng, "the release confirmed late", func(s *engine.Snapshot) bool {
		_, ok := s.ReleaseUnconfirmed()
		return !ok && s.State() == engine.Stopped
	})

	// Starting: a Start the DAC takes its time over.
	hold := make(chan struct{})
	r.tone.set(func(s *toneStandIn) { s.holdStart = hold })
	played := make(chan error, 1)
	go func() { played <- eng.Play(ctx) }()
	awaitEngine(t, eng, "starting", func(s *engine.Snapshot) bool { return s.State() == engine.Playing && s.Starting() })
	refused("starting", "player_sounding", on)
	r.tone.set(func(s *toneStandIn) { s.holdStart = nil })
	close(hold)
	if err := <-played; err != nil {
		t.Fatal(err)
	}
	if err := eng.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "stopped", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })

	// Held for maintenance.
	h, err := eng.EnterMaintenance(ctx, eng.Snapshot().Generation(), "the tone's test")
	if err != nil {
		t.Fatal(err)
	}
	refused("maintenance", "player_busy", on)
	if err := eng.ExitMaintenance(ctx, h.Lease); err != nil {
		t.Fatal(err)
	}

	// The output gone.
	sysfstest.Apply(t, r.root, consoleUnplug(1))
	r.read()
	awaitEngine(t, eng, "the output unavailable", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
	refused("the DAC unplugged", "output_unavailable", on)

	// The engine stopped.
	r.p.stop()
	refused("the engine stopped", "player_not_running", on)

	// The output not heard of yet, a backend that plays no tone, a player
	// serve did not start.
	unknown := newToneRig(t, toneRigOptions{noReadings: true})
	refused2 := func(view *consolePlayer, what, want string) {
		t.Helper()
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if res := view.Tone(bounded, on, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); res.Outcome != want || res.Admitted {
			t.Errorf("%s: the view answered %+v, want %s", what, res, want)
		}
	}
	refused2(unknown.view, "the output not heard of", "output_unknown")
	if unknown.tone.tones != 0 {
		t.Errorf("the output not heard of: the backend was asked")
	}
	noTones := newToneRig(t, toneRigOptions{noTones: true})
	refused2(noTones.view, "a backend that plays no tone", "player_not_running")
	offCfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Enabled = false })
	offPlan := planServePlayer(offCfg, "linux", true)
	off := startServePlayer(offPlan, servePlayerWiring{Cfg: offCfg, Hold: newDataDirHold(func() {}), Store: emptyLibrary{}, Paths: emptyLibrary{}, Feed: hotplug.NewFeed()})
	t.Cleanup(off.stop)
	offView := newConsolePlayer(consolePlayerWiring{Plan: offPlan, Player: off, View: newBridgeOutput(offPlan, off, offCfg), Boot: offCfg, GOOS: "linux", LockHeld: true})
	refused2(offView, "a player serve did not start", "player_not_running")
	if offView.toneView() != nil || noTones.view.toneView() != nil {
		t.Errorf("running.tone for a player with no tone to offer: want none")
	}
	if v := unknown.view.toneView(); v == nil || v.State != "" {
		// The stand-in's status is the zero one until a test sets it.
		t.Errorf("running.tone for a running player: %+v", v)
	}
}

// playerHoldRelease holds the stand-in's every Release until playerLetRelease,
// as a DAC whose close the kernel holds; set before any Release is under way.
func (r *toneRig) playerHoldRelease(t *testing.T) {
	t.Helper()
	r.tone.playerStandIn.holdRelease = make(chan struct{})
	t.Cleanup(r.playerLetRelease)
}

// playerLetRelease lets the held Releases go, once.
func (r *toneRig) playerLetRelease() {
	if h := r.tone.playerStandIn.holdRelease; h != nil {
		select {
		case <-h:
		default:
			close(h)
		}
	}
}

// TestTheConsoleJudgesTheToneOnTheConfigTheRouteLoaded pins the view's
// withholding to the config the route hands it (B601): with the supervisor
// held before the engine hears of a change, its gate still open, a config that
// turns the player off is refused withheld by the view, the backend never
// asked; the boot config, the same moment, is asked of the backend.
func TestTheConsoleJudgesTheToneOnTheConfigTheRouteLoaded(t *testing.T) {
	r := newToneRig(t, toneRigOptions{})
	r.tone.holdOpen(t, r.p.eng)
	r.tone.set(func(s *toneStandIn) { s.answer = &playback.ToneResult{Outcome: playback.TonePlayed, Admitted: true} })
	off := r.liveConfig(func(p *config.PlayerConfig) { p.Enabled = false })
	if res := r.view.Tone(context.Background(), off, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); res.Outcome != "withheld" || r.tone.tones != 0 {
		t.Fatalf("a route's config that turns the player off: %+v, the backend asked %d times; want withheld, the backend never asked", res, r.tone.tones)
	}
	if res := r.view.Tone(context.Background(), r.cfg, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); !res.Admitted || r.tone.tones != 1 {
		t.Fatalf("the boot config the same moment: %+v; want the backend's tone", res)
	}
}

// TestTheTonesLastErrorIsItsOwn pins the view's tone apart from the engine's
// last play attempt (B601): running.tone is the backend's ToneStatus, its
// lastError and outcome the last tone's, its format the tone in the slot's or
// the last tone's; running.lastError stays the engine's alone, here none; and
// a tone's answer carries its own refused open's cause.
func TestTheTonesLastErrorIsItsOwn(t *testing.T) {
	r := newToneRig(t, toneRigOptions{})
	availableOutput(t, r.p.eng)
	r.tone.set(func(s *toneStandIn) {
		s.status = playback.ToneStatus{State: playback.ToneIdle,
			Last: playback.ToneResult{Outcome: playback.ToneOpenRefused, Admitted: true, LastError: playback.ToneLastErrorBusy}}
	})
	run, _, _ := r.view.running(context.Background(), r.cfg)
	if run.LastError != "" || run.Tone == nil || run.Tone.State != "idle" || run.Tone.Outcome != "open_refused" || run.Tone.LastError != "device_busy" {
		t.Fatalf("running: lastError %q, tone %+v; want the tone's cause in the tone alone", run.LastError, run.Tone)
	}
	r.tone.set(func(s *toneStandIn) {
		s.status = playback.ToneStatus{State: playback.ToneStatePlaying, Request: playback.ToneRequest{LevelDBFS: -40, Seconds: 10},
			Format: "S24_3LE", RateHz: 48000, ValidBits: 24}
	})
	run, _, _ = r.view.running(context.Background(), r.cfg)
	if tn := run.Tone; tn == nil || tn.State != "playing" || tn.LevelDBFS != -40 || tn.Seconds != 10 || tn.Format != "S24_3LE" || tn.RateHz != 48000 ||
		tn.ValidBits != 24 || tn.Outcome != "" || tn.LastError != "" {
		t.Fatalf("a tone playing: %+v; want its request and format, the last tone's end cleared", run.Tone)
	}
	r.tone.set(func(s *toneStandIn) {
		s.answer = &playback.ToneResult{Outcome: playback.ToneOpenRefused, Admitted: true, LastError: playback.ToneLastErrorDenied}
	})
	if res := r.view.Tone(context.Background(), r.cfg, admin.PlayerToneRequest{LevelDBFS: -40, Seconds: 10}); res.LastError != "permission_denied" {
		t.Fatalf("a tone whose open was refused: %+v; want its cause", res)
	}
	if run, _, _ := r.view.running(context.Background(), r.cfg); run.LastError != "" {
		t.Errorf("running.lastError %q after a tone's refused open, want the engine's alone", run.LastError)
	}
}

// TestAnOpenHomeStopLeavesTheToneToItsEnd pins a residual as it is (B601):
// the engine's Stop, what an OpenHome controller's Stop is, changes nothing
// on a stopped player, so a tone that plays plays on to its end; a Play is
// what ends it, through the backend's Start.
func TestAnOpenHomeStopLeavesTheToneToItsEnd(t *testing.T) {
	r := newToneRig(t, toneRigOptions{})
	r.tone.holdOpen(t, r.p.eng)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tone := startTone(ctx, r.view, r.cfg)
	r.tone.awaitTone(t, "the tone admitted", func(s *toneStandIn) bool { return s.admitted == 1 })
	if err := r.p.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-tone.done:
		t.Fatalf("an OpenHome Stop ended the tone: %+v", res)
	default:
	}
	r.tone.tmu.Lock()
	playing := r.tone.ends != nil
	r.tone.tmu.Unlock()
	if !playing {
		t.Fatal("the tone is no longer in the slot after an OpenHome Stop")
	}
	cancel()
	if res := tone.await(t, "the tone's own end"); res.Outcome != "request_ended" {
		t.Errorf("the tone's own end: %+v", res)
	}
}

// toneCall posts a tone to serve's console as the page does, on a transport
// of its own, from a goroutine of its own: its status and body arrive on done.
func toneCall(addr string) <-chan [2]string {
	done := make(chan [2]string, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/output/player/tone", strings.NewReader(`{"level":-40,"seconds":10}`))
		if err != nil {
			done <- [2]string{"0", err.Error()}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			done <- [2]string{"0", err.Error()}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- [2]string{fmt.Sprint(resp.StatusCode), string(raw)}
	}()
	return done
}

// awaitCall is a toneCall's answer, until the test's deadline.
func awaitCall(t *testing.T, done <-chan [2]string, what string) (string, string) {
	t.Helper()
	select {
	case got := <-done:
		return got[0], got[1]
	case <-serveGiveUp(t):
		t.Fatalf("%s: the tone's request never answered", what)
		return "", ""
	}
}

// bootToneServe boots serve with its player over the tone stand-in.
func bootToneServe(t *testing.T) (*consoleServe, *toneStandIn) {
	t.Helper()
	needsDirectorySync(t)
	persistentMedium(t)
	tone := newToneStandIn(nil)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: tone.playerStandIn, with: func(o *serveOpts) {
		o.playerBackend = tone.build
		boot := o.playerBoot
		o.playerBoot = func(p *bridgePlayer) {
			tone.player = p
			boot(p)
		}
	}})
	tone.holdOpen(t, cs.runningEngine(t))
	return cs, tone
}

// TestAWithholdEndsTheToneAndTheWriteAnswersNotInterrupted pins a tone
// beside the console's writes, through serve's console (B601): with a tone
// held in the backend until its gate shuts, turning the player off is saved
// without the operator's go-ahead and answered not interrupted, since a tone
// never makes the player sound, and the tone's own request answers withheld;
// the same for another DAC chosen.
func TestAWithholdEndsTheToneAndTheWriteAnswersNotInterrupted(t *testing.T) {
	cs, tone := bootToneServe(t)
	call := toneCall(cs.console)
	tone.awaitTone(t, "the first tone admitted", func(s *toneStandIn) bool { return s.admitted == 1 })
	off := cs.change(t, "/api/output/player/enabled", `{"enabled":false}`, "restart")
	if off.Interrupted {
		t.Errorf("turning the player off beside a tone answered interrupted")
	}
	if code, body := awaitCall(t, call, "the player turned off"); code != "200" || !strings.Contains(body, `"outcome":"withheld"`) {
		t.Errorf("the tone beside the player turned off = %s: %s; want 200 withheld", code, body)
	}
	cs.change(t, "/api/output/player/enabled", `{"enabled":true}`, "restart")
	tone.awaitTone(t, "the gate open again", func(s *toneStandIn) bool { return s.open })
	call = toneCall(cs.console)
	tone.awaitTone(t, "the second tone admitted", func(s *toneStandIn) bool { return s.admitted == 2 })
	model := cs.change(t, "/api/output/player/dac", `{"selection":"`+cs.dacRow(t).Selection+`","binding":"uniqueModel"}`, "restart")
	if model.Interrupted {
		t.Errorf("another DAC chosen beside a tone answered interrupted")
	}
	if code, body := awaitCall(t, call, "another DAC chosen"); code != "200" || !strings.Contains(body, `"outcome":"withheld"`) {
		t.Errorf("the tone beside another DAC chosen = %s: %s; want 200 withheld", code, body)
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// TestServesStopClosesTonesFirst pins serve's stop (B601): the tones close as
// it begins, while the engine still runs and the OpenHome surface still
// answers, before it is refused and withdrawn; a tone in the slot answers
// serve_stopping, through the console that still serves it; the player says
// it shut down, and serve exits 0.
func TestServesStopClosesTonesFirst(t *testing.T) {
	t.Run("through the console", func(t *testing.T) {
		rec := loggingtest.Record(t)
		cs, tone := bootToneServe(t)
		var mu sync.Mutex
		var first *bool
		tone.onClose = func() {
			mu.Lock()
			defer mu.Unlock()
			if first == nil {
				running := !signalled(cs.player.eng.Done())
				first = &running
			}
		}
		call := toneCall(cs.console)
		tone.awaitTone(t, "the tone admitted", func(s *toneStandIn) bool { return s.admitted == 1 })
		cs.stop()
		if code, body := awaitCall(t, call, "serve's stop"); code != "200" || !strings.Contains(body, `"outcome":"serve_stopping"`) {
			t.Errorf("the tone at serve's stop = %s: %s; want 200 serve_stopping", code, body)
		}
		select {
		case code := <-cs.done:
			if code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		case <-serveGiveUp(t):
			t.Fatalf("serve did not return; stderr=%s", cs.stderr.String())
		}
		mu.Lock()
		defer mu.Unlock()
		if first == nil || !*first {
			t.Errorf("the tones closed after the engine stopped, or never")
		}
		if lines := rec.Lines("player: shut down"); len(lines) != 1 {
			t.Errorf("the player's stop said %q, want that it shut down, once", lines)
		}
	})
	t.Run("beside the OpenHome surface", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		box := lantest.NewMemory(t)
		box.Set([]lan.Address{lantest.Address("192.168.1.20/24", "eth0", 2)})
		tone := newToneStandIn(nil)
		ps, ch := bootDiscoveryServe(t, t.TempDir(), false, goBarPlayer+"    openhome: true\n    openhomePort: 0\n", "linux", box, ssdptest.New(), func(o *serveOpts) {
			o.playerBackend = tone.build
			boot := o.playerBoot
			o.playerBoot = func(p *bridgePlayer) {
				tone.player = p
				boot(p)
			}
		})
		surface := discoverySurface(t, ch)
		// What the first close saw, taken under one lock every close takes
		// first: a later close waits for it, and the teardown's close comes
		// before the surface is refused (refuseOpenHome marks the player
		// closing as it begins).
		var mu sync.Mutex
		var seen []string
		tone.onClose = func() {
			mu.Lock()
			defer mu.Unlock()
			if seen != nil {
				return
			}
			p := ps.player
			p.mu.Lock()
			closing := p.closing
			p.mu.Unlock()
			seen = []string{fmt.Sprint("engine running ", !signalled(p.eng.Done())), fmt.Sprint("surface refused ", closing),
				fmt.Sprint("surface stopped ", surface.Status().Stopped)}
		}
		// This rig's box has no card, so its output is never available and the
		// supervisor never opens the gate: once the engine has heard as much,
		// the stand-in's gate is opened here, for a tone in the slot as serve
		// stops.
		awaitEngine(t, ps.player.eng, "the output heard of", func(s *engine.Snapshot) bool { return s.Output().Known })
		tone.set(func(s *toneStandIn) { s.open = true })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := make(chan playback.ToneResult, 1)
		go func() { res <- tone.Tone(ctx, playback.ToneRequest{LevelDBFS: -40, Seconds: 10}) }()
		tone.awaitTone(t, "the tone admitted", func(s *toneStandIn) bool { return s.admitted == 1 })
		stopDiscoveryServe(t, ps)
		select {
		case got := <-res:
			if got.Outcome != playback.ToneServeStopping {
				t.Errorf("the tone at serve's stop: %+v, want serve_stopping", got)
			}
		case <-serveGiveUp(t):
			t.Fatal("the tone never answered serve's stop")
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Join(seen, ", ") != "engine running true, surface refused false, surface stopped false" {
			t.Errorf("as the tones closed: %q; want the engine running and the surface neither refused nor stopped", seen)
		}
	})
}

// toneWordsIn is the keys of the object literal const name of app.js, each
// with a sentence: a key whose value is no non-empty string fails the test.
func toneWordsIn(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	start := strings.Index(src, "\nconst "+name+" = {\n")
	if start < 0 {
		t.Fatalf("app.js declares no %s", name)
	}
	body := src[start:]
	body = body[:strings.Index(body, "\n};\n")]
	keys := map[string]bool{}
	entry := regexp.MustCompile(`(?m)^  ([a-z_]+): "([^"]*)",$`)
	for _, m := range entry.FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(m[2]) == "" {
			t.Errorf("%s words %s with nothing", name, m[1])
		}
		keys[m[1]] = true
	}
	if lines := strings.Count(body, "\n") - 1; len(keys) != lines {
		t.Errorf("%s holds %d lines and %d words in the form this test reads", name, lines, len(keys))
	}
	return keys
}

// TestEveryToneWordIsWorded holds the Player page to every word a tone is
// refused or ends with (B601): the backend's (playback.ToneWords) and the
// console's view's (consoleToneWords) each have a sentence in the page's
// PLAYER_TONE_WORDS, a refused open's cause (playback.ToneLastErrors) in
// PLAYER_TONE_LAST_ERRORS, and each state but idle (playback.ToneStates) in
// PLAYER_TONE_STATES; and no table words what is no word of the tone's.
func TestEveryToneWordIsWorded(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "admin", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	check := func(name string, want []string) {
		t.Helper()
		got := toneWordsIn(t, src, name)
		known := map[string]bool{}
		for _, w := range want {
			known[w] = true
			if !got[w] {
				t.Errorf("%s has no sentence for %s", name, w)
			}
		}
		for w := range got {
			if !known[w] {
				t.Errorf("%s words %s, which no tone answers", name, w)
			}
		}
	}
	var words []string
	for _, w := range playback.ToneWords() {
		words = append(words, string(w))
	}
	for _, w := range consoleToneWords() {
		if !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	check("PLAYER_TONE_WORDS", words)
	check("PLAYER_TONE_LAST_ERRORS", playback.ToneLastErrors())
	var states []string
	for _, s := range playback.ToneStates() {
		if s != playback.ToneIdle {
			states = append(states, string(s))
		}
	}
	check("PLAYER_TONE_STATES", states)
}
