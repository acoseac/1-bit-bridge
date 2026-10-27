package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The pause release bounds the player's engine takes (1 s to 10 min),
// narrower than output.player.pauseReleaseSeconds' 1 to 3600: bridge output
// play's --pause-release, and serve's player (planServePlayer), which does
// not start on a config past them.
const (
	playMinPauseRelease = time.Second
	playMaxPauseRelease = 10 * time.Minute
)

// playBackend is what the harness and serve's player play through: the
// player's backend (playback.Backend), or a test's stand-in, the wait for
// its goroutines their stops join, and the release of every session it
// holds, which serve's player asks for once its engine's Run has ended in a
// panic (playback.Backend.ReleaseAll).
type playBackend interface {
	engine.Backend
	Wait(ctx context.Context) error
	ReleaseAll(ctx context.Context) error
}

// outputPlayEnv is what `bridge output play` takes from the host, so a
// test can stand in for each: the OS it refuses off Linux, the backend it
// plays through, the trees the binding is resolved on and the watcher
// reads, the watcher's event source, standard input and the terminal mode
// for --keys, the clock its lines are stamped with, and where a run's own
// state directory is made.
type outputPlayEnv struct {
	goos       string
	backend    func(playback.Config) (playBackend, error)
	trees      outputTrees
	openSource func(context.Context) hotplug.Source
	stdin      io.Reader
	rawKeys    func(*os.File) (restore func(), ok bool)
	now        func() time.Time
	// stateParent is where a run without --state-dir makes its state
	// directory: the user's cache directory, on storage that outlives a
	// reboot as the engine requires, where /tmp may be a tmpfs.
	stateParent func() (string, error)
}

// outputPlayCmd implements `bridge output play` on this host.
func outputPlayCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runOutputPlay(ctx, args, outputPlayEnv{
		goos:        runtime.GOOS,
		backend:     newOutputPlayBackend,
		trees:       hostTrees,
		stdin:       os.Stdin,
		rawKeys:     playRawKeys,
		now:         time.Now,
		stateParent: os.UserCacheDir,
	}, stdout, stderr)
}

// outputPlayUsage is play's own help. `bridge output` does not list the
// subcommand: it is the B470 hardware harness, run by hand.
func outputPlayUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `bridge output play --config <bridge.yaml> [--keys] [--state-dir DIR] [--pause-release SECONDS] FILE...

The bridge player's engine and backend, by hand (B470 PR 4b2's hardware gate).
It resolves the configured binding, output.player.device, to its DAC on this
box's trees, runs the hotplug watcher, and plays the files in order through
the player's engine and backend: PCM only (integer FLAC, WAV, AIFF, AIFC and
ALAC), bit-exact, through the one DAC the binding names, opened exclusively.
DSD, floating-point and lossy files are refused with their reasons, as the
player refuses them.

Files play at their own level: the bridge never lowers a sample (no software
volume). Use headphones, with the DAC's or the amplifier's own volume set low.

It prints one timestamped line for each change: the engine's state and
generation, each occurrence's stages (prepared, submitted, audible), every
boundary, each refusal and fault with its reason, a release confirmed or not,
the output's reports, and each open and close of the DAC with the kernel's
status file for its substream, which reads "closed" once the device is let go.

Without --keys it plays the queue once and exits. With --keys, single keys
drive the engine: space pauses or resumes, n next, p previous, s stop, d
deletes the entry after the playing one (an edit after hand-off), m enters or
leaves maintenance, q quits (a bounded shutdown). Ctrl-C quits too.

It never touches bridge serve's data directory: the engine's queue and
reservation files go in --state-dir, or in a new directory of its own under
the user's cache directory, removed at exit, and a --state-dir that is
serve's data directory, lies inside it or holds it, through a link or
not, is refused. A device another process holds
(MPD playing) is refused as busy, naming the holder; the bridge never stops
MPD or changes its configuration.

Linux only. It is not part of the server and not reachable over HTTP.

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// runOutputPlay is `bridge output play`.
//
// Exit codes: 0 the queue played to its end, or q or Ctrl-C ended the run
// and it shut down; 1 refused (off Linux, a config the player cannot run, no
// binding, a file that is not there, a user's cache directory in serve's
// data directory, the DAC not available, nothing playable) or playback
// stopped short of the end of the queue (a fault, an output gone); 2 usage
// error, a --state-dir that meets serve's data directory among them.
func runOutputPlay(ctx context.Context, args []string, env outputPlayEnv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("output play", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "the bridge's config `file`: the player's binding and settings are read from it")
	keys := fs.Bool("keys", false, "drive the engine with single keys from standard input: space, n, p, s, d, m, q")
	stateDir := fs.String("state-dir", "", "the `dir`ectory the engine keeps its queue and reservation files in (default: a new one, removed at exit)")
	pauseRelease := fs.Int("pause-release", 0, "how many `seconds` a pause holds the DAC before it is let go, 1 to 600 (default: output.player.pauseReleaseSeconds)")
	fs.Usage = func() { outputPlayUsage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	files := fs.Args()
	switch {
	case len(files) == 0:
		fmt.Fprintln(stderr, "output play: name at least one FILE to play")
		return 2
	case set["pause-release"] && (time.Duration(*pauseRelease)*time.Second < playMinPauseRelease || time.Duration(*pauseRelease)*time.Second > playMaxPauseRelease):
		fmt.Fprintf(stderr, "output play: --pause-release is %d to %d seconds, not %d\n", int(playMinPauseRelease/time.Second), int(playMaxPauseRelease/time.Second), *pauseRelease)
		return 2
	}
	if env.goos != "linux" {
		fmt.Fprintf(stderr, "output play: the bridge's player is Linux only, and this is %s\n", env.goos)
		return 1
	}
	cfg, path, err := loadCLIConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "output play: %v\n", err)
		return 1
	}
	player := cfg.Output.Player
	on, why := cfg.PlayerEnabled(env.goos)
	if why != nil {
		fmt.Fprintf(stderr, "output play: %s: the player cannot run on this config: %v\n", path, why)
		return 1
	}
	if !player.Device.Configured() {
		fmt.Fprintf(stderr, "output play: %s binds no DAC (output.player.device): bridge output dac bind <card> --binding serial|uniqueModel|port\n", path)
		return 1
	}
	release := player.PauseRelease()
	if set["pause-release"] {
		release = time.Duration(*pauseRelease) * time.Second
	} else if release < playMinPauseRelease || release > playMaxPauseRelease {
		fmt.Fprintf(stderr, "output play: output.player.pauseReleaseSeconds is %d, past the %d seconds the player's engine takes: pass --pause-release\n",
			int(release/time.Second), int(playMaxPauseRelease/time.Second))
		return 1
	}
	for _, f := range files {
		st, err := os.Stat(f)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "output play: %v\n", err)
			return 1
		case !st.Mode().IsRegular():
			fmt.Fprintf(stderr, "output play: %s is not a file\n", f)
			return 1
		}
	}
	if *stateDir != "" {
		apart, err := playStateApart(*stateDir, cfg.DataDir)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "output play: --state-dir %s: %v\n", *stateDir, err)
			return 1
		case apart != "":
			fmt.Fprintf(stderr, "output play: --state-dir %s %s: the harness never writes into serve's data directory, so name a directory apart from it\n", *stateDir, apart)
			return 2
		}
	}
	dir, cleanup, err := playStateDir(*stateDir, cfg.DataDir, env.stateParent)
	if err != nil {
		fmt.Fprintf(stderr, "output play: %v\n", err)
		return 1
	}
	defer cleanup()
	out := &playOut{w: stdout, now: env.now}
	r := &playRun{env: env, out: out, stderr: stderr, files: files, keys: *keys, release: release, dsd: player.EffectiveDSDMode(),
		observer: binding.Observer{Proc: alsa.Reader{ProcRoot: env.trees.proc}, Sysfs: alsa.SysfsReader{Root: env.trees.sysfs}}}
	if !on {
		out.line("output.player.enabled is false in %s: this harness plays all the same, and bridge serve runs no player", path)
	}
	out.line("files play bit-exact, at their own level, which the bridge never lowers: use headphones, with the DAC's or the amplifier's own volume set low")
	return r.run(ctx, dir, player.Device)
}

// playStateDir is the directory the engine's files go in: dir, made if it
// is missing and left at exit, or a new one under parent's answer, removed
// at exit, which is refused when that answer lies in dataDir, serve's data
// directory. A dir the caller has not checked with playStateApart is never
// passed.
func playStateDir(dir, dataDir string, parent func() (string, error)) (string, func(), error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, fmt.Errorf("the state directory: %w", err)
		}
		return dir, func() {}, nil
	}
	base, err := parent()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	switch in, err := playWithin(base, dataDir); {
	case err != nil:
		return "", nil, fmt.Errorf("the state directory's parent %s: %w", base, err)
	case in:
		return "", nil, fmt.Errorf("the user's cache directory %s lies in serve's data directory %s, where the harness never writes: pass --state-dir with a directory apart from it", base, dataDir)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("the state directory's parent %s: %w", base, err)
	}
	d, err := os.MkdirTemp(base, "bridge-output-play-")
	if err != nil {
		return "", nil, fmt.Errorf("a state directory: %w", err)
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// playStateApart says how dir, a --state-dir, meets dataDir, serve's data
// directory, when it is that directory, lies inside it or holds it, and
// answers "" when the two are apart.
func playStateApart(dir, dataDir string) (string, error) {
	for _, c := range []struct{ p, in, says string }{
		{dir, dataDir, "is serve's data directory %s, or lies inside it"},
		{dataDir, dir, "holds serve's data directory %s"},
	} {
		in, err := playWithin(c.p, c.in)
		if err != nil {
			return "", err
		}
		if in {
			return fmt.Sprintf(c.says, dataDir), nil
		}
	}
	return "", nil
}

// playWithin reports whether p is dir or lies inside it: the two compared
// with their links resolved as the kernel resolves them (playResolve), in the
// case sensitivity of dir's volume (fsutil.IsUnderAny), either of them there
// or not yet. A bind mount is not a link, and an alias it makes is not found.
func playWithin(p, dir string) (bool, error) {
	rp, err := playResolve(p)
	if err != nil {
		return false, err
	}
	rd, err := playResolve(dir)
	if err != nil {
		return false, err
	}
	return fsutil.IsUnderAny(rp, []string{rd}) != "", nil
}

// playResolve is p, made absolute, with its links resolved as the kernel
// resolves them: the longest leading run of p's elements that is there,
// resolved as written, so that a ".." after a link goes up from where the
// link points (filepath.Abs and Clean take it from the link's own place),
// then the elements that are not there yet, which a MkdirAll makes as
// directories.
func playResolve(p string) (string, error) {
	sep := string(filepath.Separator)
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + sep + p
	}
	vol := filepath.VolumeName(p)
	elems := strings.Split(p[len(vol):], sep)
	for n := len(elems); n > 1; n-- {
		if r, err := fsutil.ResolveLinks(vol + strings.Join(elems[:n], sep)); err == nil {
			return filepath.Join(append([]string{r}, elems[n:]...)...), nil
		}
	}
	return filepath.Clean(p), nil
}

// playRun is one run of the harness.
type playRun struct {
	env      outputPlayEnv
	out      *playOut
	stderr   io.Writer
	files    []string
	keys     bool
	release  time.Duration
	dsd      string
	observer binding.Observer

	eng  *engine.Engine
	be   playBackend
	hold *engine.Hold

	mu   sync.Mutex
	occs map[engine.OccurrenceID]occName
	seen *playSeen // what the run has printed of the engine's snapshots
}

// occName is what an occurrence is, for a line: its entry and its file.
type occName struct {
	entry uint32
	ref   string
}

// fileRef is the reference of the n-th file (from 0).
func fileRef(n int) string { return "file:" + strconv.Itoa(n+1) }

// playFiles is the run's resolver: each reference "file:N" names the N-th
// file the command was given, and nothing else is opened.
type playFiles []string

func (f playFiles) Open(_ context.Context, ref engine.Reference) (playback.Content, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(ref.String(), "file:"))
	if err != nil || !strings.HasPrefix(ref.String(), "file:") || n < 1 || n > len(f) {
		return playback.Content{}, &engine.Unplayable{Reason: engine.ReasonMissing, Detail: "the harness gave no file " + ref.String()}
	}
	fh, err := os.Open(f[n-1])
	if err != nil {
		return playback.Content{}, err
	}
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = fh.Close()
		return playback.Content{}, &engine.Unplayable{Reason: engine.ReasonMissing, Detail: f[n-1] + " is not a regular file"}
	}
	return playback.Content{Data: fh, Size: st.Size(), Closer: fh}, nil
}

// name names an occurrence for a line.
func (r *playRun) name(o engine.OccurrenceID) string {
	r.mu.Lock()
	n, ok := r.occs[o]
	r.mu.Unlock()
	if !ok {
		return fmt.Sprintf("occurrence %d", o)
	}
	return fmt.Sprintf("occurrence %d (entry %d, %s)", o, n.entry, r.fileOf(n.ref))
}

// fileOf is the file a reference names, for a line.
func (r *playRun) fileOf(ref string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "file:"))
	if err != nil || n < 1 || n > len(r.files) {
		return ref
	}
	return filepath.Base(r.files[n-1])
}

func (r *playRun) noteOcc(o engine.Occurrence) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.occs[o.ID] = occName{entry: o.Entry, ref: o.Ref.String()}
}

// run plays: the backend, the engine and the watcher started, the files
// queued, played, and every change printed, until the queue ends (no
// --keys), q, or Ctrl-C; then a bounded shutdown.
func (r *playRun) run(ctx context.Context, dir string, dev config.PlayerDevice) int {
	r.occs = map[engine.OccurrenceID]occName{}
	be, err := r.env.backend(playback.Config{
		Resolver: playFiles(r.files),
		Locator:  playback.BindingLocator{Device: dev, Observer: r.observer},
		Notify:   r.note,
	})
	if err != nil {
		fmt.Fprintf(r.stderr, "output play: %v\n", err)
		return 1
	}
	r.be = be
	// The hardware harness has no controller identity. Name this run's
	// reservation without minting a player-uuid in its scratch state dir.
	id, err := uuid.NewRandom()
	if err != nil {
		fmt.Fprintf(r.stderr, "output play: %v\n", err)
		return 1
	}
	eng, err := engine.New(engine.Config{Backend: playTee{playBackend: be, r: r}, QueuePath: filepath.Join(dir, "queue.json"),
		PlayerUUID: id.String(), DataDir: dir, PauseRelease: r.release, DSD: r.dsd})
	if err != nil {
		fmt.Fprintf(r.stderr, "output play: %v\n", err)
		return 1
	}
	r.eng = eng
	runCtx, stopRun := context.WithCancel(ctx)
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		_ = eng.Run(runCtx)
	}()
	watcherDone := r.watch(runCtx, dev)
	code := r.play(ctx)
	stopRun()
	r.shutdown(engineDone, watcherDone)
	return code
}

// watch runs the hotplug watcher on the box's trees, with the player forced
// on (the harness plays whatever output.player.enabled says), and reports
// each generation it publishes to the engine through a Reporter, printing
// each report.
func (r *playRun) watch(ctx context.Context, dev config.PlayerDevice) <-chan struct{} {
	svc := hotplug.NewService(hotplug.Config{
		Settings:   func() hotplug.Settings { return hotplug.Settings{Enabled: true, Device: dev} },
		Observe:    r.observer.ObserveContext,
		OpenSource: r.env.openSource,
	})
	sub := svc.Feed().Subscribe()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		defer sub.Close()
		rep := &hotplug.Reporter{}
		for {
			select {
			case <-ctx.Done():
				return
			case snap := <-sub.C():
				rp, ok := rep.Report(snap)
				if !ok {
					continue
				}
				res := snap.Resolution()
				what := "available"
				if !rp.Available {
					what = "unavailable: " + rp.Reason
				}
				if rp.UnitChanged {
					what += ", the bound unit changed or re-enumerated"
				}
				r.out.line("output report %d: %s (%s)", rp.Generation, what, res.Reason)
				if err := r.eng.ReportOutput(ctx, rp); engine.ReasonOf(err) == engine.ReasonClosed {
					return
				}
			}
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// play queues the files, waits for the output's first report, plays, and
// drives and prints the engine until the run ends; it returns the exit
// code.
func (r *playRun) play(ctx context.Context) int {
	eng := r.eng
	if s := eng.Snapshot(); s.Len() > 0 {
		if err := eng.DeleteAll(ctx); err != nil {
			fmt.Fprintf(r.stderr, "output play: the state directory's queue: %v\n", err)
			return 1
		}
	}
	after := uint32(0)
	for i, f := range r.files {
		ref, err := engine.NewReference(fileRef(i))
		if err != nil {
			fmt.Fprintf(r.stderr, "output play: %v\n", err)
			return 1
		}
		id, err := eng.InsertAfter(ctx, after, ref, nil)
		if err != nil {
			fmt.Fprintf(r.stderr, "output play: queue %s: %v\n", f, err)
			return 1
		}
		r.out.line("queued entry %d: %s", id, f)
		after = id
	}
	if code, ok := r.waitOutput(ctx); !ok {
		return code
	}
	var keyCh <-chan byte
	if r.keys {
		ch, stop := r.readKeys()
		defer stop()
		keyCh = ch
		r.out.line("keys: space pauses or resumes, n next, p previous, s stop, d deletes the entry after the playing one, m maintenance, q quits")
	}
	if err := eng.Play(ctx); err != nil {
		r.refused("play", err)
		if !r.keys {
			r.diff(eng.Snapshot())
			return 1
		}
	}
	for {
		ch := eng.Changed()
		s := eng.Snapshot()
		r.diff(s)
		if !r.keys && s.State() == engine.Stopped && s.LastStop().Reason != "" {
			if s.LastStop().Reason == engine.ReasonEndOfQueue {
				return 0
			}
			return 1
		}
		select {
		case <-ch:
		case k, ok := <-keyCh:
			if !ok {
				keyCh = nil
				continue
			}
			if r.key(ctx, k) {
				return 0
			}
		case <-ctx.Done():
			r.out.line("stopped by signal")
			return 0
		}
	}
}

// waitOutput waits for the output's first report: the engine refuses Play
// while it is unknown. An output unavailable then is the end of the run.
func (r *playRun) waitOutput(ctx context.Context) (int, bool) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		ch := r.eng.Changed()
		o := r.eng.Snapshot().Output()
		switch {
		case o.Known && o.Available:
			return 0, true
		case o.Known:
			fmt.Fprintf(r.stderr, "output play: nothing playable: the DAC is %s\n", o.Reason)
			return 1, false
		}
		select {
		case <-ch:
		case <-timer.C:
			fmt.Fprintln(r.stderr, "output play: the hotplug watcher reported nothing about the DAC within 30 s")
			return 1, false
		case <-ctx.Done():
			return 0, false
		}
	}
}

// key runs one key's command and prints its answer; it reports whether the
// run is to end.
func (r *playRun) key(ctx context.Context, k byte) bool {
	eng := r.eng
	s := eng.Snapshot()
	switch k {
	case ' ':
		if s.State() == engine.Playing {
			r.answer("space: pause", eng.Pause(ctx))
		} else {
			r.answer("space: play", eng.Play(ctx))
		}
	case 'n':
		r.answer("n: next", eng.Next(ctx))
	case 'p':
		r.answer("p: previous", eng.Previous(ctx))
	case 's':
		r.answer("s: stop", eng.Stop(ctx))
	case 'd':
		cur := s.Selected()
		if a, ok := s.Audible(); ok {
			cur = a.Entry
		}
		i, ok := s.IndexOf(cur)
		next, okNext := s.IDAt(i + 1)
		if !ok || !okNext {
			r.out.line("d: no entry follows entry %d", cur)
			return false
		}
		r.answer(fmt.Sprintf("d: delete entry %d, after the playing entry %d", next, cur), eng.Delete(ctx, next))
	case 'm':
		if r.hold == nil {
			hold, err := eng.EnterMaintenance(ctx, s.Generation(), "bridge output play")
			if err == nil {
				r.hold = &hold
				r.out.line("m: maintenance entered, the release %s", map[bool]string{true: "confirmed", false: "unconfirmed"}[hold.ReleaseConfirmed])
				return false
			}
			r.answer("m: enter maintenance", err)
		} else {
			err := eng.ExitMaintenance(ctx, r.hold.Lease)
			if err == nil {
				r.hold = nil
			}
			r.answer("m: leave maintenance", err)
		}
	case 'q':
		r.out.line("q: quit")
		return true
	case '\n', '\r':
	default:
		r.out.line("key %q does nothing: space, n, p, s, d, m or q", k)
	}
	return false
}

// answer prints a command's answer.
func (r *playRun) answer(what string, err error) {
	if err == nil {
		r.out.line("%s: done", what)
		return
	}
	r.refused(what, err)
}

// refused prints a command the engine refused, naming the holder of a busy
// device: the holder of the endpoint the kernel refused, which the refusal
// names. The device the run opened last is not it: at the first open there
// is none, and a resume reopens where the binding resolves now, which may be
// another card.
func (r *playRun) refused(what string, err error) {
	r.out.line("%s: refused: %v", what, err)
	var ae *alsapcm.Error
	if !errors.As(err, &ae) || ae.Reason != alsapcm.ReasonBusy {
		return
	}
	var b strings.Builder
	pcmTestNameHolder(alsa.Reader{ProcRoot: r.env.trees.proc}, ae.Endpoint, &b)
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		r.out.line("%s", strings.TrimSpace(l))
	}
}

// readKeys reads single keys from standard input, a terminal put into its
// key mode for the run, until stop.
func (r *playRun) readKeys() (<-chan byte, func()) {
	restore := func() {}
	if f, ok := r.env.stdin.(*os.File); ok && r.env.rawKeys != nil {
		if undo, ok := r.env.rawKeys(f); ok {
			restore = undo
		}
	}
	ch, stop := playKeys(r.env.stdin)
	return ch, func() {
		stop()
		restore()
	}
}

// playKeys reads in a byte at a time until it ends or stop is called, as
// pcmTestKeys reads lines: standard input through poll with a wake-up, so
// stop cuts a read short, and a pipe's read end closed.
func playKeys(in io.Reader) (<-chan byte, func()) {
	interrupt, release, joinable := func() {}, func() {}, false
	switch f := in.(type) {
	case *os.File:
		if pr, intr, rel, ok := pcmTestStoppableFile(f); ok {
			in, interrupt, release, joinable = pr, intr, rel, true
		}
	case interface{ CloseWithError(error) error }:
		interrupt, joinable = func() { _ = f.CloseWithError(errPCMTestKeysStopped) }, true
	}
	keys := make(chan byte)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		defer close(keys)
		br := bufio.NewReader(in)
		for {
			b, err := br.ReadByte()
			if err != nil {
				return
			}
			select {
			case keys <- b:
			case <-done:
				return
			}
		}
	}()
	return keys, func() {
		close(done)
		interrupt()
		if joinable {
			<-exited
			release()
		}
	}
}

// shutdown waits for the engine's bounded shutdown, the watcher's and the
// backend's goroutines, at most a bound each, and says what it left.
func (r *playRun) shutdown(engineDone, watcherDone <-chan struct{}) {
	wait := func(ch <-chan struct{}, d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ch:
			return true
		case <-t.C:
			return false
		}
	}
	if !wait(engineDone, playerEngineStopBound) {
		r.out.line("the engine did not shut down within its bound")
	}
	if !wait(watcherDone, hotplug.DefaultStopWait+time.Second) {
		r.out.line("the hotplug watcher did not stop within its bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), playerBackendWaitBound)
	defer cancel()
	if err := r.be.Wait(ctx); err != nil {
		r.out.line("%v: abandoned", err)
	}
	if u, ok := r.eng.Snapshot().ReleaseUnconfirmed(); ok {
		r.out.line("the release of session %d was never confirmed: the device may still be held", u.Session)
	}
	r.out.line("shut down")
}

// note prints the backend's word of a sink: the card it opened, with the
// kernel's status file the owner reads, a close, a release and a reopen.
func (r *playRun) note(n playback.Note) {
	p := n.Params
	switch n.Kind {
	case playback.NoteOpened, playback.NoteReopened:
		r.out.line("%s card %d device %d subdevice %d (%s): %s, %d channels, %d Hz, %d valid bits; buffer %d frames in %d periods; status: %s",
			n.Kind, p.Endpoint.Card, p.Endpoint.Device, p.Subdevice, n.Unit, p.Format, p.Channels, p.RateHz, n.Transport.ValidBits,
			p.BufferFrames, p.Periods, n.StatusPath())
	default:
		r.out.line("%s card %d device %d subdevice %d: the device is let go (%s reads closed)", n.Kind, p.Endpoint.Card, p.Endpoint.Device, p.Subdevice, n.StatusPath())
	}
}

func newOutputPlayBackend(c playback.Config) (playBackend, error) { return playback.New(c) }
