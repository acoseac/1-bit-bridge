package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// playWatch is how long a harness test waits for an event before it calls
// it a hang.
const playWatch = 30 * time.Second

// playBuf is a run's output a test reads while the run goes on, and waits
// on.
type playBuf struct {
	mu      sync.Mutex
	b       bytes.Buffer
	changed chan struct{}
}

func newPlayBuf() *playBuf { return &playBuf{changed: make(chan struct{})} }

func (p *playBuf) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.changed)
	p.changed = make(chan struct{})
	return p.b.Write(b)
}

func (p *playBuf) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.String()
}

// waitFor waits until the output holds s, under the watchdog.
func (p *playBuf) waitFor(t *testing.T, s string) {
	t.Helper()
	timer := time.NewTimer(playWatch)
	defer timer.Stop()
	for {
		p.mu.Lock()
		ok, ch := strings.Contains(p.b.String(), s), p.changed
		p.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("waited %v for %q in:\n%s", playWatch, s, p.String())
		}
	}
}

// playStandIn is the backend the harness's tests play through: it plays
// each occurrence the moment it is handed off (hold off), or holds on the
// first (hold on), and tells the run of the sink it "opened".
type playStandIn struct {
	notify func(playback.Note)
	hold   bool
	refuse map[string]bool

	mu    sync.Mutex
	calls []string
	evs   map[engine.SessionID]engine.Events
}

func (b *playStandIn) record(c string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
}

func (b *playStandIn) callList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *playStandIn) Start(ctx context.Context, req engine.StartRequest) error {
	b.record("start")
	if b.refuse[req.First.Ref.String()] {
		return &engine.Unplayable{Reason: playback.ReasonDSD, Detail: "the stand-in refuses " + req.First.Ref.String()}
	}
	b.mu.Lock()
	if b.evs == nil {
		b.evs = map[engine.SessionID]engine.Events{}
	}
	b.evs[req.Session] = req.Events
	b.mu.Unlock()
	b.notify(playback.Note{Session: req.Session, Kind: playback.NoteOpened, Unit: "the stand-in DAC",
		Params:    alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}, Format: format.S243LE, Channels: 2, RateHz: 48000, BufferFrames: 9600, Periods: 4},
		Transport: alsapcm.Transport{Kind: format.KindPCM, RateHz: 48000, Channels: 2, Format: format.S243LE, ValidBits: 24}})
	go req.Events.Audible(req.Session, req.First.ID)
	return nil
}

func (b *playStandIn) Prepare(ctx context.Context, s engine.SessionID, after engine.OccurrenceID, next *engine.Occurrence) error {
	b.mu.Lock()
	ev := b.evs[s]
	b.mu.Unlock()
	if next == nil {
		b.record("prepare end")
		if !b.hold {
			ev.Finished(s)
		}
		return nil
	}
	b.record("prepare")
	if b.refuse[next.Ref.String()] {
		ev.Refused(s, next.ID, engine.Unplayable{Reason: playback.ReasonDSD, Detail: "the stand-in refuses " + next.Ref.String()})
		return nil
	}
	ev.Prepared(s, next.ID, engine.Join{Gapless: true})
	ev.Submitted(s, next.ID)
	if !b.hold {
		ev.Audible(s, next.ID)
	}
	return nil
}

func (b *playStandIn) Cut(ctx context.Context, s engine.SessionID, from engine.OccurrenceID) (bool, error) {
	b.record("cut")
	return true, nil
}

func (b *playStandIn) Pause(context.Context, engine.SessionID) error {
	b.record("pause")
	return nil
}

func (b *playStandIn) Resume(context.Context, engine.SessionID) (engine.ResumeResult, error) {
	b.record("resume")
	return engine.ResumeContinued, nil
}

func (b *playStandIn) Release(ctx context.Context, s engine.SessionID) error {
	b.record("release")
	b.notify(playback.Note{Session: s, Kind: playback.NoteClosed, Params: alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}}})
	return nil
}

func (b *playStandIn) Discard(context.Context) error  { return nil }
func (b *playStandIn) Wait(ctx context.Context) error { return nil }

// playCase is one harness run's box: a config binding the Pi 5 tree's GO
// bar, the tree, the files, and the stand-in.
type playCase struct {
	t          *testing.T
	configPath string
	dataDir    string
	trees      outputTrees
	files      []string
	stateRoot  string
	standIn    *playStandIn
	stdin      io.Reader
}

// newPlayCase writes the config, with player block player, and lays out
// the Pi 5 tree.
func newPlayCase(t *testing.T, player string) *playCase {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the player's engine keeps its IDs durable with a directory fsync, which Windows has not")
	}
	root := t.TempDir()
	music, data := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, d := range []string{music, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "bridge.yaml")
	text := "libraryRoots:\n  - " + music + "\ndataDir: " + data + "\nadminAddress: 127.0.0.1:0\noutput:\n  player:\n" + player
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := sysfstest.Build(t, filepath.Join("..", "..", "internal", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	c := &playCase{t: t, configPath: configPath, dataDir: data, stateRoot: t.TempDir(),
		trees: outputTrees{proc: filepath.Join(tree, "proc"), sysfs: filepath.Join(tree, "sys")}, standIn: &playStandIn{refuse: map[string]bool{}}}
	for _, name := range []string{"a.flac", "b.flac", "c.flac"} {
		p := filepath.Join(music, name)
		if err := os.WriteFile(p, []byte("the stand-in reads nothing"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.files = append(c.files, p)
	}
	return c
}

// goBarPlayer is a player block binding the GO bar by serial.
const goBarPlayer = "    enabled: true\n    device:\n      binding: serial\n      vendorId: \"20b1\"\n      productId: \"3009\"\n      serial: GOBAR-7Q2X-91KZ-4471\n"

func (c *playCase) env(goos string) outputPlayEnv {
	return outputPlayEnv{
		goos: goos,
		backend: func(cfg playback.Config) (playBackend, error) {
			c.standIn.notify = cfg.Notify
			return c.standIn, nil
		},
		trees:       c.trees,
		openSource:  func(context.Context) hotplug.Source { return hotplug.PollingSource("a harness test hears no kernel") },
		stdin:       c.stdin,
		now:         time.Now,
		stateParent: func() (string, error) { return c.stateRoot, nil },
	}
}

// run runs the harness with args and the files, and returns its exit code
// and its output.
func (c *playCase) run(ctx context.Context, goos string, args ...string) (int, *playBuf, *playBuf) {
	c.t.Helper()
	stdout, stderr := newPlayBuf(), newPlayBuf()
	code := runOutputPlay(ctx, append(append([]string{"--config", c.configPath}, args...), c.files...), c.env(goos), stdout, stderr)
	return code, stdout, stderr
}

// TestOutputPlayIsHidden pins the harness's place: bridge output's usage
// does not list it, and its own -h describes it.
func TestOutputPlayIsHidden(t *testing.T) {
	var usage bytes.Buffer
	outputUsage(&usage)
	for _, l := range strings.Split(usage.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "play" {
			t.Errorf("bridge output's usage lists play: %q", l)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := outputCmd(context.Background(), []string{"play", "-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("play -h exited %d", code)
	}
	help := stdout.String() + stderr.String()
	for _, want := range []string{"bridge output play --config <bridge.yaml> [--keys] [--state-dir DIR] [--pause-release SECONDS] FILE...",
		"headphones", "status", "Linux only"} {
		if !strings.Contains(help, want) {
			t.Errorf("play -h does not say %q:\n%s", want, help)
		}
	}
}

// TestOutputPlayRefusesWhatItCannotPlay pins the refusals before anything
// starts: no file, a pause release out of the engine's bounds (usage, 2);
// off Linux, a config the player cannot run, no binding, a file that is not
// there (1); and none of them builds a backend.
func TestOutputPlayRefusesWhatItCannotPlay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		player string
		goos   string
		args   []string
		files  []string
		code   int
		says   string
	}{
		{name: "no file", player: goBarPlayer, goos: "linux", files: []string{}, code: 2, says: "at least one FILE"},
		{name: "a pause release of 0", player: goBarPlayer, goos: "linux", args: []string{"--pause-release", "0"}, code: 2, says: "1 to 600"},
		{name: "a pause release past the engine's", player: goBarPlayer, goos: "linux", args: []string{"--pause-release", "601"}, code: 2, says: "1 to 600"},
		{name: "off Linux", player: goBarPlayer, goos: "darwin", code: 1, says: "Linux only"},
		{name: "a block the player cannot run", player: "    enabled: true\n    dsdMode: native\n", goos: "linux", code: 1, says: "cannot run"},
		{name: "no binding", player: "    enabled: true\n", goos: "linux", code: 1, says: "bridge output dac bind"},
		{name: "a file that is not there", player: goBarPlayer, goos: "linux", files: []string{"/nonexistent/x.flac"}, code: 1, says: "x.flac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newPlayCase(t, tc.player)
			if tc.files != nil {
				c.files = tc.files
			}
			built := false
			env := c.env(tc.goos)
			env.backend = func(playback.Config) (playBackend, error) {
				built = true
				return c.standIn, nil
			}
			stdout, stderr := newPlayBuf(), newPlayBuf()
			code := runOutputPlay(context.Background(), append(append([]string{"--config", c.configPath}, tc.args...), c.files...), env, stdout, stderr)
			if code != tc.code || !strings.Contains(stderr.String(), tc.says) {
				t.Errorf("exit %d, want %d saying %q:\n%s", code, tc.code, tc.says, stderr.String())
			}
			if built {
				t.Errorf("a refused run built a backend")
			}
		})
	}
}

// TestOutputPlayRefusesWhenNothingIsPlayable pins the two ways nothing
// plays: every file refused, each skip printed with its reason (exit 1);
// and the DAC not available, said with its state, before any Play.
func TestOutputPlayRefusesWhenNothingIsPlayable(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	for i := range c.files {
		c.standIn.refuse[fileRef(i)] = true
	}
	code, stdout, _ := c.run(context.Background(), "linux")
	if code != 1 || !strings.Contains(stdout.String(), "all_unplayable") || strings.Count(stdout.String(), "skipped: dsd_not_in_this_release") != 3 {
		t.Errorf("every file refused: exit %d:\n%s", code, stdout.String())
	}

	c = newPlayCase(t, goBarPlayer)
	sysfstest.Apply(t, filepath.Dir(c.trees.proc), "unplug sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1\n")
	code, _, stderr := c.run(context.Background(), "linux")
	if code != 1 || !strings.Contains(stderr.String(), "nothing playable: the DAC is device_missing") {
		t.Errorf("the DAC unplugged: exit %d:\n%s", code, stderr.String())
	}
	for _, call := range c.standIn.callList() {
		if call == "start" {
			t.Errorf("a session started with the DAC unplugged")
		}
	}
}

// TestOutputPlayPlaysTheQueueOnceAndSaysSo pins a run without --keys: the
// warning about the level first, the files queued in order, the output's
// report, the card opened with the kernel's status file, every boundary, the
// end of the queue, the release, and a shutdown; exit 0, and the run's own
// state directory removed.
func TestOutputPlayPlaysTheQueueOnceAndSaysSo(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	code, stdout, stderr := c.run(context.Background(), "linux")
	out := stdout.String()
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, stderr.String())
	}
	order := []string{
		"use headphones, with the DAC's or the amplifier's own volume set low",
		": " + c.files[0],
		": " + c.files[2],
		"output report 1: available",
		"session 1: start with occurrence 1 (entry ",
		"opened card 5 device 0 subdevice 0 (the stand-in DAC)",
		"status: /proc/asound/card5/pcm0p/sub0/status",
		"session 1: boundary: occurrence 1 (entry ", "a.flac) is audible",
		"session 1: boundary: occurrence 2 (entry ", "b.flac) is audible",
		"session 1: boundary: occurrence 3 (entry ", "c.flac) is audible",
		"session 1: finished",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("the output does not say %q after what came before:\n%s", want, out)
		}
		at += i + len(want)
	}
	// The engine records the stop as the release begins, and the run prints
	// the two from two goroutines: either may come first.
	for _, want := range []string{"closed card 5 device 0 subdevice 0", "session 1: released", "engine: playback stopped: end_of_queue"} {
		if !strings.Contains(out[at:], want) {
			t.Errorf("the output does not say %q after the end:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "shut down") {
		t.Errorf("the run's last line is not its shutdown:\n%s", out)
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(l) < 13 || l[2] != ':' || l[5] != ':' || l[8] != '.' {
			t.Errorf("a line without its time: %q", l)
		}
	}
	if left, _ := os.ReadDir(c.stateRoot); len(left) != 0 {
		t.Errorf("the run's state directory was left behind: %v", left)
	}
}

// TestOutputPlayKeepsItsFilesInTheStateDir pins where the engine's files go:
// --state-dir, kept, and never the config's data directory, which the run
// leaves as it was.
func TestOutputPlayKeepsItsFilesInTheStateDir(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	dir := filepath.Join(t.TempDir(), "state")
	if code, stdout, _ := c.run(context.Background(), "linux", "--state-dir", dir); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); err != nil {
		t.Errorf("the engine's queue file is not in the state directory: %v", err)
	}
	if left, _ := os.ReadDir(c.dataDir); len(left) != 0 {
		t.Errorf("the run wrote into the config's data directory: %v", left)
	}
}

// TestOutputPlayKeysDriveTheEngine pins --keys: each single key is its
// engine command, and each answer is printed: space pauses and resumes, n
// next, p previous, d deletes the entry after the playing one (a cut of
// what was handed off), m enters and leaves maintenance, s stops, and q
// quits with a shutdown, exit 0. Keys need no Enter: they arrive one byte
// at a time.
func TestOutputPlayKeysDriveTheEngine(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	c.standIn.hold = true
	pr, pw := io.Pipe()
	c.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := newPlayBuf(), newPlayBuf()
	done := make(chan int, 1)
	go func() {
		done <- runOutputPlay(ctx, append([]string{"--config", c.configPath, "--keys"}, c.files...), c.env("linux"), stdout, stderr)
	}()
	stdout.waitFor(t, "a.flac) is audible")
	// The engine's IDs start a block past its reservation: read them from
	// the run's own lines.
	ids := map[string]string{}
	for _, l := range strings.Split(stdout.String(), "\n") {
		if _, rest, ok := strings.Cut(l, "queued entry "); ok {
			id, path, _ := strings.Cut(rest, ": ")
			ids[filepath.Base(path)] = id
		}
	}
	press := func(k, says string) {
		t.Helper()
		if _, err := pw.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		stdout.waitFor(t, says)
	}
	press(" ", "space: pause: done")
	press(" ", "space: play: done")
	press("d", "d: delete entry "+ids["b.flac"]+", after the playing entry "+ids["a.flac"]+": done")
	press("n", "n: next: done")
	press("p", "p: previous: done")
	press("m", "m: maintenance entered")
	press("m", "m: leave maintenance: done")
	press("s", "s: stop: done")
	press("x", `key 'x' does nothing`)
	press("q", "q: quit")
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d:\n%s\n%s", code, stdout.String(), stderr.String())
		}
	case <-time.After(playWatch):
		t.Fatal("the run did not end after q")
	}
	calls := strings.Join(c.standIn.callList(), " ")
	for _, want := range []string{"pause", "resume", "cut"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the keys made no %s call: %s", want, calls)
		}
	}
	if !strings.Contains(stdout.String(), "shut down") {
		t.Errorf("no shutdown after q:\n%s", stdout.String())
	}
}
