package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// pcmTestSink is what the harness's sink mode drives: an *alsapcm.Sink, or
// a test's stand-in.
type pcmTestSink interface {
	Params() alsapcm.Params
	Transport() alsapcm.Transport
	Next(alsapcm.Transport) alsapcm.Join
	Write(ctx context.Context, frames []byte) (int, error)
	Pause() error
	Resume() error
	Recover() error
	Drain(ctx context.Context) error
	Drop() error
	Close() error
	Position() alsapcm.Position
	Stats() alsapcm.SinkStats
	Changed() <-chan struct{}
}

// pcmTestTick is how often the sink mode reports and looks at its timers.
const pcmTestTick = time.Second

// pcmTestFiles is --file, which may be given more than once.
type pcmTestFiles []string

func (f *pcmTestFiles) String() string { return strings.Join(*f, ", ") }

func (f *pcmTestFiles) Set(v string) error {
	if v == "" {
		return errors.New("a file is a path")
	}
	*f = append(*f, v)
	return nil
}

// pcmTestSinkFlags are the sink mode's flags, as parsed.
type pcmTestSinkFlags struct {
	files        pcmTestFiles
	ackFullLevel bool
	tracks       int
	pause        string
	pauseAt      float64
	resumeAfter  float64
	releaseAfter float64
	keys         bool
	recover      bool
}

// pcmTestTrack is one track of a sink run: its transport, and where its
// frames come from, a decoded file or a stretch of the tone.
type pcmTestTrack struct {
	name      string
	transport alsapcm.Transport
	frames    int64 // the track's frames, when known
	decoder   decode.Decoder
	file      *os.File
	tone      *pcmTestTone
	toneFrom  int64 // the tone's first frame in this track, counted from the tone's start
}

// pcmTestSinkRun is one sink run, checked.
type pcmTestSinkRun struct {
	endpoint     alsapcm.Endpoint
	procRoot     string
	pause        alsapcm.PauseMode
	pauseAt      time.Duration // content time; 0 for no timer
	resumeAfter  time.Duration // after the pause; 0 for no timer
	releaseAfter time.Duration
	keys         bool
	recover      bool
	tracks       []pcmTestTrack
	tone         string // what the tone is, for a tone run
}

// pcmTestMaxTimer bounds --pause-at and --resume-after: a day.
const pcmTestMaxTimer = 86400

// pcmTestSinkModeFrom checks the sink mode's flags. sink is false when the
// run is the PR 2a tone written to the device itself; problem, when set,
// is why the flags make no run, a usage error.
func pcmTestSinkModeFrom(sinkFlag bool, f pcmTestSinkFlags, set map[string]bool) (sink bool, problem string) {
	sink = sinkFlag || len(f.files) > 0
	if !sink {
		for _, name := range []string{"tracks", "pause", "pause-at", "resume-after", "release-after", "keys", "recover", "ack-full-level"} {
			if set[name] {
				return false, fmt.Sprintf("--%s is for the streaming sink: add --sink, or play a --file", name)
			}
		}
		return false, ""
	}
	if len(f.files) > 0 {
		for _, name := range []string{"rate", "bits", "level", "seconds", "tracks"} {
			if set[name] {
				return true, fmt.Sprintf("--file plays the file as it is and takes no --%s", name)
			}
		}
		if !f.ackFullLevel {
			return true, "--file plays the file bit-exact, at the level it was mastered at: the bridge never lowers it. " +
				"Turn the DAC or the amplifier down first, and say so with --ack-full-level"
		}
	}
	// Each range is asked as what it is, so a NaN, which fails every
	// comparison, fails the check too.
	inRange := func(v, lo, hi float64) bool { return v >= lo && v <= hi }
	switch {
	case f.tracks < 1 || f.tracks > 8:
		return true, fmt.Sprintf("--tracks is 1 to 8, not %d", f.tracks)
	case f.pause != "auto" && f.pause != "pause" && f.pause != "zeros":
		return true, fmt.Sprintf("--pause is auto, pause or zeros, not %q", f.pause)
	case !inRange(f.pauseAt, 0, pcmTestMaxTimer) || !inRange(f.resumeAfter, 0, pcmTestMaxTimer):
		return true, fmt.Sprintf("--pause-at and --resume-after are 0 to %d seconds", pcmTestMaxTimer)
	case f.resumeAfter > 0 && f.pauseAt == 0:
		return true, "--resume-after counts from the pause --pause-at makes"
	case !inRange(f.releaseAfter, 1, 600):
		return true, fmt.Sprintf("--release-after is 1 to 600 seconds, not %g", f.releaseAfter)
	}
	return true, ""
}

// pcmTestPauseMode is --pause as the sink names it.
func pcmTestPauseMode(s string) alsapcm.PauseMode {
	switch s {
	case "pause":
		return alsapcm.PauseKernel
	case "zeros":
		return alsapcm.PauseZeros
	}
	return alsapcm.PauseAuto
}

// pcmTestSeconds is a flag's seconds as a duration.
func pcmTestSeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// run is a sink run of tracks, with the flags' pause, timers, keys and
// recovery.
func (sf pcmTestSinkFlags) run(ep alsapcm.Endpoint, root string, tracks []pcmTestTrack) pcmTestSinkRun {
	return pcmTestSinkRun{
		endpoint: ep, procRoot: root,
		pause:   pcmTestPauseMode(sf.pause),
		pauseAt: pcmTestSeconds(sf.pauseAt), resumeAfter: pcmTestSeconds(sf.resumeAfter), releaseAfter: pcmTestSeconds(sf.releaseAfter),
		keys: sf.keys, recover: sf.recover, tracks: tracks,
	}
}

// runPCMTestFiles is a --file run: every file decoded and checked before
// the device is opened, then played through the sink.
func runPCMTestFiles(ctx context.Context, env outputPCMTestEnv, sf pcmTestSinkFlags, card, device int, alsaFormat, root string, stdout, stderr io.Writer) int {
	if device < 0 {
		fmt.Fprintln(stderr, "output pcm-test: --device is required, a number from 0 (the D of /dev/snd/pcmC<card>D<D>p)")
		return 2
	}
	switch format.ALSAFormat(alsaFormat) {
	case "", format.S16LE, format.S243LE, format.S24LE, format.S32LE:
	default:
		fmt.Fprintf(stderr, "output pcm-test: --format is S16_LE, S24_3LE, S24_LE or S32_LE, not %q\n", alsaFormat)
		return 2
	}
	if env.goos != "linux" {
		fmt.Fprintf(stderr, "output pcm-test: ALSA is Linux only, and this is %s\n", env.goos)
		return 1
	}
	tracks, err := pcmTestOpenFiles(sf.files, alsaFormat)
	if err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return 1
	}
	return sf.run(alsapcm.Endpoint{Card: card, Device: device}, root, tracks).play(ctx, env, stdout, stderr)
}

// playThroughSink is a --sink run: the tone, in --tracks tracks of one
// transport, through the sink.
func (r pcmTestRun) playThroughSink(ctx context.Context, env outputPCMTestEnv, sf pcmTestSinkFlags, stdout, stderr io.Writer) int {
	tone, err := buildPCMTestTone(r.rate, r.bits, r.format, r.level)
	if err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return 1
	}
	t := alsapcm.Transport{Kind: format.KindPCM, RateHz: r.rate, Channels: alsapcm.Channels, Format: r.format, ValidBits: r.bits}
	run := sf.run(r.endpoint, r.procRoot, pcmTestToneTracks(&tone, t, int64(r.seconds)*int64(r.rate), sf.tracks))
	run.tone = fmt.Sprintf("%d Hz sine on both channels, peak %.1f dBFS (%d of %d), %d valid bits packed as %s, %d s",
		pcmTestToneHz, r.level, tone.amplitude, tone.fullScale, r.bits, r.format, r.seconds)
	return run.play(ctx, env, stdout, stderr)
}

// pcmTestOpenFiles opens and decodes each file, in order, and finds the
// transport each needs: the file's own rate and valid bits, in alsaFormat
// when it carries them, in the format the tone takes for those bits
// otherwise. Every file is checked before the device is opened, so a file
// the player refuses never stops a run half way: its decoder's own checks,
// the format, and the rate, which the decoders take from 1 Hz and the ALSA
// backend opens from alsapcm.MinRateHz.
func pcmTestOpenFiles(paths []string, alsaFormat string) ([]pcmTestTrack, error) {
	var tracks []pcmTestTrack
	fail := func(err error) ([]pcmTestTrack, error) {
		pcmTestCloseTracks(tracks)
		return nil, err
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fail(err)
		}
		src, err := decode.Open(f)
		if err != nil {
			_ = f.Close()
			return fail(fmt.Errorf("%s: %w", path, err))
		}
		d, ok := src.(decode.Decoder)
		if !ok {
			_ = src.Close()
			_ = f.Close()
			return fail(fmt.Errorf("%s is native DSD: the sink plays PCM, and native DSD and DoP are B470 PRs 2c and 2d", path))
		}
		pcm := d.Format()
		af := format.ALSAFormat(alsaFormat)
		if af == "" {
			af = map[int]format.ALSAFormat{16: format.S16LE, 24: format.S243LE, 32: format.S32LE}[pcm.ValidBits]
		}
		track := pcmTestTrack{
			name: path, decoder: d, file: f, frames: pcm.Frames,
			transport: alsapcm.Transport{Kind: format.KindPCM, RateHz: pcm.RateHz, Channels: pcm.Channels, Format: af, ValidBits: pcm.ValidBits},
		}
		tracks = append(tracks, track)
		if !pcmTestPacks(af, pcm.ValidBits) {
			return fail(fmt.Errorf("%s has %d-bit samples, which --format %s does not carry: S16_LE takes 16, S24_3LE and S24_LE take 24, S32_LE takes 24 or 32", path, pcm.ValidBits, af))
		}
		if pcm.RateHz < alsapcm.MinRateHz || pcm.RateHz > alsapcm.MaxRateHz {
			return fail(fmt.Errorf("%s is %d Hz, outside the %d to %d Hz the ALSA backend opens", path, pcm.RateHz, alsapcm.MinRateHz, alsapcm.MaxRateHz))
		}
	}
	return tracks, nil
}

// pcmTestCloseTracks closes the tracks' decoders and files.
func pcmTestCloseTracks(tracks []pcmTestTrack) {
	for _, t := range tracks {
		if t.decoder != nil {
			_ = t.decoder.Close()
		}
		if t.file != nil {
			_ = t.file.Close()
		}
	}
}

// pcmTestToneTracks splits seconds of the tone into n tracks of one
// transport. Each track after the first starts 37 frames past an even
// share, which is no whole repeat of the tone, so a join that loses,
// repeats or inserts a frame is a click in a pure sine.
func pcmTestToneTracks(tone *pcmTestTone, t alsapcm.Transport, total int64, n int) []pcmTestTrack {
	tracks := make([]pcmTestTrack, n)
	for i := range tracks {
		from := total * int64(i) / int64(n)
		if i > 0 {
			from += 37
		}
		tracks[i] = pcmTestTrack{name: fmt.Sprintf("tone, track %d of %d", i+1, n), transport: t, tone: tone, toneFrom: from}
	}
	for i := range tracks {
		end := total
		if i+1 < n {
			end = tracks[i+1].toneFrom
		}
		tracks[i].frames = end - tracks[i].toneFrom
	}
	return tracks
}

// pcmTestOut is the run's stdout, written by the producer and the main loop
// alike, one whole line at a time.
type pcmTestOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *pcmTestOut) printf(format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintf(o.w, format, args...)
}

// pcmTestPlayer is what the producer and the main loop share: the sink now
// playing, which a track of another transport replaces, and the content
// time the sinks before it played, each counted at its own rate.
type pcmTestPlayer struct {
	mu     sync.Mutex
	sink   pcmTestSink
	before time.Duration // content time the sinks closed before this one played
	starts []pcmTestStart
	gaps   int
}

// pcmTestStart is where a gapless track starts in its sink's content, to
// say when it reaches the DAC.
type pcmTestStart struct {
	track int
	sink  pcmTestSink
	frame int64
	told  bool
}

func (p *pcmTestPlayer) current() pcmTestSink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sink
}

// played is the content time the run has played, over every sink, and the
// position of the sink now playing.
func (p *pcmTestPlayer) played() (time.Duration, alsapcm.Position) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.sink.Position()
	return p.before + pcmTestDuration(pos.Played, pos.RateHz), pos
}

// swap makes next the sink the run plays and returns the one it replaces,
// which has drained: its content counts in the run's time at its own rate,
// and every gapless start in it has reached the DAC.
func (p *pcmTestPlayer) swap(next pcmTestSink, out *pcmTestOut) pcmTestSink {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.sink
	pos := old.Position()
	p.before += pcmTestDuration(pos.Played, pos.RateHz)
	p.tellStartsLocked(old, pos.Played, out)
	p.sink = next
	return old
}

// play is a sink run: open the sink for the first track's transport, check
// what the kernel committed against its own file, play every track through
// it, and drain; pause and resume on the timers and the keys; stop at once
// on Ctrl-C or q.
func (r pcmTestSinkRun) play(ctx context.Context, env outputPCMTestEnv, stdout, stderr io.Writer) int {
	defer pcmTestCloseTracks(r.tracks)
	out := &pcmTestOut{w: stdout}
	first := r.tracks[0]
	sink, code := r.open(ctx, env, first.transport, out, stderr)
	if sink == nil {
		return code
	}
	player := &pcmTestPlayer{sink: sink}
	mode := map[alsapcm.PauseMode]string{alsapcm.PauseAuto: "auto", alsapcm.PauseKernel: "PAUSE alone", alsapcm.PauseZeros: "zeros"}[r.pause]
	out.printf("pause: %s (the hardware %s PAUSE); a pause releases the device after %v\n", mode,
		map[bool]string{true: "offers", false: "does not offer"}[sink.Params().CanPause()], r.releaseAfter)
	if r.tone != "" {
		out.printf("tone: %s\n", r.tone)
	}
	for i, t := range r.tracks {
		length := "its length not declared"
		if t.frames > 0 {
			length = pcmTestFrames(t.frames, t.transport.RateHz)
		}
		out.printf("track %d: %s: %s, %s\n", i+1, t.name, t.transport, length)
	}
	hint := "Ctrl-C stops at once"
	if r.keys {
		hint = "p and Enter pauses and resumes, q and Enter stops, Ctrl-C stops at once"
	}
	out.printf("playing; %s\n", hint)

	// The producer runs on the run's own context, which a stop ends first.
	runCtx, endRun := context.WithCancel(ctx)
	defer endRun()
	produced := make(chan error, 1)
	go func() { produced <- r.produce(runCtx, env, player, out, stderr) }()
	var keys <-chan string
	if r.keys {
		var stopKeys func()
		keys, stopKeys = pcmTestKeys(env.stdin)
		defer stopKeys()
	}
	ticks, stop := env.ticks(pcmTestTick)
	defer stop()

	var pausedAt time.Time
	second := 0
	paused, resumed := false, false
	// stopNow ends the run's context and drops the sink playing, so the
	// sound stops at once and the producer opens and writes nothing more;
	// waits for the producer, unless it has ended already; and then drops
	// and closes the sink it left playing, which is a sink it opened for
	// another transport when the stop came as it changed.
	stopNow := func(code int, say string, producerDone bool) int {
		endRun()
		_ = player.current().Drop()
		if !producerDone {
			<-produced
		}
		s := player.current()
		_ = s.Drop()
		played, _ := player.played()
		out.printf("%s after %s played: dropped\n", say, pcmTestRunTime(played))
		return pcmTestCloseSink(s, code, out, stderr)
	}
	for {
		select {
		case err := <-produced:
			// The producer's own cancel can come first when Ctrl-C ends both.
			if ctx.Err() != nil {
				return stopNow(0, "stopped by signal", true)
			}
			return r.finish(err, player, out, stderr)
		case <-ctx.Done():
			return stopNow(0, "stopped by signal", false)
		case k, ok := <-keys:
			if !ok {
				keys = nil
				continue
			}
			switch k {
			case "p":
				if s := player.current(); pcmTestIsPaused(s) {
					r.resume(player, out, stderr)
				} else if err := s.Pause(); err != nil {
					fmt.Fprintf(stderr, "output pcm-test: pause: %v\n", err)
				} else {
					pcmTestSayPaused(player, out)
				}
			case "q":
				return stopNow(0, "stopped (q)", false)
			}
		case now := <-ticks:
			second++
			s := player.current()
			played, pos := player.played()
			rate := pos.RateHz
			stream := pos.Stream.String()
			if pos.State == alsapcm.SinkReleased || pos.State == alsapcm.SinkStopped || pos.State == alsapcm.SinkEnded {
				stream = "closed"
			}
			out.printf("  %d s: played %s, queued %d frames (%s), %s, %s\n", second,
				pcmTestRunTime(played), pos.Queued, pcmTestMillis(pos.Queued, rate), stream, pos.State)
			player.tellStarts(out)
			switch {
			case r.pauseAt > 0 && !paused && played >= r.pauseAt:
				paused, pausedAt = true, now
				if err := s.Pause(); err != nil {
					fmt.Fprintf(stderr, "output pcm-test: pause: %v\n", err)
					continue
				}
				pcmTestSayPaused(player, out)
			case r.resumeAfter > 0 && paused && !resumed && now.Sub(pausedAt) >= r.resumeAfter:
				resumed = true
				if !r.resume(player, out, stderr) && !r.keys {
					return stopNow(1, "stopped: the resume was refused", false)
				}
			}
		}
	}
}

// open opens a sink for t, prints what the kernel committed, and checks it
// against the kernel's own hw_params file. It returns nil and the exit code
// when the sink cannot play; an open the run's stop cut short is no
// failure to report.
func (r pcmTestSinkRun) open(ctx context.Context, env outputPCMTestEnv, t alsapcm.Transport, out *pcmTestOut, stderr io.Writer) (pcmTestSink, int) {
	sink, err := env.openSink(ctx, alsapcm.SinkConfig{Endpoint: r.endpoint, Transport: t, Pause: r.pause, ReleaseAfter: r.releaseAfter})
	if err != nil {
		if ctx.Err() != nil {
			return nil, 1
		}
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		if alsapcm.ReasonOf(err) == alsapcm.ReasonBusy {
			pcmTestNameHolder(alsa.Reader{ProcRoot: r.procRoot}, r.endpoint, stderr)
		}
		return nil, 1
	}
	p := sink.Params()
	var report strings.Builder
	printPCMTestParams(&report, p)
	agrees := pcmTestKernelAgrees(alsa.Reader{ProcRoot: r.procRoot}, p, &report)
	out.printf("%s", report.String())
	if !agrees {
		fmt.Fprintln(stderr, "output pcm-test: the kernel's own hw_params file disagrees with what the backend read back: not playing")
		_ = sink.Close()
		return nil, 1
	}
	st := sink.Stats()
	out.printf("sink: %s; a store of %d frames (%d bytes): %s ahead, %d frames kept to write again after a release\n",
		t, st.RingFrames, st.RingBytes, pcmTestFrames(st.LookaheadFrames, t.RateHz), st.KeptFrames)
	return sink, 0
}

// produce feeds every track to the sink, joining one of the same transport
// to the one before with nothing between, and draining the sink and opening
// one for a track of another transport, an honest gap; then it drains the
// last sink. A sink it replaces it closes once the next one plays, so each
// sink has one closer: the producer for one it replaced, the run for the
// last. ctx ends with the run, a q or Ctrl-C included, so nothing is opened
// or written after a stop.
func (r pcmTestSinkRun) produce(ctx context.Context, env outputPCMTestEnv, player *pcmTestPlayer, out *pcmTestOut, stderr io.Writer) error {
	var at time.Duration // the run's content time handed to the sinks so far
	var sinkFrames int64 // the content frames handed to the sink now playing
	for i, t := range r.tracks {
		sink := player.current()
		if i > 0 {
			if j := sink.Next(t.transport); j.Gapless {
				out.printf("track %d joins track %d gaplessly at %s (frame %d of its sink): the same transport, nothing between\n",
					i+1, i, pcmTestRunTime(at), sinkFrames)
				player.mu.Lock()
				player.starts = append(player.starts, pcmTestStart{track: i + 1, sink: sink, frame: sinkFrames})
				player.mu.Unlock()
			} else {
				out.printf("track %d needs another transport (%s): drain, reopen and refill, an honest gap\n", i+1, strings.Join(j.Differs, ", "))
				if err := r.drain(ctx, sink, player, out); err != nil {
					return err
				}
				next, code := r.open(ctx, env, t.transport, out, stderr)
				if next == nil {
					if err := ctx.Err(); err != nil {
						return err
					}
					return fmt.Errorf("track %d: the sink for its transport did not open (exit %d)", i+1, code)
				}
				_ = player.swap(next, out).Close()
				sink, sinkFrames = next, 0
			}
		}
		n, err := r.feed(ctx, t, sink, player, out)
		at += pcmTestDuration(n, t.transport.RateHz)
		sinkFrames += n
		if err != nil {
			return err
		}
	}
	return r.drain(ctx, player.current(), player, out)
}

// feed writes one track to the sink and returns the frames it took.
func (r pcmTestSinkRun) feed(ctx context.Context, t pcmTestTrack, sink pcmTestSink, player *pcmTestPlayer, out *pcmTestOut) (int64, error) {
	fb := int64(format.PackedBytes(t.transport.Channels, t.transport.Format))
	if t.tone != nil {
		var done int64
		tone := t.tone
		for done < t.frames {
			// From the tone's frame toneFrom+done to the end of its repeat,
			// at most a tenth of a second.
			at := (t.toneFrom + done) % int64(tone.frames)
			n := min(int64(tone.frames)-at, t.frames-done, int64(t.transport.RateHz/10))
			if err := r.write(ctx, sink, tone.packed[at*fb:(at+n)*fb], fb, player, out); err != nil {
				return done, err
			}
			done += n
		}
		return done, nil
	}
	chunk := max(t.transport.RateHz/10, 1)
	samples := make([]int32, chunk*t.transport.Channels)
	packed := make([]byte, int64(chunk)*fb)
	var done int64
	for {
		n, err := t.decoder.ReadFrames(ctx, samples)
		if n > 0 {
			b, perr := format.Pack(packed, samples[:n], t.transport.ValidBits, t.transport.Format)
			if perr != nil {
				return done, perr
			}
			if werr := r.write(ctx, sink, packed[:b], fb, player, out); werr != nil {
				return done, werr
			}
			done += int64(n / t.transport.Channels)
		}
		if errors.Is(err, io.EOF) {
			return done, nil
		}
		if err != nil {
			return done, fmt.Errorf("%s: %w", t.name, err)
		}
	}
}

// write hands b to the sink. An xrun stops the run, unless --recover asks
// for the stream to be prepared again and played on, the gap counted.
func (r pcmTestSinkRun) write(ctx context.Context, sink pcmTestSink, b []byte, fb int64, player *pcmTestPlayer, out *pcmTestOut) error {
	for len(b) > 0 {
		n, err := sink.Write(ctx, b)
		b = b[int64(n)*fb:]
		if err == nil {
			continue
		}
		if alsapcm.ReasonOf(err) != alsapcm.ReasonXrun || !r.recover {
			return err
		}
		r.sayXrun(sink, err, player, out)
		if err := sink.Recover(); err != nil {
			return err
		}
		out.printf("  recovered: playing on from the next frame; the gap stays counted\n")
	}
	return nil
}

// sayXrun reports an xrun: where it stopped the stream and what was held.
func (r pcmTestSinkRun) sayXrun(sink pcmTestSink, err error, player *pcmTestPlayer, out *pcmTestOut) {
	player.mu.Lock()
	player.gaps++
	gaps := player.gaps
	player.mu.Unlock()
	pos := sink.Position()
	rate := sink.Transport().RateHz
	out.printf("xrun %d: %v\n", gaps, err)
	if g := sink.Stats().Gaps; len(g) > 0 {
		last := g[len(g)-1]
		out.printf("  the stream stopped at content frame %d (%s), every frame before it played; %d frames held, none lost\n",
			last.At, pcmTestFrames(last.At, rate), last.Lookahead)
	} else {
		out.printf("  the stream stopped at %s played\n", pcmTestFrames(pos.Played, rate))
	}
}

// drain drains the sink. A paused sink is waited for until it plays again,
// since a pause holds what was submitted; a refusal whose state has changed
// since is asked again, since a resume can come between the refusal and the
// look at the state; an xrun is recovered and the drain tried again when
// --recover allows it.
func (r pcmTestSinkRun) drain(ctx context.Context, sink pcmTestSink, player *pcmTestPlayer, out *pcmTestOut) error {
	for {
		changed := sink.Changed()
		err := sink.Drain(ctx)
		switch {
		case err == nil:
			return nil
		case alsapcm.ReasonOf(err) == alsapcm.ReasonBadState:
			// The state is read before changed is looked at, so a changed
			// still open says the state read is the one the refusal answered.
			paused := pcmTestIsPaused(sink)
			select {
			case <-changed:
				continue
			default:
			}
			if !paused {
				return err
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
		case alsapcm.ReasonOf(err) == alsapcm.ReasonXrun && r.recover:
			r.sayXrun(sink, err, player, out)
			if rerr := sink.Recover(); rerr != nil {
				return rerr
			}
		default:
			return err
		}
	}
}

// resume resumes the sink playing and reports it, and reports a refusal,
// naming whoever holds a busy device. It returns whether the sink plays
// again.
func (r pcmTestSinkRun) resume(player *pcmTestPlayer, out *pcmTestOut, stderr io.Writer) bool {
	s := player.current()
	released := s.Position().State == alsapcm.SinkReleased
	if err := s.Resume(); err != nil {
		fmt.Fprintf(stderr, "output pcm-test: resume: %v\n", err)
		if alsapcm.ReasonOf(err) == alsapcm.ReasonBusy {
			pcmTestNameHolder(alsa.Reader{ProcRoot: r.procRoot}, r.endpoint, stderr)
		}
		return false
	}
	played, _ := player.played()
	if released {
		out.printf("resumed: opened %s again, writing again first what the DAC had not played, from %s\n",
			r.endpoint, pcmTestRunTime(played))
		var report strings.Builder
		printPCMTestParams(&report, s.Params())
		out.printf("%s", report.String())
		return true
	}
	out.printf("resumed at %s\n", pcmTestRunTime(played))
	return true
}

// pcmTestSayPaused reports a pause of the sink playing: how, and where in
// the run.
func pcmTestSayPaused(player *pcmTestPlayer, out *pcmTestOut) {
	played, _ := player.played()
	how := map[string]string{"pause": "by the kernel's PAUSE", "zeros": "by zeros, after what was queued plays", "idle": "before the stream started: nothing more written"}[player.current().Stats().PausedBy]
	out.printf("paused %s, at %s\n", how, pcmTestRunTime(played))
}

// pcmTestIsPaused reports whether the sink waits on a pause.
func pcmTestIsPaused(s pcmTestSink) bool {
	st := s.Position().State
	return st == alsapcm.SinkPaused || st == alsapcm.SinkReleased
}

// finish reports how the run ended and closes the sink. A run with a gap
// does not pass, recovered or not: exit 1.
func (r pcmTestSinkRun) finish(err error, player *pcmTestPlayer, out *pcmTestOut, stderr io.Writer) int {
	s := player.current()
	played, _ := player.played()
	if err != nil {
		switch alsapcm.ReasonOf(err) {
		case alsapcm.ReasonXrun:
			r.sayXrun(s, err, player, out)
			out.printf("  stopped at the xrun; --recover plays on after one, counting the gap\n")
		case alsapcm.ReasonDisconnected:
			out.printf("the device went away after %s played: the sink closed it\n", pcmTestRunTime(played))
		default:
			out.printf("stopped after %s played\n", pcmTestRunTime(played))
		}
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return pcmTestCloseSink(s, 1, out, stderr)
	}
	st := s.Stats()
	player.mu.Lock()
	gaps := player.gaps
	player.mu.Unlock()
	out.printf("drained: %s played; gaps %d\n", pcmTestRunTime(played), gaps)
	out.printf("  the last sink: pauses %d, releases %d, reopens %d; %d writes (%d partial), %d waits for room, %d interrupted; %d frames of silence\n",
		st.Pauses, st.Releases, st.Reopens, st.Counters.Writes, st.Counters.Partial, st.Counters.Waits, st.Counters.Interrupted, st.Silence)
	code := 0
	if gaps > 0 {
		out.printf("  %d gaps, each an xrun recovered and counted: not a clean run\n", gaps)
		code = 1
	}
	return pcmTestCloseSink(s, code, out, stderr)
}

// tellStarts says when a gapless track's first frame reaches the DAC: when
// the sink now playing, the one it starts in, has played it.
func (p *pcmTestPlayer) tellStarts(out *pcmTestOut) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tellStartsLocked(p.sink, p.sink.Position().Played, out)
}

// tellStartsLocked tells the starts in sink that played reaches, with p.mu
// held.
func (p *pcmTestPlayer) tellStartsLocked(sink pcmTestSink, played int64, out *pcmTestOut) {
	for i := range p.starts {
		if s := &p.starts[i]; !s.told && s.sink == sink && played >= s.frame {
			s.told = true
			out.printf("  track %d reached the DAC: its first frame, frame %d of its sink, has played\n", s.track, s.frame)
		}
	}
}

// pcmTestCloseSink closes the sink and says so, keeping code unless the
// close fails.
func pcmTestCloseSink(s pcmTestSink, code int, out *pcmTestOut, stderr io.Writer) int {
	if err := s.Close(); err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return 1
	}
	out.printf("closed\n")
	return code
}

// pcmTestFrames is a frame count and its time at rate.
func pcmTestFrames(frames int64, rate int) string {
	if rate <= 0 {
		return fmt.Sprintf("%d frames", frames)
	}
	return fmt.Sprintf("%.3f s (%d frames)", float64(frames)/float64(rate), frames)
}

// pcmTestDuration is frames' time at rate, rounded down to the nanosecond.
// The whole seconds and the remainder are counted apart: frames times a
// second's nanoseconds passes int64 at 9.22e9 frames, 3 h 20 min into a sink
// at 768 kHz.
func pcmTestDuration(frames int64, rate int) time.Duration {
	if rate <= 0 {
		return 0
	}
	r := int64(rate)
	return time.Duration(frames/r)*time.Second + time.Duration(frames%r)*time.Second/time.Duration(r)
}

// pcmTestRunTime is the run's content time, which may span sinks of
// different rates, so it has no one frame count.
func pcmTestRunTime(d time.Duration) string { return fmt.Sprintf("%.3f s", d.Seconds()) }

// errPCMTestKeysStopped ends the key reader's read when the run ends.
var errPCMTestKeysStopped = errors.New("the run has ended")

// pcmTestKeys reads commands from in, one a line, trimmed and lowercased,
// until in ends or stop is called. stop ends the reader and waits for it,
// so the reader never outlives the run, cutting short a read under way:
// standard input, a file, is read through poll with a wake-up
// (pcmTestStoppableFile), and a pipe's read end is closed. A reader of
// another kind cannot be cut short, and stop leaves it to its read.
func pcmTestKeys(in io.Reader) (<-chan string, func()) {
	interrupt, release, joinable := func() {}, func() {}, false
	switch r := in.(type) {
	case *os.File:
		if pr, intr, rel, ok := pcmTestStoppableFile(r); ok {
			in, interrupt, release, joinable = pr, intr, rel, true
		}
	case interface{ CloseWithError(error) error }:
		interrupt, joinable = func() { _ = r.CloseWithError(errPCMTestKeysStopped) }, true
	}
	keys := make(chan string)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		defer close(keys)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			select {
			case keys <- strings.ToLower(strings.TrimSpace(sc.Text())):
			case <-done:
				return
			}
		}
	}()
	stop := func() {
		close(done)
		interrupt()
		if joinable {
			<-exited
			release()
		}
	}
	return keys, stop
}
