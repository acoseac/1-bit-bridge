package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// observed gives the state what this box observed of native DSD_U32_BE and
// of DoP at rate under the unit's firmware: the driver's acceptance under
// the running kernel, and 5 s streamed with no fault.
func (r *rig) observed(rate int) {
	r.evidence = append(r.evidence,
		dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE", BitRate: rate, BCDDevice: bcd, DriverAcceptedAt: t0, Kernel: kernel, StreamingObservedAt: t0.Add(time.Minute)},
		dsdstate.Evidence{Transport: dsdstate.TransportDoP, Format: "S32_LE", BitRate: rate, BCDDevice: bcd, DriverAcceptedAt: t0, Kernel: kernel, StreamingObservedAt: t0.Add(time.Minute)})
}

// TestARejectionOutranksEveryPositiveInput: native DSD under auto, on a
// known DAC (its profile's claim, the driver's acceptance, streaming
// observed, an acknowledgement) and on an unknown one (a clean answer, an
// acknowledgement), plays while the latest answer is clean, and a later
// noise or nothing, which replaces that clean, takes native out at that rate
// whatever else holds: auto then tries DoP, or converts; an explicit native
// refuses. An answer under another firmware neither rejects nor confirms on
// this one.
func TestARejectionOutranksEveryPositiveInput(t *testing.T) {
	for _, known := range []bool{true, false} {
		name := "a known DAC"
		if !known {
			name = "an unknown DAC"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t).everything(dsdstate.ModeAuto).withoutCarrier()
			r.observed(testRate)
			if !known {
				r.known, r.profileNative, r.profileDoP = false, nil, nil
			}
			p := r.plan(testRate)
			wantOutcome(t, "after a clean answer", p, OutcomeNative)
			if p.Evidence != RungListenerConfirmed {
				t.Errorf("after a clean answer the tuple's rung is %q, want %q", p.Evidence, RungListenerConfirmed)
			}
			for _, answer := range []string{dsdstate.AnswerNoise, dsdstate.AnswerNothing} {
				r.hear(dsdstate.TransportNative, testRate, answer)
				wantOutcome(t, "after a later "+answer, r.plan(testRate), OutcomeConverted)
				wantOutcome(t, "at another rate, after a later "+answer, r.plan(dsd64), OutcomeNative)
			}
			// With DoP qualifying, auto takes it in native's place.
			d := newRig(t).everything(dsdstate.ModeAuto)
			d.observed(testRate)
			if !known {
				d.known, d.profileNative, d.profileDoP = false, nil, nil
			}
			d.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise)
			wantOutcome(t, "after a rejection of native with DoP qualifying", d.plan(testRate), OutcomeDoP)
			d.hear(dsdstate.TransportDoP, testRate, dsdstate.AnswerNoise)
			wantOutcome(t, "after rejections of both", d.plan(testRate), OutcomeConverted)
			// An explicit native refuses.
			e := newRig(t).everything(dsdstate.ModeNative)
			e.observed(testRate)
			if !known {
				e.known, e.profileNative, e.profileDoP = false, nil, nil
			}
			e.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise)
			wantOutcome(t, "an explicit native after a rejection", e.plan(testRate), OutcomeRefused, ReasonListenerRejected)
			// The latest answer is this firmware's: a clean one under an older
			// firmware beside it confirms nothing, and a noise under another
			// rejects nothing here.
			f := newRig(t).everything(dsdstate.ModeAuto).withoutCarrier()
			if !known {
				f.known, f.profileNative, f.profileDoP = false, nil, nil
			}
			f.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise)
			f.listener = append(f.listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: testRate, BCDDevice: "0099", Answer: dsdstate.AnswerClean})
			wantOutcome(t, "a noise beside a clean under an older firmware", f.plan(testRate), OutcomeConverted)
			g := newRig(t).everything(dsdstate.ModeAuto).withoutCarrier()
			if !known {
				g.known, g.profileNative, g.profileDoP = false, nil, nil
			}
			g.listener = append(g.listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: dsd256, BCDDevice: "0099", Answer: dsdstate.AnswerNoise})
			wantOutcome(t, "a noise under another firmware", g.plan(dsd256), OutcomeNative)
		})
	}
}

// TestAutoConvertsAboveTheCapAndAnExplicitChoiceRefuses: a maxBitRate caps
// native and DoP; auto converts the rates above it, an explicit native or
// dop refuses them as dsd_rate_above_choice and never converts, and convert
// has no maximum.
func TestAutoConvertsAboveTheCapAndAnExplicitChoiceRefuses(t *testing.T) {
	cases := []struct {
		mode    string
		without func(*rig) *rig
		under   Outcome
	}{
		{dsdstate.ModeAuto, (*rig).withoutCarrier, OutcomeNative},
		{dsdstate.ModeAuto, (*rig).withoutNative, OutcomeDoP},
		{dsdstate.ModeNative, (*rig).withoutCarrier, OutcomeNative},
		{dsdstate.ModeDoP, (*rig).withoutNative, OutcomeDoP},
		{dsdstate.ModeConvert, (*rig).withoutNative, OutcomeConverted},
	}
	for _, c := range cases {
		r := c.without(newRig(t).everything(c.mode))
		r.max = dsd64
		what := c.mode + " capped at DSD64"
		wantOutcome(t, what+", DSD64", r.plan(dsd64), c.under)
		for _, rate := range []int{dsd128, dsd256} {
			p := r.plan(rate)
			switch c.mode {
			case dsdstate.ModeAuto, dsdstate.ModeConvert:
				wantOutcome(t, what+", above the cap", p, OutcomeConverted)
			default:
				wantOutcome(t, what+", above the cap", p, OutcomeRefused, ReasonAboveChoice)
			}
		}
		r.max = 0
		for _, rate := range dsdRates {
			wantOutcome(t, c.mode+" with no cap", r.plan(rate), c.under)
		}
	}
	// A DAC with no choice of its own takes defaultMode with no cap.
	r := newRig(t).everything(dsdstate.ModeAuto).withoutCarrier()
	r.mode = ""
	for _, rate := range dsdRates {
		wantOutcome(t, "defaultMode auto", r.plan(rate), OutcomeNative)
	}
	if s := Effective(r.inputs()); s.Own || s.Mode != dsdstate.ModeAuto || s.MaxBitRate != 0 {
		t.Errorf("the choice that applied: mode %q, cap %d, own %v; want defaultMode auto with no cap", s.Mode, s.MaxBitRate, s.Own)
	}
	r.defaultMode = dsdstate.ModeOff
	for _, rate := range dsdRates {
		wantOutcome(t, "defaultMode off", r.plan(rate), OutcomeRefused, ReasonOffByChoice)
	}
}

// migrated is the state a migration of a native, auto or dop owner record
// that approved native DSD64 alone writes for the test DAC, through the
// migration's own logic (dsdstate.Migrate).
func migrated(t *testing.T, mode string, allowDoP bool) dsdstate.State {
	t.Helper()
	legacy := dsdstate.Legacy{DSDMode: mode, AllowDoP: &allowDoP, DSDQualification: "approved.json"}
	s, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: legacy, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	bound := &dsdstate.Bound{Key: dacKey(t), Identity: qualification.Identity{Vendor: "1234", ProductID: "5678", Manufacturer: "Test", Product: "Test DAC", BCDDevice: bcd, Port: "platform-xhci-hcd.0/usb1-1:1.0"}}
	record := dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: bcd, QualifiedOn: "2026-11-01", Native: []int{dsd64}}
	s, _, err = dsdstate.Migrate(s, dsdstate.Input{Legacy: legacy, Bound: bound, Record: record, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAdmissionIsPerRateForAMigratedAcknowledgement: an owner who approved
// native DSD64 alone has, once migrated, native's first use acknowledged at
// DSD64 alone: native plays there, and DSD128 and DSD256 refuse as
// first_use_pending under the migrated native and convert under a migrated
// auto, until an acknowledgement of the transport covers every rate.
func TestAdmissionIsPerRateForAMigratedAcknowledgement(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeNative, dsdstate.ModeAuto} {
		r := newRig(t).withoutCarrier()
		r.known = true
		in := r.inputs()
		in.State = migrated(t, mode, false)
		s := Effective(in)
		wantOutcome(t, "migrated "+mode+", DSD64", s.At(dsd64).Plan, OutcomeNative)
		for _, rate := range []int{dsd128, dsd256} {
			p := s.At(rate).Plan
			if mode == dsdstate.ModeNative {
				wantOutcome(t, "migrated native above its record's rates", p, OutcomeRefused, ReasonFirstUsePending)
			} else {
				wantOutcome(t, "migrated auto above its record's rates", p, OutcomeConverted)
			}
		}
		// A later acknowledgement of the transport covers every rate.
		d := &in.State.DACs[0]
		for i := range d.FirstUse {
			d.FirstUse[i].BitRates = nil
		}
		for _, rate := range dsdRates {
			wantOutcome(t, "migrated "+mode+" acknowledged since", Effective(in).At(rate).Plan, OutcomeNative)
		}
	}
}

// TestNoDoPOnAnUnknownDACWithoutConfirmation: DoP on a DAC the database does
// not know needs the user's confirmation under this firmware: auto converts
// without it, however clean the answers, an explicit dop refuses as
// dop_confirmation_required; and a known DAC with no DoP claim at a rate
// treats DoP there as an unknown DAC does.
func TestNoDoPOnAnUnknownDACWithoutConfirmation(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeDoP} {
		r := newRig(t).everything(mode).withoutNative()
		r.known, r.profileNative, r.profileDoP = false, nil, nil
		for _, rate := range dsdRates {
			wantOutcome(t, mode+" on an unknown DAC, confirmed", r.plan(rate), OutcomeDoP)
		}
		for _, how := range []string{"without a confirmation", "with a confirmation under another firmware"} {
			r.confirm()
			if how == "without a confirmation" {
				r.confirmed = nil
			} else {
				r.confirmed.BCDDevice = "0200"
			}
			for _, rate := range dsdRates {
				p := r.plan(rate)
				if mode == dsdstate.ModeAuto {
					wantOutcome(t, "auto on an unknown DAC "+how, p, OutcomeConverted)
				} else {
					wantOutcome(t, "dop on an unknown DAC "+how, p, OutcomeRefused, ReasonConfirmationRequired)
				}
			}
		}
		// A known DAC whose profile claims DoP to DSD64 alone.
		k := newRig(t).everything(mode).withoutNative()
		k.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd64, StatedMaxBitRate: dsd64}
		k.confirmed = nil
		wantOutcome(t, mode+" on a known DAC at its claimed rate", k.plan(dsd64), OutcomeDoP)
		for _, rate := range []int{dsd128, dsd256} {
			p := k.plan(rate)
			if mode == dsdstate.ModeAuto {
				wantOutcome(t, "auto on a known DAC past its claim, unconfirmed", p, OutcomeConverted)
			} else {
				wantOutcome(t, "dop on a known DAC past its claim, unconfirmed", p, OutcomeRefused, ReasonConfirmationRequired)
			}
		}
		k.confirm()
		for _, rate := range dsdRates {
			wantOutcome(t, mode+" on a known DAC, confirmed", k.plan(rate), OutcomeDoP)
		}
	}
}

// TestAStateThatDoesNotReadRefusesEveryRate: a state that did not read
// refuses DSD at every rate as dsd_state_unreadable, whatever the choice it
// holds and whatever the DAC offers, conversion included, before any of
// section 3.2's rows is read.
func TestAStateThatDoesNotReadRefusesEveryRate(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative, dsdstate.ModeDoP, dsdstate.ModeConvert, dsdstate.ModeOff} {
		in := newRig(t).everything(mode).inputs()
		in.State.Warnings = append(in.State.Warnings, dsdstate.Warning{Code: dsdstate.WarningStateUnreadable, At: t0})
		s := Effective(in)
		if !s.Unreadable || s.Conversion != nil {
			t.Errorf("%s: the set says unreadable %v, conversion %v", mode, s.Unreadable, s.Conversion)
		}
		for _, rate := range dsdRates {
			wantOutcome(t, mode+" over a state that did not read", s.At(rate).Plan, OutcomeRefused, ReasonStateUnreadable)
		}
	}
	// The state that takes a damaged one's place refuses it the same way.
	r := newRig(t).everything(dsdstate.ModeNative)
	r.unreadable = true
	for _, rate := range dsdRates {
		wantOutcome(t, "the refusing state", r.plan(rate), OutcomeRefused, ReasonStateUnreadable)
	}
}

// TestAProfilesRefusedRateOutranksItsStatedMaximumAndAConfirmation: a rate
// the profile records as refused or heard as silence is no DoP claim at that
// rate, whatever its stated maximum, and the user's confirmation does not
// count there: an explicit dop refuses it as dop_not_offered and auto
// converts it. Only this box's own latest listener answer for DoP at that
// rate, clean, overrides it; a rejection there does not. The profile's
// native claim holds no refused rate.
func TestAProfilesRefusedRateOutranksItsStatedMaximumAndAConfirmation(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeDoP, dsdstate.ModeAuto} {
		r := newRig(t).everything(mode).withoutNative()
		r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd128, StatedMaxBitRate: dsd256, RefusedBitRates: []int{dsd256}}
		r.forget(dsdstate.TransportDoP, dsd256)
		wantOutcome(t, mode+" at a heard rate", r.plan(dsd128), OutcomeDoP)
		p := r.plan(dsd256)
		if mode == dsdstate.ModeDoP {
			wantOutcome(t, "dop at a refused rate, stated and confirmed", p, OutcomeRefused, ReasonDoPNotOffered)
		} else {
			wantOutcome(t, "auto at a refused rate, stated and confirmed", p, OutcomeConverted)
		}
		d := Effective(r.inputs()).At(dsd256).DoP
		if !d.ProfileRefused || d.Stated || d.Confirmed {
			t.Errorf("%s at a refused rate: refused %v, stated %v, confirmed %v; want the refusal, and neither the stated maximum nor the confirmation counting", mode, d.ProfileRefused, d.Stated, d.Confirmed)
		}
		r.hear(dsdstate.TransportDoP, dsd256, dsdstate.AnswerClean)
		wantOutcome(t, mode+" at a refused rate this box heard clean", r.plan(dsd256), OutcomeDoP)
		r.hear(dsdstate.TransportDoP, dsd256, dsdstate.AnswerNoise)
		if mode == dsdstate.ModeDoP {
			wantOutcome(t, "dop at a refused rate this box rejected", r.plan(dsd256), OutcomeRefused, ReasonDoPNotOffered)
		} else {
			wantOutcome(t, "auto at a refused rate this box rejected", r.plan(dsd256), OutcomeConverted)
		}
	}
	// The Mytek Brooklyn DAC+'s profile: DSD256 refused, nothing heard or
	// stated; confirmed, DoP plays at the rates it does not refuse.
	r := newRig(t).everything(dsdstate.ModeDoP).withoutNative()
	r.profileDoP = &dacprofile.DoP{RefusedBitRates: []int{dsd256}}
	r.forget(dsdstate.TransportDoP, dsd256)
	wantOutcome(t, "a confirmed DoP at DSD64 beside a refused DSD256", r.plan(dsd64), OutcomeDoP)
	wantOutcome(t, "a confirmed DoP at the refused DSD256", r.plan(dsd256), OutcomeRefused, ReasonDoPNotOffered)
	// A native claim with a maximum below a rate refuses nothing there: the
	// live altsets and the probe decide what is offered.
	n := newRig(t).everything(dsdstate.ModeNative).withoutCarrier()
	n.profileNative = &dacprofile.Native{MaxBitRate: dsd64, Formats: []string{"DSD_U32_BE"}, Source: dacprofile.Source{Kind: dacprofile.SourceCapture}}
	wantOutcome(t, "native past its claim's maximum", n.plan(dsd256), OutcomeNative)
}

// TestEachTuplePublishesItsHighestRungOnThisFirmware: a tuple's rung is the
// highest of listener confirmed, streaming observed and driver accepted on
// this box, under the unit's firmware (and a durable driver acceptance under
// the running kernel), else reported where the profile claims it; a kernel
// quirk's native claim, which states no rate, reports none; DoP's driver
// acceptance is its carrier's alone; and a later noise leaves no listener
// confirmation.
func TestEachTuplePublishesItsHighestRungOnThisFirmware(t *testing.T) {
	r := newRig(t).everything(dsdstate.ModeNative)
	r.forget(dsdstate.TransportNative, testRate)
	r.forget(dsdstate.TransportDoP, testRate)
	delete(r.probe, alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: testRate / 32})
	delete(r.probe, alsapcm.ProbeCandidate{Format: format.S32LE, RateHz: testRate / 16})
	rung := func(what string, want Rung, on string) {
		t.Helper()
		rate := Effective(r.inputs()).At(testRate)
		if rate.Native.Evidence != want || rate.Native.EvidenceOn != on {
			t.Errorf("native, %s: %q on %q, want %q on %q", what, rate.Native.Evidence, rate.Native.EvidenceOn, want, on)
		}
	}
	rung("the profile's claim alone", RungReported, "")
	r.answer(format.DSDU32BE, testRate/32, alsapcm.ProbeAccepted, 32)
	rung("the probe's acceptance", RungDriverAccepted, "")
	r.evidence = []dsdstate.Evidence{{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE", BitRate: testRate, BCDDevice: bcd, DriverAcceptedAt: t0, Kernel: "6.6.31"}}
	rung("a durable acceptance under another kernel beside the probe's", RungDriverAccepted, "")
	r.evidence[0].Kernel = kernel
	rung("a durable acceptance under this kernel", RungDriverAccepted, "2026-11-05")
	r.evidence[0].StreamingObservedAt = t0.Add(25 * time.Hour)
	rung("streaming observed", RungStreamingObserved, "2026-11-06")
	r.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerClean)
	rung("a clean answer", RungListenerConfirmed, "2026-11-05")
	r.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise)
	rung("a later noise", RungStreamingObserved, "2026-11-06")
	// Under another firmware none of what was established counts.
	r.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerClean)
	r.unitBCD = "0200"
	delete(r.probe, alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: testRate / 32})
	rung("after a firmware change", RungReported, "")
	r.unitBCD = bcd
	r.profileNative = &dacprofile.Native{Formats: []string{"DSD_U32_BE"}, Source: dacprofile.Source{Kind: dacprofile.SourceKernelQuirk}}
	r.evidence, r.listener = nil, nil
	rung("a kernel quirk's claim", "", "")
	// DoP's acceptance is its carrier's, and nothing above it follows from
	// it.
	r.answer(format.S32LE, testRate/16, alsapcm.ProbeAccepted, 24)
	r.profileDoP = nil
	if d := Effective(r.inputs()).At(testRate).DoP; d.Evidence != RungDriverAccepted || !d.Offered {
		t.Errorf("DoP with its carrier accepted: %q, offered %v; want %q", d.Evidence, d.Offered, RungDriverAccepted)
	}
	// A plan carries its tuple's rung.
	r.mode = dsdstate.ModeNative
	r.answer(format.DSDU32BE, testRate/32, alsapcm.ProbeAccepted, 32)
	r.acknowledge(dsdstate.TransportNative)
	if p := r.plan(testRate); p.Outcome != OutcomeNative || p.Evidence != RungDriverAccepted {
		t.Errorf("the plan: %s with %q, want native with %q", p.Outcome, p.Evidence, RungDriverAccepted)
	}
}

// TestTheRefusalsComeInTheirOrder: where several conditions fail under an
// explicit native or dop, the reason is the first in the order the policy
// gives (decide's doc).
func TestTheRefusalsComeInTheirOrder(t *testing.T) {
	steps := func(mode string, fixes []func(*rig), want []engine.Reason) {
		t.Helper()
		r := newRig(t)
		r.known, r.mode = true, mode
		r.withoutNative().withoutCarrier()
		r.answer(format.DSDU32BE, testRate/32, alsapcm.ProbeAccepted, 32)
		r.max = dsd64
		r.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise)
		r.hear(dsdstate.TransportDoP, testRate, dsdstate.AnswerNoise)
		r.mixer = withVolumeUnit(r.mixer, volume("PCM Playback Volume", 128), unreadable("Digital Playback Volume", 0))
		for i, w := range want {
			wantOutcome(t, mode+" step "+string(w), r.plan(testRate), OutcomeRefused, w)
			if i < len(fixes) {
				fixes[i](r)
			}
		}
		if mode == dsdstate.ModeNative {
			wantOutcome(t, "native once every condition holds", r.plan(testRate), OutcomeNative)
		} else {
			wantOutcome(t, "dop once every condition holds", r.plan(testRate), OutcomeDoP)
		}
	}
	steps(dsdstate.ModeNative, []func(*rig){
		func(r *rig) { r.altsets = append(r.altsets, nativeAltset()) },
		func(r *rig) { r.forget(dsdstate.TransportNative, testRate) },
		func(r *rig) { r.max = 0 },
		func(r *rig) { r.acknowledge(dsdstate.TransportNative) },
	}, []engine.Reason{ReasonNativeNotOffered, ReasonListenerRejected, ReasonAboveChoice, ReasonFirstUsePending})
	steps(dsdstate.ModeDoP, []func(*rig){
		func(r *rig) { r.altsets = append(r.altsets, s32Altset()) },
		func(r *rig) { r.forget(dsdstate.TransportDoP, testRate) },
		func(r *rig) {
			r.mixer = withVolumeUnit(r.mixer, volume("PCM Playback Volume", 128), switchControl("PCM Playback Switch"))
		},
		func(r *rig) {
			r.mixer = withVolumeUnit(r.mixer, volume("PCM Playback Volume", 255), switchControl("PCM Playback Switch"))
		},
		func(r *rig) { r.confirm() },
		func(r *rig) { r.max = 0 },
		func(r *rig) { r.acknowledge(dsdstate.TransportDoP) },
	}, []engine.Reason{ReasonDoPNotOffered, ReasonListenerRejected, ReasonGainUnverifiable, ReasonGainNotUnity, ReasonConfirmationRequired, ReasonAboveChoice, ReasonFirstUsePending})
}

// TestTheConversionTargetIsTheDACsBest: a conversion's target is 176.4 kHz
// where the DAC takes it at 24 bits or more, else 88.2 kHz, else 44.1 kHz,
// and 16 bits only for a DAC that takes no 24-bit target, in the first
// packing of the table that carries the bits (B728 B11: the packing a
// converted occurrence opens as is the plan's); what the probe refuses is not
// taken, and a DAC that takes none converts nothing.
func TestTheConversionTargetIsTheDACsBest(t *testing.T) {
	pcm := func(f string, bits *int, rates ...int) alsa.Altset {
		return alsa.Altset{Interface: ptr(1), Altset: ptr(4), Formats: []string{f}, Channels: ptr(2), Rates: rates, Bits: bits}
	}
	cases := []struct {
		name    string
		altsets []alsa.Altset
		refuse  []alsapcm.ProbeCandidate
		want    *Target
	}{
		{"24 bits to 176.4 kHz", []alsa.Altset{pcm("S24_3LE", ptr(24), 44100, 88200, 176400)}, nil, &Target{176400, 24, format.S243LE}},
		{"24 bits to 88.2 kHz", []alsa.Altset{pcm("S24_LE", ptr(24), 44100, 88200)}, nil, &Target{88200, 24, format.S24LE}},
		{"24 bits at 44.1 kHz beside 16 at 176.4", []alsa.Altset{pcm("S16_LE", ptr(16), 176400), pcm("S32_LE", ptr(24), 44100)}, nil, &Target{44100, 24, format.S32LE}},
		{"16 bits alone", []alsa.Altset{pcm("S16_LE", ptr(16), 44100, 88200, 176400)}, nil, &Target{176400, 16, format.S16LE}},
		{"a 32-bit container of 16 significant bits", []alsa.Altset{pcm("S32_LE", ptr(16), 88200)}, nil, &Target{88200, 16, format.S32LE}},
		{"no Bits line, S24_3LE", []alsa.Altset{pcm("S24_3LE", nil, 176400)}, nil, &Target{176400, 24, format.S243LE}},
		{"the probe refuses 176.4 kHz", []alsa.Altset{pcm("S24_3LE", ptr(24), 88200, 176400)},
			[]alsapcm.ProbeCandidate{{Format: format.S243LE, RateHz: 176400}}, &Target{88200, 24, format.S243LE}},
		{"the table's first packing that carries 24 bits", []alsa.Altset{pcm("S32_LE", ptr(24), 176400), pcm("S24_LE", ptr(24), 176400)}, nil, &Target{176400, 24, format.S24LE}},
		{"a packing of fewer significant bits than its depth is passed over", []alsa.Altset{pcm("S24_3LE", ptr(20), 176400), pcm("S32_LE", ptr(24), 176400)}, nil, &Target{176400, 24, format.S32LE}},
		{"the 48 kHz family alone", []alsa.Altset{pcm("S24_3LE", ptr(24), 48000, 96000, 192000)}, nil, nil},
		{"mono", []alsa.Altset{{Interface: ptr(1), Altset: ptr(4), Formats: []string{"S24_3LE"}, Channels: ptr(1), Rates: []int{176400}}}, nil, nil},
		{"a continuous range", []alsa.Altset{{Interface: ptr(1), Altset: ptr(4), Formats: []string{"S24_3LE"}, Channels: ptr(2), RateRange: &alsa.RateRange{Min: 8000, Max: 192000}, Bits: ptr(24)}}, nil, &Target{176400, 24, format.S243LE}},
	}
	for _, c := range cases {
		r := newRig(t).everything(dsdstate.ModeConvert)
		r.altsets = c.altsets
		r.probe = map[alsapcm.ProbeCandidate]alsapcm.ProbeResult{}
		for _, cand := range c.refuse {
			r.answer(cand.Format, cand.RateHz, alsapcm.ProbeRefused, 0)
		}
		s := Effective(r.inputs())
		switch {
		case c.want == nil && s.Conversion != nil, c.want != nil && (s.Conversion == nil || *s.Conversion != *c.want):
			t.Errorf("%s: target %v, want %v", c.name, s.Conversion, c.want)
		case c.want == nil:
			wantOutcome(t, c.name, s.At(testRate).Plan, OutcomeRefused, ReasonConversionNotOffered)
		default:
			if p := s.At(testRate).Plan; p.Outcome != OutcomeConverted || p.Target != *c.want {
				t.Errorf("%s: the plan converts to %v, want %v", c.name, p.Target, *c.want)
			}
		}
	}
	// An accepted probe's refined significant bits constrain the target, as
	// they do the DoP carrier: fewer than the altset's Bits line says, they
	// decide; unknown (0), they say nothing.
	for _, c := range []struct {
		name   string
		bits   *int
		msbits int
		want   Target
	}{
		{"an accepted 16 bits under the altset's 24", ptr(24), 16, Target{176400, 16, format.S32LE}},
		{"an accepted 24 bits with no Bits line", nil, 24, Target{176400, 24, format.S32LE}},
		{"an accepted 32 bits over the altset's 24", ptr(24), 32, Target{176400, 24, format.S32LE}},
		{"an accepted refine whose bits are unknown", ptr(24), 0, Target{176400, 24, format.S32LE}},
	} {
		r := newRig(t).everything(dsdstate.ModeConvert)
		r.altsets = []alsa.Altset{pcm("S32_LE", c.bits, 176400)}
		r.probe = map[alsapcm.ProbeCandidate]alsapcm.ProbeResult{}
		r.answer(format.S32LE, 176400, alsapcm.ProbeAccepted, c.msbits)
		if s := Effective(r.inputs()); s.Conversion == nil || *s.Conversion != c.want {
			t.Errorf("%s: target %v, want %v", c.name, s.Conversion, c.want)
		}
	}
}

// TestATupleCarriesItsTiming: a tuple pauses with the kernel's PAUSE where
// the probe found the setup offers it and with silence otherwise, a
// candidate not probed included; its tail is 100 ms and its prefill the
// profile's, or 400 ms.
func TestATupleCarriesItsTiming(t *testing.T) {
	r := newRig(t).everything(dsdstate.ModeNative).withoutCarrier()
	want := Tuple{Transport: dsdstate.TransportNative, Format: format.DSDU32BE, BitRate: testRate, Pause: PauseKernel, Tail: 100 * time.Millisecond, Prefill: 400 * time.Millisecond}
	if p := r.plan(testRate); p.Tuple != want {
		t.Errorf("the tuple: %+v, want %+v", p.Tuple, want)
	}
	delete(r.probe, alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: testRate / 32})
	r.timing = &dacprofile.Timing{DSDPrefillMs: 700}
	want.Pause, want.Prefill = PauseSilence, 700*time.Millisecond
	if p := r.plan(testRate); p.Tuple != want {
		t.Errorf("not probed, with the profile's timing: %+v, want %+v", p.Tuple, want)
	}
	d := newRig(t).everything(dsdstate.ModeDoP).withoutNative()
	wantDoP := Tuple{Transport: dsdstate.TransportDoP, Format: format.S32LE, BitRate: testRate, Pause: PauseKernel, Tail: 100 * time.Millisecond, Prefill: 400 * time.Millisecond}
	if p := d.plan(testRate); p.Tuple != wantDoP {
		t.Errorf("the DoP tuple: %+v, want %+v", p.Tuple, wantDoP)
	}
}

// TestNoRateButDSD64ToDSD256IsDecided: a rate past DSD256, or no rate the
// player knows, is refused as rate_not_supported in every mode.
func TestNoRateButDSD64ToDSD256IsDecided(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeConvert} {
		s := Effective(newRig(t).everything(mode).inputs())
		for _, rate := range []int{22579200, 0, 3072000} {
			wantOutcome(t, mode, s.At(rate).Plan, OutcomeRefused, ReasonRateNotSupported)
		}
		if got := len(s.All()); got != 3 {
			t.Errorf("%s: the set decides %d rates, want DSD64, DSD128 and DSD256", mode, got)
		}
	}
}
