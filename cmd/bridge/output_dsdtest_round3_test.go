package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
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

func TestNativeReceiptEndpoint(t *testing.T) {
	for _, name := range []string{"device", "subdevice", "missing_device", "missing_subdevice", "valid_nonzero"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			h.env.resolve = func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
				return nativeEndpoint{Endpoint: alsapcm.Endpoint{Device: 2}, Subdevice: 3}, nil, nil
			}
			b, _ := json.Marshal(h.contact)
			var c map[string]any
			_ = json.Unmarshal(b, &c)
			c["Device"] = 2
			c["Subdevice"] = 3
			switch name {
			case "device":
				c["Device"] = 0
			case "subdevice":
				c["Subdevice"] = 0
			case "missing_device":
				delete(c, "Device")
			case "missing_subdevice":
				delete(c, "Subdevice")
			}
			b, _ = json.Marshal(c)
			if e := os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600); e != nil {
				t.Fatal(e)
			}
			calls := 0
			h.env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
				calls++
				return nil, errors.New("test stop")
			}
			h.env.openPCM = func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) {
				calls++
				return nil, errors.New("test stop")
			}
			_ = runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard)
			want := 0
			if name == "valid_nonzero" {
				want = 1
			}
			if calls != want {
				t.Fatalf("opener calls=%d want=%d", calls, want)
			}
		})
	}
}
func TestNativeReceiptEnvelope(t *testing.T) {
	for _, name := range []string{"incomplete", "error"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			r := nativeReport{End: "complete", Contact: &h.contact}
			if name == "incomplete" {
				r.End = "incomplete"
			} else {
				r.Error = "final controls failed"
			}
			b, _ := json.Marshal(r)
			if e := os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600); e != nil {
				t.Fatal(e)
			}
			calls := 0
			h.env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
				calls++
				return nil, errors.New("stop")
			}
			runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard)
			if calls != 0 {
				t.Fatal("failed envelope reached opener")
			}
		})
	}
}

type nativeCompleteFake struct {
	*nativeHarnessFake
	bits      atomic.Int64
	played    int64
	finishing chan struct{}
}

func (s *nativeCompleteFake) WriteDSD(_ context.Context, _ uint64, _, _ []byte, _, n int) (int, error) {
	s.bits.Add(int64(n))
	return n, nil
}
func (s *nativeCompleteFake) Finish(ctx context.Context) error {
	if s.finishing != nil {
		close(s.finishing)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (s *nativeCompleteFake) Position() alsapcm.DSDPosition {
	p := s.played
	if p < 0 {
		p = s.bits.Load()
	}
	return alsapcm.DSDPosition{Submitted: s.bits.Load(), Played: p}
}

type nativePromptDriver struct {
	w     *io.PipeWriter
	final func()
}

func (d nativePromptDriver) Write(b []byte) (int, error) {
	text := string(b)
	answer := ""
	switch {
	case strings.Contains(text, qualification.CalibrationPhrase):
		answer = qualification.CalibrationPhrase
	case strings.Contains(text, qualification.PCMConfirmation):
		answer = qualification.PCMConfirmation
	case strings.Contains(text, "knob position"):
		answer = "one"
	case strings.Contains(text, qualification.DSDConfirmation):
		if d.final != nil {
			d.final()
		}
		answer = qualification.DSDConfirmation
	}
	if answer != "" {
		_, e := io.WriteString(d.w, answer+"\n")
		if e != nil {
			return 0, e
		}
	}
	return len(b), nil
}
func TestNativeContactFinalValidation(t *testing.T) {
	for _, name := range []string{"value", "membership", "attachment", "read_error", "cancel", "cancel_final", "deadline_final", "volatile_pcm", "valid"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			h.args = h.args[:len(h.args)-3]
			for i, a := range h.args {
				if a == "--scenario=interactive" {
					h.args[i] = "--scenario=level-check"
				}
				if a == "--ack-level=contact-position" {
					h.args[i] = "--ack-level=off-ear"
				}
			}
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			h.env.stdin = nativePipeInput{r}
			h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				return &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var final atomic.Bool
			controls := h.env.controls
			attachment := h.env.attachment
			h.env.controls = func(card int) (alsactl.Snapshot, error) {
				s, e := controls(card)
				s.Controls = []alsactl.Control{{Name: "Gain", Values: []byte{1}}}
				if final.Load() {
					switch name {
					case "read_error":
						return s, errors.New("read failed")
					case "value":
						s.Controls[0].Values = []byte{2}
					case "membership":
						s.Controls = append(s.Controls, alsactl.Control{Name: "changed"})
					case "volatile_pcm":
						s.Controls = append(s.Controls, alsactl.Control{Name: "volatile", Interface: 3, Access: 1 << 2})
					case "cancel_final":
						cancel()
					case "deadline_final":
						h.ticks <- h.env.now()
						<-ctx.Done()
					}
				}
				return s, e
			}
			h.env.attachment = func(card int) (qualification.Attachment, error) {
				a, e := attachment(card)
				if final.Load() && name == "attachment" {
					a.DevNum++
				}
				return a, e
			}
			out := nativePromptDriver{w: w, final: func() {
				final.Store(true)
				if name == "cancel" {
					cancel()
				}
			}}
			code := runOutputDSDTest(ctx, h.args, h.env, out, io.Discard)
			b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
			if e != nil {
				t.Fatal(e)
			}
			var report nativeReport
			if e = json.Unmarshal(b, &report); e != nil {
				t.Fatal(e)
			}
			if name == "valid" || name == "volatile_pcm" {
				if code != 0 || report.Contact == nil {
					t.Fatalf("valid final validation: code=%d report=%+v", code, report)
				}
			} else if code == 0 || report.Contact != nil || report.End == "complete" {
				t.Fatalf("failed final validation published contact: code=%d contact=%v end=%s", code, report.Contact != nil, report.End)
			}
		})
	}
}
func TestNativeRequiredOccurrences(t *testing.T) {
	for _, name := range []string{"skip_queued", "duplicate", "subset"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			o, e := parseNativeOptions(h.args, io.Discard)
			if e != nil {
				t.Fatal(e)
			}
			o.scenario = "matrix"
			o.list = []string{"dsd"}
			if name == "duplicate" {
				o.list = append(o.list, "dsd")
			}
			fixtures := map[string]qualification.Fixture{}
			for _, f := range h.manifest.Fixtures {
				fixtures[f.ID] = f
			}
			finish := make(chan struct{})
			keys := make(chan byte, 1)
			h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				f := &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: fixtures["dsd"].Frames}
				if name == "skip_queued" {
					f.played = 0
					f.finishing = finish
				}
				return f, nil
			}
			if name == "skip_queued" {
				go func() { <-finish; keys <- 'n' }()
			}
			var report nativeReport
			e = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report)
			if name == "subset" {
				if e != nil || report.End != "complete" {
					t.Fatalf("supplied subset not complete: %v %s", e, report.End)
				}
			} else if e == nil || report.End == "complete" {
				t.Fatalf("unplayed occurrence complete: %v %s", e, report.End)
			}
		})
	}
}

type nativeParkedClose struct {
	*nativeCompleteFake
	entered, gate chan struct{}
}

func (s *nativeParkedClose) Release(ctx context.Context, m alsapcm.ReleaseMode) error {
	close(s.entered)
	go func() { <-s.gate; _ = s.nativeHarnessFake.Release(context.Background(), m) }()
	select {
	case <-s.done:
		return nil
	case <-time.After(2 * time.Second):
		return alsapcm.ErrReleaseUnconfirmed
	}
}
func TestNativeHarnessPendingWorkers(t *testing.T) {
	for _, name := range []string{"native_open", "pcm_open", "admission", "teardown"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			h.args = h.args[:len(h.args)-3]
			entered, gate := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unpark := func() { once.Do(func() { close(gate) }) }
			defer unpark()
			var cancelled atomic.Bool
			var finalReads atomic.Int32
			old := h.env.controls
			h.env.controls = func(card int) (alsactl.Snapshot, error) {
				if cancelled.Load() {
					finalReads.Add(1)
				}
				return old(card)
			}
			h.env.open = func(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				if name == "native_open" {
					close(entered)
					<-gate
					return nil, ctx.Err()
				}
				if name == "admission" {
					close(entered)
					<-gate
					return nil, ctx.Err()
				}
				f := &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}
				return &nativeParkedClose{nativeCompleteFake: f, entered: entered, gate: gate}, nil
			}
			if name == "pcm_open" {
				for i, a := range h.args {
					if a == "--scenario=interactive" {
						h.args[i] = "--scenario=matrix"
					}
				}
				h.env.openPCM = func(ctx context.Context, _ alsapcm.SinkConfig) (nativePCMSink, error) {
					close(entered)
					<-gate
					return nil, ctx.Err()
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(ctx, h.args, h.env, io.Discard, io.Discard) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker never started")
			}
			cancelled.Store(true)
			cancel()
			returned := false
			select {
			case code := <-answer:
				returned = true
				if code != 1 {
					t.Errorf("pending worker exit=%d", code)
				}
			case <-time.After(2400 * time.Millisecond):
				t.Error("harness exceeded original cleanup allowance")
			}
			if finalReads.Load() != 0 {
				t.Error("read final controls with worker still in flight")
			}
			if returned {
				b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
				if e != nil {
					t.Fatal(e)
				}
				var r nativeReport
				_ = json.Unmarshal(b, &r)
				if !strings.Contains(r.Error, "unconfirmed") {
					t.Errorf("missing unconfirmed evidence: %s", r.Error)
				}
			}
			unpark()
			if !returned {
				select {
				case <-answer:
				case <-time.After(time.Second):
					t.Fatal("worker did not settle")
				}
			}
		})
	}
}

func TestNativeFirstPreviousIgnored(t *testing.T) {
	h := harnessRig(t)
	o, e := parseNativeOptions(h.args, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	keys := make(chan byte, 2)
	keys <- 'p'
	keys <- 's'
	var report nativeReport
	if e = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report); e != nil {
		t.Fatal(e)
	}
	if h.opens.Load() != 1 || len(report.IgnoredKeys) != 1 || report.Navigated {
		t.Fatalf("first previous restarted: opens=%d ignored=%v navigated=%v", h.opens.Load(), report.IgnoredKeys, report.Navigated)
	}
}

type nativeFinishKey struct {
	*nativeCompleteFake
	keys chan byte
}

func (s *nativeFinishKey) Finish(context.Context) error { return nil }
func (s *nativeFinishKey) Release(ctx context.Context, mode alsapcm.ReleaseMode) error {
	// The EOF branch is already selected, but it has not published completion.
	// Deliver navigation in that window, without depending on select randomness.
	s.keys <- ' '
	s.keys <- 'n'
	return s.nativeHarnessFake.Release(ctx, mode)
}
func TestNativeNavigationRacesCompletion(t *testing.T) {
	h := harnessRig(t)
	o, e := parseNativeOptions(h.args, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	o.scenario = "matrix"
	o.list = []string{"dsd"}
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	for range 20 {
		keys := make(chan byte, 2)
		h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
			return &nativeFinishKey{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}, keys: keys}, nil
		}
		var report nativeReport
		e = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report)
		if e == nil || report.End == "complete" {
			t.Fatal("queued navigation lost to natural completion")
		}
	}
}

type nativeEndpointComplete struct {
	*nativeCompleteFake
	node alsapcm.NodeInfo
}

func (s *nativeEndpointComplete) Activate(_ context.Context, _ uint64) error {
	return s.cfg.Check(s.node)
}
func TestNativeNonzeroEndpointLegs(t *testing.T) {
	h := harnessRig(t)
	for i, a := range h.args {
		if a == "--scenario=interactive" {
			h.args[i] = "--scenario=matrix"
		}
	}
	device, subdevice := 2, 3
	h.contact.Device = &device
	h.contact.Subdevice = &subdevice
	b, _ := json.Marshal(h.contact)
	if e := os.WriteFile(filepath.Join(h.dir, "contact.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	h.env.resolve = func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
		return nativeEndpoint{Endpoint: alsapcm.Endpoint{Device: device}, Subdevice: subdevice}, nil, nil
	}
	node := alsapcm.NodeInfo{Device: device, Subdevice: subdevice}
	var pcm, native int
	h.env.openPCM = func(_ context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) {
		pcm++
		if e := c.Admit(node); e != nil {
			return nil, e
		}
		return nativePCMStub{}, nil
	}
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		native++
		return &nativeEndpointComplete{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}, node: node}, nil
	}
	if code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard); code != 0 || pcm != 2 || native != 1 {
		t.Fatalf("valid nonzero endpoint: exit=%d PCM=%d native=%d", code, pcm, native)
	}
}

func TestNativeQualificationNavigationStops(t *testing.T) {
	h := harnessRig(t)
	o, e := parseNativeOptions(h.args, io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	o.scenario = "matrix"
	o.list = []string{"dsd", "dsd"}
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	keys := make(chan byte, 1)
	finish := make(chan struct{})
	calls := 0
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		calls++
		f := &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}
		if calls == 1 {
			f.played = 0
			f.finishing = finish
		}
		return f, nil
	}
	go func() { <-finish; keys <- 'n' }()
	var report nativeReport
	e = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report)
	if e == nil || calls != 1 || !report.Navigated {
		t.Fatalf("qualification navigation continued: opens=%d err=%v", calls, e)
	}
}
