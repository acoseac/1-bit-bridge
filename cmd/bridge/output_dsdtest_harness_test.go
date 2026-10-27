package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nativeHarnessFake struct {
	cfg                 alsapcm.DSDConfig
	events              chan string
	now                 func() time.Time
	done                chan struct{}
	once                sync.Once
	lastWrite, closedAt atomic.Int64
}

func (f *nativeHarnessFake) Activate(ctx context.Context, _ uint64) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if f.cfg.Check != nil {
		if e := f.cfg.Check(alsapcm.NodeInfo{}); e != nil {
			return e
		}
	}
	f.events <- "activate"
	return nil
}
func (f *nativeHarnessFake) WriteDSD(ctx context.Context, _ uint64, l, r []byte, off, n int) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	f.lastWrite.Store(f.now().UnixNano())
	f.events <- "write"
	<-ctx.Done()
	return 0, ctx.Err()
}
func (f *nativeHarnessFake) BeginRelease(alsapcm.ReleaseMode) {}
func (f *nativeHarnessFake) Finish(context.Context) error     { return nil }
func (f *nativeHarnessFake) Release(_ context.Context, m alsapcm.ReleaseMode) error {
	f.once.Do(func() { f.closedAt.Store(f.now().UnixNano()); close(f.done); f.events <- fmt.Sprintf("release:%d", m) })
	return nil
}
func (f *nativeHarnessFake) Pause(context.Context) error   { f.events <- "pause"; return nil }
func (f *nativeHarnessFake) Resume(context.Context) error  { f.events <- "resume"; return nil }
func (f *nativeHarnessFake) Params() alsapcm.Params        { return alsapcm.Params{RateHz: 88200} }
func (f *nativeHarnessFake) Position() alsapcm.DSDPosition { return alsapcm.DSDPosition{} }
func (f *nativeHarnessFake) Done() <-chan struct{}         { return f.done }

type nativeHarnessRig struct {
	args     []string
	env      outputDSDTestEnv
	dir      string
	manifest qualification.Manifest
	contact  qualification.Contact
	events   chan string
	ticks    chan time.Time
	clock    atomic.Int64
	opens    atomic.Int32
	mu       sync.Mutex
	sinks    []*nativeHarnessFake
}

func harnessRig(t *testing.T) *nativeHarnessRig {
	t.Helper()
	h := &nativeHarnessRig{dir: t.TempDir(), events: make(chan string, 100), ticks: make(chan time.Time, 1)}
	var dsf bytes.Buffer
	if e := dsdtone.WriteDSF(&dsf, 2822400, 2, bytes.Repeat([]byte{0x69}, 2822400/8*3), 2822400*3); e != nil {
		t.Fatal(e)
	}
	pcm := make([]byte, 44+44100*3*4)
	copy(pcm, "RIFF")
	binary.LittleEndian.PutUint32(pcm[4:], uint32(len(pcm)-8))
	copy(pcm[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(pcm[16:], 16)
	binary.LittleEndian.PutUint16(pcm[20:], 1)
	binary.LittleEndian.PutUint16(pcm[22:], 2)
	binary.LittleEndian.PutUint32(pcm[24:], 44100)
	binary.LittleEndian.PutUint32(pcm[28:], 44100*4)
	binary.LittleEndian.PutUint16(pcm[32:], 4)
	binary.LittleEndian.PutUint16(pcm[34:], 16)
	copy(pcm[36:], "data")
	binary.LittleEndian.PutUint32(pcm[40:], uint32(len(pcm)-44))
	h.manifest = qualification.Manifest{SchemaVersion: 1, Calibration: qualification.Calibration{Reference: "test-only; never live audio", PeakDBFS: -30, LeadingSilenceMS: 1000, TrailingSilenceMS: 1000, Attestation: qualification.CalibrationPhrase}, Fixtures: []qualification.Fixture{{ID: "pcm", Path: "pcm.wav", SHA256: fmt.Sprintf("%x", sha256.Sum256(pcm)), Container: "WAV", Order: "LR", Rate: 44100, Channels: 2, Frames: 44100 * 3}, {ID: "dsd", Path: "dsd.dsf", SHA256: fmt.Sprintf("%x", sha256.Sum256(dsf.Bytes())), Container: "DSF", Order: "LR", PCMReference: "pcm", Rate: 2822400, Channels: 2, Frames: 2822400 * 3}}}
	mustWrite := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(h.dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	mustWrite("pcm.wav", pcm)
	mustWrite("dsd.dsf", dsf.Bytes())
	b, _ := json.Marshal(h.manifest)
	mustWrite("manifest.json", b)
	mustWrite("mixer.txt", []byte("# card=0\nno controls\n"))
	a := qualification.Attachment{BootID: "boot", Port: "port", Manufacturer: "iFi", Product: "GO bar", BCDDevice: "0123", DevNum: 2}
	controls := alsactl.Snapshot{}
	h.contact = qualification.Contact{Device: new(int), Subdevice: new(int), SchemaVersion: 2, QualificationVersion: qualification.Version, Tuple: qualification.Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel"}, Attachment: a, MixerHash: controls.Hash(), ManifestHash: fmt.Sprintf("%x", sha256.Sum256(b)), KnobPosition: "1", PCMAudible: qualification.PCMConfirmation, DSDMatched: qualification.DSDConfirmation}
	b, _ = json.Marshal(h.contact)
	mustWrite("contact.json", b)
	h.args = []string{"--ack-level=contact-position", "--format=DSD_U32_BE", "--bit-rate=2822400", "--seconds=5", "--pause-method=kernel", "--tail-ms=0", "--prefill-ms=0", "--scenario=interactive", "--config=fake", "--manifest=" + filepath.Join(h.dir, "manifest.json"), "--fixtures=" + h.dir, "--contact-report=" + filepath.Join(h.dir, "contact.json"), "--mixer-snapshot=" + filepath.Join(h.dir, "mixer.txt"), "--report=" + filepath.Join(h.dir, "report.json"), "dsd", "dsd", "dsd", "dsd"}
	h.env = outputDSDTestEnv{goos: "linux", now: func() time.Time { return time.Unix(0, h.clock.Load()) }, at: func(at time.Time) (<-chan time.Time, func()) {
		if !at.Equal(time.Unix(5, 0)) {
			timer := time.NewTimer(at.Sub(time.Unix(0, h.clock.Load())))
			return timer.C, func() { timer.Stop() }
		}
		return h.ticks, func() {}
	}, stdin: strings.NewReader(""), controls: func(int) (alsactl.Snapshot, error) { return controls, nil }, attachment: func(int) (qualification.Attachment, error) { return a, nil }, resolve: func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
		return nativeEndpoint{}, nil, nil
	}, openPCM: func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) { return nativePCMStub{}, nil }}
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		h.opens.Add(1)
		f := &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}
		h.mu.Lock()
		h.sinks = append(h.sinks, f)
		h.mu.Unlock()
		return f, nil
	}
	return h
}
func harnessEvent(t *testing.T, h *nativeHarnessRig, want string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-h.events:
			if e == want {
				return
			}
		case <-timer.C:
			t.Fatalf("no event %s", want)
		}
	}
}

// TestNativeHarnessWallClock spans repeated pauses/resumes and track changes.
func TestNativeHarnessWallClock(t *testing.T) {
	h := harnessRig(t)
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	h.env.stdin = nativePipeInput{r}
	h.env.tty = true
	result := make(chan int, 1)
	go func() { result <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
	harnessEvent(t, h, "write")
	for i := range 3 {
		h.clock.Store(int64(i+1) * int64(time.Second))
		_, _ = w.Write([]byte(" "))
		harnessEvent(t, h, "pause")
		_, _ = w.Write([]byte(" "))
		harnessEvent(t, h, "resume")
		_, _ = w.Write([]byte("n"))
		harnessEvent(t, h, "write")
	}
	h.clock.Store(5 * int64(time.Second))
	h.ticks <- h.env.now()
	select {
	case code := <-result:
		if code == 0 {
			t.Fatal("deadline reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadline did not stop")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sinks {
		if s.lastWrite.Load() > 5*int64(time.Second) || s.closedAt.Load() > 7*int64(time.Second) {
			t.Fatal("wall budget exceeded")
		}
	}
	b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
	if e != nil {
		t.Fatal(e)
	}
	var report nativeReport
	if e = json.Unmarshal(b, &report); e != nil || report.End != "deadline" || report.PauseReleaseSeconds != 30 {
		t.Fatalf("report: %+v %v", report, e)
	}
}

// TestNativeHarnessEvidenceRefusesBeforeOpen reaches the whole CLI evidence gate.
func TestNativeHarnessEvidenceRefusesBeforeOpen(t *testing.T) {
	for _, name := range []string{"calibration", "asset", "tuple", "attachment", "missing_contact"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			switch name {
			case "calibration":
				h.manifest.Calibration.Attestation = ""
				b, _ := json.Marshal(h.manifest)
				_ = os.WriteFile(filepath.Join(h.dir, "manifest.json"), b, 0600)
			case "asset":
				_ = os.WriteFile(filepath.Join(h.dir, "dsd.dsf"), []byte("altered"), 0600)
			case "tuple":
				h.contact.Tuple.TailMS = 1
			case "attachment":
				h.contact.Attachment.DevNum++
			case "missing_contact":
				_ = os.Remove(filepath.Join(h.dir, "contact.json"))
			}
			if name == "tuple" || name == "attachment" {
				b, _ := json.Marshal(h.contact)
				_ = os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600)
			}
			if code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard); code == 0 || h.opens.Load() != 0 {
				t.Fatal("bad evidence reached opener")
			}
		})
	}
}

// TestNativeProductionOpenerRejectsTestBinary keeps happy-path tests off hardware.
func TestNativeProductionOpenerRejectsTestBinary(t *testing.T) {
	if _, e := productionNativeOpen(context.Background(), alsapcm.DSDConfig{}); e == nil || !strings.Contains(e.Error(), "prohibited in tests") {
		t.Fatalf("production opener: %v", e)
	}
	if _, e := productionNativePCMOpen(context.Background(), alsapcm.SinkConfig{}); e == nil || !strings.Contains(e.Error(), "prohibited in tests") {
		t.Fatal(e)
	}
}

// TestNativePCMReferenceBudget bounds the harness caller while the unchanged PCM closer owns cleanup.
func TestNativePCMReferenceBudget(t *testing.T) {
	ticks := make(chan time.Time)
	armed := make(chan struct{})
	env := outputDSDTestEnv{now: func() time.Time { return time.Unix(0, 0) }, at: func(at time.Time) (<-chan time.Time, func()) {
		if !at.Equal(time.Unix(2, 0)) {
			t.Errorf("cleanup deadline=%v", at)
		}
		close(armed)
		return ticks, func() {}
	}}
	entered, gate := make(chan struct{}), make(chan struct{})
	defer close(gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan error, 1)
	go func() {
		answer <- nativePCMCall(ctx, env, func(context.Context) error { close(entered); <-gate; return nil })
	}()
	select {
	case <-entered:
	case e := <-answer:
		t.Fatalf("PCM leg never entered: %v", e)
	case <-time.After(time.Second):
		t.Fatal("PCM leg never started")
	}
	cancel()
	<-armed
	close(ticks)
	select {
	case e := <-answer:
		if !errors.Is(e, alsapcm.ErrReleaseUnconfirmed) {
			t.Fatal("claimed cleanup", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PCM reference held harness past release budget")
	}
}

type nativeTailFake struct {
	*nativeHarnessFake
	stopped  chan struct{}
	stopOnce sync.Once
}

func (s *nativeTailFake) Release(ctx context.Context, m alsapcm.ReleaseMode) error {
	if m == alsapcm.TrackChange {
		s.events <- "tail"
		select {
		case <-s.stopped:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.stopOnce.Do(func() { close(s.stopped) })
	return s.nativeHarnessFake.Release(ctx, m)
}

// TestNativeHarnessStopInterruptsTail keeps the keyboard live during graceful release.
func TestNativeHarnessStopInterruptsTail(t *testing.T) {
	h := harnessRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		return &nativeTailFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, stopped: make(chan struct{})}, nil
	}
	o, e := parseNativeOptions(h.args, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	keys := make(chan byte, 2)
	answer := make(chan error, 1)
	go func() {
		answer <- runNativeQueue(ctx, o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &nativeReport{})
	}()
	harnessEvent(t, h, "write")
	keys <- 'n'
	harnessEvent(t, h, "tail")
	keys <- 's'
	select {
	case e := <-answer:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop could not interrupt pending tail")
	}
}

type nativePCMStub struct{}

func (nativePCMStub) Write(_ context.Context, b []byte) (int, error) { return len(b), nil }
func (nativePCMStub) Drain(context.Context) error                    { return nil }
func (nativePCMStub) Close() error                                   { return nil }

// nativePipeInput is test-owned; its explicit interrupt contract permits closing
// its own pipe, while production file readers remain caller-owned and open.
type nativePipeInput struct{ *io.PipeReader }

func (r nativePipeInput) Interrupt() { _ = r.CloseWithError(context.Canceled) }
