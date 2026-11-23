package playback

import (
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// boardBox is fixtureBox whose native altset lists DSD_U32_BE and then
// DSD_U32_LE, so a probe's refusal of the first offers the second.
type boardBox struct{ fixtureBox }

func (b boardBox) PlaybackAltsets(card, device int) ([]alsa.Altset, error) {
	alts, err := b.fixtureBox.PlaybackAltsets(card, device)
	for i := range alts {
		if slices.Contains(alts[i].Formats, "DSD_U32_BE") {
			alts[i].Formats = []string{"SPECIAL", "DSD_U32_BE", "DSD_U32_LE"}
		}
	}
	return alts, err
}

// TestACandidatesChangeIsANewSet (B728 B12 PR 3, round 3): a candidate
// (policy.Plan.Candidate) is part of what the set publishes and admits, so a
// change of one alone, its refusal's word the same, publishes a new set at
// the next generation, which the DSD check is judged against: on a generic
// board's IDs under an explicit native (no DAC list reads), the cap moved
// down to DSD64 and back up (DSD128's and DSD256's candidates gone and back),
// the kernel's pause found at DSD64 (its timing), DSD_U32_BE refused at
// DSD128 where the DAC offers DSD_U32_LE (its format). Evidence alone, a
// driver_accepted of a candidate's tuple, is no change of it.
func TestACandidatesChangeIsANewSet(t *testing.T) {
	b, store := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Profiles, p.Box = nil, boardBox{} })
	key, err := dsdstate.KeyOf(fixtureUSB(), "Test DAC")
	if err != nil {
		t.Fatal(err)
	}
	unit := alsapcm.ProbeUnit{Model: key.Canonical(), Stamp: string(fixtureStamp), Firmware: fixtureBCD, Kernel: fixtureKernel, Enumeration: "test/2"}
	update := func(f func(d *dsdstate.DAC)) func() {
		return func() {
			if err := store.Update(func(st *dsdstate.State) error { f(st.Entry(key, fixtureStamp)); return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	capAt := func(max int) func() {
		return update(func(d *dsdstate.DAC) {
			d.Choice = &dsdstate.Choice{Mode: dsdstate.ModeNative, MaxBitRate: max, Revision: 1, SetAt: fixtureT0, SetBy: dsdstate.ByApp}
		})
	}
	probed := func(res alsapcm.ProbeResult) func() {
		return func() {
			if !b.cfg.DSD.Probes.Record(b.cfg.DSD.Probes.Begin(unit), alsapcm.ProbeReport{Opened: true, Results: []alsapcm.ProbeResult{res}}) {
				t.Fatal("the probe's answer was not kept")
			}
		}
	}
	u32be := func(rate int) alsapcm.ProbeCandidate {
		return alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: rate / 32}
	}
	// at is the published set's plan at rate, a native_check_required
	// refusal, and its candidate.
	at := func(what string, rate int) *policy.Plan {
		t.Helper()
		p := b.dsd.Load().set.At(rate).Plan
		if p.Outcome != policy.OutcomeRefused || p.Reason != policy.ReasonNativeCheckRequired {
			t.Fatalf("%s: the set at %d is %s %s; want native_check_required", what, rate, p.Outcome, p.Reason)
		}
		return p.Candidate
	}
	// carries fails the test unless the set carries a candidate at each rate
	// with want's format and pause, and none where want has no entry.
	type tuple struct {
		f     format.ALSAFormat
		pause string
	}
	carries := func(what string, want map[int]tuple) {
		t.Helper()
		for _, rate := range []int{dsdstate.DSD64, dsdstate.DSD128, dsdstate.DSD256} {
			c, w := at(what, rate), want[rate]
			switch {
			case c == nil && w.f != "":
				t.Errorf("%s: no candidate at %d; want %s with pause %s", what, rate, w.f, w.pause)
			case c != nil && w.f == "":
				t.Errorf("%s: a candidate at %d, %+v; want none", what, rate, c.Tuple)
			case c != nil && (c.Tuple.Format != w.f || c.Tuple.Pause != w.pause):
				t.Errorf("%s: the candidate at %d is %s with pause %s; want %s with pause %s", what, rate, c.Tuple.Format, c.Tuple.Pause, w.f, w.pause)
			}
		}
	}
	every := map[int]tuple{dsdstate.DSD64: {format.DSDU32BE, policy.PauseSilence}, dsdstate.DSD128: {format.DSDU32BE, policy.PauseSilence},
		dsdstate.DSD256: {format.DSDU32BE, policy.PauseSilence}}
	carries("as the backend starts", every)
	for _, c := range []struct {
		what   string
		change func()
		moves  bool
		want   map[int]tuple
	}{
		{"the cap moved down to DSD64", capAt(dsdstate.DSD64), true, map[int]tuple{dsdstate.DSD64: every[dsdstate.DSD64]}},
		{"the cap moved up again", capAt(0), true, every},
		{"the kernel's pause found at DSD64", probed(alsapcm.ProbeResult{Candidate: u32be(dsdstate.DSD64), Answer: alsapcm.ProbeAccepted, Msbits: 32, Pause: true}), true,
			map[int]tuple{dsdstate.DSD64: {format.DSDU32BE, policy.PauseKernel}, dsdstate.DSD128: every[dsdstate.DSD128], dsdstate.DSD256: every[dsdstate.DSD256]}},
		{"DSD_U32_BE refused at DSD128", probed(alsapcm.ProbeResult{Candidate: u32be(dsdstate.DSD128), Answer: alsapcm.ProbeRefused}), true,
			map[int]tuple{dsdstate.DSD64: {format.DSDU32BE, policy.PauseKernel}, dsdstate.DSD128: {format.DSDU32LE, policy.PauseSilence}, dsdstate.DSD256: every[dsdstate.DSD256]}},
		{"a driver_accepted of the candidate's tuple at DSD256", update(func(d *dsdstate.DAC) {
			d.Evidence = append(d.Evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: string(format.DSDU32BE), BitRate: dsdstate.DSD256,
				BCDDevice: fixtureBCD, DriverAcceptedAt: fixtureT0, Kernel: fixtureKernel})
		}), false, map[int]tuple{dsdstate.DSD64: {format.DSDU32BE, policy.PauseKernel}, dsdstate.DSD128: {format.DSDU32LE, policy.PauseSilence}, dsdstate.DSD256: every[dsdstate.DSD256]}},
	} {
		before := b.dsd.Load()
		c.change()
		b.RefreshDSD(true)
		after := b.dsd.Load()
		switch {
		case c.moves && (after == before || after.gen != before.gen+1):
			t.Errorf("%s: the set stayed at generation %d; want a new set at the next", c.what, before.gen)
		case !c.moves && after != before:
			t.Errorf("%s: a new set at generation %d; want the set as it was", c.what, after.gen)
		}
		carries(c.what, c.want)
		if c.moves && before.same(after) {
			t.Errorf("%s: the sets before and after compare the same", c.what)
		}
	}
}
