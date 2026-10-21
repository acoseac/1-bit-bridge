package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// fakeSink is the sink in the harness's tests: it keeps what it is given,
// plays it at once unless a test sets what has played, and answers as
// scripted, with no kernel behind it. Its Drain waits for the test to open
// gate, and, like the real sink's, for a resume while it is paused.
type fakeSink struct {
	mu       sync.Mutex
	cfg      alsapcm.SinkConfig
	params   alsapcm.Params
	written  []byte
	played   int64 // what the fake says has played: setPlayed, and all of it once drained
	state    alsapcm.SinkState
	err      error
	pausedBy string
	gaps     []alsapcm.Gap
	draining bool
	closes   int
	changed  chan struct{}
	dropped  chan struct{}
	gate     chan struct{}
	calls    chan string

	// Scripts.
	xrunAt    int64 // Write stops at this frame with an xrun, once
	failAt    int64 // with failErr: Write stops at this frame and the sink ends
	failErr   error
	resumeErr []error // Resume's answers, in turn, before one that resumes
	// Hooks, set before the run has the sink: enterDrain is called as each
	// Drain begins, and refused once a Drain has refused a paused sink, just
	// before it returns the refusal, so a test can hold the producer there.
	enterDrain, refused func()
}

// newFakeSink is a sink opened for cfg, playing, with its drain gate shut.
func newFakeSink(cfg alsapcm.SinkConfig) *fakeSink {
	t := cfg.Transport
	return &fakeSink{
		cfg: cfg,
		params: alsapcm.Params{
			Endpoint: cfg.Endpoint, ID: "USB Audio", Name: "USB Audio", Protocol: 2<<16 | 18,
			Format: t.Format, Channels: t.Channels, RateHz: t.RateHz, RateNum: uint32(t.RateHz), RateDen: 1,
			PeriodFrames: t.RateHz / 20, Periods: 4, BufferFrames: t.RateHz / 5,
			StartThreshold: t.RateHz / 5, StopThreshold: t.RateHz / 5, AvailMin: t.RateHz / 20, Info: 0x90000,
		},
		changed: make(chan struct{}),
		dropped: make(chan struct{}),
		gate:    make(chan struct{}),
		calls:   make(chan string, 100),
	}
}

func (f *fakeSink) frames() int64 { return int64(len(f.written) / f.params.FrameBytes()) }

// setState moves the fake to st, with f.mu held.
func (f *fakeSink) setState(st alsapcm.SinkState, err error) {
	f.state, f.err = st, err
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeSink) call(name string) { f.calls <- name }

func (f *fakeSink) Params() alsapcm.Params                { return f.params }
func (f *fakeSink) Transport() alsapcm.Transport          { return f.cfg.Transport }
func (f *fakeSink) Next(t alsapcm.Transport) alsapcm.Join { return f.cfg.Transport.Join(t) }

func (f *fakeSink) Write(ctx context.Context, b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb := f.params.FrameBytes()
	switch {
	case ctx.Err() != nil:
		return 0, &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "sink write", Err: ctx.Err()}
	case f.state == alsapcm.SinkXrun || f.state == alsapcm.SinkEnded:
		return 0, f.err
	case f.state == alsapcm.SinkStopped:
		return 0, &alsapcm.Error{Reason: alsapcm.ReasonClosed, Op: "sink write", Detail: "the sink has stopped"}
	case f.draining:
		return 0, &alsapcm.Error{Reason: alsapcm.ReasonBadState, Op: "sink write", Detail: "draining"}
	}
	n := int64(len(b) / fb)
	at := f.frames()
	switch {
	case f.xrunAt > 0 && at+n >= f.xrunAt:
		take := f.xrunAt - at
		f.written = append(f.written, b[:take*int64(fb)]...)
		f.gaps = append(f.gaps, alsapcm.Gap{At: f.xrunAt, Lookahead: n - take, Op: "write"})
		f.xrunAt = 0
		f.setState(alsapcm.SinkXrun, &alsapcm.Error{Reason: alsapcm.ReasonXrun, Op: "write", Endpoint: f.cfg.Endpoint, Detail: "the buffer ran empty"})
		return int(take), f.err
	case f.failErr != nil && at+n >= f.failAt:
		take := f.failAt - at
		f.written = append(f.written, b[:take*int64(fb)]...)
		f.setState(alsapcm.SinkEnded, f.failErr)
		return int(take), f.err
	}
	f.written = append(f.written, b...)
	return int(n), nil
}

func (f *fakeSink) Pause() error {
	f.call("pause")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == alsapcm.SinkPlaying || f.state == alsapcm.SinkDraining {
		f.pausedBy = "pause"
		f.setState(alsapcm.SinkPaused, nil)
	}
	return nil
}

func (f *fakeSink) Resume() error {
	f.call("resume")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resumeErr) > 0 {
		err := f.resumeErr[0]
		f.resumeErr = f.resumeErr[1:]
		return err
	}
	if f.state == alsapcm.SinkPaused || f.state == alsapcm.SinkReleased {
		st := alsapcm.SinkPlaying
		if f.draining {
			st = alsapcm.SinkDraining
		}
		f.setState(st, nil)
	}
	return nil
}

func (f *fakeSink) Recover() error {
	f.call("recover")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != alsapcm.SinkXrun {
		return &alsapcm.Error{Reason: alsapcm.ReasonBadState, Op: "recover"}
	}
	f.gaps[len(f.gaps)-1].Recovered = time.Unix(1, 0)
	f.setState(alsapcm.SinkPlaying, nil)
	return nil
}

func (f *fakeSink) Drain(ctx context.Context) error {
	if f.enterDrain != nil {
		f.enterDrain()
	}
	f.call("drain")
	f.mu.Lock()
	switch f.state {
	case alsapcm.SinkPaused, alsapcm.SinkReleased:
		f.mu.Unlock()
		if f.refused != nil {
			f.refused()
		}
		return &alsapcm.Error{Reason: alsapcm.ReasonBadState, Op: "drain", Detail: "paused: resume or drop it"}
	case alsapcm.SinkStopped:
		f.mu.Unlock()
		return &alsapcm.Error{Reason: alsapcm.ReasonClosed, Op: "drain", Detail: "the sink has stopped"}
	case alsapcm.SinkXrun, alsapcm.SinkEnded:
		defer f.mu.Unlock()
		return f.err
	}
	f.draining = true
	f.setState(alsapcm.SinkDraining, nil)
	f.mu.Unlock()
	for {
		f.mu.Lock()
		changed, paused := f.changed, f.state == alsapcm.SinkPaused
		f.mu.Unlock()
		if !paused {
			select {
			case <-f.gate:
				f.mu.Lock()
				if f.state == alsapcm.SinkDraining {
					f.played = f.frames()
					f.setState(alsapcm.SinkStopped, nil)
					f.mu.Unlock()
					return nil
				}
				f.mu.Unlock()
				continue
			case <-f.dropped:
				return &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "drain", Detail: "dropped while draining"}
			case <-ctx.Done():
				return &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "drain", Err: ctx.Err()}
			case <-changed:
				continue
			}
		}
		select {
		case <-changed:
		case <-f.dropped:
			return &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "drain", Detail: "dropped while draining"}
		case <-ctx.Done():
			return &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "drain", Err: ctx.Err()}
		}
	}
}

func (f *fakeSink) Drop() error {
	f.call("drop")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != alsapcm.SinkStopped && f.state != alsapcm.SinkEnded {
		close(f.dropped)
		f.setState(alsapcm.SinkStopped, nil)
	}
	return nil
}

func (f *fakeSink) Close() error {
	_ = f.Drop()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeSink) Position() alsapcm.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return alsapcm.Position{State: f.state, Stream: 3, Played: f.played, Written: f.frames(), Submitted: f.frames(),
		RateHz: f.cfg.Transport.RateHz, Gaps: len(f.gaps), Err: f.err}
}

func (f *fakeSink) Stats() alsapcm.SinkStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	rate := int64(f.cfg.Transport.RateHz)
	return alsapcm.SinkStats{Params: f.params, RingFrames: 2*rate + rate/5, LookaheadFrames: 2 * rate, KeptFrames: rate / 5,
		RingBytes: int(2*rate+rate/5) * f.params.FrameBytes(), PausedBy: f.pausedBy, Gaps: slices.Clone(f.gaps)}
}

func (f *fakeSink) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

// setPlayed sets what the fake says has played.
func (f *fakeSink) setPlayed(frames int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = frames
}

// sinkHarness is a sink run's world: its env, the sinks it opened, the
// ticks the test sends, and standard input.
type sinkHarness struct {
	env    outputPCMTestEnv
	ticks  chan time.Time
	opens  chan *fakeSink // each sink as the run opens it
	stdin  *io.PipeWriter
	mu     sync.Mutex
	sinks  []*fakeSink
	opened []alsapcm.SinkConfig
	// prepare scripts each sink as it opens; openWait, when set, is called
	// first, outside the lock, to hold the i-th open.
	prepare  func(i int, f *fakeSink)
	openWait func(i int)
	openErr  error
}

// newSinkHarness is a harness on Linux whose sinks are fakes.
func newSinkHarness(t *testing.T) *sinkHarness {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	h := &sinkHarness{ticks: make(chan time.Time), opens: make(chan *fakeSink, 8), stdin: pw}
	h.env = outputPCMTestEnv{
		goos: "linux",
		open: func(alsapcm.Endpoint, alsapcm.Config) (pcmTestDevice, error) {
			t.Error("a sink run opened the device without the sink")
			return nil, &alsapcm.Error{Reason: alsapcm.ReasonKernel}
		},
		openSink: func(ctx context.Context, cfg alsapcm.SinkConfig) (pcmTestSink, error) {
			if h.openWait != nil {
				h.mu.Lock()
				i := len(h.opened)
				h.mu.Unlock()
				h.openWait(i)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			// As alsapcm.OpenSink does, a context already over opens nothing.
			if err := ctx.Err(); err != nil {
				return nil, &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "open", Endpoint: cfg.Endpoint, Err: err}
			}
			h.opened = append(h.opened, cfg)
			if h.openErr != nil {
				return nil, h.openErr
			}
			f := newFakeSink(cfg)
			if h.prepare != nil {
				h.prepare(len(h.sinks), f)
			}
			h.sinks = append(h.sinks, f)
			h.opens <- f
			return f, nil
		},
		stdin: pr,
		ticks: func(time.Duration) (<-chan time.Time, func()) { return h.ticks, func() {} },
	}
	return h
}

// sink is the i-th sink the run opened.
func (h *sinkHarness) sink(i int) *fakeSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sinks[i]
}

// run runs the subcommand in the background and returns its result
// channel.
func (h *sinkHarness) run(ctx context.Context, args ...string) <-chan pcmTestResult {
	done := make(chan pcmTestResult, 1)
	go func() {
		so, se, code := runPCMTest(ctx, h.env, args...)
		done <- pcmTestResult{so, se, code}
	}()
	return done
}

// pcmTestResult is what a run printed and how it exited.
type pcmTestResult struct {
	stdout, stderr string
	code           int
}

// wait waits for a run's result. The minute is a watchdog that turns a run
// that never ends into a failure; nothing waits on it to let the run go on.
func (h *sinkHarness) wait(t *testing.T, done <-chan pcmTestResult) pcmTestResult {
	t.Helper()
	watchdog := time.NewTimer(time.Minute)
	defer watchdog.Stop()
	select {
	case r := <-done:
		return r
	case <-watchdog.C:
		t.Fatal("the run did not end within a minute")
	}
	return pcmTestResult{}
}

// openGate lets the drain of every sink opened from now on end at once.
func openGate(_ int, f *fakeSink) { close(f.gate) }

// writePCMWAV writes a WAV of interleaved stereo PCM, 16 or 24 bits, whose
// data is frames frames of a pattern no two neighbouring bytes share, and
// returns the data: the bytes S16_LE or S24_3LE carry for those samples.
func writePCMWAV(t *testing.T, path string, rate, bits, frames int, seed byte) []byte {
	t.Helper()
	return writeWAV(t, path, rate, bits, 2, frames, seed)
}

// writeWAV is writePCMWAV with any number of channels.
func writeWAV(t *testing.T, path string, rate, bits, channels, frames int, seed byte) []byte {
	t.Helper()
	block := channels * bits / 8
	data := make([]byte, frames*block)
	for i := range data {
		data[i] = byte(i*13+i/7) ^ seed
	}
	var b bytes.Buffer
	le32 := func(v int) { _ = binary.Write(&b, binary.LittleEndian, uint32(v)) }
	le16 := func(v int) { _ = binary.Write(&b, binary.LittleEndian, uint16(v)) }
	b.WriteString("RIFF")
	le32(36 + len(data))
	b.WriteString("WAVEfmt ")
	le32(16)
	le16(1)
	le16(channels)
	le32(rate)
	le32(rate * block)
	le16(block)
	le16(bits)
	b.WriteString("data")
	le32(len(data))
	b.Write(data)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// fileArgs is a --file run of paths on card 5 device 0, with the level
// acknowledged and the /proc tree an empty directory.
func fileArgs(t *testing.T, paths ...string) []string {
	args := []string{"--card", "5", "--device", "0", "--ack-full-level", "--proc-root", t.TempDir()}
	for _, p := range paths {
		args = append(args, "--file", p)
	}
	return args
}

// TestOutputPCMTestSinkPlaysAFileBitExact pins --file: the file decoded and
// packed in the format its bits take, the sink opened for the file's own
// transport, every sample reaching the sink as the bytes the WAV holds,
// the drain, one close and exit 0.
func TestOutputPCMTestSinkPlaysAFileBitExact(t *testing.T) {
	for _, c := range []struct {
		rate, bits int
		format     format.ALSAFormat
	}{{44100, 16, format.S16LE}, {96000, 24, format.S243LE}} {
		h := newSinkHarness(t)
		h.prepare = openGate
		path := filepath.Join(t.TempDir(), "a.wav")
		data := writePCMWAV(t, path, c.rate, c.bits, 30011, 0)
		r := h.wait(t, h.run(context.Background(), fileArgs(t, path)...))
		if r.code != 0 {
			t.Fatalf("%d/%d: exit %d\nstdout:\n%s\nstderr:\n%s", c.rate, c.bits, r.code, r.stdout, r.stderr)
		}
		want := alsapcm.Transport{Kind: format.KindPCM, RateHz: c.rate, Channels: 2, Format: c.format, ValidBits: c.bits}
		if len(h.opened) != 1 || h.opened[0].Transport != want || h.opened[0].Endpoint != (alsapcm.Endpoint{Card: 5}) {
			t.Errorf("%d/%d: opened %+v, want one sink for %v on card 5 device 0", c.rate, c.bits, h.opened, want)
		}
		f := h.sink(0)
		if !bytes.Equal(f.written, data) {
			t.Errorf("%d/%d: the sink got %d bytes that are not the file's %d, in order", c.rate, c.bits, len(f.written), len(data))
		}
		if f.closes != 1 {
			t.Errorf("%d/%d: closed %d times", c.rate, c.bits, f.closes)
		}
		for _, say := range []string{"committed exactly", "sink: " + want.String(), "a store of", "track 1: " + path, "drained", "gaps 0", "closed\n"} {
			if !strings.Contains(r.stdout, say) {
				t.Errorf("%d/%d: stdout does not say %q:\n%s", c.rate, c.bits, say, r.stdout)
			}
		}
	}
}

// TestOutputPCMTestSinkJoinsTwoFilesOfOneTransport pins the gapless join:
// two files of one transport through one sink, the second's frames
// straight after the first's, one drain, and the join named where it is.
func TestOutputPCMTestSinkJoinsTwoFilesOfOneTransport(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = openGate
	dir := t.TempDir()
	a := writePCMWAV(t, filepath.Join(dir, "a.wav"), 44100, 16, 22050, 0)
	b := writePCMWAV(t, filepath.Join(dir, "b.wav"), 44100, 16, 10007, 0x5a)
	r := h.wait(t, h.run(context.Background(), fileArgs(t, filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav"))...))
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if len(h.opened) != 1 {
		t.Fatalf("opened %d sinks for two files of one transport, want one", len(h.opened))
	}
	if f := h.sink(0); !bytes.Equal(f.written, append(slices.Clone(a), b...)) {
		t.Errorf("the sink got %d bytes that are not the two files' %d, back to back", len(f.written), len(a)+len(b))
	}
	if !strings.Contains(r.stdout, "track 2 joins track 1 gaplessly at 0.500 s (frame 22050 of its sink): the same transport, nothing between") {
		t.Errorf("stdout does not name the join:\n%s", r.stdout)
	}
}

// TestOutputPCMTestSinkReopensForAnotherTransport pins two files of
// different transports: the first drained and its sink closed, a sink
// opened for the second's, an honest gap named for what differs.
func TestOutputPCMTestSinkReopensForAnotherTransport(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = openGate
	dir := t.TempDir()
	a := writePCMWAV(t, filepath.Join(dir, "a.wav"), 44100, 16, 4410, 0)
	b := writePCMWAV(t, filepath.Join(dir, "b.wav"), 48000, 24, 4800, 1)
	r := h.wait(t, h.run(context.Background(), fileArgs(t, filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav"))...))
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if len(h.opened) != 2 || h.opened[1].Transport.RateHz != 48000 || h.opened[1].Transport.Format != format.S243LE {
		t.Fatalf("opened %+v, want a second sink for 48 kHz S24_3LE", h.opened)
	}
	first, second := h.sink(0), h.sink(1)
	if !bytes.Equal(first.written, a) || !bytes.Equal(second.written, b) || first.closes != 1 || second.closes != 1 {
		t.Errorf("first sink %d bytes, closed %d; second %d bytes, closed %d", len(first.written), first.closes, len(second.written), second.closes)
	}
	if !strings.Contains(r.stdout, "track 2 needs another transport (rate 44100 → 48000 Hz, format S16_LE → S24_3LE, valid bits 16 → 24): drain, reopen and refill, an honest gap") {
		t.Errorf("stdout does not name the honest gap:\n%s", r.stdout)
	}
}

// TestOutputPCMTestSinkQDuringATransportChangeStopsTheRun pins q while the
// run changes transport (#1303's review: q dropped the sink it found, the
// producer went on to open the next file's sink and play it to the end,
// and the run closed the first sink twice). The q comes while the second
// file's sink opens: no sink plays on, nothing is opened after the q, the
// first sink is closed once, and the stop is no failure.
func TestOutputPCMTestSinkQDuringATransportChangeStopsTheRun(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = openGate
	opening, release := make(chan struct{}), make(chan struct{})
	h.openWait = func(i int) {
		if i == 1 {
			close(opening)
			<-release
		}
	}
	dir := t.TempDir()
	writePCMWAV(t, filepath.Join(dir, "a.wav"), 44100, 16, 4410, 0)
	writePCMWAV(t, filepath.Join(dir, "b.wav"), 48000, 24, 4800, 1)
	done := h.run(context.Background(), append(fileArgs(t, filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")), "--keys")...)
	first := waitSink(t, h, done)
	awaitRun(t, opening, "the run to open the second file's sink")
	if _, err := io.WriteString(h.stdin, "q\n"); err != nil {
		t.Fatal(err)
	}
	if got := nextCall(t, first, "drain"); got != "drop" {
		t.Fatalf("after q: %q, want the sink the run found dropped", got)
	}
	close(release)
	r := h.wait(t, done)
	if r.code != 0 || !strings.Contains(r.stdout, "stopped (q) after") || strings.Contains(r.stderr, "cancel") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	h.mu.Lock()
	opened := len(h.opened)
	h.mu.Unlock()
	if first.closes != 1 || opened != 1 {
		t.Errorf("the first sink closed %d times, %d sinks opened; want it closed once and none opened after q", first.closes, opened)
	}
}

// TestOutputPCMTestSinkCountsTheRunsTimeAcrossRates pins the run's content
// time across sinks of different rates (#1303's review: their frames were
// summed and read at the last sink's rate). A second of 44.1 kHz drains,
// then half a second of a 48 kHz file plays: the run says 1.5 s played,
// pauses there for --pause-at 1.5, and drains at 2 s.
func TestOutputPCMTestSinkCountsTheRunsTimeAcrossRates(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = func(i int, f *fakeSink) {
		if i == 0 {
			close(f.gate)
		}
	}
	dir := t.TempDir()
	writePCMWAV(t, filepath.Join(dir, "a.wav"), 44100, 16, 44100, 0)
	writePCMWAV(t, filepath.Join(dir, "b.wav"), 48000, 16, 48000, 1)
	done := h.run(context.Background(), append(fileArgs(t, filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")), "--pause-at", "1.5", "--resume-after", "1")...)
	waitSink(t, h, done)
	second := waitSink(t, h, done)
	// The second sink's drain is the producer's last call: the run plays it.
	if got := nextCall(t, second, ""); got != "drain" {
		t.Fatalf("the second sink's first call: %q, want its drain", got)
	}
	second.setPlayed(24000)
	t0 := time.Unix(1_000, 0)
	h.ticks <- t0
	h.ticks <- t0.Add(time.Second)
	close(second.gate)
	r := h.wait(t, done)
	calls := drainCalls(second)
	for _, say := range []string{"1 s: played 1.500 s", "paused by the kernel's PAUSE, at 1.500 s", "drained: 2.000 s played"} {
		if !strings.Contains(r.stdout, say) {
			t.Errorf("stdout does not say %q:\n%s", say, r.stdout)
		}
	}
	if r.code != 0 || !slices.Contains(calls, "pause") {
		t.Errorf("exit %d, the second sink's calls %v; want 0 and a pause", r.code, calls)
	}
}

// TestOutputPCMTestSinkKeysReaderEndsWithTheRun pins the --keys reader's
// life (#1303's review: it outlived the run, blocked on standard input):
// once the run has returned, its reader has stopped, and a line written
// after it is read by nobody.
func TestOutputPCMTestSinkKeysReaderEndsWithTheRun(t *testing.T) {
	h := newSinkHarness(t)
	done := h.run(context.Background(), "--sink", "--keys", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16",
		"--seconds", "3", "--proc-root", t.TempDir())
	waitSink(t, h, done)
	if _, err := io.WriteString(h.stdin, "q\n"); err != nil {
		t.Fatal(err)
	}
	if r := h.wait(t, done); r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	// A reader still alive takes this line; one that stopped with the run
	// has closed its end of the pipe.
	if _, err := io.WriteString(h.stdin, "p\n"); err == nil {
		t.Error("a line written after the run was read: the run's key reader outlived it")
	}
}

// TestOutputPCMTestSinkTwoToneTracksAreOneSeamlessTone pins --tracks: the
// tone split mid-cycle into two tracks of one transport reaches the sink
// as the one continuous tone, and the join is named.
func TestOutputPCMTestSinkTwoToneTracksAreOneSeamlessTone(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = openGate
	r := h.wait(t, h.run(context.Background(), "--sink", "--tracks", "2", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "24",
		"--seconds", "1", "--proc-root", t.TempDir()))
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	tone, err := buildPCMTestTone(48000, 24, format.S243LE, -30)
	if err != nil {
		t.Fatal(err)
	}
	if f := h.sink(0); !bytes.Equal(f.written, bytes.Repeat(tone.packed, 48000/tone.frames)) {
		t.Errorf("the sink got %d bytes that are not one second of the continuous tone", len(f.written))
	}
	if 24037%tone.frames == 0 {
		t.Fatal("the second track starts on a whole repeat of the tone, where a lost frame would not show")
	}
	for _, say := range []string{"track 2 joins track 1 gaplessly at 0.501 s (frame 24037 of its sink)", "tone: 1000 Hz sine on both channels, peak -30.0 dBFS", "drained"} {
		if !strings.Contains(r.stdout, say) {
			t.Errorf("stdout does not say %q:\n%s", say, r.stdout)
		}
	}
}

// TestOutputPCMTestSinkPausesAndResumesOnTimers pins --pause-at and
// --resume-after: the pause once the content played reaches its time, on
// a tick, and the resume once the time after it has passed, on a later
// tick; the run then drains.
func TestOutputPCMTestSinkPausesAndResumesOnTimers(t *testing.T) {
	h := newSinkHarness(t)
	done := h.run(context.Background(), "--sink", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16",
		"--seconds", "3", "--pause-at", "1", "--resume-after", "2", "--proc-root", t.TempDir())
	t0 := time.Unix(1_000, 0)
	f := waitSink(t, h, done)
	f.setPlayed(24000)
	h.ticks <- t0
	f.setPlayed(48000)
	h.ticks <- t0.Add(time.Second)
	if got := nextCall(t, f, "drain"); got != "pause" {
		t.Fatalf("after the content reached 1 s: %q, want a pause", got)
	}
	h.ticks <- t0.Add(2 * time.Second)
	h.ticks <- t0.Add(3 * time.Second)
	if got := nextCall(t, f, "drain"); got != "resume" {
		t.Fatalf("2 s after the pause: %q, want a resume", got)
	}
	close(f.gate)
	r := h.wait(t, done)
	if r.code != 0 || !strings.Contains(r.stdout, "paused by the kernel's PAUSE, at 1.000 s") || !strings.Contains(r.stdout, "resumed at") || !strings.Contains(r.stdout, "drained") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
}

// TestOutputPCMTestSinkDrainsWhenAResumeOvertakesItsRefusal pins the drain a
// pause refused when the resume comes before the producer looks at the
// refusal (#1303's Windows leg: TestOutputPCMTestSinkPausesAndResumesOnTimers
// exited 1, "drain: bad_state: paused: resume or drop it", the producer
// having found the sink playing again and taken the refusal for a failure).
// The producer is held where it has the refusal and the resume comes: the
// drain is asked again and plays out, exit 0.
func TestOutputPCMTestSinkDrainsWhenAResumeOvertakesItsRefusal(t *testing.T) {
	h := newSinkHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	refused, goOn := make(chan struct{}), make(chan struct{})
	h.prepare = func(_ int, f *fakeSink) {
		close(f.gate)
		var first, firstRefusal sync.Once
		f.enterDrain = func() { first.Do(func() { close(entered); <-release }) }
		f.refused = func() { firstRefusal.Do(func() { close(refused); <-goOn }) }
	}
	done := h.run(context.Background(), "--sink", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16",
		"--seconds", "1", "--pause-at", "0.5", "--resume-after", "1", "--proc-root", t.TempDir())
	f := waitSink(t, h, done)
	// Every frame is written and the producer is about to ask for the drain:
	// the pause comes first.
	awaitRun(t, entered, "the producer to ask for the drain")
	playing := f.Changed()
	f.setPlayed(24000)
	t0 := time.Unix(1_000, 0)
	h.ticks <- t0
	awaitRun(t, playing, "the pause at half a second")
	// The paused sink refuses the drain, and the resume comes before the
	// producer looks at why.
	paused := f.Changed()
	close(release)
	awaitRun(t, refused, "the paused sink to refuse the drain")
	h.ticks <- t0.Add(time.Second)
	awaitRun(t, paused, "the resume a second after the pause")
	close(goOn)
	r := h.wait(t, done)
	drains := 0
	for _, c := range drainCalls(f) {
		if c == "drain" {
			drains++
		}
	}
	if r.code != 0 || drains != 2 || f.closes != 1 {
		t.Errorf("exit %d, %d drains asked, closed %d times; want 0, the refused drain asked again, closed once\nstdout:\n%s\nstderr:\n%s",
			r.code, drains, f.closes, r.stdout, r.stderr)
	}
	for _, say := range []string{"paused by the kernel's PAUSE, at 0.500 s", "resumed at 0.500 s", "drained: 1.000 s played; gaps 0", "closed\n"} {
		if !strings.Contains(r.stdout, say) {
			t.Errorf("stdout does not say %q:\n%s", say, r.stdout)
		}
	}
}

// waitSink waits for the run to open its first sink.
func waitSink(t *testing.T, h *sinkHarness, done <-chan pcmTestResult) *fakeSink {
	t.Helper()
	select {
	case f := <-h.opens:
		return f
	case r := <-done:
		t.Fatalf("the run ended before it opened a sink: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	return nil
}

// nextCall returns the next call to the fake other than skip, under the
// run's watchdog.
func nextCall(t *testing.T, f *fakeSink, skip string) string {
	t.Helper()
	watchdog := time.NewTimer(time.Minute)
	defer watchdog.Stop()
	for {
		select {
		case c := <-f.calls:
			if c != skip {
				return c
			}
		case <-watchdog.C:
			t.Fatalf("waited a minute for a call other than %q", skip)
			return ""
		}
	}
}

// awaitRun waits for ch, under the run's watchdog.
func awaitRun(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	watchdog := time.NewTimer(time.Minute)
	defer watchdog.Stop()
	select {
	case <-ch:
	case <-watchdog.C:
		t.Fatalf("waited a minute for %s", what)
	}
}

// TestOutputPCMTestSinkPausesAndResumesOnKeys pins --keys: p pauses, p
// again resumes, q stops at once, dropped and closed, exit 0.
func TestOutputPCMTestSinkPausesAndResumesOnKeys(t *testing.T) {
	h := newSinkHarness(t)
	done := h.run(context.Background(), "--sink", "--keys", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16",
		"--seconds", "3", "--proc-root", t.TempDir())
	f := waitSink(t, h, done)
	for _, want := range []string{"pause", "resume"} {
		if _, err := io.WriteString(h.stdin, "p\n"); err != nil {
			t.Fatal(err)
		}
		if got := nextCall(t, f, "drain"); got != want {
			t.Fatalf("after p: %q, want %q", got, want)
		}
	}
	if _, err := io.WriteString(h.stdin, "q\n"); err != nil {
		t.Fatal(err)
	}
	r := h.wait(t, done)
	if r.code != 0 || !strings.Contains(r.stdout, "stopped (q) after") || !strings.Contains(r.stdout, "closed") || f.closes != 1 {
		t.Errorf("exit %d, closes %d\nstdout:\n%s\nstderr:\n%s", r.code, f.closes, r.stdout, r.stderr)
	}
}

// TestOutputPCMTestSinkReportsAnXrunAndRecoversOnlyWhenAsked pins the
// harness's xrun: without --recover the run stops at it, reports where it
// was, and exits 1 with no Recover; with --recover it recovers, plays on
// to the end, and still exits 1, the gap counted.
func TestOutputPCMTestSinkReportsAnXrunAndRecoversOnlyWhenAsked(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		h := newSinkHarness(t)
		h.prepare = func(_ int, f *fakeSink) {
			f.xrunAt = 30000
			close(f.gate)
		}
		args := []string{"--sink", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16", "--seconds", "1", "--proc-root", t.TempDir()}
		if recovered {
			args = append(args, "--recover")
		}
		r := h.wait(t, h.run(context.Background(), args...))
		f := h.sink(0)
		calls := drainCalls(f)
		if r.code != 1 || !strings.Contains(r.stdout, "xrun 1:") || !strings.Contains(r.stdout, "stopped at content frame 30000") {
			t.Errorf("recover %v: exit %d\nstdout:\n%s\nstderr:\n%s", recovered, r.code, r.stdout, r.stderr)
		}
		if got := slices.Contains(calls, "recover"); got != recovered {
			t.Errorf("recover %v: calls %v", recovered, calls)
		}
		wantFrames := int64(30000)
		if recovered {
			wantFrames = 48000
		}
		if f.frames() != wantFrames || f.closes != 1 {
			t.Errorf("recover %v: %d frames written, closed %d times; want %d, once", recovered, f.frames(), f.closes, wantFrames)
		}
		if recovered && !strings.Contains(r.stdout, "1 gaps, each an xrun recovered and counted: not a clean run") {
			t.Errorf("recover: the gap is not counted:\n%s", r.stdout)
		}
	}
}

// drainCalls is every call the fake has recorded so far.
func drainCalls(f *fakeSink) []string {
	var out []string
	for {
		select {
		case c := <-f.calls:
			out = append(out, c)
		default:
			return out
		}
	}
}

// TestOutputPCMTestSinkEndsOnADisconnect pins a device that goes away mid
// stream: the run says so, closes the sink once, and exits 1.
func TestOutputPCMTestSinkEndsOnADisconnect(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = func(_ int, f *fakeSink) {
		f.failAt, f.failErr = 20000, &alsapcm.Error{Reason: alsapcm.ReasonDisconnected, Op: "write", Endpoint: alsapcm.Endpoint{Card: 5}, Detail: "the device went away"}
	}
	r := h.wait(t, h.run(context.Background(), "--sink", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16", "--seconds", "2", "--proc-root", t.TempDir()))
	if r.code != 1 || !strings.Contains(r.stdout, "the device went away after") || !strings.Contains(r.stderr, "disconnected") || h.sink(0).closes != 1 {
		t.Errorf("exit %d, closes %d\nstdout:\n%s\nstderr:\n%s", r.code, h.sink(0).closes, r.stdout, r.stderr)
	}
}

// TestOutputPCMTestSinkCtrlCDropsAndCloses pins Ctrl-C while the sink
// plays: dropped, closed once, exit 0.
func TestOutputPCMTestSinkCtrlCDropsAndCloses(t *testing.T) {
	h := newSinkHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.run(ctx, "--sink", "--card", "5", "--device", "0", "--rate", "48000", "--bits", "16", "--seconds", "2", "--proc-root", t.TempDir())
	f := waitSink(t, h, done)
	if got := <-f.calls; got != "drain" {
		t.Fatalf("first call %q, want the drain the run waits in", got)
	}
	cancel()
	r := h.wait(t, done)
	if r.code != 0 || !strings.Contains(r.stdout, "stopped by signal after") || !strings.Contains(r.stdout, "dropped\nclosed\n") || f.closes != 1 {
		t.Errorf("exit %d, closes %d\nstdout:\n%s\nstderr:\n%s", r.code, f.closes, r.stdout, r.stderr)
	}
}

// TestOutputPCMTestSinkNamesTheHolderWhenAResumeIsBusy pins the contention
// gate: a resume after a release that finds the device busy says so, names
// the holder from /proc (MPD's output thread in the GO bar's 44.1 kHz
// capture), and, on a timer, stops with exit 1, leaving MPD alone.
func TestOutputPCMTestSinkNamesTheHolderWhenAResumeIsBusy(t *testing.T) {
	h := newSinkHarness(t)
	h.prepare = func(_ int, f *fakeSink) {
		// The geometry MPD's capture shows, so the kernel's file agrees.
		f.params.PeriodFrames, f.params.BufferFrames = 5513, 22050
		f.resumeErr = []error{&alsapcm.Error{Reason: alsapcm.ReasonBusy, Op: "open", Endpoint: alsapcm.Endpoint{Card: 5}, Detail: "another process has /dev/snd/pcmC5D0p open"}}
	}
	done := h.run(context.Background(), "--sink", "--card", "5", "--device", "0", "--rate", "44100", "--bits", "16", "--seconds", "2",
		"--pause-at", "1", "--resume-after", "40", "--proc-root", outputFixtureRoot(t, "pcm-44k1-16"))
	t0 := time.Unix(1_000, 0)
	f := waitSink(t, h, done)
	f.setPlayed(44100)
	h.ticks <- t0
	h.ticks <- t0.Add(40 * time.Second)
	r := h.wait(t, done)
	for _, want := range []string{"busy", `held by sub 0: thread 6584 "output:alsa", process 6551 "mpd"`, "does not stop MPD"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, r.stderr)
		}
	}
	if r.code != 1 || !strings.Contains(r.stdout, "the resume was refused") || f.closes != 1 {
		t.Errorf("exit %d, closes %d\nstdout:\n%s", r.code, f.closes, r.stdout)
	}
}

// TestOutputPCMTestSinkRefusesWhatItCannotPlay pins the sink mode's
// refusals: flags that do not make a run (exit 2) and files the player
// does not play (exit 1), each before any sink is opened. A file of other
// than two channels is the decoder's to refuse, before the run sees its
// transport (#1303's review asked for a channel check in pcmTestOpenFiles,
// which no decoded file could reach).
func TestOutputPCMTestSinkRefusesWhatItCannotPlay(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	writePCMWAV(t, wav, 44100, 16, 100, 0)
	mono := filepath.Join(dir, "mono.wav")
	writeWAV(t, mono, 44100, 16, 1, 100, 0)
	mp3 := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(mp3, append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x00}, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	dsf := filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", "lsb_dsd64_whole_blocks.dsf")
	tone := []string{"--card", "5", "--device", "0", "--rate", "48000", "--bits", "16"}
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"--card", "5", "--device", "0", "--file", wav}, 2, "--ack-full-level"},
		{append([]string{"--file", wav, "--ack-full-level", "--rate", "44100"}, "--card", "5", "--device", "0"), 2, "--file plays the file as it is and takes no --rate"},
		{[]string{"--card", "5", "--file", wav, "--ack-full-level"}, 2, "--device is required"},
		{[]string{"--card", "5", "--device", "0", "--file", wav, "--ack-full-level", "--format", "FLOAT_LE"}, 2, "--format is S16_LE, S24_3LE, S24_LE or S32_LE"},
		{append(slices.Clone(tone), "--pause-at", "1"), 2, "--pause-at is for the streaming sink"},
		{append(slices.Clone(tone), "--keys"), 2, "--keys is for the streaming sink"},
		{append(slices.Clone(tone), "--sink", "--tracks", "9"), 2, "--tracks is 1 to 8"},
		{append(slices.Clone(tone), "--sink", "--pause", "later"), 2, "--pause is auto, pause or zeros"},
		{append(slices.Clone(tone), "--sink", "--resume-after", "3"), 2, "--resume-after counts from the pause"},
		{append(slices.Clone(tone), "--sink", "--release-after", "0.5"), 2, "--release-after is 1 to 600 seconds"},
		{append(slices.Clone(tone), "--sink", "--release-after", "NaN"), 2, "--release-after is 1 to 600 seconds"},
		{append(slices.Clone(tone), "--sink", "--pause-at", "NaN"), 2, "--pause-at and --resume-after are 0 to 86400 seconds"},
		{append(slices.Clone(tone), "--sink", "--pause-at", "1", "--resume-after", "1e300"), 2, "--pause-at and --resume-after are 0 to 86400 seconds"},
		{append(slices.Clone(tone), "--sink", "--level", "-20"), 2, "never louder than -30 dBFS"},
		{[]string{"--passive", "--card", "5", "--sink"}, 2, "--passive opens nothing and plays nothing, and takes no --sink"},
		{[]string{"--card", "5", "--device", "0", "--file", dsf, "--ack-full-level"}, 1, "native DSD"},
		{[]string{"--card", "5", "--device", "0", "--file", mono, "--ack-full-level"}, 1, "want stereo (2 channels), got 1"},
		{[]string{"--card", "5", "--device", "0", "--file", mp3, "--ack-full-level"}, 1, "refused"},
		{[]string{"--card", "5", "--device", "0", "--file", filepath.Join(dir, "missing.flac"), "--ack-full-level"}, 1, "missing.flac"},
		{[]string{"--card", "5", "--device", "0", "--file", wav, "--ack-full-level", "--format", "S32_LE"}, 1, "--format S32_LE does not carry"},
	} {
		h := newSinkHarness(t)
		// A run wrongly let through drains at once and fails the checks
		// below, rather than waiting on a drain nobody ends.
		h.prepare = openGate
		r := h.wait(t, h.run(context.Background(), c.args...))
		if r.code != c.code || !strings.Contains(r.stderr, c.says) {
			t.Errorf("%q: exit %d, stderr %q; want %d and %q", c.args, r.code, r.stderr, c.code, c.says)
		}
		if len(h.opened) != 0 {
			t.Errorf("%q: opened a sink", c.args)
		}
	}
	h := newSinkHarness(t)
	h.env.goos = "darwin"
	if r := h.wait(t, h.run(context.Background(), "--card", "5", "--device", "0", "--file", wav, "--ack-full-level")); r.code != 1 || !strings.Contains(r.stderr, "Linux only") || len(h.opened) != 0 {
		t.Errorf("off Linux: exit %d, %q, %d opens", r.code, r.stderr, len(h.opened))
	}
}
