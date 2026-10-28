//go:build nativeintegration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

type nativeIntegrationSink struct {
	*alsapcm.DSDSink
	events chan string
}

func (s *nativeIntegrationSink) Finish(ctx context.Context) error {
	s.events <- "finish"
	e := s.DSDSink.Finish(ctx)
	s.events <- "finish-return"
	return e
}
func (s *nativeIntegrationSink) BeginRelease(m alsapcm.ReleaseMode) {
	s.DSDSink.BeginRelease(m)
	s.events <- "cutoff"
}

type nativePromptOutput struct {
	h    *nativeHarnessRig
	hook func(string)
}

func (o nativePromptOutput) Write(b []byte) (int, error) {
	text := string(b)
	if o.hook != nil {
		o.hook(text)
	}
	for _, phrase := range []string{qualification.CalibrationPhrase, qualification.PCMConfirmation, "recorded knob position", qualification.DSDConfirmation, "DAC DSD indicator confirmed", "L/R pitch and duration correct"} {
		if strings.Contains(text, phrase) {
			o.h.events <- phrase
		}
	}
	return len(b), nil
}
func realNativeRig(t *testing.T) (*nativeHarnessRig, *alsapcm.FakeKernel, *io.PipeWriter) {
	t.Helper()
	h := harnessRig(t)
	k := alsapcm.NewFakeKernel(t)
	k.Renumber(0)
	k.HardwareFormats(2, 10, 32, 49, 50, 52)
	h.args = h.args[:len(h.args)-3] // one 3-second occurrence; all input fits in the native ring
	r, w := io.Pipe()
	t.Cleanup(func() { r.Close(); w.Close() })
	h.env.stdin = nativePipeInput{r}
	h.env.tty = true
	h.env.openPCM = func(ctx context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) { return k.OpenSink(ctx, c) }
	h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		s, e := k.OpenNativeSink(ctx, c)
		if e != nil {
			return nil, e
		}
		return &nativeIntegrationSink{s, h.events}, nil
	}
	return h, k, w
}
func nativeReportResult(t *testing.T, h *nativeHarnessRig, answer <-chan int, want int, end string) nativeReport {
	t.Helper()
	select {
	case code := <-answer:
		if code != want {
			t.Errorf("exit=%d want=%d", code, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("key did not stop within release budget")
	}
	b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
	if e != nil {
		t.Fatal(e)
	}
	var r nativeReport
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	if r.End != end {
		t.Errorf("serialized End=%s want=%s Error=%s", r.End, end, r.Error)
	}
	if end != "complete" && r.Contact != nil {
		t.Fatal("stopped run issued contact receipt")
	}
	return r
}
func nativeKey(w *io.PipeWriter, key byte) { _, _ = w.Write([]byte{key}) }

func TestNativeIntegrationStopEveryLeg(t *testing.T) {
	for _, scenario := range []string{"matrix", "level-check"} {
		legs := []string{"leading", "native", "trailing"}
		if scenario == "level-check" {
			legs = []string{"calibration", "leading", "native"}
		}
		for _, leg := range legs {
			for _, key := range []byte{'s', 'q'} {
				t.Run(scenario+"/"+leg+"/"+string(key), func(t *testing.T) {
					h, k, w := realNativeRig(t)
					h.args = setNativeArg(h.args, "scenario", scenario)
					if scenario == "level-check" {
						h.args = setNativeArg(h.args, "ack-level", "off-ear")
					}
					pcmOpen := h.env.openPCM
					var pcm atomic.Int32
					h.env.openPCM = func(ctx context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) {
						n := pcm.Add(1)
						if (leg == "leading" && n == 1) || (leg == "trailing" && n == 2) {
							k.Hold()
						} else {
							k.Free()
						}
						return pcmOpen(ctx, c)
					}
					nativeOpen := h.env.open
					h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
						if leg == "native" {
							k.Hold()
						} else {
							k.Free()
						}
						return nativeOpen(ctx, c)
					}
					answer := make(chan int, 1)
					go func() {
						answer <- runOutputDSDTest(context.Background(), h.args, h.env, nativePromptOutput{h: h}, io.Discard)
					}()
					if scenario == "level-check" {
						harnessEvent(t, h, qualification.CalibrationPhrase)
						if leg == "calibration" {
							_, _ = w.Write([]byte{key, '\n'})
						} else {
							_, _ = io.WriteString(w, qualification.CalibrationPhrase+"\n")
						}
					}
					expected := 1
					if leg == "native" {
						expected = 2
						if scenario == "level-check" {
							harnessEvent(t, h, qualification.PCMConfirmation)
							_, _ = io.WriteString(w, qualification.PCMConfirmation+"\n")
							harnessEvent(t, h, "recorded knob position")
							_, _ = io.WriteString(w, "1\n")
						}
					} else if leg == "trailing" {
						expected = 3
					} else if leg == "calibration" {
						expected = 0
					}
					if leg != "calibration" {
						k.Await(t, "target leg running", func(v alsapcm.KernelView) bool { return v.Opens == expected && v.Appl > 0 && v.Starved })
						nativeKey(w, key)
					}
					end := "stopped"
					if key == 'q' {
						end = "quit"
					}
					nativeReportResult(t, h, answer, 0, end)
					v := k.View()
					if v.Opens != expected || v.Open {
						t.Errorf("later open or incomplete Immediate: %+v", v)
					}
					before := len(k.Given())
					k.Free()
					if len(k.Given()) != before {
						t.Fatal("write after handled key")
					}
					k.CheckClosedEach(t)
				})
			}
		}
	}
}

func TestNativeIntegrationNextInsideFinish(t *testing.T) {
	for _, stopAt := range []string{"natural", "tail", "drain"} {
		t.Run(stopAt, func(t *testing.T) {
			h, k, w := realNativeRig(t)
			h.args = setNativeArg(h.args, "tail-ms", "1")
			h.contact.Tuple.TailMS = 1
			b, _ := json.Marshal(h.contact)
			_ = os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600)
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
			harnessEvent(t, h, "finish")
			k.Await(t, "native buffer held", func(v alsapcm.KernelView) bool { return v.State == "RUNNING" && v.Starved })
			prefix := len(k.Given())
			nativeKey(w, 'n')
			harnessEvent(t, h, "cutoff")
			harnessEvent(t, h, "finish-return")
			end := "complete"
			if stopAt == "natural" {
				k.Free()
			} else {
				if stopAt == "drain" {
					k.Grant(100 * time.Millisecond)
					k.Await(t, "drain waiting", func(v alsapcm.KernelView) bool { return v.Drains > 0 && v.Starved })
				}
				nativeKey(w, 's')
				end = "stopped"
			}
			nativeReportResult(t, h, answer, 0, end)
			if k.View().Opens != 1 {
				t.Fatal("successor opened after last track/stop")
			}
			if stopAt == "natural" {
				want := append(append([]byte(nil), k.Given()[:prefix]...), bytes.Repeat([]byte{0x69}, 89*8)...)
				if !bytes.Equal(k.Given(), want) {
					t.Fatalf("Finish cancellation lost qualified tail: got %d bytes want %d", len(k.Given()), len(want))
				}
			}
			k.CheckClosedEach(t)
		})
	}
}

func TestNativeIntegrationPCMAdmission(t *testing.T) {
	h, k, w := realNativeRig(t)
	h.args = setNativeArg(h.args, "scenario", "matrix")
	base := h.env.openPCM
	var replaced atomic.Bool
	attachment := h.env.attachment
	h.env.attachment = func(card int) (qualification.Attachment, error) {
		a, e := attachment(card)
		if replaced.Load() {
			a.DevNum++
		}
		return a, e
	}
	h.env.openPCM = func(ctx context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) {
		replaced.Store(true)
		k.Free()
		return base(ctx, c)
	}
	answer := make(chan int, 1)
	go func() { answer <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
	_ = w
	r := nativeReportResult(t, h, answer, 1, "incomplete")
	if len(k.Given()) != 0 || k.View().Opens != 1 || !strings.Contains(r.Error, "binding/mixer reference changed") {
		t.Fatalf("reference bypassed admission: %+v %s", k.View(), r.Error)
	}
	k.CheckClosedEach(t)
}

func TestNativeIntegrationPCMDescriptor(t *testing.T) {
	for _, mode := range []string{"hash", "geometry", "same_descriptor"} {
		t.Run(mode, func(t *testing.T) {
			h, k, w := realNativeRig(t)
			h.args = setNativeArg(setNativeArg(h.args, "scenario", "level-check"), "ack-level", "off-ear")
			var opened atomic.Int32
			base := h.env.openPCM
			h.env.openPCM = func(ctx context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) {
				opened.Add(1)
				if mode == "hash" {
					return nil, errors.New("unexpected replacement opener")
				}
				// Replacement after verification must not change the descriptor decoded.
				if mode == "same_descriptor" {
					path := filepath.Join(h.dir, "pcm.wav")
					if e := os.Rename(path, path+".verified"); e != nil {
						t.Error(e)
					}
					if e := os.WriteFile(path, []byte("unverified replacement"), 0600); e != nil {
						t.Error(e)
					}
				}
				k.Free()
				return base(ctx, c)
			}
			hook := func(text string) {
				if strings.Contains(text, qualification.CalibrationPhrase) && mode == "hash" {
					path := filepath.Join(h.dir, "pcm.wav")
					data, e := os.ReadFile(path)
					if e != nil {
						t.Error(e)
					}
					data[len(data)-1] ^= 0xff
					_ = os.WriteFile(path, data, 0600)
				}
			}
			// Geometry is checked again by the playback helper on its opened descriptor.
			if mode == "geometry" {
				f := h.manifest.Fixtures[0]
				f.Frames++
				e := playNativeReference(context.Background(), h.dir, f, alsapcm.Endpoint{}, nil, h.env.openPCM)
				if e == nil || opened.Load() != 0 {
					t.Fatalf("geometry reached opener: %v opens=%d", e, opened.Load())
				}
				return
			}
			answer := make(chan int, 1)
			go func() {
				answer <- runOutputDSDTest(context.Background(), h.args, h.env, nativePromptOutput{h: h, hook: hook}, io.Discard)
			}()
			harnessEvent(t, h, qualification.CalibrationPhrase)
			_, _ = io.WriteString(w, qualification.CalibrationPhrase+"\n")
			if mode == "hash" {
				r := nativeReportResult(t, h, answer, 1, "incomplete")
				if opened.Load() != 0 || !strings.Contains(r.Error, "opened fixture hash mismatch") {
					t.Fatalf("unverified replacement opened: %d %s", opened.Load(), r.Error)
				}
			} else {
				harnessEvent(t, h, qualification.PCMConfirmation)
				_, _ = io.WriteString(w, "s\n")
				nativeReportResult(t, h, answer, 0, "stopped")
				if len(k.Given()) != 44100*3*4 {
					t.Fatalf("did not decode verified descriptor: %d bytes", len(k.Given()))
				}
			}
		})
	}
}

func TestNativeIntegrationFingerprint(t *testing.T) {
	for _, mode := range []string{"volatile_pcm", "receipt_reuse", "volatile_mixer", "stable_value", "metadata", "membership"} {
		t.Run(mode, func(t *testing.T) {
			h, k, _ := realNativeRig(t)
			k.Free()
			before := alsactl.Snapshot{Controls: []alsactl.Control{{Interface: 3, Access: 5, Name: "Playback Channel Map", Values: []byte{0, 0}}}}
			if mode == "volatile_mixer" {
				before.Controls[0].Interface = 2
			}
			if mode == "stable_value" || mode == "metadata" || mode == "membership" {
				before.Controls[0].Interface = 2
				before.Controls[0].Access = 1
			}
			after := before
			after.Controls = append([]alsactl.Control(nil), before.Controls...)
			after.Controls[0].Values = []byte{3, 4}
			if mode == "metadata" {
				after.Controls[0].Values = before.Controls[0].Values
				after.Controls[0].Info = []byte{9}
			}
			if mode == "membership" {
				after.Controls[0].Values = before.Controls[0].Values
				after.Controls = append(after.Controls, alsactl.Control{Interface: 2, Name: "new"})
			}
			h.contact.MixerHash = before.Hash()
			b, _ := json.Marshal(h.contact)
			_ = os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600)
			var changed atomic.Bool
			if mode == "receipt_reuse" {
				changed.Store(true)
			}
			h.env.controls = func(int) (alsactl.Snapshot, error) {
				if changed.Load() {
					return after, nil
				}
				return before, nil
			}
			base := h.env.open
			h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				changed.Store(true)
				return base(ctx, c)
			}
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
			code, end := 1, "incomplete"
			if mode == "volatile_pcm" || mode == "receipt_reuse" {
				code, end = 0, "complete"
			}
			r := nativeReportResult(t, h, answer, code, end)
			if code == 1 && len(k.Given()) != 0 {
				t.Fatal("changed constrained controls wrote")
			}
			if mode == "volatile_pcm" {
				if len(r.ExcludedBefore) != 1 || len(r.ExcludedAfter) != 1 || len(r.ControlObservations) == 0 || !bytes.Equal(r.ExcludedAfter[0].Values, []byte{3, 4}) {
					t.Fatal("excluded values missing from report")
				}
			}
		})
	}
}

func TestNativeIntegrationStopCleanupFailure(t *testing.T) {
	for _, key := range []byte{'s', 'q'} {
		t.Run(string(key), func(t *testing.T) {
			h, k, w := realNativeRig(t)
			base := h.env.open
			h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				s, e := base(ctx, c)
				if e != nil {
					return nil, e
				}
				return &nativeCleanupFailureSink{nativeTestSink: s}, nil
			}
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
			harnessEvent(t, h, "finish")
			k.Await(t, "native playback", func(v alsapcm.KernelView) bool { return v.State == "RUNNING" && v.Starved })
			nativeKey(w, key)
			end := "stopped"
			if key == 'q' {
				end = "quit"
			}
			r := nativeReportResult(t, h, answer, 1, end)
			if !strings.Contains(r.Error, "cleanup failure") {
				t.Fatalf("cleanup error lost: %s", r.Error)
			}
		})
	}
}

type nativeCleanupFailureSink struct{ nativeTestSink }

func (s *nativeCleanupFailureSink) Release(ctx context.Context, m alsapcm.ReleaseMode) error {
	return errors.Join(s.nativeTestSink.Release(ctx, m), alsapcm.ErrReleaseUnconfirmed, errors.New("cleanup failure"))
}

// These park beneath the real sinks, not at a replacement playback function.
func TestNativeIntegrationPendingWorkers(t *testing.T) {
	for _, name := range []string{"native_open", "pcm_open", "admission", "admission_deadline", "attachment_deadline", "teardown"} {
		t.Run(name, func(t *testing.T) {
			h, k, _ := realNativeRig(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var entered <-chan struct{}
			var release func()
			switch name {
			case "native_open", "pcm_open":
				entered, release = k.ParkNextOpen()
			case "teardown":
				entered, release = k.ParkNextClose()
				k.Free()
			case "admission", "admission_deadline", "attachment_deadline":
				in, gate := make(chan struct{}), make(chan struct{})
				entered = in
				var once sync.Once
				release = func() { once.Do(func() { close(gate) }) }
				if name == "attachment_deadline" {
					attachment := h.env.attachment
					var calls atomic.Int32
					h.env.attachment = func(card int) (qualification.Attachment, error) {
						if calls.Add(1) == 2 {
							close(in)
							<-gate
						}
						return attachment(card)
					}
				} else {
					h.env.resolve = func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
						return nativeEndpoint{}, func(alsapcm.NodeInfo) error { close(in); <-gate; return nil }, nil
					}
				}
			}
			defer release()
			if name == "pcm_open" {
				for i, a := range h.args {
					if a == "--scenario=interactive" {
						h.args[i] = "--scenario=matrix"
					}
				}
			}
			var stopped atomic.Bool
			var reads atomic.Int32
			controls := h.env.controls
			h.env.controls = func(card int) (alsactl.Snapshot, error) {
				if stopped.Load() {
					reads.Add(1)
				}
				return controls(card)
			}
			completionObserved := make(chan struct{})
			baseOpen := h.env.open
			h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				sink, e := baseOpen(ctx, c)
				var pending interface{ Completion() <-chan struct{} }
				if errors.As(e, &pending) {
					e = &nativeObservedPending{error: e, pending: pending, entered: completionObserved}
				}
				return sink, e
			}
			armed := make(chan time.Time, 1)
			cleanupTicks := make(chan time.Time)
			baseAt := h.env.at
			h.env.at = func(at time.Time) (<-chan time.Time, func()) {
				if at.Equal(time.Unix(5, 0)) {
					return baseAt(at)
				}
				armed <- at
				return cleanupTicks, func() {}
			}
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(ctx, h.args, h.env, io.Discard, io.Discard) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("real worker never parked")
			}
			stopped.Store(true)
			if name == "admission_deadline" || name == "attachment_deadline" {
				h.ticks <- h.env.now()
			} else if name != "teardown" {
				cancel()
				select {
				case at := <-armed:
					if !at.Equal(time.Unix(2, 0)) {
						t.Errorf("cleanup deadline=%v", at)
					}
				case <-time.After(time.Second):
					t.Fatal("cleanup clock not armed")
				}
				if name == "native_open" || name == "admission" {
					select {
					case <-completionObserved:
					case code := <-answer:
						t.Fatalf("returned without joining inner completion: %d", code)
					case <-time.After(time.Second):
						t.Fatal("inner completion fence never observed")
					}
				}
				select {
				case code := <-answer:
					t.Fatalf("returned before expiry: %d", code)
				default:
				}
				close(cleanupTicks)
			}
			if name == "teardown" {
				k.AdvanceUnwatched(2 * time.Second)
			}
			end := "incomplete"
			if name == "admission_deadline" || name == "attachment_deadline" {
				end = "deadline"
			}
			r := nativeReportResult(t, h, answer, 1, end)
			if !strings.Contains(r.Error, "unconfirmed") {
				t.Error("missing unconfirmed evidence", r.Error)
			}
			if reads.Load() != 0 {
				t.Error("final controls read before worker completion")
			}
			release()
			// The real node eventually closes; no successor has been opened in the meantime.
			k.Await(t, "eventual cleanup", func(v alsapcm.KernelView) bool { return v.Opens > 0 && v.Opens == v.Closes })
			k.CheckClosedEach(t)
			if reads.Load() != 0 {
				t.Error("cancelled opener read controls after returning to the caller")
			}
			if v := k.View(); v.Opens != 1 {
				t.Errorf("successor opens=%d", v.Opens)
			}
		})
	}
}

// Observe the real opener's completion fence without a wall-clock sleep.
type nativeObservedPending struct {
	error
	pending interface{ Completion() <-chan struct{} }
	entered chan struct{}
	once    sync.Once
}

func (e *nativeObservedPending) Unwrap() error { return e.error }
func (e *nativeObservedPending) Completion() <-chan struct{} {
	e.once.Do(func() { close(e.entered) })
	return e.pending.Completion()
}
func (e *nativeObservedPending) CompletionResult() error {
	if r, ok := e.pending.(interface{ CompletionResult() error }); ok {
		return r.CompletionResult()
	}
	return e.error
}

func realDoPRig(t *testing.T) (*nativeHarnessRig, *alsapcm.FakeKernel, *io.PipeWriter) {
	h, k, w := realNativeRig(t)
	k.Free()
	h.args = append([]string{"--transport=dop", "--ack-dop-noise=true"}, h.args...)
	h.args = setNativeArg(h.args, "format", "S32_LE")
	h.args = setNativeArg(setNativeArg(h.args, "scenario", "level-check"), "ack-level", "off-ear")
	h.env.gain = func(int) (bool, error) { return false, nil }
	h.env.kernel = func() (string, error) { return "fake-kernel", nil }
	pcm := make([]byte, 44+176400*3*6)
	copy(pcm, "RIFF")
	binary.LittleEndian.PutUint32(pcm[4:], uint32(len(pcm)-8))
	copy(pcm[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(pcm[16:], 16)
	binary.LittleEndian.PutUint16(pcm[20:], 1)
	binary.LittleEndian.PutUint16(pcm[22:], 2)
	binary.LittleEndian.PutUint32(pcm[24:], 176400)
	binary.LittleEndian.PutUint32(pcm[28:], 176400*6)
	binary.LittleEndian.PutUint16(pcm[32:], 6)
	binary.LittleEndian.PutUint16(pcm[34:], 24)
	copy(pcm[36:], "data")
	binary.LittleEndian.PutUint32(pcm[40:], uint32(len(pcm)-44))
	if e := os.WriteFile(filepath.Join(h.dir, "pcm.wav"), pcm, 0600); e != nil {
		t.Fatal(e)
	}
	h.manifest.Fixtures[0].Rate = 176400
	h.manifest.Fixtures[0].Frames = 176400 * 3
	h.manifest.Fixtures[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256(pcm))
	h.contact.SchemaVersion = 3
	h.contact.Tuple.Transport = "dop"
	h.contact.Tuple.Format = "S32_LE"
	yes := true
	h.contact.DoP = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
	writeDoPEvidence(t, h)
	return h, k, w
}
func writeDoPEvidence(t *testing.T, h *nativeHarnessRig) {
	t.Helper()
	b, e := json.Marshal(h.manifest)
	if e != nil {
		t.Fatal(e)
	}
	h.contact.ManifestHash = fmt.Sprintf("%x", sha256.Sum256(b))
	if e := os.WriteFile(filepath.Join(h.dir, "manifest.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if b, e = json.Marshal(h.contact); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestNativeIntegrationDoPProbeRequiresAcknowledgement(t *testing.T) {
	for _, name := range []string{"level", "noise"} {
		t.Run(name, func(t *testing.T) {
			h, k, _ := realDoPRig(t)
			h.args = setNativeArg(h.args, "scenario", "matrix")
			h.env.stdin = strings.NewReader("")
			if name == "level" {
				h.args = setNativeArg(h.args, "ack-level", "")
			} else {
				h.args = setNativeArg(h.args, "ack-dop-noise", "false")
			}
			if code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard); code == 0 {
				t.Fatal("missing acknowledgement accepted")
			}
			if k.View().Opens != 0 || len(k.Given()) != 0 {
				t.Fatal("acknowledgement opened hardware")
			}
		})
	}
}
func TestNativeIntegrationDoPProbeEvidence(t *testing.T) {
	for _, name := range []string{"pcm_lock_only", "wrong_rate", "replug", "mixer", "missing_snapshot", "overlong", "two_fixtures", "repeated_fixture", "stable_recapture", "no_controls", "feature_unit"} {
		t.Run(name, func(t *testing.T) {
			h, k, w := realDoPRig(t)
			preflight := true
			switch name {
			case "wrong_rate":
				h.manifest.Fixtures[0].Rate = 44100
				h.manifest.Fixtures[0].Frames = 44100 * 3
			case "replug":
				h.args = setNativeArg(h.args, "scenario", "matrix")
				h.contact.Attachment.DevNum++
			case "mixer":
				h.args = setNativeArg(h.args, "scenario", "matrix")
				preflight = false
				calls := 0
				h.env.controls = func(int) (alsactl.Snapshot, error) {
					calls++
					snap := alsactl.Snapshot{}
					if calls > 1 {
						snap.Controls = []alsactl.Control{{Interface: 2, Name: "Playback Volume", Values: []byte{3}, Access: 1}}
					}
					return snap, nil
				}
			case "missing_snapshot":
				h.args = setNativeArg(h.args, "mixer-snapshot", filepath.Join(h.dir, "absent"))
			case "overlong":
				h.args = setNativeArg(h.args, "prefill-ms", "1")
				h.manifest.Fixtures[1].Frames = 2822400 * 10
				h.manifest.Fixtures[0].Frames = 176400 * 10
			case "two_fixtures":
				f := h.manifest.Fixtures[1]
				f.ID = "second"
				h.manifest.Fixtures = append(h.manifest.Fixtures, f)
				h.args = append(h.args, "second")
			case "repeated_fixture":
				h.args = append(h.args, "dsd")
			default:
				preflight = false
			}
			if name == "feature_unit" {
				h.env.gain = func(int) (bool, error) { return true, nil }
			}
			if name == "stable_recapture" {
				snap := alsactl.Snapshot{Controls: []alsactl.Control{{Interface: 2, Name: "Playback Volume", Values: []byte{7}, Access: 1}}}
				h.env.controls = func(int) (alsactl.Snapshot, error) { return snap, nil }
				h.contact.MixerHash = snap.Hash()
			}
			writeDoPEvidence(t, h)
			if name == "mixer" {
				h.env.stdin = strings.NewReader("")
				var diagnostics bytes.Buffer
				code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, &diagnostics)
				if code == 0 || !strings.Contains(diagnostics.String(), "binding/mixer reference changed") || len(k.Given()) != 0 {
					t.Fatalf("drift admitted: code=%d writes=%d error=%s", code, len(k.Given()), diagnostics.String())
				}
				k.CheckClosedEach(t)
				return
			}
			if preflight {
				h.env.stdin = strings.NewReader("")
				var diagnostics bytes.Buffer
				code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, &diagnostics)
				wantError := map[string]string{"two_fixtures": "exactly one fixture", "repeated_fixture": "exactly one fixture", "wrong_rate": "same-carrier", "overlong": "ten seconds", "replug": "attachment mismatch", "mixer": "evidence mismatch", "missing_snapshot": "mixer-snapshot"}[name]
				if code == 0 || k.View().Opens != 0 || !strings.Contains(diagnostics.String(), wantError) {
					t.Fatalf("preflight=%d opens=%d error=%s", code, k.View().Opens, diagnostics.String())
				}
				return
			}
			answer := make(chan int, 1)
			var diagnostics bytes.Buffer
			go func() {
				answer <- runOutputDSDTest(context.Background(), h.args, h.env, nativePromptOutput{h: h}, &diagnostics)
			}()
			for _, phrase := range []string{qualification.CalibrationPhrase, qualification.PCMConfirmation, "recorded knob position", qualification.DSDConfirmation, "DAC DSD indicator confirmed", "L/R pitch and duration correct"} {
				harnessEvent(t, h, phrase)
				response := phrase
				if phrase == "recorded knob position" {
					response = "stable position 7"
				}
				if name == "pcm_lock_only" && phrase == "DAC DSD indicator confirmed" {
					response = "PCM lock only"
				}
				_, _ = io.WriteString(w, response+"\n")
				if response == "PCM lock only" {
					break
				}
			}
			want := 0
			if name == "pcm_lock_only" {
				want = 1
			}
			code := 0
			if name == "pcm_lock_only" {
				select {
				case code = <-answer:
				case <-h.events:
					_, _ = io.WriteString(w, "L/R pitch and duration correct\n")
					code = <-answer
				}
			} else {
				code = <-answer
			}
			if code != want {
				t.Fatalf("code=%d want=%d: %s", code, want, diagnostics.String())
			}
			b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
			if e != nil {
				t.Fatal(e)
			}
			var report nativeReport
			json.Unmarshal(b, &report)
			if want == 0 {
				if report.Contact == nil || report.DoP.Validate() != nil || report.End != "complete" {
					t.Fatalf("missing receipt: %s", b)
				}
			} else if report.Contact != nil || report.Error == "" {
				t.Fatal("PCM lock issued contact")
			}
			if k.View().Opens != 2 {
				t.Fatal("expected PCM and DoP opens", k.View())
			}
			k.CheckClosedEach(t)
		})
	}
}

func TestNativeIntegrationDoPProductionDisabled(t *testing.T) {
	persistentMedium(t)
	for _, mode := range []string{"serve", "output_play", "api"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "api" {
				// GET /v1/outputs' view of a player whose fresh owner-approved
				// record holds only DoP: no DSD codec, no DoP tuple, and nothing
				// opened to say so.
				k := alsapcm.NewFakeKernel(t)
				be := playbacktest.Approved(t, "dop_only", func(cfg playback.Config) (*playback.Backend, error) {
					cfg.OpenSink = k.OpenSink
					return playback.New(cfg)
				})
				ready := make(chan struct{})
				close(ready)
				cfg := playerOutputConfig(t.TempDir(), nil)
				seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
				f := newBridgeOutput(servePlayerPlan{lists: true}, &bridgePlayer{be: be, ready: ready}, cfg).Facts(context.Background())
				for _, c := range f.Codecs {
					if c == "dsf" || c == "dff" {
						t.Fatalf("codecs %v advertise DSD from a DoP-only record", f.Codecs)
					}
				}
				if f.DSDPlayback == nil {
					t.Fatal("no DSD authorization epoch published")
				}
				for _, tu := range f.DSDPlayback.Tuples {
					if tu.Format == "S32_LE" {
						t.Fatalf("published a DoP tuple: %+v", tu)
					}
				}
				if len(f.DSDPlayback.Tuples) != 0 || k.View().Opens != 0 {
					t.Fatalf("tuples %v, opens %d", f.DSDPlayback.Tuples, k.View().Opens)
				}
				return
			}
			data, e := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", "dsd64.dff"))
			if e != nil {
				t.Fatal(e)
			}
			k := alsapcm.NewFakeKernel(t)
			build := newServePlayerBackend
			if mode == "output_play" {
				build = newOutputPlayBackend
			}
			b := playbacktest.Approved(t, "dop_only", func(cfg playback.Config) (*playback.Backend, error) {
				cfg.Resolver = refusedNativeResolver(data)
				cfg.OpenSink = k.OpenSink
				x, e := build(cfg)
				if e != nil {
					return nil, e
				}
				return x.(*playback.Backend), nil
			})
			if _, e = playOneOnEngine(t, b, "dsd", "DoP production refusal engine"); e == nil {
				t.Fatal("production selected DoP")
			}
			if k.View().Opens != 0 || len(b.Policy().Tuples) != 0 {
				t.Fatal("production opened/published DoP")
			}
		})
	}
}

// TestNativeIntegrationDoPEnvelope reads a DoP contact report in both of its
// forms: the harness's report envelope, and the bare contact receipt that
// output_dsdtest.go decodes as a Contact when no envelope holds one. A
// contact the check takes reaches the hardware; each defect is refused by
// the contact check itself, with its own line and nothing else, before
// anything opens, so a run refused for any other reason (a flag, a file, a
// fixture's hash) fails here rather than passing for the wrong refusal.
func TestNativeIntegrationDoPEnvelope(t *testing.T) {
	refusals := map[string]string{
		"version":              "DoP contact report version mismatch",
		"indicator":            "DoP contact report observations incomplete",
		"listening":            "DoP contact report observations incomplete",
		"standalone_indicator": "DoP DSD indicator unconfirmed",
	}
	for _, name := range []string{"valid", "version", "indicator", "listening", "standalone_valid", "standalone_indicator"} {
		t.Run(name, func(t *testing.T) {
			h, k, _ := realDoPRig(t)
			h.args = setNativeArg(h.args, "scenario", "matrix")
			h.env.stdin = strings.NewReader("")
			if name == "standalone_indicator" {
				h.contact.DoP.Indicator = nil
			}
			// writeDoPEvidence leaves the bare contact in contact.json: the
			// standalone cases read it as it is.
			writeDoPEvidence(t, h)
			if !strings.HasPrefix(name, "standalone_") {
				yes := true
				r := nativeReport{SchemaVersion: 2, QualificationVersion: qualification.Version, End: "complete", Contact: &h.contact, DoP: &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}}
				switch name {
				case "version":
					r.SchemaVersion = 1
				case "indicator":
					r.DoP.Indicator = nil
				case "listening":
					r.DoP.PitchDuration = nil
				}
				data, e := json.Marshal(r)
				if e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(h.dir, "contact.json"), data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			var errout strings.Builder
			code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, &errout)
			if refusal, refused := refusals[name]; refused {
				if want := "--contact-report: " + refusal + "\n"; code != 1 || errout.String() != want || k.View().Opens != 0 {
					t.Fatalf("contact %s: code=%d opens=%d errout=%q; want 1, 0 and %q", name, code, k.View().Opens, errout.String(), want)
				}
			} else if code != 1 || errout.String() != "EOF\n" || k.View().Opens == 0 {
				// The check takes the contact, and the matrix plays through to
				// confirmDoP, whose first prompt reads this rig's empty stdin:
				// the console hands it io.EOF, which runOutputDSDTest prints
				// and answers with 1, after the hardware opened.
				t.Fatalf("contact %s: code=%d opens=%d errout=%q; want 1, opens and %q", name, code, k.View().Opens, errout.String(), "EOF\n")
			}
			k.CheckClosedEach(t)
		})
	}
}
