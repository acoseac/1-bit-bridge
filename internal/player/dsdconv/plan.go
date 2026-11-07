package dsdconv

import (
	"errors"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// ContainerConverted is the container a converter's Format reports: PCM
// the converter made from DSD. It is neither DSF nor DFF, so IsDSD is false
// and the occurrence plays down the PCM path (the packings table, the PCM
// sink); a DSD container there would send converted PCM down the native
// path. It is in no list of containers a decoder reconstructs samples from
// (format.Containers), so playback.Plays does not take it and
// PlayedContainers does not list it: what tells converted PCM apart is this
// value, or the *Converter itself, never IsDSD.
const ContainerConverted format.Container = "dsd-converted"

// FilterName names this package's filter, the plan's filter on the wire
// (dsdPlayback.converted's filter, B728 §7.1): a change to the chain's
// response is a new name.
const FilterName = "dsdconv-1"

// The dither words a plan's Dither returns, as the wire says them.
const (
	DitherNone = "none"
	DitherTPDF = "tpdf"
)

// Plan is a conversion's target, which the caller chooses from the DAC
// (B728 §4.3): a rate of the 44.1 kHz family, capped at 176.4 kHz, and 24
// valid bits, or 16 for a DAC that takes no 24-bit target. Every claim made
// about a converted occurrence (its rate, valid bits, passband, dither,
// filter and gain) is read from its Plan, and nothing else states them.
type Plan struct {
	RateHz    int
	ValidBits int
}

// ErrPlan refuses a plan that is not one of Plans.
var ErrPlan = errors.New("dsdconv: not a conversion plan")

// Plans is every plan, in the order a DAC's best is chosen: 176.4, 88.2 and
// 44.1 kHz at 24 bits, then the same at 16. The rates are the 44.1 kHz
// family's, whose ratios to every DSD rate are integers, so there is no
// resampler; 352.8 kHz is not one (the owner's cap, B728 §4.3).
func Plans() []Plan {
	return []Plan{{176400, 24}, {88200, 24}, {44100, 24}, {176400, 16}, {88200, 16}, {44100, 16}}
}

// Validate refuses a plan that is not one of Plans.
func (p Plan) Validate() error {
	for _, q := range Plans() {
		if p == q {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrPlan, p)
}

// PassbandHz is the plan's passband edge: 30 kHz, or 20 kHz at 44.1 kHz,
// the compact tier's band; 0 for a plan that is none.
func (p Plan) PassbandHz() int {
	switch {
	case p.Validate() != nil:
		return 0
	case p.RateHz == 44100:
		return 20000
	}
	return 30000
}

// StopbandHz is where the plan's stopband begins, at least 100 dB down:
// 40 kHz, or 22.05 kHz, its Nyquist, at 44.1 kHz; 0 for a plan that is
// none.
func (p Plan) StopbandHz() int {
	switch {
	case p.Validate() != nil:
		return 0
	case p.RateHz == 44100:
		return 22050
	}
	return 40000
}

// Dither is the plan's dither word: DitherNone at 24 bits, which round half
// to even, and DitherTPDF at 16; "" for a plan that is none.
func (p Plan) Dither() string {
	switch {
	case p.Validate() != nil:
		return ""
	case p.ValidBits == 16:
		return DitherTPDF
	}
	return DitherNone
}

// Filter is the plan's filter, FilterName; "" for a plan that is none.
func (p Plan) Filter() string {
	if p.Validate() != nil {
		return ""
	}
	return FilterName
}

// GainDB is the plan's gain, 0: unity, with no gain a stream was not
// measured for (B728 §4.4).
func (p Plan) GainDB() float64 { return 0 }

// String names the plan as rate/bits, 176400/24.
func (p Plan) String() string { return fmt.Sprintf("%d/%d", p.RateHz, p.ValidBits) }

// Decimation is D, the total ratio from a DSD rate to the plan's rate: 16
// for DSD64 to 176.4 kHz. A rate the converter does not take is a
// *RateError.
func (p Plan) Decimation(bitRateHz int) (int, error) {
	g, err := geometryFor(bitRateHz, p)
	if err != nil {
		return 0, err
	}
	return g.d, nil
}

// Window is H, the composite filter's half-length in output frames at a DSD
// rate: the frames before a continuous join that depend on the successor's
// first bits (B728 §4.7). It is read from the chain's taps, never typed:
// 72 at 176.4 kHz, 36 at 88.2 and 78 at 44.1, at every DSD rate.
func (p Plan) Window(bitRateHz int) (int, error) {
	g, err := geometryFor(bitRateHz, p)
	if err != nil {
		return 0, err
	}
	return g.h, nil
}

// RateError refuses a DSD rate the converter does not take: DSD512, which
// B728 §3.2 refuses in every mode, and any rate but DSD64, DSD128 and
// DSD256, whose ratios to the 44.1 kHz family are integers (a 48 kHz
// family's 3.072 MHz has none).
type RateError struct{ BitRateHz int }

func (e *RateError) Error() string {
	if e.BitRateHz == 8*format.DSD64RateHz {
		return "dsdconv: DSD512 is not converted"
	}
	return fmt.Sprintf("dsdconv: a DSD rate of %d Hz is not DSD64, DSD128 or DSD256", e.BitRateHz)
}
