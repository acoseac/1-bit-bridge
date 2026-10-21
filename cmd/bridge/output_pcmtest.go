package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The test tone: a 1 kHz sine, never louder than pcmTestLoudest dBFS,
// because the hardware gate is run on headphones.
const (
	pcmTestToneHz   = 1000
	pcmTestLoudest  = -30.0
	pcmTestQuietest = -90.0
)

// pcmTestDevice is what the harness drives: an *alsapcm.Device, or a
// test's stand-in.
type pcmTestDevice interface {
	Params() alsapcm.Params
	Counters() alsapcm.Counters
	Write(ctx context.Context, frames []byte) (int, error)
	Status() (alsapcm.Status, error)
	Drain(ctx context.Context, timeout time.Duration) error
	Drop() error
	Close() error
}

// outputPCMTestEnv is what `bridge output pcm-test` takes from the host,
// so a test can stand in for each: the OS it refuses off Linux, the opener
// of an endpoint and of a sink, standard input (read only with --keys),
// and the ticks the sink mode reports and runs its timers on.
type outputPCMTestEnv struct {
	goos     string
	open     func(alsapcm.Endpoint, alsapcm.Config) (pcmTestDevice, error)
	openSink func(context.Context, alsapcm.SinkConfig) (pcmTestSink, error)
	stdin    io.Reader
	ticks    func(time.Duration) (<-chan time.Time, func())
}

// outputPCMTestCmd implements `bridge output pcm-test` on this host.
func outputPCMTestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runOutputPCMTest(ctx, args, outputPCMTestEnv{
		goos: runtime.GOOS,
		open: func(ep alsapcm.Endpoint, cfg alsapcm.Config) (pcmTestDevice, error) {
			d, err := alsapcm.Open(ep, cfg)
			if err != nil {
				return nil, err
			}
			return d, nil
		},
		openSink: func(ctx context.Context, cfg alsapcm.SinkConfig) (pcmTestSink, error) {
			s, err := alsapcm.OpenSink(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		stdin: os.Stdin,
		ticks: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	}, stdout, stderr)
}

// outputPCMTestUsage is pcm-test's own help. `bridge output` does not list
// the subcommand: it is the B470 hardware harness, run by hand.
func outputPCMTestUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `bridge output pcm-test --card N --device N --rate HZ --bits 16|24|32 [--format F] [--seconds S] [--level DBFS]
bridge output pcm-test --sink --card N --device N --rate HZ --bits B [--tracks N] [sink flags]
bridge output pcm-test --file PATH [--file PATH] --ack-full-level --card N --device N [--format F] [sink flags]
bridge output pcm-test --passive --card N [--device N] [--proc-root DIR]

The bridge player's ALSA backend, by hand (B470 PR 2a's and PR 2b's hardware
gates). It opens /dev/snd/pcmC<card>D<device>p, the card and device given by
number, never by a name: no default, hw:, plughw:, dmix, softvol, alias or
path, and no .asoundrc. It asks for the rate, two channels and the format
exactly and prints what the kernel committed; anything else is refused, never
played. It then plays a quiet 1 kHz sine, at most -30 dBFS, on both channels,
and reports the delay, the writes and any xrun, then drains and closes. Ctrl-C
stops it at once: the stream is dropped and the device closed.

--sink plays the tone through the streaming sink instead (B470 PR 2b): two
seconds decoded ahead, the position the kernel establishes, pause and resume,
the release of a paused device, and xruns reported. --tracks 2 splits the
tone into two tracks joined back to back mid-cycle: a gapless join is no
click. --file decodes a file (FLAC, WAV, AIFF or ALAC) and plays it through
the sink bit-exact, at the level it was mastered at, which the bridge never
lowers: turn the volume down first and pass --ack-full-level. Two --file
flags play back to back, with nothing between when their transport matches,
and with a drain and a reopen, an honest gap, when it does not.

Sink flags: --pause-at S pauses once S seconds have played, --resume-after S
resumes S seconds later; --keys reads p (pause or resume) and q (stop) from
standard input, one a line. --pause auto|pause|zeros chooses the kernel's
PAUSE or zeros (auto: PAUSE when the hardware offers it). A pause releases
the device after --release-after seconds (30), and the resume opens it again.
An xrun stops the run unless --recover asks to play on, the gap counted; a
run with a gap exits 1.

A device another process holds (MPD playing) is refused as busy, naming the
holder, and so is a resume after a release. The bridge never stops MPD or
touches its configuration: stop the player first.

--passive opens nothing: it prints the card's /proc/asound files (stream,
hw_params, sw_params, status) as the kernel wrote them.

Linux only. It is not part of the server and not reachable over HTTP.

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// runOutputPCMTest is `bridge output pcm-test`.
//
// Exit codes: 0 the tone or the files played and drained, with no gap, or
// Ctrl-C (or q) stopped them and the device was dropped and closed, or
// --passive printed the card; 1 the endpoint or a file was refused, a
// resume was refused, the stream failed, or a recovered xrun left a gap; 2
// usage error.
func runOutputPCMTest(ctx context.Context, args []string, env outputPCMTestEnv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("output pcm-test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	card := fs.Int("card", -1, "the card `number`: the N of /proc/asound/cardN (never a name)")
	device := fs.Int("device", -1, "the PCM device `number` on that card: the D of pcmC<N>D<D>p")
	rate := fs.Int("rate", 0, "the frame rate to open at, exactly, in `Hz`")
	bits := fs.Int("bits", 0, "the tone's valid bits: 16, 24 or 32")
	alsaFormat := fs.String("format", "", "the ALSA `format` to pack them in: S16_LE, S24_3LE, S24_LE or S32_LE (default: S16_LE for 16 bits, S24_3LE for 24, S32_LE for 32)")
	seconds := fs.Int("seconds", 10, "how long to play, 1 to 600")
	level := fs.Float64("level", pcmTestLoudest, "the tone's peak in `dBFS`, -90 to -30")
	passive := fs.Bool("passive", false, "open nothing: print the card's /proc/asound files")
	procRoot := fs.String("proc-root", "", "the /proc tree --passive and a busy device's holder are read from (default /proc)")
	sinkFlag := fs.Bool("sink", false, "play the tone through the streaming sink (B470 PR 2b)")
	var sf pcmTestSinkFlags
	fs.Var(&sf.files, "file", "a `file` to decode and play through the sink, bit-exact; give it twice to play two back to back")
	fs.BoolVar(&sf.ackFullLevel, "ack-full-level", false, "with --file: the volume is down; files play at their own level, never lowered")
	fs.IntVar(&sf.tracks, "tracks", 1, "with --sink: the tone in this many tracks joined back to back, 1 to 8")
	fs.StringVar(&sf.pause, "pause", "auto", "how the sink pauses: auto, pause (the kernel's PAUSE) or zeros")
	fs.Float64Var(&sf.pauseAt, "pause-at", 0, "pause once this many `seconds` have played")
	fs.Float64Var(&sf.resumeAfter, "resume-after", 0, "resume this many `seconds` after the pause")
	fs.Float64Var(&sf.releaseAfter, "release-after", 30, "release a paused device after this many `seconds`, 1 to 600")
	fs.BoolVar(&sf.keys, "keys", false, "read p (pause or resume) and q (stop) from standard input, one a line")
	fs.BoolVar(&sf.recover, "recover", false, "after an xrun, prepare the stream and play on, counting the gap")
	fs.Usage = func() { outputPCMTestUsage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "output pcm-test: unexpected argument %q: the endpoint is --card and --device, by number\n", fs.Arg(0))
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *card < 0 {
		fmt.Fprintln(stderr, "output pcm-test: --card is required, a number from 0 (the N of /proc/asound/cardN)")
		return 2
	}
	root := *procRoot
	if *passive {
		for _, name := range []string{"rate", "bits", "format", "seconds", "level", "sink", "file", "ack-full-level", "tracks",
			"pause", "pause-at", "resume-after", "release-after", "keys", "recover"} {
			if set[name] {
				fmt.Fprintf(stderr, "output pcm-test: --passive opens nothing and plays nothing, and takes no --%s\n", name)
				return 2
			}
		}
		if root == "" {
			if env.goos != "linux" {
				fmt.Fprintf(stderr, "output pcm-test: ALSA is Linux only, and this is %s\n", env.goos)
				fmt.Fprintln(stderr, "  --proc-root reads a /proc tree captured on a Linux box.")
				return 1
			}
			root = alsa.DefaultProcRoot
		}
		dev := -1
		if set["device"] {
			dev = *device
		}
		return printPCMTestPassive(alsa.Reader{ProcRoot: root}, *card, dev, stdout, stderr)
	}
	sink, problem := pcmTestSinkModeFrom(*sinkFlag, sf, set)
	if problem != "" {
		fmt.Fprintln(stderr, "output pcm-test: "+problem)
		return 2
	}
	if root == "" {
		root = alsa.DefaultProcRoot
	}
	if len(sf.files) > 0 {
		return runPCMTestFiles(ctx, env, sf, *card, *device, *alsaFormat, root, stdout, stderr)
	}
	run, code := pcmTestRunFrom(*device, *rate, *bits, *alsaFormat, *seconds, *level)
	if code != 0 {
		fmt.Fprintln(stderr, "output pcm-test: "+run.problem)
		return code
	}
	if env.goos != "linux" {
		fmt.Fprintf(stderr, "output pcm-test: ALSA is Linux only, and this is %s\n", env.goos)
		return 1
	}
	run.endpoint = alsapcm.Endpoint{Card: *card, Device: *device}
	run.procRoot = root
	if sink {
		return run.playThroughSink(ctx, env, sf, stdout, stderr)
	}
	return run.play(ctx, env, stdout, stderr)
}

// pcmTestRun is one active run's request, checked.
type pcmTestRun struct {
	endpoint alsapcm.Endpoint
	procRoot string
	rate     int
	bits     int
	format   format.ALSAFormat
	seconds  int
	level    float64
	problem  string // why the flags were refused
}

// pcmTestRunFrom checks an active run's flags. code is 2, with problem
// set, for flags that do not make one.
func pcmTestRunFrom(device, rate, bits int, alsaFormat string, seconds int, level float64) (pcmTestRun, int) {
	bad := func(format string, args ...any) (pcmTestRun, int) {
		return pcmTestRun{problem: fmt.Sprintf(format, args...)}, 2
	}
	if device < 0 {
		return bad("--device is required, a number from 0 (the D of /dev/snd/pcmC<card>D<D>p)")
	}
	if rate <= 0 {
		return bad("--rate is required: the frame rate in Hz, exactly as the DAC is to run")
	}
	// Checked here, before the tone is built: its table is the frames one
	// 1 kHz cycle takes to repeat exactly, which for a rate sharing no
	// factor with 1000 is the rate itself.
	if rate < alsapcm.MinRateHz || rate > alsapcm.MaxRateHz {
		return bad("--rate is %d to %d Hz, not %d", alsapcm.MinRateHz, alsapcm.MaxRateHz, rate)
	}
	f := format.ALSAFormat(alsaFormat)
	switch {
	case bits != 16 && bits != 24 && bits != 32:
		return bad("--bits is 16, 24 or 32, not %d", bits)
	case f == "":
		f = map[int]format.ALSAFormat{16: format.S16LE, 24: format.S243LE, 32: format.S32LE}[bits]
	}
	if !pcmTestPacks(f, bits) {
		return bad("--format %s does not carry %d-bit samples: S16_LE takes 16, S24_3LE and S24_LE take 24, S32_LE takes 24 or 32", alsaFormat, bits)
	}
	if seconds < 1 || seconds > 600 {
		return bad("--seconds is 1 to 600, not %d", seconds)
	}
	if math.IsNaN(level) || level > pcmTestLoudest || level < pcmTestQuietest {
		return bad("--level is %g to %g dBFS, not %g: the tone is never louder than %g dBFS", pcmTestQuietest, pcmTestLoudest, level, pcmTestLoudest)
	}
	return pcmTestRun{rate: rate, bits: bits, format: f, seconds: seconds, level: level}, 0
}

// pcmTestPacks reports whether the format package's packer writes bits-bit
// samples as f.
func pcmTestPacks(f format.ALSAFormat, bits int) bool {
	switch f {
	case format.S16LE:
		return bits == 16
	case format.S243LE, format.S24LE:
		return bits == 24
	case format.S32LE:
		return bits == 24 || bits == 32
	}
	return false
}

// pcmTestTone is the test tone packed for one run: the frames a 1 kHz sine
// at the run's rate takes to come back to its first sample exactly, so the
// tone repeats them without a seam.
type pcmTestTone struct {
	packed    []byte
	frames    int
	amplitude int64
	fullScale int64
}

// buildPCMTestTone builds the tone for a rate, valid bits, format and
// level: a sine whose peak is the level's fraction of full scale, rounded
// down, on both channels, packed by the format package's packer.
func buildPCMTestTone(rate, bits int, f format.ALSAFormat, level float64) (pcmTestTone, error) {
	frames := rate / gcd(rate, pcmTestToneHz)
	full := int64(1)<<(bits-1) - 1
	amp := int64(math.Floor(float64(full) * math.Pow(10, level/20)))
	samples := make([]int32, 2*frames)
	for n := range frames {
		v := int32(math.Round(float64(amp) * math.Sin(2*math.Pi*pcmTestToneHz*float64(n)/float64(rate))))
		samples[2*n], samples[2*n+1] = v, v
	}
	packed := make([]byte, format.PackedBytes(len(samples), f))
	if _, err := format.Pack(packed, samples, bits, f); err != nil {
		return pcmTestTone{}, err
	}
	return pcmTestTone{packed: packed, frames: frames, amplitude: amp, fullScale: full}, nil
}

// gcd is the greatest common divisor of two positive integers.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// play is one active run: open, check against the kernel's own file, play
// the tone, drain, close.
func (r pcmTestRun) play(ctx context.Context, env outputPCMTestEnv, stdout, stderr io.Writer) int {
	tone, err := buildPCMTestTone(r.rate, r.bits, r.format, r.level)
	if err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return 1
	}
	dev, err := env.open(r.endpoint, alsapcm.Config{RateHz: r.rate, Format: r.format})
	if err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		if alsapcm.ReasonOf(err) == alsapcm.ReasonBusy {
			pcmTestNameHolder(alsa.Reader{ProcRoot: r.procRoot}, r.endpoint, stderr)
		}
		return 1
	}
	p := dev.Params()
	printPCMTestParams(stdout, p)
	if !pcmTestKernelAgrees(alsa.Reader{ProcRoot: r.procRoot}, p, stdout) {
		fmt.Fprintln(stderr, "output pcm-test: the kernel's own hw_params file disagrees with what the backend read back: not playing")
		_ = dev.Close()
		return 1
	}
	fmt.Fprintf(stdout, "tone: %d Hz sine on both channels, peak %.1f dBFS (%d of %d), %d valid bits packed as %s\n",
		pcmTestToneHz, r.level, tone.amplitude, tone.fullScale, r.bits, r.format)
	total := r.seconds * r.rate
	fmt.Fprintf(stdout, "playing %d s (%d frames); Ctrl-C stops at once\n", r.seconds, total)

	// A chunk of whole tone repeats, about a tenth of a second: the
	// waveform runs on from one chunk into the next.
	reps := max(1, (r.rate/10+tone.frames-1)/tone.frames)
	chunk := []byte(strings.Repeat(string(tone.packed), reps))
	chunkFrames := reps * tone.frames
	frameBytes := p.FrameBytes()
	written, nextReport := 0, r.rate
	for written < total {
		n := min(chunkFrames, total-written)
		k, err := dev.Write(ctx, chunk[:n*frameBytes])
		written += k
		if err != nil {
			return pcmTestStopped(dev, err, written, r.rate, stdout, stderr)
		}
		for written >= nextReport && nextReport <= total {
			pcmTestProgress(dev, nextReport/r.rate, written, r.rate, stdout)
			nextReport += r.rate
		}
	}
	c := dev.Counters()
	fmt.Fprintf(stdout, "done: %d frames in %d writes (%d partial), %d waits for room, %d interrupted\n",
		written, c.Writes, c.Partial, c.Waits, c.Interrupted)
	if st, err := dev.Status(); err == nil {
		fmt.Fprintf(stdout, "delay before drain: %d frames (%s), %s\n", st.Delay, pcmTestMillis(st.Delay, r.rate), st.State)
	}
	// No xrun is claimed before the drain has said so: an underrun after
	// the last write is the drain's to report (#1301 review).
	if err := dev.Drain(ctx, p.BufferTime()+2*time.Second); err != nil {
		switch alsapcm.ReasonOf(err) {
		case alsapcm.ReasonCancelled:
			fmt.Fprintln(stdout, "stopped by signal while draining: dropped")
			return pcmTestClose(dev, 0, stdout, stderr)
		case alsapcm.ReasonXrun:
			fmt.Fprintln(stdout, "xruns 1: the stream underran before the drain")
		}
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return pcmTestClose(dev, 1, stdout, stderr)
	}
	fmt.Fprintln(stdout, "drained; xruns 0")
	return pcmTestClose(dev, 0, stdout, stderr)
}

// pcmTestStopped reports a write that ended early: Ctrl-C (exit 0, the
// stream dropped and the device closed) or a failure, an xrun counted as
// one (exit 1). Recovering from an xrun is the streaming sink's (B470 PR
// 2b), not this harness's.
func pcmTestStopped(dev pcmTestDevice, err error, written, rate int, stdout, stderr io.Writer) int {
	reason := alsapcm.ReasonOf(err)
	if reason == alsapcm.ReasonCancelled {
		dropped := "dropped"
		if derr := dev.Drop(); derr != nil {
			dropped = "drop failed: " + derr.Error()
		}
		fmt.Fprintf(stdout, "stopped by signal after %d frames (%s): %s\n", written, pcmTestMillis(int64(written), rate), dropped)
		return pcmTestClose(dev, 0, stdout, stderr)
	}
	fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
	xruns := 0
	if reason == alsapcm.ReasonXrun {
		xruns = 1
	}
	c := dev.Counters()
	fmt.Fprintf(stdout, "stopped after %d frames in %d writes (%d partial), %d waits for room, %d interrupted; xruns %d\n",
		written, c.Writes, c.Partial, c.Waits, c.Interrupted, xruns)
	if reason == alsapcm.ReasonXrun {
		fmt.Fprintln(stdout, "  the stream stopped at the underrun; recovering from one is the streaming sink's (B470 PR 2b)")
	}
	_ = dev.Drop()
	return pcmTestClose(dev, 1, stdout, stderr)
}

// pcmTestClose closes the device and says so, keeping code unless the
// close fails.
func pcmTestClose(dev pcmTestDevice, code int, stdout, stderr io.Writer) int {
	if err := dev.Close(); err != nil {
		fmt.Fprintf(stderr, "output pcm-test: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "closed")
	return code
}

// pcmTestProgress prints one second's line: frames so far and the kernel's
// delay and state.
func pcmTestProgress(dev pcmTestDevice, second, written, rate int, stdout io.Writer) {
	st, err := dev.Status()
	if err != nil {
		fmt.Fprintf(stdout, "  %d s: %d frames written; status: %v\n", second, written, err)
		return
	}
	fmt.Fprintf(stdout, "  %d s: %d frames written, delay %d frames (%s), %s\n", second, written, st.Delay, pcmTestMillis(st.Delay, rate), st.State)
}

// pcmTestMillis is a frame count at rate in milliseconds.
func pcmTestMillis(frames int64, rate int) string {
	return strconv.FormatFloat(float64(frames)*1000/float64(rate), 'f', 1, 64) + " ms"
}

// printPCMTestParams prints what the kernel committed.
func printPCMTestParams(w io.Writer, p alsapcm.Params) {
	fmt.Fprintf(w, "endpoint: %s (%s), subdevice %d: %s [%s]\n", p.Endpoint, p.Endpoint.Path(), p.Subdevice, p.ID, p.Name)
	fmt.Fprintf(w, "kernel PCM protocol %s (this backend opens %s and newer; its definitions follow Linux %s)\n",
		alsapcm.ProtocolString(p.Protocol), alsapcm.ProtocolString(alsapcm.ProtocolFloor()), alsapcm.PinnedKernel())
	fmt.Fprintf(w, "committed exactly: %s, %d channels, %d Hz (%d/%d), RW_INTERLEAVED\n", p.Format, p.Channels, p.RateHz, p.RateNum, p.RateDen)
	if p.Msbits > 0 {
		fmt.Fprintf(w, "  significant bits: %d\n", p.Msbits)
	}
	fmt.Fprintf(w, "  buffer: %d frames (%s) in %d periods of %d frames (%s)\n",
		p.BufferFrames, pcmTestMillis(int64(p.BufferFrames), p.RateHz), p.Periods, p.PeriodFrames, pcmTestMillis(int64(p.PeriodFrames), p.RateHz))
	fmt.Fprintf(w, "  starts when %d frames are buffered; stops, an xrun, when %d are free (an underrun is never played); wakes every %d frames\n",
		p.StartThreshold, p.StopThreshold, p.AvailMin)
	flags := strings.Join(p.InfoFlags(), ", ")
	if flags == "" {
		flags = "none of pause, resume, batch, block_transfer"
	}
	fmt.Fprintf(w, "  hardware: %s (info %#x)\n", flags, p.Info)
}

// pcmTestKernelAgrees reads the substream's hw_params as the kernel prints
// it and compares it with what the backend read back, printing both. A
// file that cannot be read is said and is no disagreement; a value that
// differs is.
func pcmTestKernelAgrees(r alsa.Reader, p alsapcm.Params, w io.Writer) bool {
	where := fmt.Sprintf("card%d/pcm%dp/sub%d/hw_params", p.Endpoint.Card, p.Endpoint.Device, p.Subdevice)
	hw, err := pcmTestHWParams(r, p.Endpoint, p.Subdevice)
	if err != nil {
		fmt.Fprintf(w, "the kernel's %s: not read (%v)\n", where, err)
		return true
	}
	var diffs []string
	check := func(name, got, want string) {
		if got != want {
			diffs = append(diffs, fmt.Sprintf("%s %s, read back %s", name, got, want))
		}
	}
	check("access", hw.Access, "RW_INTERLEAVED")
	check("format", hw.Format, string(p.Format))
	check("channels", intOr(hw.Channels, "?"), strconv.Itoa(p.Channels))
	check("rate", intOr(hw.Rate, "?"), strconv.Itoa(p.RateHz))
	check("period_size", int64Or(hw.PeriodSize), strconv.Itoa(p.PeriodFrames))
	check("buffer_size", int64Or(hw.BufferSize), strconv.Itoa(p.BufferFrames))
	fmt.Fprintf(w, "the kernel's %s: %s, %s ch, %s Hz, period %s, buffer %s: ", where,
		orUnknown(hw.Format), intOr(hw.Channels, "?"), intOr(hw.Rate, "?"), int64Or(hw.PeriodSize), int64Or(hw.BufferSize))
	if len(diffs) > 0 {
		fmt.Fprintf(w, "DIFFERS (%s)\n", strings.Join(diffs, "; "))
		return false
	}
	fmt.Fprintln(w, "agrees")
	return true
}

// pcmTestHWParams finds one substream's hw_params in a reading.
func pcmTestHWParams(r alsa.Reader, ep alsapcm.Endpoint, sub int) (*alsa.HWParams, error) {
	snap, err := r.Read()
	if err != nil {
		return nil, err
	}
	for _, c := range snap.Cards {
		if c.Index != ep.Card {
			continue
		}
		for _, d := range c.Devices {
			if d.Device != ep.Device {
				continue
			}
			for _, s := range d.Substreams {
				if s.Subdevice == sub && s.HWParams != nil && !s.HWParams.Closed && !s.HWParams.NoSetup {
					return s.HWParams, nil
				}
			}
		}
	}
	return nil, errors.New("no open substream there in the reading")
}

// int64Or renders an optional count.
func int64Or(n *int64) string {
	if n == nil {
		return "?"
	}
	return strconv.FormatInt(*n, 10)
}

// pcmTestNameHolder says who holds a busy endpoint, from the passive
// reading of its substreams' status, and that the bridge leaves it alone.
func pcmTestNameHolder(r alsa.Reader, ep alsapcm.Endpoint, w io.Writer) {
	var holders []string
	if snap, err := r.Read(); err == nil {
		for _, c := range snap.Cards {
			for _, d := range c.Devices {
				if c.Index != ep.Card || d.Device != ep.Device {
					continue
				}
				for _, s := range d.Substreams {
					if s.Owner != nil {
						holders = append(holders, fmt.Sprintf("sub %d: %s", s.Subdevice, ownerSummary(*s.Owner)))
					}
				}
			}
		}
	}
	if len(holders) == 0 {
		fmt.Fprintln(w, "  held by: not named in /proc/asound (another user's process, or one outside this pid namespace)")
	}
	for _, h := range holders {
		fmt.Fprintf(w, "  held by %s\n", h)
	}
	fmt.Fprintln(w, "  The bridge does not stop MPD or change its configuration: stop playback in the player and run this again.")
}

// printPCMTestPassive is --passive: the card's /proc/asound files as the
// kernel wrote them, each under a "==> path <==" line as `bridge output
// dac --raw` prints them, and then the card read the way `bridge output
// dac` reads it. device, when not -1, keeps that device's files alone.
// Nothing is opened.
func printPCMTestPassive(r alsa.Reader, card, device int, stdout, stderr io.Writer) int {
	files, err := r.Raw()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	snap, err := r.Read()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	var reading *alsa.CardReading
	for i := range snap.Cards {
		if snap.Cards[i].Index == card {
			reading = &snap.Cards[i]
		}
	}
	if reading == nil {
		fmt.Fprintf(stderr, "output pcm-test: no card %d in %s/asound/cards\n", card, snap.ProcRoot)
		return 1
	}
	prefix := "asound/card" + strconv.Itoa(card) + "/"
	fmt.Fprintf(stdout, "passive: nothing is opened; %s as the kernel printed it\n", strings.TrimSuffix(prefix, "/"))
	for _, f := range files {
		rel, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || (device >= 0 && !pcmTestDeviceFile(rel, device)) {
			continue
		}
		fmt.Fprintf(stdout, "==> %s <==\n", f.Path)
		if f.Err != nil {
			fmt.Fprintf(stdout, "(unreadable: %v)\n", f.Err)
			continue
		}
		_, _ = stdout.Write(f.Data)
		if len(f.Data) > 0 && f.Data[len(f.Data)-1] != '\n' {
			fmt.Fprint(stdout, "\n\\ No newline at end of file\n")
		}
	}
	fmt.Fprintln(stdout, "---")
	renderCard(stdout, *reading)
	return 0
}

// pcmTestDeviceFile reports whether a file under a card's directory is the
// given device's (its stream file or its playback directory) or the card's
// own (usbid, id).
func pcmTestDeviceFile(rel string, device int) bool {
	d := strconv.Itoa(device)
	cardOwn := !strings.ContainsRune(rel, '/') && !strings.HasPrefix(rel, "stream")
	return rel == "stream"+d || strings.HasPrefix(rel, "pcm"+d+"p/") || cardOwn
}
