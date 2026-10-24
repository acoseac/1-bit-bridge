package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/flactest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
)

// playerStandIn is the backend serve's player plays through in these tests,
// since no test opens a PCM node. A session's Start "opens" the DAC, after
// opening its first occurrence through the resolver serve wired when asked
// (resolve), and is heard at once; a successor is prepared and submitted and
// never heard, so a session plays until something stops it; Release
// "closes" the DAC. holdRelease, when set, holds every Release until it is
// closed, whatever its context says: a call the kernel never returns.
type playerStandIn struct {
	resolve     bool
	holdRelease chan struct{}

	mu      sync.Mutex
	cfg     playback.Config
	built   bool
	starts  []engine.StartRequest
	read    map[string][]byte
	opened  int
	closed  int
	evs     map[engine.SessionID]engine.Events
	last    engine.SessionID
	calls   int // calls under way
	changed chan struct{}
	// releasedAll counts the ReleaseAll calls.
	releasedAll int
}

func newPlayerStandIn() *playerStandIn {
	return &playerStandIn{read: map[string][]byte{}, evs: map[engine.SessionID]engine.Events{}, changed: make(chan struct{})}
}

// build is serve's playerBackend: it keeps the Config serve wired.
func (b *playerStandIn) build(cfg playback.Config) (playBackend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg, b.built = cfg, true
	return b, nil
}

// signal wakes whoever waits on a change; the caller holds mu.
func (b *playerStandIn) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *playerStandIn) enter() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.signal()
}

func (b *playerStandIn) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls--
	b.signal()
}

// await waits until cond holds of the stand-in, read under its lock, until
// the test's deadline.
func (b *playerStandIn) await(t *testing.T, what string, cond func(b *playerStandIn) bool) {
	t.Helper()
	for {
		b.mu.Lock()
		ok, ch := cond(b), b.changed
		b.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("waiting for the stand-in backend: %s", what)
		}
	}
}

func (b *playerStandIn) Start(ctx context.Context, req engine.StartRequest) error {
	b.enter()
	defer b.leave()
	b.mu.Lock()
	b.starts = append(b.starts, req)
	res := b.cfg.Resolver
	b.mu.Unlock()
	if b.resolve {
		c, err := res.Open(ctx, req.First.Ref)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.NewSectionReader(c.Data, 0, c.Size))
		if c.Closer != nil {
			_ = c.Closer.Close()
		}
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.read[req.First.Ref.String()] = data
		b.mu.Unlock()
	}
	b.mu.Lock()
	b.opened++
	b.evs[req.Session], b.last = req.Events, req.Session
	b.signal()
	notify := b.cfg.Notify
	b.mu.Unlock()
	if notify != nil {
		notify(playback.Note{Session: req.Session, Kind: playback.NoteOpened, Unit: "card 5, the stand-in DAC",
			Params:    alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}, Format: format.S243LE, Channels: 2, RateHz: 48000},
			Transport: alsapcm.Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S243LE, ValidBits: 24}})
	}
	req.Events.Audible(req.Session, req.First.ID)
	return nil
}

func (b *playerStandIn) Prepare(_ context.Context, s engine.SessionID, _ engine.OccurrenceID, next *engine.Occurrence) error {
	if next == nil {
		return nil
	}
	b.mu.Lock()
	ev := b.evs[s]
	b.mu.Unlock()
	if ev != nil {
		ev.Prepared(s, next.ID, engine.Join{Gapless: true})
		ev.Submitted(s, next.ID)
	}
	return nil
}

func (b *playerStandIn) Cut(context.Context, engine.SessionID, engine.OccurrenceID) (bool, error) {
	return true, nil
}
func (b *playerStandIn) Pause(context.Context, engine.SessionID) error { return nil }
func (b *playerStandIn) Resume(context.Context, engine.SessionID) (engine.ResumeResult, error) {
	return engine.ResumeContinued, nil
}
func (b *playerStandIn) Discard(context.Context) error { return nil }

// Release closes the DAC a session's Start opened, once holdRelease lets
// it, its context ignored as a call held in the kernel ignores it.
func (b *playerStandIn) Release(_ context.Context, s engine.SessionID) error {
	b.enter()
	defer b.leave()
	if b.holdRelease != nil {
		<-b.holdRelease
	}
	b.mu.Lock()
	_, opened := b.evs[s]
	if opened {
		delete(b.evs, s)
		b.closed++
		b.signal()
	}
	notify := b.cfg.Notify
	b.mu.Unlock()
	if opened && notify != nil {
		notify(playback.Note{Session: s, Kind: playback.NoteClosed, Params: alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}}})
	}
	return nil
}

// ReleaseAll closes every DAC a Start opened and no Release closed, as the
// backend's releases every session it holds, and counts the call.
func (b *playerStandIn) ReleaseAll(context.Context) error {
	b.enter()
	defer b.leave()
	b.mu.Lock()
	b.releasedAll++
	var released []engine.SessionID
	for s := range b.evs {
		delete(b.evs, s)
		b.closed++
		released = append(released, s)
	}
	b.signal()
	notify := b.cfg.Notify
	b.mu.Unlock()
	for _, s := range released {
		if notify != nil {
			notify(playback.Note{Session: s, Kind: playback.NoteClosed, Params: alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}}})
		}
	}
	return nil
}

// Wait waits until no call is under way, at most until ctx ends, as the
// backend's does.
func (b *playerStandIn) Wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		n, ch := b.calls, b.changed
		b.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("the stand-in backend: %d calls still run: %w", n, ctx.Err())
		}
	}
}

// fault reports a fault on the session last started.
func (b *playerStandIn) fault(f engine.Fault) {
	b.mu.Lock()
	ev, s := b.evs[b.last], b.last
	b.mu.Unlock()
	ev.Fault(s, f)
}

// counts are the stand-in's opens, closes and starts.
func (b *playerStandIn) counts() (opened, closed, starts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opened, b.closed, len(b.starts)
}

// playerServeCase is one boot of serve with its player: the output.player
// block (indented under "  player:"), the host it stands in for, the
// backend (nil is the real one), the library's files, which are indexed
// before serve starts, and the seams.
type playerServeCase struct {
	player  string
	goos    string
	standIn *playerStandIn
	files   map[string][]byte
	// before runs once the directories exist and before serve starts.
	before func(dataDir string)
	seams  *servePlayerSeams
	bounds servePlayerBounds
	// with changes the serve options last.
	with func(*serveOpts)
	// top is config written after the output block: top-level keys of the
	// case's own.
	top string
}

// playerServe is a serve a test booted with its player.
type playerServe struct {
	servedBridge
	music, dataDir, configPath string
	player                     *bridgePlayer
	holder                     *config.RuntimeConfig
	hotplug                    *hotplug.Service
	hotplugDone                <-chan struct{}
	standIn                    *playerStandIn
}

// goBarTrees is the Raspberry Pi 5 with the iFi GO bar as card 5, which a
// binding of goBarPlayer resolves to.
func goBarTrees(t *testing.T) outputTrees {
	t.Helper()
	tree := sysfstest.Build(t, filepath.Join("..", "..", "internal", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	return outputTrees{proc: filepath.Join(tree, "proc"), sysfs: filepath.Join(tree, "sys")}
}

// needsDirectorySync skips a serve test of the player on Windows, before
// it does anything: the engine keeps its IDs durable with a directory
// fsync, which Windows has not, and refuses to start there, and sysfstest
// cannot lay out the GO bar's tree the watcher reads (its names hold
// colons).
func needsDirectorySync(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the player's engine keeps its IDs durable with a directory fsync, which Windows has not, and the GO bar's sysfs tree cannot be laid out there")
	}
}

// bootPlayerServe boots serve on a config whose output block is the case's
// player block, standing in for Linux unless the case names another host,
// with the watcher reading the GO bar's box and hearing no kernel, and
// returns once serve listens, its player decided.
func bootPlayerServe(t *testing.T, c playerServeCase) *playerServe {
	t.Helper()
	root := t.TempDir()
	music, dataDir := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, d := range []string{music, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, data := range c.files {
		p := filepath.Join(music, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		seedTrack(t, dataDir, rel, len(data))
	}
	if c.before != nil {
		c.before(dataDir)
	}
	return bootPlayerServeAt(t, c, root, music, dataDir)
}

// bootPlayerServeAt is bootPlayerServe on directories laid out already: a
// second serve of one data directory.
func bootPlayerServeAt(t *testing.T, c playerServeCase, root, music, dataDir string) *playerServe {
	t.Helper()
	lan, admin := holdLoopback(t), holdLoopback(t)
	configPath := filepath.Join(root, "bridge.yaml")
	text := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir + "\nadminAddress: " + admin.addr +
		"\noutput:\n  mpd: off\n  player:\n" + c.player + c.top
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	goos := c.goos
	if goos == "" {
		goos = "linux"
	}
	ps := &playerServe{music: music, dataDir: dataDir, configPath: configPath, standIn: c.standIn}
	var mu sync.Mutex
	opts := serveOpts{
		configPath:    configPath,
		addrOverride:  lan.addr,
		lanListener:   lan.ln,
		adminListener: admin.ln,
		playerGOOS:    goos,
		hotplugSource: func(context.Context) hotplug.Source { return hotplug.PollingSource(hearsNothing) },
		hotplugBoot: func(svc *hotplug.Service, holder *config.RuntimeConfig, done <-chan struct{}) {
			mu.Lock()
			defer mu.Unlock()
			ps.hotplug, ps.holder, ps.hotplugDone = svc, holder, done
		},
		playerTrees: goBarTrees(t),
		playerBoot: func(p *bridgePlayer) {
			mu.Lock()
			defer mu.Unlock()
			ps.player = p
		},
		playerSeams:  c.seams,
		playerBounds: c.bounds,
	}
	if c.standIn != nil {
		opts.playerBackend = c.standIn.build
	}
	if c.with != nil {
		c.with(&opts)
	}
	ps.servedBridge = launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
	mu.Lock()
	defer mu.Unlock()
	if ps.player == nil {
		t.Fatalf("serve listens and has not decided about its player; stderr=%s", ps.stderr.String())
	}
	return ps
}

// engine waits for the player to run or fail, and returns its engine, nil
// for a player that did not start.
func (ps *playerServe) engine(t *testing.T) *engine.Engine {
	t.Helper()
	select {
	case <-ps.player.ready:
	case <-serveGiveUp(t):
		t.Fatalf("the player neither ran nor failed; stderr=%s\nserve's goroutines:\n%s", ps.stderr.String(), serveStacks())
	}
	return ps.player.eng
}

// runningEngine is engine, failing the test when the player did not start.
func (ps *playerServe) runningEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng := ps.engine(t)
	if eng == nil {
		t.Fatalf("the player did not start: plan %+v, failed %q; stderr=%s", ps.player.plan, ps.player.failed, ps.stderr.String())
	}
	return eng
}

// awaitEngine waits until cond holds of the engine's snapshot, until the
// test's deadline, and returns that snapshot.
func awaitEngine(t *testing.T, eng *engine.Engine, what string, cond func(*engine.Snapshot) bool) *engine.Snapshot {
	t.Helper()
	for {
		ch := eng.Changed()
		s := eng.Snapshot()
		if cond(s) {
			return s
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("waiting for the player's engine: %s; it is %s, the output %+v, the last stop %+v", what, s.State(), s.Output(), s.LastStop())
		}
	}
}

// availableOutput waits until the engine has the output available.
func availableOutput(t *testing.T, eng *engine.Engine) {
	t.Helper()
	awaitEngine(t, eng, "the output available", func(s *engine.Snapshot) bool { return s.Output().Known && s.Output().Available })
}

// queue inserts ref at the head of the engine's queue and returns its ID,
// as a controller's insert will (5a).
func queue(t *testing.T, eng *engine.Engine, after uint32, ref engine.Reference) uint32 {
	t.Helper()
	id, err := eng.InsertAfter(context.Background(), after, ref, nil)
	if err != nil {
		t.Fatalf("insert %s: %v", ref, err)
	}
	return id
}

// someReference is a reference for an entry a stand-in that resolves
// nothing plays.
func someReference(t *testing.T, s string) engine.Reference {
	t.Helper()
	ref, err := engine.NewReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// stopServe stops serve and returns its exit code.
func (ps *playerServe) stopServe(t *testing.T) int {
	t.Helper()
	ps.stop()
	select {
	case code := <-ps.done:
		return code
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after a stop; stderr=%s\nserve's goroutines:\n%s", ps.stderr.String(), serveStacks())
		return -1
	}
}

// livePlayer changes output.player in the live config while serve runs,
// through the holder its watcher and its player read, as the console's DAC
// page will (6a).
func (ps *playerServe) livePlayer(t *testing.T, change func(*config.PlayerConfig)) {
	t.Helper()
	if err := ps.holder.Update(ps.configPath, func(c *config.Config) error {
		change(&c.Output.Player)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// goBarByModel is the GO bar bound by its model: another binding than
// goBarPlayer's, which the watcher resolves to the same DAC.
var goBarByModel = config.PlayerDevice{Binding: config.BindingUniqueModel, VendorID: "20b1", ProductID: "3009"}

// playerFiles are the player's files the data directory holds.
func playerFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	var found []string
	for _, name := range []string{config.PlayerUUIDFile, playerQueueFile, playerReservationFile} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

// closedNow reports whether ch is closed.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// health is GET /v1/health's status from a serve: the library still served.
func (ps *playerServe) health(t *testing.T) int {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	resp, err := client.Get("https://" + ps.addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// logWatch is the default logger's handler while a test waits for a line
// that a goroutine of the player's writes on its own time, after the
// engine's snapshot the test saw (the supervisor logs each change once it
// wakes): it keeps every record, rendered as loggingtest's Recorder renders
// one, and wakes a waiter at each. Installed through loggingtest.SetDefault.
type logWatch struct {
	mu      sync.Mutex
	lines   []string
	changed chan struct{}
}

func watchLog(t *testing.T) *logWatch {
	t.Helper()
	w := &logWatch{changed: make(chan struct{})}
	loggingtest.SetDefault(t, slog.New(w))
	return w
}

func (w *logWatch) Enabled(context.Context, slog.Level) bool { return true }

func (w *logWatch) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, b.String())
	close(w.changed)
	w.changed = make(chan struct{})
	return nil
}

func (w *logWatch) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *logWatch) WithGroup(string) slog.Handler      { return w }

// await waits until a line logged holds every one of says, until the test's
// deadline, and returns it.
func (w *logWatch) await(t *testing.T, says ...string) string {
	t.Helper()
	for {
		w.mu.Lock()
		lines, ch := slices.Clone(w.lines), w.changed
		w.mu.Unlock()
		for _, l := range lines {
			if !slices.ContainsFunc(says, func(s string) bool { return !strings.Contains(l, s) }) {
				return l
			}
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("no line logged saying %q; the log:\n%s", says, strings.Join(lines, "\n"))
		}
	}
}

// all is every line logged so far.
func (w *logWatch) all() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.lines, "\n")
}

// TestServeStartsThePlayerOnlyWhenEveryGateHolds pins serve's decision at
// boot, from the boot snapshot. The player starts only on Linux, with the
// player enabled in a block that reads, a DAC bound, and a pause release
// the engine takes (1 s to 10 min, of the config's hour); each refusal
// leaves it off with its reason, and adds nothing: no goroutine of the
// player's, no backend built, no player file. A started player hands every
// session the boot snapshot's pause release and DSD mode unchanged: the
// default 30 s, and 600, the engine's last. A player turned on in the live
// config while serve runs starts at the next start, never in this one.
func TestServeStartsThePlayerOnlyWhenEveryGateHolds(t *testing.T) {
	needsDirectorySync(t)
	release := func(n int) string {
		return goBarPlayer + fmt.Sprintf("    pauseReleaseSeconds: %d\n", n)
	}
	for _, tc := range []struct {
		name, player, goos string
		// off is what the reason says, "" for a player simply off; start is
		// a player that runs, with its sessions' pause release.
		off     string
		start   bool
		release time.Duration
	}{
		{name: "the player off", player: "    enabled: false\n"},
		{name: "no DAC bound", player: "    enabled: true\n", off: "bridge output dac bind"},
		{name: "a pause release of 601 s", player: release(601), off: "pauseReleaseSeconds"},
		{name: "a pause release of an hour", player: release(3600), off: "pauseReleaseSeconds"},
		{name: "a block that cannot run", player: goBarPlayer + "    dsdMode: native\n", off: "dsdMode"},
		{name: "off Linux", player: goBarPlayer, goos: "darwin", off: "Linux only"},
		{name: "an OpenHome port serve binds already", player: goBarPlayer + "    openhome: true\n    openhomePort: 7790\n", off: "openhomePort"},
		{name: "the default pause release", player: goBarPlayer, start: true, release: 30 * time.Second},
		{name: "a pause release of 600 s", player: release(600), start: true, release: 600 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standIn := newPlayerStandIn()
			ps := bootPlayerServe(t, playerServeCase{player: tc.player, goos: tc.goos, standIn: standIn})
			p := ps.player
			if !tc.start {
				if p.plan.start || p.cancel != nil || !closedNow(p.settled) {
					t.Fatalf("the player was started: %+v", p.plan)
				}
				if tc.off == "" && p.plan.off != "" || !strings.Contains(p.plan.off, tc.off) {
					t.Errorf("the reason %q, want one saying %q", p.plan.off, tc.off)
				}
				if tc.name == "the player off" {
					// Turned on while serve runs: the watcher starts a watch, as
					// it did before the player, and the player waits for the
					// next start.
					if err := ps.holder.Update(ps.configPath, func(c *config.Config) error {
						c.Output.Player.Enabled = true
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					waitHotplug(t, ps.hotplug, "the watch the live config started", func(st hotplug.Status) bool { return st.Running })
				}
				if code := ps.stopServe(t); code != 0 {
					t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
				}
				if standIn.built {
					t.Error("a backend was built for a player that did not start")
				}
				if files := playerFiles(t, ps.dataDir); len(files) > 0 {
					t.Errorf("the player made %v, and did not start", files)
				}
				return
			}
			eng := ps.runningEngine(t)
			availableOutput(t, eng)
			queue(t, eng, 0, someReference(t, "0123456789abcdef"))
			if err := eng.Play(context.Background()); err != nil {
				t.Fatalf("play: %v", err)
			}
			standIn.await(t, "a session started", func(b *playerStandIn) bool { return len(b.starts) == 1 })
			standIn.mu.Lock()
			opts := standIn.starts[0].Options
			standIn.mu.Unlock()
			if opts.PauseRelease != tc.release || opts.DSD != "off" {
				t.Errorf("the session's options %+v, want a pause release of %v and DSD off", opts, tc.release)
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
			}
			if files := playerFiles(t, ps.dataDir); len(files) != 3 {
				t.Errorf("the data directory holds %v of the player's files, want its identity, its queue and its reservation", files)
			}
		})
	}
}

// TestServeStartsNoPlayerWithoutTheDataDirLock: a serve that could not lock
// its data directory for a reason other than another serve holding it (a
// directory where the lock file goes) serves all the same, and its player
// stays off with that reason, before its identity or any of the engine's
// state is made: they are made only under that lock.
func TestServeStartsNoPlayerWithoutTheDataDirLock(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn, before: func(dataDir string) {
		if err := os.MkdirAll(filepath.Join(dataDir, serveLockFileName), 0o755); err != nil {
			t.Fatal(err)
		}
	}})
	if !strings.Contains(ps.stderr.String(), "could not lock") {
		t.Fatalf("serve did not say it could not lock its data directory; stderr=%s", ps.stderr.String())
	}
	if p := ps.player; p.plan.start || !strings.Contains(p.plan.off, "lock") {
		t.Fatalf("the player's plan %+v, want it off for the lock", p.plan)
	}
	if lines := rec.Lines("player: not started"); len(lines) != 1 || !strings.Contains(lines[0], "lock") {
		t.Errorf("the refusal logged %q, want one line naming the lock", lines)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	if files := playerFiles(t, ps.dataDir); len(files) > 0 || standIn.built {
		t.Errorf("the player made %v, or built a backend (%v), without the lock", files, standIn.built)
	}
}

// TestAPlayerWhoseEngineRefusesItsStorageLeavesNoIdentityItMade: the
// player's identity is made under serve's lock before the engine is built,
// and the engine is what refuses storage it cannot keep IDs on (here a
// regular file where the reservation's directory goes, which New's probe
// refuses as it refuses a directory that cannot be synced). A refused
// player starts nothing: the identity this start made is removed before the
// refusal is reported, and never announced; one the data directory held
// already (bind's, a restore's) is left as it was.
func TestAPlayerWhoseEngineRefusesItsStorageLeavesNoIdentityItMade(t *testing.T) {
	needsDirectorySync(t)
	const before = "7d444840-9dc0-11d1-b245-5ffdce74fad2"
	for _, tc := range []struct {
		name string
		had  bool
	}{{name: "made by this start"}, {name: "there before", had: true}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
				before: func(dataDir string) {
					if err := os.WriteFile(filepath.Join(dataDir, "not-a-directory"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					if tc.had {
						if err := config.WritePlayerUUIDFile(filepath.Join(dataDir, config.PlayerUUIDFile), before); err != nil {
							t.Fatal(err)
						}
					}
				},
				seams: &servePlayerSeams{engineConfig: func(c *engine.Config) {
					c.ReservePath = filepath.Join(c.DataDir, "not-a-directory", playerReservationFile)
				}}})
			if eng := ps.engine(t); eng != nil || ps.player.failed == "" {
				t.Fatalf("the player started (%v) on storage its engine refuses, or did not say why (%q)", eng != nil, ps.player.failed)
			}
			id, err := config.ReadPlayerUUID(ps.dataDir)
			switch {
			case tc.had && (err != nil || id != before):
				t.Errorf("the identity the data directory held is %q (%v), want it kept as it was", id, err)
			case !tc.had && !errors.Is(err, os.ErrNotExist):
				t.Errorf("the identity this start made is still there (%q, %v) after its engine refused the storage", id, err)
			}
			if lines := rec.Lines("player: made the player's identity"); len(lines) > 0 {
				t.Errorf("serve announced an identity its refused player did not keep: %q", lines)
			}
			if lines := rec.Lines("player: not started"); len(lines) != 1 {
				t.Errorf("the refusal logged %q, want once", lines)
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
			}
		})
	}
}

// TestServesPlayerResolvesATrackFromTheLibraryAtEveryOpen: an entry is the
// DLNA id of an indexed original (library.Reference), and serve's player
// opens it through the index and the filesystem, the file's own bytes; once
// the file is gone, the next open refuses it, missing_file, skipped and
// logged, and the player stops with nothing playable.
func TestServesPlayerResolvesATrackFromTheLibraryAtEveryOpen(t *testing.T) {
	needsDirectorySync(t)
	logs := watchLog(t)
	track := flactest.Stream(44100, 2, 16, 4096, 7)
	standIn := newPlayerStandIn()
	standIn.resolve = true
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn, files: map[string][]byte{"Artist/a.flac": track}})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	ref := library.Reference("Artist/a.flac")
	id := queue(t, eng, 0, ref)
	if err := eng.Play(context.Background()); err != nil {
		t.Fatalf("play: %v", err)
	}
	standIn.mu.Lock()
	got := standIn.read[ref.String()]
	standIn.mu.Unlock()
	if string(got) != string(track) {
		t.Fatalf("the session read %d bytes, want the file's %d", len(got), len(track))
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ps.music, "Artist", "a.flac")); err != nil {
		t.Fatal(err)
	}
	_ = eng.Play(context.Background())
	s := awaitEngine(t, eng, "the entry skipped", func(s *engine.Snapshot) bool { _, ok := s.Skipped(id); return ok && s.State() == engine.Stopped })
	if sk, _ := s.Skipped(id); sk.Reason != engine.ReasonMissing {
		t.Errorf("the entry skipped %+v, want missing_file", sk)
	}
	logs.await(t, "player: an entry was skipped", "reason=missing_file")
	if all := logs.all(); strings.Contains(all, ps.music) {
		t.Errorf("a line names the library's path:\n%s", all)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestServesPlayerRefusesWhatItCannotPlayBeforeTheDevice drives serve's own
// backend (playback.New), resolver and engine, as production wires them: a
// library file that is no audio is refused by its content before any of it
// plays (unknown_container), and a row whose file is gone by the resolver
// (missing_file). Neither touches the DAC: this binary fails on any open of
// a PCM node.
func TestServesPlayerRefusesWhatItCannotPlayBeforeTheDevice(t *testing.T) {
	needsDirectorySync(t)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, files: map[string][]byte{
		"a.flac": []byte("not audio at all, whatever its name says"), "b.flac": []byte("gone"),
	}})
	if err := os.Remove(filepath.Join(ps.music, "b.flac")); err != nil {
		t.Fatal(err)
	}
	eng := ps.runningEngine(t)
	if _, ok := ps.player.be.(*playback.Backend); !ok {
		t.Fatalf("serve's player plays through %T, want the player's backend", ps.player.be)
	}
	availableOutput(t, eng)
	a := queue(t, eng, 0, library.Reference("a.flac"))
	b := queue(t, eng, a, library.Reference("b.flac"))
	_ = eng.Play(context.Background())
	s := awaitEngine(t, eng, "both entries refused", func(s *engine.Snapshot) bool {
		return s.State() == engine.Stopped && s.LastStop().Reason == engine.ReasonAllUnplayable
	})
	for id, want := range map[uint32]engine.Reason{a: "unknown_container", b: engine.ReasonMissing} {
		if sk, ok := s.Skipped(id); !ok || sk.Reason != want {
			t.Errorf("entry %d skipped %+v (%v), want %s", id, sk, ok, want)
		}
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestALiveDisableReportsThePlayersOutputUnavailable: the player keeps the
// boot snapshot, and the hotplug watcher follows the live config; the two
// agree because a player the live config turns off has its output reported
// unavailable (disabled): playback stops, the DAC is let go, and Play is
// refused. Turned on again, the output comes back and plays nothing until
// asked.
func TestALiveDisableReportsThePlayersOutputUnavailable(t *testing.T) {
	needsDirectorySync(t)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := func(on bool) {
		t.Helper()
		if err := ps.holder.Update(ps.configPath, func(c *config.Config) error {
			c.Output.Player.Enabled = on
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(false)
	awaitEngine(t, eng, "the output unavailable, disabled, and playback stopped", func(s *engine.Snapshot) bool {
		return s.Output().Known && !s.Output().Available && s.Output().Reason == "disabled" && s.State() == engine.Stopped
	})
	standIn.await(t, "the DAC let go", func(b *playerStandIn) bool { return b.closed == 1 })
	if err := eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonOutputUnavailable {
		t.Errorf("play with the player off in the live config: %v, want output_unavailable", err)
	}
	set(true)
	s := awaitEngine(t, eng, "the output available again", func(s *engine.Snapshot) bool { return s.Output().Available })
	if _, _, starts := standIn.counts(); s.State() != engine.Stopped || starts != 1 {
		t.Errorf("the output's return: %s with %d starts, want stopped with the first start alone", s.State(), starts)
	}
	if err := eng.Play(context.Background()); err != nil {
		t.Errorf("play once the player is on again: %v", err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestALiveBindingChangeReportsThePlayersOutputUnavailable: a binding
// changed in the live config, here to the same GO bar by its model, which
// the watcher resolves available, is not the binding the player started
// with: the output is reported unavailable (binding_changed), playback
// stops and Play is refused, rather than play on a DAC the live config no
// longer names. The binding put back brings the output back, playing
// nothing until asked.
func TestALiveBindingChangeReportsThePlayersOutputUnavailable(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	boot := ps.holder.Load().Output.Player.Device
	bind := func(d config.PlayerDevice) {
		t.Helper()
		if err := ps.holder.Update(ps.configPath, func(c *config.Config) error {
			c.Output.Player.Device = d
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	bind(config.PlayerDevice{Binding: config.BindingUniqueModel, VendorID: "20b1", ProductID: "3009"})
	awaitEngine(t, eng, "the output unavailable, binding_changed, and playback stopped", func(s *engine.Snapshot) bool {
		return s.Output().Known && !s.Output().Available && s.Output().Reason == reasonBindingChanged && s.State() == engine.Stopped
	})
	if err := eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonOutputUnavailable {
		t.Errorf("play on another binding: %v, want output_unavailable", err)
	}
	if lines := rec.Lines("player: output.player.device changed while serve runs: the player keeps the binding it started with until bridge serve next starts, and its output is unavailable until then, or until the binding is put back"); len(lines) != 1 {
		t.Errorf("the change logged %d times, want once", len(lines))
	}
	bind(boot)
	s := awaitEngine(t, eng, "the output available again", func(s *engine.Snapshot) bool { return s.Output().Available })
	if _, _, starts := standIn.counts(); s.State() != engine.Stopped || starts != 1 {
		t.Errorf("the binding put back: %s with %d starts, want stopped with the first start alone", s.State(), starts)
	}
	if err := eng.Play(context.Background()); err != nil {
		t.Errorf("play on the boot binding again: %v", err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestTheLiveConfigReachesThePlayersOutputWithoutAReadingOfTheBox: the
// watcher follows the live config only as fast as it reads the box, and a
// reading can stall (a USB device's string attributes take its lock, which
// a disconnect can hold) or fail, so serve's player reads the live config
// itself. With every reading of the box after the boot one held, never
// returning whatever its context says: a binding changed while the GO bar
// plays is reported at once, binding_changed, playback stops and Play is
// refused; a live disable is reported at once, disabled, while the
// watcher's stop still waits on its held reading; the player on again,
// still on the other binding, is binding_changed; and the boot binding put
// back gives the output back from the watcher's last snapshot of it, which
// plays nothing until asked. The watcher published nothing after its first
// snapshot.
func TestTheLiveConfigReachesThePlayersOutputWithoutAReadingOfTheBox(t *testing.T) {
	needsDirectorySync(t)
	var held atomic.Bool
	release, reading := make(chan struct{}), make(chan struct{}, 1)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn, with: func(o *serveOpts) {
		o.hotplugObserve = func(next func(context.Context) (binding.Inventory, error)) func(context.Context) (binding.Inventory, error) {
			return func(ctx context.Context) (binding.Inventory, error) {
				if held.Load() {
					select {
					case reading <- struct{}{}:
					default:
					}
					<-release
				}
				return next(ctx)
			}
		}
		// The watcher's stop waits for its held reading for as long as the
		// test does: what a live disable reports here, the watcher never
		// published.
		timing := hotplug.DefaultTiming()
		timing.StopWait = time.Hour
		o.hotplugTiming = timing
	}})
	var once sync.Once
	letGo := func() {
		once.Do(func() {
			held.Store(false)
			close(release)
		})
	}
	t.Cleanup(letGo)
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishes := ps.hotplug.Status().Stats.Publishes
	boot := ps.holder.Load().Output.Player.Device
	held.Store(true)
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Device = goBarByModel })
	select {
	case <-reading:
	case <-serveGiveUp(t):
		t.Fatal("the watcher took no reading of the changed binding")
	}
	awaitEngine(t, eng, "binding_changed and playback stopped, the watcher's reading held", func(s *engine.Snapshot) bool {
		return s.Output().Known && !s.Output().Available && s.Output().Reason == reasonBindingChanged && s.State() == engine.Stopped
	})
	if err := eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonOutputUnavailable {
		t.Errorf("play on another binding: %v, want output_unavailable", err)
	}
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = false })
	awaitEngine(t, eng, "disabled, the watcher's stop held", func(s *engine.Snapshot) bool { return s.Output().Reason == "disabled" })
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = true })
	awaitEngine(t, eng, "binding_changed, the player on again on the other binding", func(s *engine.Snapshot) bool {
		return s.Output().Reason == reasonBindingChanged
	})
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Device = boot })
	s := awaitEngine(t, eng, "the output back from the watcher's snapshot of the boot binding", func(s *engine.Snapshot) bool { return s.Output().Available })
	if _, _, starts := standIn.counts(); s.State() != engine.Stopped || starts != 1 {
		t.Errorf("the boot binding put back: %s with %d starts, want stopped with the first start alone", s.State(), starts)
	}
	if got := ps.hotplug.Status().Stats.Publishes; got != publishes {
		t.Errorf("the watcher published %d snapshots while its readings were held: the reports were theirs, not the live config's", got-publishes)
	}
	letGo()
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestALiveBindingChangeIsLoggedOnceEachWayWhateverThePlayersState: the
// line that says output.player.device changed while serve runs follows the
// binding alone. The binding changed (logged), then the player turned off
// and on again in the live config, still on the other binding, logs nothing
// more, since the config never named the boot binding again; and the
// output says binding_changed, then disabled, then binding_changed: while
// the player is off it is disabled, whatever it would bind.
func TestALiveBindingChangeIsLoggedOnceEachWayWhateverThePlayersState(t *testing.T) {
	needsDirectorySync(t)
	logs := watchLog(t)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	unavailable := func(what, why string) {
		t.Helper()
		awaitEngine(t, eng, what, func(s *engine.Snapshot) bool {
			o := s.Output()
			return o.Known && !o.Available && o.Reason == why
		})
	}
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Device = goBarByModel })
	unavailable("another binding", reasonBindingChanged)
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = false })
	unavailable("the player off, on the other binding", "disabled")
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = true })
	unavailable("the player on again, on the other binding", reasonBindingChanged)
	all := logs.all()
	changed := strings.Count(all, "player: output.player.device changed while serve runs")
	again := strings.Count(all, "player: output.player.device names the binding the player started with again")
	if changed != 1 || again != 0 {
		t.Fatalf("the binding's change logged %d times and its return %d, want once and never: the config never named the boot binding again\n%s", changed, again, all)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// requireStoppedCleanly is what a stop and a restart leave: the engine shut
// down, the session's release confirmed and the DAC closed, every goroutine
// the player started returned and its share of the lock let go, the
// player's stop within its bounds, and the watcher stopped.
func requireStoppedCleanly(t *testing.T, ps *playerServe, eng *engine.Engine) {
	t.Helper()
	if !closedNow(eng.Done()) || !closedNow(ps.player.settled) || !closedNow(ps.hotplugDone) {
		t.Fatalf("serve returned with the player's engine running (%v), its goroutines (%v) or its watcher (%v)",
			!closedNow(eng.Done()), !closedNow(ps.player.settled), !closedNow(ps.hotplugDone))
	}
	s := eng.Snapshot()
	if u, ok := s.ReleaseUnconfirmed(); ok {
		t.Errorf("the release of session %d was not confirmed", u.Session)
	}
	if opened, closed, _ := ps.standIn.counts(); opened != 1 || closed != 1 {
		t.Errorf("the DAC opened %d times and closed %d, want once each", opened, closed)
	}
	if s.LastStop().Reason != engine.ReasonStopped {
		t.Errorf("the last stop %+v, want the shutdown's", s.LastStop())
	}
	ps.player.mu.Lock()
	took := ps.player.stopped
	ps.player.mu.Unlock()
	if took <= 0 || took > ps.player.bounds.engine+ps.player.bounds.backend {
		t.Errorf("the player's stop took %v, past its bounds (%v and %v)", took, ps.player.bounds.engine, ps.player.bounds.backend)
	}
}

// TestServeStopReleasesThePlayersDACWithinItsBounds: serve's stop (SIGINT,
// SIGTERM) ends the engine and joins it, then the backend, before the
// watcher and the store close, and exits 0 with the session's release
// confirmed and the DAC closed. The journal says the player started, the
// output is available, the DAC opened with the kernel's status file of its
// substream and was let go, and how long the stop took.
func TestServeStopReleasesThePlayersDACWithinItsBounds(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	requireStoppedCleanly(t, ps, eng)
	for msg, says := range map[string]string{
		"player: started":                 "uuid=",
		"player: the output is available": "report=1",
		"player: the DAC is opened":       "status=/proc/asound/card5/pcm0p/sub0/status",
		"player: the DAC is let go (closed), and its status file reads closed": "status=/proc/asound/card5/pcm0p/sub0/status",
		"player: shut down": "took=",
	} {
		if lines := rec.Lines(msg); len(lines) != 1 || !strings.Contains(lines[0], says) {
			t.Errorf("%q logged %q, want once saying %q", msg, lines, says)
		}
	}
	// The teardown's order: the player shut down, its DAC let go, before
	// the watcher its reports came from stopped.
	order := map[string]int{}
	for i, l := range rec.All() {
		for _, msg := range []string{"player: the DAC is let go (closed)", "player: shut down", "player hotplug: stopped watching"} {
			if _, seen := order[msg]; !seen && strings.Contains(l, msg) {
				order[msg] = i
			}
		}
	}
	if !(order["player: the DAC is let go (closed)"] < order["player: shut down"] && order["player: shut down"] < order["player hotplug: stopped watching"]) {
		t.Errorf("the teardown's lines came in the order %v: the player's stop must come before the watcher's", order)
	}
}

// TestServeRestartReleasesThePlayersDACWithinItsBounds: a restart request
// (the console's Restart) stops the player the way a stop does, and serve
// exits to be restarted.
func TestServeRestartReleasesThePlayersDACWithinItsBounds(t *testing.T) {
	needsDirectorySync(t)
	standIn := newPlayerStandIn()
	var mu sync.Mutex
	ps := &playerServe{standIn: standIn}
	b := startConsoleBridge(t, "output:\n  mpd: off\n  player:\n"+goBarPlayer, nil, func(o *serveOpts) {
		o.playerGOOS = "linux"
		o.hotplugSource = func(context.Context) hotplug.Source { return hotplug.PollingSource(hearsNothing) }
		o.playerTrees = goBarTrees(t)
		o.playerBackend = standIn.build
		o.playerBoot = func(p *bridgePlayer) {
			mu.Lock()
			defer mu.Unlock()
			ps.player = p
		}
		o.hotplugBoot = func(_ *hotplug.Service, _ *config.RuntimeConfig, done <-chan struct{}) {
			mu.Lock()
			defer mu.Unlock()
			ps.hotplugDone = done
		}
	})
	mu.Lock()
	ps.servedBridge = b.servedBridge
	mu.Unlock()
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := b.console.Post(b.adminBase+"/api/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case code := <-b.done:
		if code != supervision.RestartExitCode {
			t.Fatalf("exit %d, want %d; stderr=%s", code, supervision.RestartExitCode, b.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after a restart request; stderr=%s", b.stderr.String())
	}
	requireStoppedCleanly(t, ps, eng)
}

// TestServeStopWithAPlayerCallHeldInTheKernel: a backend call the kernel
// never returns (the session's release, held) does not hold serve: the
// engine records the release unconfirmed and abandons the call, the
// backend's wait gives up at its bound, serve says the DAC may still be
// held, and returns within the player's bounds, exit 0. Once the call is
// let go every goroutine of the stand-in's ends and the DAC is closed.
func TestServeStopWithAPlayerCallHeldInTheKernel(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	standIn.holdRelease = make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(standIn.holdRelease) }) }
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn})
	t.Cleanup(letGo)
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	if _, ok := eng.Snapshot().ReleaseUnconfirmed(); !ok {
		t.Error("the held release is not recorded unconfirmed")
	}
	ps.player.mu.Lock()
	took := ps.player.stopped
	ps.player.mu.Unlock()
	if bound := ps.player.bounds.engine + ps.player.bounds.backend + time.Second; took <= 0 || took > bound {
		t.Errorf("the player's stop took %v, past %v", took, bound)
	}
	if lines := rec.Failures("player: the backend did not finish within its bound: a call to the device is held in the kernel, and the device may still be held until the process exits",
		"player: a session's release was never confirmed: the device may still be held until the process exits"); len(lines) != 2 {
		t.Errorf("serve said %q, want both lines saying the device may still be held", lines)
	}
	letGo()
	wctx, cancel := context.WithDeadline(context.Background(), serveGiveUpTime(t))
	defer cancel()
	if err := standIn.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if opened, closed, _ := standIn.counts(); opened != 1 || closed != 1 {
		t.Errorf("the DAC opened %d times and closed %d once let go, want once each", opened, closed)
	}
}

// TestAPanicInServesPlayerStopsThePlayerAndKeepsTheLibrary: a panic in a
// goroutine serve's player owns, here its supervisor as it reports the
// output, is recovered and logged with its stack and never its value, the
// player stops and joins its engine, and the library is served as before;
// serve exits 0.
func TestAPanicInServesPlayerStopsThePlayerAndKeepsTheLibrary(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	secret := "a value a panic may carry: /srv/music/Secret/Track.flac"
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		seams: &servePlayerSeams{reports: func(engine.OutputReport) { panic(secret) }}})
	eng := ps.runningEngine(t)
	select {
	case <-ps.player.done:
	case <-serveGiveUp(t):
		t.Fatalf("the player did not stop after its panic; stderr=%s\nserve's goroutines:\n%s", ps.stderr.String(), serveStacks())
	}
	if !closedNow(eng.Done()) {
		t.Error("the player stopped with its engine running")
	}
	lines := rec.Lines("player: a panic was recovered; the library is served as before")
	if len(lines) != 1 || !strings.Contains(lines[0], "stack=") || !strings.Contains(lines[0], "supervisor") {
		t.Errorf("the panic logged %q, want once, with where and the stack", lines)
	}
	if all := strings.Join(rec.All(), "\n"); strings.Contains(all, secret) {
		t.Errorf("the log carries the panic's value")
	}
	if code := ps.health(t); code != http.StatusOK {
		t.Errorf("GET /v1/health after the player's panic: %d", code)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestAPanicOnTheEnginesGoroutineKeepsItsShareOfTheLock: a panic on the
// goroutine that runs the engine's Run is recovered, but Run's shutdown,
// which waits for the engine's reservation writes and its queue writer,
// never runs, so what the engine started may still write in the data
// directory: the player abandons the engine, says so, and keeps its share
// of the data directory's lock until the process exits. Once serve has
// stopped, exit 0, a second lock of the data directory is refused. The
// panic comes as the first output report is on its way to the engine, whose
// owner then answers nothing: the call ends with Run's goroutine, and the
// player stops without waiting for serve's stop. The player says so as it
// settles, before done closes, so the log holds the line once done has.
func TestAPanicOnTheEnginesGoroutineKeepsItsShareOfTheLock(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	var once sync.Once
	reported := make(chan struct{})
	reports := func() { once.Do(func() { close(reported) }) }
	t.Cleanup(reports)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
		seams: &servePlayerSeams{
			reports: func(engine.OutputReport) { reports() },
			runs: func() {
				<-reported
				panic("on the engine's goroutine")
			},
		}})
	select {
	case <-ps.player.done:
	case <-serveGiveUp(t):
		t.Fatalf("the player did not stop after its engine's panic; stderr=%s\nserve's goroutines:\n%s", ps.stderr.String(), serveStacks())
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	release, err := lockServeDataDir(ps.dataDir)
	release()
	var held *serveDataDirHeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second lock of the data directory once serve stopped, its player's engine abandoned by a panic: %v, want it refused", err)
	}
	if closedNow(ps.player.settled) {
		t.Error("the player settled, its engine's writers never joined")
	}
	if lines := rec.Lines("player: a panic was recovered; the library is served as before"); len(lines) != 1 || !strings.Contains(lines[0], "the player's engine") {
		t.Errorf("the panic logged %q, want once, on the engine's goroutine", lines)
	}
	if lines := rec.Failures("player: the engine's Run ended in a panic, before its own shutdown, which waits for its state writes: they may still run, so the player keeps its share of the data directory's lock until the process exits"); len(lines) != 1 {
		t.Errorf("the abandonment logged %q, want once", lines)
	}
}

// nopEvents is the engine's side of a session a test starts on the stand-in
// itself: the session an engine left when its Run ended in a panic.
type nopEvents struct{}

func (nopEvents) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join)      {}
func (nopEvents) Refused(engine.SessionID, engine.OccurrenceID, engine.Unplayable) {}
func (nopEvents) Submitted(engine.SessionID, engine.OccurrenceID)                  {}
func (nopEvents) Audible(engine.SessionID, engine.OccurrenceID)                    {}
func (nopEvents) Position(engine.SessionID, engine.Position)                       {}
func (nopEvents) Fault(engine.SessionID, engine.Fault)                             {}
func (nopEvents) Finished(engine.SessionID)                                        {}
func (nopEvents) DeviceReleased(engine.SessionID)                                  {}
func (nopEvents) Released(engine.SessionID)                                        {}

// TestAPanicOnTheEnginesGoroutineLetsGoOfTheDAC: a panic on the goroutine
// that runs the engine's Run leaves the session the backend plays holding
// the DAC, with no owner to release it: the player has the backend let go
// of every session it holds, within its bound, and says so, so the DAC is
// free rather than held until the process exits, while the player keeps its
// share of the data directory's lock, as for any panic in Run.
func TestAPanicOnTheEnginesGoroutineLetsGoOfTheDAC(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	// The session the engine started and its Run never released.
	req := engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: someReference(t, "0123456789abcdef")}, Events: nopEvents{}}
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		seams: &servePlayerSeams{runs: func() {
			if err := standIn.Start(context.Background(), req); err != nil {
				panic(err)
			}
			panic("on the engine's goroutine")
		}}})
	select {
	case <-ps.player.done:
	case <-serveGiveUp(t):
		t.Fatalf("the player did not stop after its engine's panic; stderr=%s\nserve's goroutines:\n%s", ps.stderr.String(), serveStacks())
	}
	standIn.mu.Lock()
	releasedAll := standIn.releasedAll
	standIn.mu.Unlock()
	if opened, closed, _ := standIn.counts(); opened != 1 || closed != 1 || releasedAll != 1 {
		t.Errorf("the DAC opened %d times and closed %d, ReleaseAll asked %d times; want the session the engine left let go, once", opened, closed, releasedAll)
	}
	if lines := rec.Lines("player: the engine's Run ended in a panic, so the backend let go of every session it held: the device is free"); len(lines) != 1 {
		t.Errorf("the release logged %q, want once", lines)
	}
	if closedNow(ps.player.settled) {
		t.Error("the player settled, its engine's writers never joined")
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestAPanicInThePlayersResolverRefusesItsEntry: a panic in the resolver
// serve wires, a callback the backend calls on a goroutine of its own, is
// the entry's refusal (resolver_failed), and the player plays on.
func TestAPanicInThePlayersResolverRefusesItsEntry(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	standIn := newPlayerStandIn()
	standIn.resolve = true
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		files: map[string][]byte{"a.flac": flactest.Stream(44100, 2, 16, 4096, 7)},
		seams: &servePlayerSeams{opens: func(engine.Reference) { panic("in the resolver") }}})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	id := queue(t, eng, 0, library.Reference("a.flac"))
	_ = eng.Play(context.Background())
	s := awaitEngine(t, eng, "the entry refused", func(s *engine.Snapshot) bool { _, ok := s.Skipped(id); return ok && s.State() == engine.Stopped })
	if sk, _ := s.Skipped(id); sk.Reason != library.ReasonResolverFailed {
		t.Errorf("skipped %+v, want resolver_failed", sk)
	}
	if closedNow(ps.player.done) {
		t.Error("the resolver's panic stopped the player")
	}
	if lines := rec.Lines("player: the library resolver panicked; the entry is refused"); len(lines) != 1 {
		t.Errorf("the panic logged %d times, want once", len(lines))
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestAPlayerFaultStaysInsideThePlayer: a fault after content played (the
// DAC gone) stops playback and is logged, with no serial of the bound DAC,
// while the library is served and serve exits 0.
func TestAPlayerFaultStaysInsideThePlayer(t *testing.T) {
	needsDirectorySync(t)
	logs := watchLog(t)
	standIn := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	standIn.fault(engine.Fault{Reason: engine.ReasonDisconnected, Detail: "the device is gone (ENODEV)", Frames: 4096})
	awaitEngine(t, eng, "the fault recorded and playback stopped", func(s *engine.Snapshot) bool {
		return len(s.Faults()) == 1 && s.State() == engine.Stopped
	})
	standIn.await(t, "the DAC let go", func(b *playerStandIn) bool { return b.closed == 1 })
	logs.await(t, "WARN player: a fault stopped playback", "reason=disconnected")
	if n := strings.Count(logs.all(), "player: a fault stopped playback"); n != 1 {
		t.Errorf("the fault logged %d times, want once", n)
	}
	if all := logs.all(); strings.Contains(all, "GOBAR-7Q2X-91KZ-4471") {
		t.Errorf("a line carries the DAC's serial")
	}
	if code := ps.health(t); code != http.StatusOK {
		t.Errorf("GET /v1/health after the fault: %d", code)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestServeRestoresThePlayersQueueStopped: the queue a serve leaves is the
// next serve's, stopped, and the output's first report plays nothing.
func TestServeRestoresThePlayersQueueStopped(t *testing.T) {
	needsDirectorySync(t)
	first := newPlayerStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: first})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	a := queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	queue(t, eng, a, someReference(t, "fedcba9876543210"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	second := newPlayerStandIn()
	again := bootPlayerServeAt(t, playerServeCase{player: goBarPlayer, standIn: second}, filepath.Dir(ps.dataDir), ps.music, ps.dataDir)
	eng = again.runningEngine(t)
	s := awaitEngine(t, eng, "the output available", func(s *engine.Snapshot) bool { return s.Output().Available })
	if s.Len() != 2 || s.State() != engine.Stopped {
		t.Errorf("the next serve's queue: %d entries, %s; want both, stopped", s.Len(), s.State())
	}
	if _, _, starts := second.counts(); starts != 0 {
		t.Errorf("the next serve started %d sessions on its own", starts)
	}
	if code := again.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, again.stderr.String())
	}
}

// TestOnlyServeAndTheHarnessBuildThePlayer: bridge doctor and the
// console's doctor stay passive. In this package's own code the engine,
// the backend and the library's resolver are built in serve's player
// (serve_player.go) and bridge output play's harness (output_play.go)
// alone, and runServe starts its player once.
func TestOnlyServeAndTheHarnessBuildThePlayer(t *testing.T) {
	allowed := map[string]map[string]bool{
		"engine.New":   {"serve_player.go": true, "output_play.go": true},
		"playback.New": {"serve_player.go": true, "output_play.go": true},
		"library.New":  {"serve_player.go": true},
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	starts := 0
	for _, name := range nonTestGoFilesInPackage(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isIdent(call.Fun, "startServePlayer") {
				starts++
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			key := pkg.Name + "." + sel.Sel.Name
			if files, ok := allowed[key]; ok {
				found[key]++
				if !files[filepath.Base(name)] {
					t.Errorf("%s calls %s: the player is built in serve's player and the harness alone", fset.Position(call.Pos()), key)
				}
			}
			return true
		})
	}
	for key := range allowed {
		if found[key] == 0 {
			t.Errorf("no call of %s found: the sweep reads nothing", key)
		}
	}
	if starts != 1 {
		t.Errorf("startServePlayer is called %d times, want once, in runServe", starts)
	}
}

// TestServeStopsThePlayerBeforeItsWatcherAndItsStore: runServe defers the
// player's stop after the watcher's stop and the store's close, so it runs
// before both: the engine and the backend are joined before anything they
// read closes.
func TestServeStopsThePlayerBeforeItsWatcherAndItsStore(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	at := map[string]token.Pos{}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		switch {
		case func() bool {
			lit, ok := d.Call.Fun.(*ast.FuncLit)
			if !ok {
				return false
			}
			found := false
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && isSelector(c.Fun, "player", "stop") {
					found = true
				}
				return true
			})
			return found
		}():
			at["player"] = d.Pos()
		case isSelector(d.Call.Fun, "player", "stop"):
			at["player"] = d.Pos()
		case isIdent(d.Call.Fun, "stopPlayerHotplug"):
			at["watcher"] = d.Pos()
		case isSelector(d.Call.Fun, "manifestStore", "Close"):
			at["store"] = d.Pos()
		case isSelector(d.Call.Fun, "dataDirLock", "done"):
			at["lock"] = d.Pos()
		}
		return true
	})
	for _, k := range []string{"player", "watcher", "store", "lock"} {
		if at[k] == token.NoPos {
			t.Fatalf("runServe defers no %s stop", k)
		}
	}
	if !(at["lock"] < at["store"] && at["store"] < at["watcher"] && at["watcher"] < at["player"]) {
		t.Errorf("runServe's defers, in the order they run: the player's stop must come first, then the watcher's, then the store's close, then the lock's release; registered at %v",
			map[string]string{"player": fset.Position(at["player"]).String(), "watcher": fset.Position(at["watcher"]).String(),
				"store": fset.Position(at["store"]).String(), "lock": fset.Position(at["lock"]).String()})
	}
}

// TestBridgeDoctorMakesNoPlayerFile: bridge doctor on a config whose
// player is enabled and bound builds no player and writes none of its
// files: it is passive.
func TestBridgeDoctorMakesNoPlayerFile(t *testing.T) {
	root := t.TempDir()
	music, dataDir := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, d := range []string{music, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "bridge.yaml")
	text := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir + "\nadminAddress: 127.0.0.1:0\nlistenAddress: 127.0.0.1:0\noutput:\n  mpd: off\n  player:\n" + goBarPlayer
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCapture(t, "doctor", "--config", configPath)
	if code == 2 || !strings.Contains(stdout, filepath.Base(configPath)) {
		t.Fatalf("bridge doctor did not run over the config: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if files := playerFiles(t, dataDir); len(files) > 0 {
		t.Errorf("bridge doctor made %v", files)
	}
}
