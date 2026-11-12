package policy

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// A choice write's checks that read the state and what it writes (B728 B9;
// the design's sections 2.5 and 7.2): the compare-and-set, the mode offered,
// DoP's confirmation and first use, each in that order and each refusing with
// the state as it was; and the write itself, its revision, its confirmation,
// its acknowledgements, the rejections it clears and the warnings it clears.

// t1 is when the tests' writes are made.
var t1 = t0.Add(time.Hour)

// choose is c written over in, which must land.
func choose(t *testing.T, in Inputs, c Choice) dsdstate.State {
	t.Helper()
	if c.By == "" {
		c.By = dsdstate.ByApp
	}
	st, err := Choose(in, c, t1)
	if err != nil {
		t.Fatalf("the write of %+v was refused: %v", c, err)
	}
	return st
}

// refusedWith fails t unless c over in is refused with reason, the state in
// handed over unchanged.
func refusedWith(t *testing.T, in Inputs, c Choice, reason ChoiceReason) {
	t.Helper()
	if c.By == "" {
		c.By = dsdstate.ByApp
	}
	before := in.State.Clone()
	_, err := Choose(in, c, t1)
	var r *ChoiceRefusal
	if !errors.As(err, &r) || r.Reason != reason {
		t.Fatalf("the write of %+v: %v, want refused as %s", c, err, reason)
	}
	if !reflect.DeepEqual(in.State, before) {
		t.Errorf("a refused write changed the state it was handed: %+v, was %+v", in.State, before)
	}
}

// dacOfState is what st holds for the test DAC.
func dacOfState(t *testing.T, st dsdstate.State) dsdstate.DAC {
	t.Helper()
	d, ok := st.DAC(dacKey(t))
	if !ok {
		t.Fatalf("the state holds nothing for the test DAC: %+v", st)
	}
	return d
}

// firstUseIn is d's acknowledgement of transport, nil for none.
func firstUseIn(d dsdstate.DAC, transport string) *dsdstate.FirstUse {
	for i, f := range d.FirstUse {
		if f.Transport == transport {
			return &d.FirstUse[i]
		}
	}
	return nil
}

// everyRate reports whether f is an acknowledgement under the unit's firmware
// that a write made: every rate, made at t1.
func everyRate(f *dsdstate.FirstUse) bool {
	return f != nil && f.BCDDevice == bcd && f.BitRates == nil && f.AcknowledgedAt.Equal(t1) && f.ObservedAt.IsZero()
}

// TestTheChecksThatReadTheStateRunInTheirOrder: a write that fails several
// checks is refused by the first in the design's order, the compare-and-set,
// then the mode, then DoP's confirmation, then first use.
func TestTheChecksThatReadTheStateRunInTheirOrder(t *testing.T) {
	// An unknown DAC with no choice (revision 0), no carrier for DoP at all,
	// and nothing acknowledged.
	r := newRig(t)
	in := r.inputs()
	refusedWith(t, in, Choice{BaseRevision: 7, Mode: dsdstate.ModeDoP}, ChoiceStale)
	r.withoutCarrier()
	refusedWith(t, r.inputs(), Choice{BaseRevision: 0, Mode: dsdstate.ModeDoP}, ChoiceModeNotOffered)
	r = newRig(t)
	refusedWith(t, r.inputs(), Choice{Mode: dsdstate.ModeDoP}, ChoiceConfirmationRequired)
	refusedWith(t, r.inputs(), Choice{Mode: dsdstate.ModeDoP, ConfirmDoP: true}, ChoiceFirstUseUnacknowledged)
	choose(t, r.inputs(), Choice{Mode: dsdstate.ModeDoP, ConfirmDoP: true, FirstUseAcknowledged: true})
}

// TestTheStoredRevisionIsWhatTheReadSidePublishes: the revision a write's base
// is compared with is the one GET /v1/outputs publishes for the DAC: its own
// choice's, 0 for a DAC with none, and 0 over a state that did not read,
// whose read side takes no choice.
func TestTheStoredRevisionIsWhatTheReadSidePublishes(t *testing.T) {
	r := newRig(t)
	key := dacKey(t)
	if got := StoredRevision(r.inputs().State, key); got != 0 {
		t.Errorf("no choice of its own: revision %d, want 0", got)
	}
	r.mode = dsdstate.ModeConvert
	st := r.inputs().State
	st.DACs[0].Choice.Revision = 4
	if got := StoredRevision(st, key); got != 4 {
		t.Errorf("its own choice at revision 4: revision %d, want 4", got)
	}
	st.Warnings = append(st.Warnings, dsdstate.Warning{Code: dsdstate.WarningStateUnreadable, At: t0})
	if got := StoredRevision(st, key); got != 0 {
		t.Errorf("a state that does not read: revision %d, want 0, the one its read side publishes", got)
	}
	if got := StoredRevision(st, otherModel(t)); got != 0 {
		t.Errorf("another DAC: revision %d, want 0", got)
	}
}

// TestAStaleBaseIsRefusedAndAMatchingOneMovesTheRevision: the compare-and-set
// is the stored revision exactly; every write that lands moves it by one, a
// repeated choice included, and names who made it and when.
func TestAStaleBaseIsRefusedAndAMatchingOneMovesTheRevision(t *testing.T) {
	r := newRig(t)
	r.mode = dsdstate.ModeConvert
	in := r.inputs()
	refusedWith(t, in, Choice{BaseRevision: 0, Mode: dsdstate.ModeConvert}, ChoiceStale)
	refusedWith(t, in, Choice{BaseRevision: 2, Mode: dsdstate.ModeConvert}, ChoiceStale)
	st := choose(t, in, Choice{BaseRevision: 1, Mode: dsdstate.ModeConvert, MaxBitRate: dsd128, By: dsdstate.ByConsole})
	c := dacOfState(t, st).Choice
	if c == nil || c.Mode != dsdstate.ModeConvert || c.MaxBitRate != dsd128 || c.Revision != 2 || !c.SetAt.Equal(t1) || c.SetBy != dsdstate.ByConsole {
		t.Fatalf("a repeated choice wrote %+v, want convert, capped at DSD128, revision 2, set at %v by the console", c, t1)
	}
	in.State = st
	st = choose(t, in, Choice{BaseRevision: 2, Mode: dsdstate.ModeConvert, MaxBitRate: dsd128})
	if got := dacOfState(t, st).Choice.Revision; got != 3 {
		t.Errorf("the same choice written again: revision %d, want 3", got)
	}
}

// TestAModeTheDACDoesNotOfferIsRefused: native and DoP are taken where the
// DAC offers them at some rate, convert where it takes a conversion target;
// auto and off always.
func TestAModeTheDACDoesNotOfferIsRefused(t *testing.T) {
	ack := Choice{FirstUseAcknowledged: true, ConfirmDoP: true}
	r := newRig(t).withoutNative()
	refusedWith(t, r.inputs(), Choice{Mode: dsdstate.ModeNative, FirstUseAcknowledged: true}, ChoiceModeNotOffered)
	r = newRig(t).withoutCarrier()
	c := ack
	c.Mode = dsdstate.ModeDoP
	refusedWith(t, r.inputs(), c, ChoiceModeNotOffered)
	// A carrier the probe refused at every rate is no carrier.
	r = newRig(t)
	for _, rate := range dsdRates {
		r.answer(format.S32LE, rate/16, alsapcm.ProbeRefused, 0)
	}
	refusedWith(t, r.inputs(), c, ChoiceModeNotOffered)
	// No target: the S24_3LE and S32_LE altsets gone.
	r = newRig(t).withoutCarrier()
	r.altsets = slices.DeleteFunc(r.altsets, func(a alsa.Altset) bool { return slices.Contains(a.Formats, "S24_3LE") })
	refusedWith(t, r.inputs(), Choice{Mode: dsdstate.ModeConvert}, ChoiceModeNotOffered)
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeOff} {
		choose(t, r.inputs(), Choice{Mode: mode})
	}
	// Native offered at DSD128 alone is offered at some rate, whatever the cap.
	r = newRig(t)
	r.answer(format.DSDU32BE, dsd64/32, alsapcm.ProbeRefused, 0)
	r.answer(format.DSDU32BE, dsd256/32, alsapcm.ProbeRefused, 0)
	choose(t, r.inputs(), Choice{Mode: dsdstate.ModeNative, MaxBitRate: dsd64, FirstUseAcknowledged: true})
}

// TestDoPOnAnUnknownDACNeedsConfirmation (B728 B9; the design's sections 3.2
// and 7.2): DoP on a DAC whose profile does not claim it at every rate DoP is
// offered at carries confirmDoP, which records the confirmation under the
// unit's firmware; a profile that claims DoP at every rate it is offered at
// needs none, and neither does a DAC whose confirmation counts on this
// firmware. One given under another firmware does not.
func TestDoPOnAnUnknownDACNeedsConfirmation(t *testing.T) {
	dop := Choice{Mode: dsdstate.ModeDoP, FirstUseAcknowledged: true}
	t.Run("an unknown DAC", func(t *testing.T) {
		r := newRig(t)
		refusedWith(t, r.inputs(), dop, ChoiceConfirmationRequired)
		c := dop
		c.ConfirmDoP = true
		st := choose(t, r.inputs(), c)
		got := dacOfState(t, st).DoPConfirmed
		if got == nil || got.BCDDevice != bcd || !got.ConfirmedAt.Equal(t1) || got.SetBy != dsdstate.ByApp {
			t.Fatalf("the confirmation written: %+v, want one under %s at %v by the app", got, bcd, t1)
		}
	})
	t.Run("a profile that claims DoP at a lower rate alone", func(t *testing.T) {
		r := newRig(t)
		r.known = true
		r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd64}
		refusedWith(t, r.inputs(), dop, ChoiceConfirmationRequired)
	})
	t.Run("a profile that claims DoP at every rate it is offered at", func(t *testing.T) {
		r := newRig(t)
		r.known = true
		r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd128, StatedMaxBitRate: dsd256}
		st := choose(t, r.inputs(), dop)
		if got := dacOfState(t, st).DoPConfirmed; got != nil {
			t.Errorf("a write that needed no confirmation and carried none wrote one: %+v", got)
		}
	})
	t.Run("a confirmation that counts on this firmware", func(t *testing.T) {
		r := newRig(t)
		r.confirm()
		choose(t, r.inputs(), dop)
	})
	t.Run("a confirmation given under another firmware", func(t *testing.T) {
		r := newRig(t)
		r.confirmed = &dsdstate.DoPConfirmation{BCDDevice: "0200", ConfirmedAt: t0, SetBy: dsdstate.ByApp}
		refusedWith(t, r.inputs(), dop, ChoiceConfirmationRequired)
	})
	t.Run("no other mode needs it", func(t *testing.T) {
		for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative, dsdstate.ModeConvert, dsdstate.ModeOff} {
			choose(t, newRig(t).inputs(), Choice{Mode: mode, FirstUseAcknowledged: true})
		}
	})
}

// TestTheConfirmationSurvivesLeavingDoP (the design's section 2.5): DoP's
// confirmation is the DAC model's, so a write that leaves DoP keeps it, and
// coming back to DoP on the same firmware needs no confirmation again.
func TestTheConfirmationSurvivesLeavingDoP(t *testing.T) {
	r := newRig(t)
	in := r.inputs()
	st := choose(t, in, Choice{Mode: dsdstate.ModeDoP, ConfirmDoP: true, FirstUseAcknowledged: true})
	in.State = st
	st = choose(t, in, Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, FirstUseAcknowledged: true})
	if dacOfState(t, st).DoPConfirmed == nil {
		t.Fatal("leaving DoP dropped its confirmation")
	}
	in.State = st
	st = choose(t, in, Choice{BaseRevision: 2, Mode: dsdstate.ModeDoP})
	if c := dacOfState(t, st).Choice; c.Mode != dsdstate.ModeDoP || c.Revision != 3 {
		t.Errorf("coming back to DoP wrote %+v, want dop at revision 3", c)
	}
}

// TestNativeOrDoPNeedsFirstUseAcknowledged (B728 B9; the design's sections 3.2,
// 6.1 and 7.2): an explicit native or DoP whose first use is not acknowledged
// at every rate under the unit's firmware carries firstUseAcknowledged, which
// records an acknowledgement of that transport at every rate under it; one
// acknowledged at every rate under it needs none, one acknowledged under
// another firmware does; auto, convert and off never need it.
func TestNativeOrDoPNeedsFirstUseAcknowledged(t *testing.T) {
	for _, tc := range []struct{ mode, transport string }{
		{dsdstate.ModeNative, dsdstate.TransportNative},
		{dsdstate.ModeDoP, dsdstate.TransportDoP},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newRig(t)
			r.confirm()
			refusedWith(t, r.inputs(), Choice{Mode: tc.mode}, ChoiceFirstUseUnacknowledged)
			st := choose(t, r.inputs(), Choice{Mode: tc.mode, FirstUseAcknowledged: true})
			d := dacOfState(t, st)
			if !everyRate(firstUseIn(d, tc.transport)) {
				t.Fatalf("the acknowledgement written: %+v, want %s at every rate under %s", d.FirstUse, tc.transport, bcd)
			}
			if len(d.FirstUse) != 1 {
				t.Errorf("an explicit %s acknowledged %+v: the other transport too", tc.mode, d.FirstUse)
			}
			r.acknowledge(tc.transport)
			choose(t, r.inputs(), Choice{Mode: tc.mode})
			r.firstUse = []dsdstate.FirstUse{{Transport: tc.transport, BCDDevice: "0200", AcknowledgedAt: t0}}
			refusedWith(t, r.inputs(), Choice{Mode: tc.mode}, ChoiceFirstUseUnacknowledged)
		})
	}
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeConvert, dsdstate.ModeOff} {
		choose(t, newRig(t).inputs(), Choice{Mode: mode})
	}
}

// TestChoosingARejectedTransportAgainClearsTheRejection (B728 B9; the design's
// sections 1.2 and 7.2): a listener's rejection re-arms that transport's first
// use, so a state that holds one for the transport at any rate under the
// unit's firmware needs the acknowledgement, even beside an acknowledgement
// that would cover the rate; the write that carries it clears that
// transport's rejections under the firmware, and leaves its clean answers,
// the other transport's and another firmware's alone. auto clears no
// rejection, which only an explicit choice of the transport does.
func TestChoosingARejectedTransportAgainClearsTheRejection(t *testing.T) {
	rejected := func() *rig {
		r := newRig(t)
		r.known = true
		r.profileNative = &dacprofile.Native{MaxBitRate: dsd256, Formats: []string{"DSD_U32_BE"}}
		r.mode = dsdstate.ModeNative
		r.acknowledge(dsdstate.TransportNative)
		r.hear(dsdstate.TransportNative, dsd64, dsdstate.AnswerNoise)
		r.hear(dsdstate.TransportNative, dsd128, dsdstate.AnswerClean)
		r.hear(dsdstate.TransportDoP, dsd64, dsdstate.AnswerNothing)
		r.listener = append(r.listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: dsd256, BCDDevice: "0200", Answer: dsdstate.AnswerNoise})
		return r
	}
	r := rejected()
	refusedWith(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeNative}, ChoiceFirstUseUnacknowledged)
	st := choose(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, FirstUseAcknowledged: true})
	d := dacOfState(t, st)
	want := []dsdstate.Answer{
		{Transport: dsdstate.TransportNative, BitRate: dsd128, BCDDevice: bcd, Answer: dsdstate.AnswerClean, On: "2026-11-05"},
		{Transport: dsdstate.TransportNative, BitRate: dsd256, BCDDevice: "0200", Answer: dsdstate.AnswerNoise},
		{Transport: dsdstate.TransportDoP, BitRate: dsd64, BCDDevice: bcd, Answer: dsdstate.AnswerNothing, On: "2026-11-05"},
	}
	got := slices.Clone(d.Listener)
	slices.SortFunc(got, func(a, b dsdstate.Answer) int {
		return cmpAnswer(a, b)
	})
	slices.SortFunc(want, cmpAnswer)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the answers after native was chosen again: %+v, want %+v", got, want)
	}
	if !everyRate(firstUseIn(d, dsdstate.TransportNative)) {
		t.Errorf("the acknowledgement after native was chosen again: %+v", d.FirstUse)
	}
	// The rejection cleared, native plays at DSD64 again.
	in := r.inputs()
	in.State = st
	if p := Effective(in).At(dsd64).Plan; p.Outcome != OutcomeNative {
		t.Errorf("native at DSD64 once its rejection was cleared: %s %s", p.Outcome, p.Reason)
	}
	t.Run("auto clears no rejection", func(t *testing.T) {
		r := rejected()
		st := choose(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeAuto, FirstUseAcknowledged: true})
		if !slices.ContainsFunc(dacOfState(t, st).Listener, func(a dsdstate.Answer) bool {
			return a.Transport == dsdstate.TransportNative && a.BitRate == dsd64 && a.Answer == dsdstate.AnswerNoise
		}) {
			t.Errorf("an auto write cleared native's rejection: %+v", dacOfState(t, st).Listener)
		}
	})
}

// cmpAnswer orders answers by transport, rate and firmware.
func cmpAnswer(a, b dsdstate.Answer) int {
	switch {
	case a.Transport != b.Transport:
		if a.Transport < b.Transport {
			return -1
		}
		return 1
	case a.BitRate != b.BitRate:
		return a.BitRate - b.BitRate
	case a.BCDDevice != b.BCDDevice:
		if a.BCDDevice < b.BCDDevice {
			return -1
		}
		return 1
	}
	return 0
}

// TestAnAcknowledgementWidensAMigratedChoiceToEveryRate (B728 B9; the design's
// sections 2.5 and 3.2): a migrated acknowledgement covers its record's rates
// alone, so choosing the mode again needs the acknowledgement, and the write
// that carries it replaces the entry with one that covers every rate.
func TestAnAcknowledgementWidensAMigratedChoiceToEveryRate(t *testing.T) {
	r := newRig(t)
	r.known = true
	r.profileNative = &dacprofile.Native{MaxBitRate: dsd256, Formats: []string{"DSD_U32_BE"}}
	r.mode = dsdstate.ModeNative
	r.by = dsdstate.ByMigration
	r.acknowledge(dsdstate.TransportNative, dsd64)
	r.firstUse[0].ObservedAt = t0
	refusedWith(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeNative}, ChoiceFirstUseUnacknowledged)
	st := choose(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, FirstUseAcknowledged: true})
	if f := firstUseIn(dacOfState(t, st), dsdstate.TransportNative); !everyRate(f) {
		t.Fatalf("the acknowledgement after the write: %+v, want every rate, acknowledged now and not yet observed", f)
	}
	in := r.inputs()
	in.State = st
	for _, rate := range dsdRates {
		if p := Effective(in).At(rate).Plan; p.Outcome != OutcomeNative {
			t.Errorf("native at %d once the acknowledgement was widened: %s %s", rate, p.Outcome, p.Reason)
		}
	}
	t.Run("an auto write widens it where auto plays native", func(t *testing.T) {
		r.mode = dsdstate.ModeAuto
		st := choose(t, r.inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeAuto, FirstUseAcknowledged: true})
		if f := firstUseIn(dacOfState(t, st), dsdstate.TransportNative); !everyRate(f) {
			t.Errorf("the acknowledgement after an auto write that carried it: %+v, want every rate", f)
		}
	})
}

// TestAnAcknowledgementOnAutoCoversTheTransportsAutoPlays (the design's
// sections 3.2 and 6.1): auto resolves each rate by itself, native first, so
// an auto write that carries the acknowledgement records it for each
// transport auto would play at some rate once acknowledged (under its cap,
// its rejections kept, DoP by the profile's claim or the write's own
// confirmation), and for no other: native and DoP both where they share the
// rates, native alone where it is offered at every rate, nothing on an
// unknown DAC that no listener answer lets auto play natively.
func TestAnAcknowledgementOnAutoCoversTheTransportsAutoPlays(t *testing.T) {
	auto := Choice{Mode: dsdstate.ModeAuto, FirstUseAcknowledged: true}
	transports := func(st dsdstate.State) []string {
		var out []string
		for _, f := range dacOfState(t, st).FirstUse {
			out = append(out, f.Transport)
		}
		slices.Sort(out)
		return out
	}
	known := func() *rig {
		r := newRig(t)
		r.known = true
		r.profileNative = &dacprofile.Native{MaxBitRate: dsd256, Formats: []string{"DSD_U32_BE"}}
		r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd256}
		return r
	}
	t.Run("native at the lower rates, DoP above them", func(t *testing.T) {
		r := known()
		r.answer(format.DSDU32BE, dsd256/32, alsapcm.ProbeRefused, 0)
		got := transports(choose(t, r.inputs(), auto))
		if !slices.Equal(got, []string{dsdstate.TransportDoP, dsdstate.TransportNative}) {
			t.Errorf("acknowledged %v, want native and DoP", got)
		}
	})
	t.Run("native at every rate", func(t *testing.T) {
		got := transports(choose(t, known().inputs(), auto))
		if !slices.Equal(got, []string{dsdstate.TransportNative}) {
			t.Errorf("acknowledged %v, want native alone: auto never plays DoP where native plays", got)
		}
	})
	t.Run("a cap below the rates DoP would play", func(t *testing.T) {
		r := known()
		r.answer(format.DSDU32BE, dsd256/32, alsapcm.ProbeRefused, 0)
		c := auto
		c.MaxBitRate = dsd128
		got := transports(choose(t, r.inputs(), c))
		if !slices.Equal(got, []string{dsdstate.TransportNative}) {
			t.Errorf("acknowledged %v, want native alone: DSD256 converts above the cap", got)
		}
	})
	t.Run("native rejected at a rate, DoP there", func(t *testing.T) {
		r := known()
		r.hear(dsdstate.TransportNative, dsd64, dsdstate.AnswerNoise)
		got := transports(choose(t, r.inputs(), auto))
		if !slices.Equal(got, []string{dsdstate.TransportDoP, dsdstate.TransportNative}) {
			t.Errorf("acknowledged %v, want native, for DSD128 and DSD256, and DoP, for DSD64", got)
		}
	})
	t.Run("no DoP claim, the write's own confirmation", func(t *testing.T) {
		r := known().withoutNative()
		r.profileDoP = nil
		if got := transports(choose(t, r.inputs(), auto)); len(got) != 0 {
			t.Errorf("acknowledged %v with DoP unconfirmed and native not offered, want nothing", got)
		}
		c := auto
		c.ConfirmDoP = true
		if got := transports(choose(t, r.inputs(), c)); !slices.Equal(got, []string{dsdstate.TransportDoP}) {
			t.Errorf("acknowledged %v with DoP confirmed in the same write, want DoP", got)
		}
	})
	t.Run("an unknown DAC", func(t *testing.T) {
		if got := transports(choose(t, newRig(t).inputs(), auto)); len(got) != 0 {
			t.Errorf("acknowledged %v on an unknown DAC no listener answer lets auto play natively, want nothing", got)
		}
	})
	t.Run("a migrated acknowledgement covering some rates", func(t *testing.T) {
		r := known()
		r.acknowledge(dsdstate.TransportNative, dsd64, dsd128)
		st := choose(t, r.inputs(), auto)
		if f := firstUseIn(dacOfState(t, st), dsdstate.TransportNative); !everyRate(f) {
			t.Errorf("the native acknowledgement after an auto write: %+v, want every rate", f)
		}
	})
}

// TestAnAcknowledgementAWriteDoesNotNeedRecordsNothing (the design's section
// 6.1): firstUseAcknowledged on a write that needs no acknowledgement records
// nothing false: convert and off play neither transport and record none, and
// a transport already acknowledged at every rate under the firmware, with no
// rejection, keeps its entry, its observation with it.
func TestAnAcknowledgementAWriteDoesNotNeedRecordsNothing(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeConvert, dsdstate.ModeOff} {
		st := choose(t, newRig(t).inputs(), Choice{Mode: mode, FirstUseAcknowledged: true})
		if fu := dacOfState(t, st).FirstUse; len(fu) != 0 {
			t.Errorf("%s with firstUseAcknowledged recorded %+v, want nothing", mode, fu)
		}
	}
	r := newRig(t)
	r.acknowledge(dsdstate.TransportNative)
	r.firstUse[0].ObservedAt = t0.Add(time.Minute)
	st := choose(t, r.inputs(), Choice{Mode: dsdstate.ModeNative, FirstUseAcknowledged: true})
	f := firstUseIn(dacOfState(t, st), dsdstate.TransportNative)
	if f == nil || !f.AcknowledgedAt.Equal(t0) || !f.ObservedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("an acknowledgement already whole: %+v, want it kept as it was, observed", f)
	}
}

// TestAWriteClearsItsDACsWarnings (B728 B9; the design's section 2.5): a choice
// the app writes for a DAC clears that DAC's warnings and state_unreadable,
// which no DAC owns; migrated_off stays while defaultMode off still applies to
// every DAC with no choice, and another DAC's warnings stay with it.
func TestAWriteClearsItsDACsWarnings(t *testing.T) {
	other := otherModel(t)
	key := dacKey(t)
	in := newRig(t).inputs()
	in.State.Warnings = []dsdstate.Warning{
		{Code: dsdstate.WarningMigrationRecordRefused, Reason: dsdstate.ReasonStaleVersion, DAC: &key, At: t0},
		{Code: dsdstate.WarningMigrationRecordMissing, DAC: &other, At: t0},
		{Code: dsdstate.WarningMigratedOff, At: t0},
	}
	st := choose(t, in, Choice{Mode: dsdstate.ModeConvert})
	var codes []string
	for _, w := range st.Warnings {
		codes = append(codes, w.Code)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []string{dsdstate.WarningMigratedOff, dsdstate.WarningMigrationRecordMissing}) {
		t.Errorf("the warnings after the write: %+v, want migrated_off and the other DAC's", st.Warnings)
	}
	t.Run("state_unreadable", func(t *testing.T) {
		in := newRig(t).inputs()
		in.State = dsdstate.Refusing(t0)
		st := choose(t, in, Choice{BaseRevision: 0, Mode: dsdstate.ModeConvert})
		if st.Unreadable() {
			t.Errorf("a write over a state that did not read kept state_unreadable: %+v", st.Warnings)
		}
		if st.DefaultMode != dsdstate.ModeOff {
			t.Errorf("the write changed the default %s, want off kept for every DAC with no choice", st.DefaultMode)
		}
		in.State = st
		if p := Effective(in).At(dsd64).Plan; p.Outcome != OutcomeConverted {
			t.Errorf("the DAC once the refusal was cleared: %s %s, want converted", p.Outcome, p.Reason)
		}
	})
	t.Run("migrated_off", func(t *testing.T) {
		in := newRig(t).inputs()
		in.State.DefaultMode = dsdstate.ModeOff
		in.State.Warnings = []dsdstate.Warning{{Code: dsdstate.WarningMigratedOff, At: t0}}
		st := choose(t, in, Choice{Mode: dsdstate.ModeConvert})
		if len(st.Warnings) != 1 || st.Warnings[0].Code != dsdstate.WarningMigratedOff {
			t.Errorf("the warnings after a write under a migrated off: %+v, want migrated_off kept", st.Warnings)
		}
	})
}

// TestAWriteOverAStateThatDidNotReadSeesTheDACsOffers: a state that does not
// read refuses every rate before one is read, but a write clears that
// refusal, so the write's own checks read what the DAC offers as they would
// once it is cleared: native is offered, and its acknowledgement records.
func TestAWriteOverAStateThatDidNotReadSeesTheDACsOffers(t *testing.T) {
	in := newRig(t).inputs()
	in.State = dsdstate.Refusing(t0)
	st := choose(t, in, Choice{Mode: dsdstate.ModeNative, FirstUseAcknowledged: true})
	if !everyRate(firstUseIn(dacOfState(t, st), dsdstate.TransportNative)) {
		t.Errorf("a native write over a state that did not read: %+v", st)
	}
}

// TestAFirmwareThatDoesNotReadHoldsNoAcknowledgement: every acknowledgement
// and confirmation is held under the unit's firmware, so a unit whose firmware
// does not read takes the choice and records neither: nothing false is held
// under no firmware.
func TestAFirmwareThatDoesNotReadHoldsNoAcknowledgement(t *testing.T) {
	r := newRig(t)
	r.unitBCD = ""
	st := choose(t, r.inputs(), Choice{Mode: dsdstate.ModeDoP, ConfirmDoP: true, FirstUseAcknowledged: true})
	d := dacOfState(t, st)
	if d.Choice == nil || d.Choice.Mode != dsdstate.ModeDoP || d.DoPConfirmed != nil || len(d.FirstUse) != 0 {
		t.Errorf("a write on a unit whose firmware does not read: %+v, want the choice alone", d)
	}
}

// TestAWriteRefusesWordsItDoesNotKnow: a mode, a cap or a writer the state
// does not hold is no write at all, refused without a check's word.
func TestAWriteRefusesWordsItDoesNotKnow(t *testing.T) {
	in := newRig(t).inputs()
	for _, c := range []Choice{
		{Mode: "loud", By: dsdstate.ByApp},
		{Mode: dsdstate.ModeAuto, MaxBitRate: 22579200, By: dsdstate.ByApp},
		{Mode: dsdstate.ModeAuto, MaxBitRate: 1, By: dsdstate.ByApp},
		{Mode: dsdstate.ModeAuto, By: dsdstate.ByMigration},
		{Mode: dsdstate.ModeAuto, By: ""},
	} {
		_, err := Choose(in, c, t1)
		var r *ChoiceRefusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("%+v: %v, want an error that is no check's refusal", c, err)
		}
	}
}

// TestTheIntersectionRefusesWhatTheTwoStatesPlayApart: while a write that
// reached the state's file is not known to be durable, a rate plays only where
// the state in memory and the one the write left both play it the same way;
// every other rate it plays is refused as dsd_choice_uncertain, and what the
// state in memory refuses keeps its own word.
func TestTheIntersectionRefusesWhatTheTwoStatesPlayApart(t *testing.T) {
	r := newRig(t).everything(dsdstate.ModeNative)
	cur := Effective(r.inputs())
	r.max = dsd128
	landed := Effective(r.inputs())
	got := Intersect(cur, landed)
	for _, tc := range []struct {
		rate    int
		outcome Outcome
		reason  any
	}{{dsd64, OutcomeNative, nil}, {dsd128, OutcomeNative, nil}, {dsd256, OutcomeRefused, ReasonChoiceUncertain}} {
		p := got.At(tc.rate).Plan
		if tc.reason == nil {
			wantOutcome(t, "the intersection", p, tc.outcome)
		} else {
			wantOutcome(t, "the intersection", p, tc.outcome, tc.reason)
		}
	}
	r.max = 0
	r.mode = dsdstate.ModeOff
	off := Effective(r.inputs())
	got = Intersect(off, cur)
	for _, rate := range dsdRates {
		wantOutcome(t, "off beside native", got.At(rate).Plan, OutcomeRefused, ReasonOffByChoice)
	}
	if !got.Identity().Same(cur.Identity()) || got.Mode != dsdstate.ModeOff {
		t.Errorf("the intersection's identity or choice moved: %+v", got)
	}
}
