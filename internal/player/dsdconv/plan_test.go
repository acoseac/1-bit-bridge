package dsdconv

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestThePlanIsTheSourceOfItsClaims: every claim a converted occurrence
// makes (rate, valid bits, passband, dither, filter, gain) is its plan's
// method, and the converter's format is the plan's (B728 §4.3, §4.8).
func TestThePlanIsTheSourceOfItsClaims(t *testing.T) {
	if got := Plans(); !slices.Equal(got, allPlans) {
		t.Fatalf("Plans() = %v, want %v", got, allPlans)
	}
	for _, p := range allPlans {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", p, err)
		}
		wantPass, wantStop := 30000, 40000
		if p.RateHz == 44100 {
			wantPass, wantStop = 20000, 22050
		}
		wantDither := DitherNone
		if p.ValidBits == 16 {
			wantDither = DitherTPDF
		}
		if p.PassbandHz() != wantPass || p.StopbandHz() != wantStop {
			t.Errorf("%s: passband %d Hz, stopband %d Hz, want %d and %d", p, p.PassbandHz(), p.StopbandHz(), wantPass, wantStop)
		}
		if p.Dither() != wantDither {
			t.Errorf("%s: dither %q, want %q", p, p.Dither(), wantDither)
		}
		if p.Filter() != FilterName || FilterName != "dsdconv-1" {
			t.Errorf("%s: filter %q (FilterName %q), want dsdconv-1", p, p.Filter(), FilterName)
		}
		if p.GainDB() != 0 {
			t.Errorf("%s: gain %v dB, want 0 (unity)", p, p.GainDB())
		}
		for _, rate := range dsdRates {
			d, err := p.Decimation(rate)
			if err != nil || d != rate/p.RateHz {
				t.Errorf("%s at %s: D = %d, %v; want %d", p, rateName(rate), d, err, rate/p.RateHz)
			}
			const frames = 100000
			c, err := New(newStub(rate, frames, [2][]byte{make([]byte, frames/8), make([]byte, frames/8)}), p, Options{})
			if err != nil {
				t.Errorf("%s at %s: New: %v", p, rateName(rate), err)
				continue
			}
			f := c.Format()
			want := format.PCM{RateHz: p.RateHz, Channels: 2, ValidBits: p.ValidBits, Container: ContainerConverted, Frames: frames / int64(rate/p.RateHz)}
			if f != want {
				t.Errorf("%s at %s: Format() = %+v, want %+v", p, rateName(rate), f, want)
			}
			if err := f.Validate(); err != nil {
				t.Errorf("%s at %s: the format does not validate as PCM: %v", p, rateName(rate), err)
			}
			h, err := p.Window(rate)
			if err != nil || h <= 0 || c.Window() != h {
				t.Errorf("%s at %s: Window %d (%v), the converter's %d", p, rateName(rate), h, err, c.Window())
			}
			if c.Plan() != p || c.Source() != (format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: frames}) {
				t.Errorf("%s at %s: Plan %v, Source %+v", p, rateName(rate), c.Plan(), c.Source())
			}
		}
	}
	// The container plays down the PCM path and is no container a decoder
	// reconstructs samples from.
	if ContainerConverted.IsDSD() {
		t.Error("ContainerConverted.IsDSD() is true: converted PCM would take the native path")
	}
	if slices.Contains(format.Containers(), ContainerConverted) {
		t.Error("format.Containers() lists ContainerConverted")
	}
	for _, p := range []Plan{{192000, 24}, {176400, 32}, {48000, 24}, {44100, 20}, {352800, 24}, {0, 0}, {88200, 8}} {
		if err := p.Validate(); !errors.Is(err, ErrPlan) {
			t.Errorf("%s: Validate = %v, want ErrPlan", p, err)
		}
		if _, err := New(newStub(format.DSD64RateHz, 64, [2][]byte{make([]byte, 8), make([]byte, 8)}), p, Options{}); !errors.Is(err, ErrPlan) {
			t.Errorf("%s: New = %v, want ErrPlan", p, err)
		}
	}
}

// TestOnlyDSD64ToDSD256AreConverted: the converter checks the rate itself,
// against the three it takes. DSD512 is refused in every mode (§3.2), and a
// rate of the 48 kHz family has no integer ratio to the 44.1 kHz one: both
// are a *RateError at construction, never a guess. NativeDSD.Validate takes
// DSD512 and refuses the rest, so no real demuxer reaches those, and a stub
// stands in.
func TestOnlyDSD64ToDSD256AreConverted(t *testing.T) {
	p := Plan{176400, 24}
	planes := [2][]byte{make([]byte, 1024), make([]byte, 1024)}
	for _, rate := range dsdRates {
		c, err := New(newStub(rate, 8192, planes), p, Options{})
		if err != nil {
			t.Errorf("%s: New: %v", rateName(rate), err)
			continue
		}
		_ = c.Close()
	}
	for _, rate := range []int{8 * format.DSD64RateHz, 3072000, 6144000, 12288000, 3 * format.DSD64RateHz, format.DSD64RateHz / 2, 0, -2822400} {
		_, err := New(newStub(rate, 8192, planes), p, Options{})
		var re *RateError
		if !errors.As(err, &re) || re.BitRateHz != rate {
			t.Errorf("%d Hz: New = %v, want a *RateError for it", rate, err)
		}
		if rate == 8*format.DSD64RateHz && (err == nil || !strings.Contains(err.Error(), "DSD512")) {
			t.Errorf("DSD512's refusal %v does not say DSD512", err)
		}
		if _, err := p.Decimation(rate); !errors.As(err, &re) {
			t.Errorf("%d Hz: Decimation = %v, want a *RateError", rate, err)
		}
		if _, err := p.Window(rate); !errors.As(err, &re) {
			t.Errorf("%d Hz: Window = %v, want a *RateError", rate, err)
		}
		f := format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: 1 << 20}
		if Continuous(f, p, f, p) {
			t.Errorf("%d Hz: Continuous is true", rate)
		}
	}
	for _, channels := range []int{1, 6, 0} {
		s := newStub(format.DSD64RateHz, 8192, planes)
		s.f.Channels = channels
		if _, err := New(s, p, Options{}); !errors.Is(err, ErrChannels) {
			t.Errorf("%d channels: New = %v, want ErrChannels", channels, err)
		}
	}
	if _, err := New(newStub(format.DSD64RateHz, -8, planes), p, Options{}); err == nil {
		t.Error("a negative Frames was taken")
	}
	if _, err := New(nil, p, Options{}); err == nil {
		t.Error("a nil source was taken")
	}
}
