package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// fakePCMDevice is the harness's device in these tests: it keeps what it is
// given and answers as scripted, with no kernel behind it.
type fakePCMDevice struct {
	params  alsapcm.Params
	written []byte
	// failAt, when failErr is set, is the frame count at which Write
	// stops taking frames and returns failErr.
	failAt  int
	failErr error
	// block makes Write wait for its context once entered is closed.
	block    bool
	entered  chan struct{}
	drainErr error
	drops    int
	drains   int
	closes   int
}

func (f *fakePCMDevice) Params() alsapcm.Params { return f.params }
func (f *fakePCMDevice) Counters() alsapcm.Counters {
	return alsapcm.Counters{Writes: 3, Partial: 2, Waits: 1}
}

func (f *fakePCMDevice) Write(ctx context.Context, frames []byte) (int, error) {
	if f.block {
		close(f.entered)
		<-ctx.Done()
		return 0, &alsapcm.Error{Reason: alsapcm.ReasonCancelled, Op: "write", Endpoint: f.params.Endpoint, Err: ctx.Err()}
	}
	fb := f.params.FrameBytes()
	n := len(frames) / fb
	if f.failErr != nil && len(f.written)/fb+n > f.failAt {
		take := f.failAt - len(f.written)/fb
		f.written = append(f.written, frames[:take*fb]...)
		return take, f.failErr
	}
	f.written = append(f.written, frames...)
	return n, nil
}

func (f *fakePCMDevice) Status() (alsapcm.Status, error) {
	return alsapcm.Status{State: 3, Delay: int64(f.params.BufferFrames)}, nil
}

func (f *fakePCMDevice) Drain(ctx context.Context, timeout time.Duration) error {
	f.drains++
	return f.drainErr
}

func (f *fakePCMDevice) Drop() error  { f.drops++; return nil }
func (f *fakePCMDevice) Close() error { f.closes++; return nil }

// pcmTestParams48k is what a fake device committed: S24_3LE stereo at
// 48 kHz, 200 ms of buffer in four periods, on card 5 device 0.
func pcmTestParams48k() alsapcm.Params {
	return alsapcm.Params{
		Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}, ID: "USB Audio", Name: "USB Audio",
		Protocol: 2<<16 | 18, Format: format.S243LE, Channels: 2, RateHz: 48000, RateNum: 48000, RateDen: 1,
		Msbits: 24, PeriodFrames: 2400, Periods: 4, BufferFrames: 9600,
		StartThreshold: 9600, StopThreshold: 9600, AvailMin: 2400, Info: 0x50000,
	}
}

// pcmTestEnvWith is an env on Linux whose open hands back dev, or err, and
// counts its calls.
func pcmTestEnvWith(dev *fakePCMDevice, err error, opens *int) outputPCMTestEnv {
	return outputPCMTestEnv{
		goos: "linux",
		open: func(ep alsapcm.Endpoint, cfg alsapcm.Config) (pcmTestDevice, error) {
			*opens++
			if err != nil {
				return nil, err
			}
			return dev, nil
		},
	}
}

// pcmTestProcTree writes a /proc tree with card 5 and, unless hwParams is
// empty, the hw_params of its device 0's substream 0, and returns its root.
func pcmTestProcTree(t *testing.T, hwParams string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("asound/cards", " 5 [Audio          ]: USB-Audio - iFi (by AMR) HD USB Audio\n"+
		"                      iFi (by AMR) iFi (by AMR) HD USB Audio at usb-xhci-hcd.0-1, high speed\n")
	if hwParams != "" {
		write("asound/card5/pcm0p/sub0/hw_params", hwParams)
	}
	return root
}

// pcmTestKernelHW48k is the hw_params file the kernel prints for
// pcmTestParams48k.
const pcmTestKernelHW48k = "access: RW_INTERLEAVED\nformat: S24_3LE\nsubformat: STD\nchannels: 2\nrate: 48000 (48000/1)\nperiod_size: 2400\nbuffer_size: 9600\n"

// runPCMTest runs the subcommand with env and returns what it printed.
func runPCMTest(ctx context.Context, env outputPCMTestEnv, args ...string) (stdout, stderr string, code int) {
	var so, se bytes.Buffer
	code = runOutputPCMTest(ctx, args, env, &so, &se)
	return so.String(), se.String(), code
}

// TestOutputPCMTestTakesAnEndpointOnlyByNumber pins that the endpoint is
// two numbers: a name, an alias, a device string or a path for either is a
// usage error, as is a request the harness does not make, and nothing is
// opened for any of them.
func TestOutputPCMTestTakesAnEndpointOnlyByNumber(t *testing.T) {
	ok := []string{"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24"}
	with := func(over ...string) []string {
		out := append([]string(nil), ok...)
		for i := 0; i+1 < len(over); i += 2 {
			replaced := false
			for j := 0; j+1 < len(out); j += 2 {
				if out[j] == over[i] {
					out[j+1], replaced = over[i+1], true
				}
			}
			if !replaced {
				out = append(out, over[i], over[i+1])
			}
		}
		return out
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{with("--card", "default"), `invalid value "default" for flag -card`},
		{with("--card", "hw:0"), `invalid value "hw:0" for flag -card`},
		{with("--card", "plughw:0,0"), `invalid value "plughw:0,0" for flag -card`},
		{with("--card", "dmix"), `invalid value "dmix" for flag -card`},
		{with("--device", "/dev/snd/pcmC0D0p"), `invalid value "/dev/snd/pcmC0D0p" for flag -device`},
		{append(ok, "hw:5,0"), `unexpected argument "hw:5,0"`},
		{[]string{"--device", "0", "--rate", "48000", "--bits", "24"}, "--card is required"},
		{[]string{"--card", "5", "--rate", "48000", "--bits", "24"}, "--device is required"},
		{[]string{"--card", "5", "--device", "0", "--bits", "24"}, "--rate is required"},
		{with("--rate", "7999"), "--rate is 8000 to 1536000 Hz, not 7999"},
		{with("--rate", "1536001"), "--rate is 8000 to 1536000 Hz, not 1536001"},
		{with("--bits", "20"), "--bits is 16, 24 or 32"},
		{with("--format", "S16_LE"), "--format S16_LE does not carry 24-bit samples"},
		{with("--format", "FLOAT_LE"), "--format FLOAT_LE does not carry 24-bit samples"},
		{with("--seconds", "0"), "--seconds is 1 to 600"},
		{with("--seconds", "601"), "--seconds is 1 to 600"},
		{with("--level", "-29"), "never louder than -30 dBFS"},
		{with("--level", "-91"), "--level is -90 to -30 dBFS"},
		{[]string{"--passive", "--card", "5", "--rate", "48000"}, "--passive opens nothing and plays nothing, and takes no --rate"},
	} {
		// The opener refuses, so a request wrongly let through ends at
		// the open, counted, rather than playing on a device with no
		// parameters.
		var opens int
		refused := &alsapcm.Error{Reason: alsapcm.ReasonKernel, Op: "open", Detail: "a usage error reached the open"}
		_, stderr, code := runPCMTest(context.Background(), pcmTestEnvWith(nil, refused, &opens), c.args...)
		if code != 2 || !strings.Contains(stderr, c.says) {
			t.Errorf("%q: exit %d, stderr %q; want 2 and %q", c.args, code, stderr, c.says)
		}
		if opens != 0 {
			t.Errorf("%q: opened the device", c.args)
		}
	}
}

// TestOutputPCMTestIsHidden pins that `bridge output` does not list the
// harness, and that its own -h describes it.
func TestOutputPCMTestIsHidden(t *testing.T) {
	stdout, _, code := runCapture(t, "output", "help")
	if code != 0 || strings.Contains(stdout, "pcm-test") {
		t.Errorf("`bridge output help` (exit %d) lists pcm-test:\n%s", code, stdout)
	}
	_, stderr, code := runCapture(t, "output", "pcm-test", "-h")
	if code != 0 || !strings.Contains(stderr, "never by a name") || !strings.Contains(stderr, "not reachable over HTTP") {
		t.Errorf("`bridge output pcm-test -h` exit %d:\n%s", code, stderr)
	}
}

// TestOutputPCMTestPlaysTheToneAndReports pins an active run: the request
// handed to the backend, the committed parameters printed and checked
// against the kernel's own hw_params file, every frame of the tone
// written, the delay and counters reported, the drain, and one close.
func TestOutputPCMTestPlaysTheToneAndReports(t *testing.T) {
	dev := &fakePCMDevice{params: pcmTestParams48k()}
	var opens int
	env := pcmTestEnvWith(dev, nil, &opens)
	var gotEP alsapcm.Endpoint
	var gotCfg alsapcm.Config
	open := env.open
	env.open = func(ep alsapcm.Endpoint, cfg alsapcm.Config) (pcmTestDevice, error) {
		gotEP, gotCfg = ep, cfg
		return open(ep, cfg)
	}
	root := pcmTestProcTree(t, pcmTestKernelHW48k)
	stdout, stderr, code := runPCMTest(context.Background(), env,
		"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24", "--seconds", "2", "--proc-root", root)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if gotEP != (alsapcm.Endpoint{Card: 5, Device: 0}) || gotCfg.RateHz != 48000 || gotCfg.Format != format.S243LE {
		t.Errorf("opened %v with %+v", gotEP, gotCfg)
	}
	for _, want := range []string{
		"endpoint: card 5 device 0 (/dev/snd/pcmC5D0p), subdevice 0: USB Audio [USB Audio]",
		"kernel PCM protocol 2.0.18 (this backend opens 2.0.15 and newer; its definitions follow Linux v6.12)",
		"committed exactly: S24_3LE, 2 channels, 48000 Hz (48000/1), RW_INTERLEAVED",
		"buffer: 9600 frames (200.0 ms) in 4 periods of 2400 frames (50.0 ms)",
		"stops, an xrun, when 9600 are free (an underrun is never played)",
		"the kernel's card5/pcm0p/sub0/hw_params: S24_3LE, 2 ch, 48000 Hz, period 2400, buffer 9600: agrees",
		"tone: 1000 Hz sine on both channels, peak -30.0 dBFS (265271 of 8388607), 24 valid bits packed as S24_3LE",
		"playing 2 s (96000 frames)",
		"  1 s: 48000 frames written, delay 9600 frames (200.0 ms), RUNNING",
		"  2 s: 96000 frames written",
		"done: 96000 frames in 3 writes (2 partial), 1 waits for room, 0 interrupted; xruns 0",
		"delay before drain: 9600 frames (200.0 ms), RUNNING",
		"drained\nclosed\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not say %q:\n%s", want, stdout)
		}
	}
	tone, err := buildPCMTestTone(48000, 24, format.S243LE, -30)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat(tone.packed, 96000/tone.frames); !bytes.Equal(dev.written, want) {
		t.Errorf("wrote %d bytes that are not the tone's 96000 frames, %d bytes, in order", len(dev.written), len(want))
	}
	if dev.drains != 1 || dev.closes != 1 || dev.drops != 0 {
		t.Errorf("drains %d, closes %d, drops %d; want one drain, one close, no drop", dev.drains, dev.closes, dev.drops)
	}
}

// TestOutputPCMTestCtrlCDropsAndCloses pins the cancel: a signal while a
// write waits ends the run with the stream dropped and the device closed,
// once each, and exit 0. The fake says when the write is waiting, so
// nothing here sleeps.
func TestOutputPCMTestCtrlCDropsAndCloses(t *testing.T) {
	dev := &fakePCMDevice{params: pcmTestParams48k(), block: true, entered: make(chan struct{})}
	var opens int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		stdout, stderr string
		code           int
	}
	done := make(chan result, 1)
	root := pcmTestProcTree(t, pcmTestKernelHW48k)
	go func() {
		so, se, code := runPCMTest(ctx, pcmTestEnvWith(dev, nil, &opens),
			"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24", "--proc-root", root)
		done <- result{so, se, code}
	}()
	<-dev.entered
	cancel()
	r := <-done
	if r.code != 0 || !strings.Contains(r.stdout, "stopped by signal after 0 frames (0.0 ms): dropped\nclosed\n") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if dev.drops != 1 || dev.closes != 1 || dev.drains != 0 {
		t.Errorf("drops %d, closes %d, drains %d; want one drop, one close, no drain", dev.drops, dev.closes, dev.drains)
	}
}

// TestOutputPCMTestReportsAnXrunAndCloses pins a stream that fails: the
// typed error, the frames written and an xrun counted, the stream dropped,
// the device closed, and exit 1, with no recovery.
func TestOutputPCMTestReportsAnXrunAndCloses(t *testing.T) {
	dev := &fakePCMDevice{params: pcmTestParams48k(), failAt: 7000,
		failErr: &alsapcm.Error{Reason: alsapcm.ReasonXrun, Op: "write", Endpoint: alsapcm.Endpoint{Card: 5}, Detail: "the buffer ran empty"}}
	var opens int
	stdout, stderr, code := runPCMTest(context.Background(), pcmTestEnvWith(dev, nil, &opens),
		"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24", "--proc-root", pcmTestProcTree(t, pcmTestKernelHW48k))
	if code != 1 || !strings.Contains(stderr, "xrun") || !strings.Contains(stdout, "stopped after 7000 frames") ||
		!strings.Contains(stdout, "xruns 1") || !strings.Contains(stdout, "B470 PR 2b") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if dev.drops != 1 || dev.closes != 1 || dev.drains != 0 {
		t.Errorf("drops %d, closes %d, drains %d", dev.drops, dev.closes, dev.drains)
	}
}

// TestOutputPCMTestAnUnderrunBeforeTheDrainFails pins that the harness
// claims no xrun-free run before the drain has said so: an underrun after
// the last write is an xrun the drain reports, the run exits 1, and "xruns
// 0" is never printed (the Claude API review and Codex on #1301).
func TestOutputPCMTestAnUnderrunBeforeTheDrainFails(t *testing.T) {
	dev := &fakePCMDevice{params: pcmTestParams48k(),
		drainErr: &alsapcm.Error{Reason: alsapcm.ReasonXrun, Op: "drain", Endpoint: alsapcm.Endpoint{Card: 5}, Detail: "the buffer ran empty"}}
	var opens int
	stdout, stderr, code := runPCMTest(context.Background(), pcmTestEnvWith(dev, nil, &opens),
		"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24", "--seconds", "1", "--proc-root", pcmTestProcTree(t, pcmTestKernelHW48k))
	if code != 1 || strings.Contains(stdout, "xruns 0") || strings.Contains(stdout, "drained") || !strings.Contains(stdout, "xruns 1") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if dev.drains != 1 || dev.closes != 1 {
		t.Errorf("drains %d, closes %d", dev.drains, dev.closes)
	}
}

// TestOutputPCMTestNamesTheHolderOfABusyDevice pins the busy refusal: the
// typed error, then the thread and process holding the substream, read
// passively from /proc (here the GO bar capture, where MPD's output thread
// holds card 5), and that the bridge leaves the player alone.
func TestOutputPCMTestNamesTheHolderOfABusyDevice(t *testing.T) {
	busy := &alsapcm.Error{Reason: alsapcm.ReasonBusy, Op: "open", Endpoint: alsapcm.Endpoint{Card: 5},
		Detail: "another process has /dev/snd/pcmC5D0p open; the bridge never stops it"}
	var opens int
	_, stderr, code := runPCMTest(context.Background(), pcmTestEnvWith(nil, busy, &opens),
		"--card", "5", "--device", "0", "--rate", "352800", "--bits", "32", "--proc-root", outputFixtureRoot(t, "dsd256-native"))
	for _, want := range []string{"busy", `held by sub 0: thread 6584 "output:alsa", process 6551 "mpd"`, "does not stop MPD"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	if code != 1 || opens != 1 {
		t.Errorf("exit %d after %d opens; want 1 after one", code, opens)
	}
}

// TestOutputPCMTestStopsWhenTheKernelDisagrees pins the cross-check: when
// the kernel's own hw_params file says something other than the backend
// read back, nothing is played and the device is closed.
func TestOutputPCMTestStopsWhenTheKernelDisagrees(t *testing.T) {
	dev := &fakePCMDevice{params: pcmTestParams48k()}
	var opens int
	root := pcmTestProcTree(t, strings.Replace(pcmTestKernelHW48k, "48000 (48000/1)", "44100 (44100/1)", 1))
	stdout, stderr, code := runPCMTest(context.Background(), pcmTestEnvWith(dev, nil, &opens),
		"--card", "5", "--device", "0", "--rate", "48000", "--bits", "24", "--proc-root", root)
	if code != 1 || !strings.Contains(stdout, "DIFFERS (rate 44100, read back 48000)") || !strings.Contains(stderr, "not playing") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if len(dev.written) != 0 || dev.closes != 1 {
		t.Errorf("wrote %d bytes, closed %d times; want nothing written and one close", len(dev.written), dev.closes)
	}
}

// TestOutputPCMTestPassiveOpensNothing pins --passive: the card's
// /proc/asound files verbatim under their paths, the device's alone with
// --device, then the card's reading; no other card's file; and the device
// never opened.
func TestOutputPCMTestPassiveOpensNothing(t *testing.T) {
	var opens int
	env := pcmTestEnvWith(&fakePCMDevice{}, nil, &opens)
	env.goos = "darwin"
	root := outputFixtureRoot(t, "dsd256-native")
	stdout, stderr, code := runPCMTest(context.Background(), env, "--passive", "--card", "5", "--device", "0", "--proc-root", root)
	if code != 0 || opens != 0 {
		t.Fatalf("exit %d after %d opens\nstderr:\n%s", code, opens, stderr)
	}
	hw, err := os.ReadFile(filepath.Join(root, "asound", "card5", "pcm0p", "sub0", "hw_params"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"passive: nothing is opened; asound/card5 as the kernel printed it",
		"==> asound/card5/usbid <==",
		"==> asound/card5/stream0 <==",
		"==> asound/card5/pcm0p/sub0/hw_params <==\n" + string(hw),
		"==> asound/card5/pcm0p/sub0/status <==",
		"---\ncard 5: Audio [iFi (by AMR) HD USB Audio]",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not have %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "==> asound/cards") || strings.Contains(stdout, "6584/status") {
		t.Errorf("printed a file outside card 5's directory:\n%s", stdout)
	}

	if _, _, code := runPCMTest(context.Background(), env, "--passive", "--card", "7", "--proc-root", root); code != 1 {
		t.Errorf("a card that is not there: exit %d, want 1", code)
	}
	if _, stderr, code := runPCMTest(context.Background(), env, "--passive", "--card", "5"); code != 1 || !strings.Contains(stderr, "Linux only") {
		t.Errorf("the host's own tree off Linux: exit %d, %q", code, stderr)
	}
	if opens != 0 {
		t.Errorf("--passive opened the device %d times", opens)
	}
}

// TestOutputPCMTestOffLinuxRefuses pins that an active run off Linux is
// refused before anything is opened.
func TestOutputPCMTestOffLinuxRefuses(t *testing.T) {
	var opens int
	env := pcmTestEnvWith(&fakePCMDevice{}, nil, &opens)
	env.goos = "windows"
	_, stderr, code := runPCMTest(context.Background(), env, "--card", "0", "--device", "0", "--rate", "48000", "--bits", "16")
	if code != 1 || !strings.Contains(stderr, "ALSA is Linux only, and this is windows") || opens != 0 {
		t.Errorf("exit %d, %d opens, %q", code, opens, stderr)
	}
}

// TestOutputPCMTestToneIsQuietAndSeamless pins the stimulus for every
// format and bit depth the packers write: both channels equal, a peak at
// most -30 dBFS of the valid bits' full scale, each packed sample
// unpacking to the sine at its index, and a repeat that ends one sample
// before the sine starts again, so repeats join without a seam.
func TestOutputPCMTestToneIsQuietAndSeamless(t *testing.T) {
	for _, c := range []struct {
		f    format.ALSAFormat
		bits int
	}{{format.S16LE, 16}, {format.S243LE, 24}, {format.S24LE, 24}, {format.S32LE, 24}, {format.S32LE, 32}} {
		for _, rate := range []int{44100, 48000, 352800, 384000} {
			tone, err := buildPCMTestTone(rate, c.bits, c.f, -30)
			if err != nil {
				t.Fatalf("%s/%d at %d: %v", c.f, c.bits, rate, err)
			}
			if tone.frames*pcmTestToneHz%rate != 0 {
				t.Errorf("%d frames at %d Hz is not a whole number of 1 kHz cycles", tone.frames, rate)
			}
			if limit := math.Pow(10, -1.5) * float64(tone.fullScale); float64(tone.amplitude) > limit {
				t.Errorf("%s/%d: amplitude %d over -30 dBFS (%f)", c.f, c.bits, tone.amplitude, limit)
			}
			peak := int64(0)
			for n := range tone.frames {
				l, r := unpackPCMTest(t, tone.packed, 2*n, c.f, c.bits), unpackPCMTest(t, tone.packed, 2*n+1, c.f, c.bits)
				if l != r {
					t.Fatalf("%s/%d frame %d: left %d, right %d", c.f, c.bits, n, l, r)
				}
				want := int64(math.Round(float64(tone.amplitude) * math.Sin(2*math.Pi*pcmTestToneHz*float64(n)/float64(rate))))
				if l != want {
					t.Fatalf("%s/%d at %d frame %d: %d, want %d", c.f, c.bits, rate, n, l, want)
				}
				peak = max(peak, l, -l)
			}
			if peak == 0 || peak > tone.amplitude {
				t.Errorf("%s/%d at %d: peak %d, amplitude %d", c.f, c.bits, rate, peak, tone.amplitude)
			}
		}
	}
}

// unpackPCMTest reads sample i of a packed buffer back to its valid-bit
// value, independently of the packer.
func unpackPCMTest(t *testing.T, b []byte, i int, f format.ALSAFormat, bits int) int64 {
	t.Helper()
	switch f {
	case format.S16LE:
		return int64(int16(binary.LittleEndian.Uint16(b[2*i:])))
	case format.S243LE:
		v := int32(b[3*i]) | int32(b[3*i+1])<<8 | int32(b[3*i+2])<<16
		return int64(v << 8 >> 8)
	case format.S24LE:
		return int64(int32(binary.LittleEndian.Uint32(b[4*i:])<<8) >> 8)
	case format.S32LE:
		v := int64(int32(binary.LittleEndian.Uint32(b[4*i:])))
		if bits == 24 {
			if v&0xff != 0 {
				t.Fatalf("S32_LE from 24 bits has a low byte %#x", v&0xff)
			}
			return v >> 8
		}
		return v
	}
	t.Fatalf("no unpacker for %s", f)
	return 0
}
