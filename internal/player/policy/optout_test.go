package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// B1043: a 0.2.3 bridge that ran dsdMode auto without allowDoP: true never
// played DoP. Its migration's marker keeps the keys as they were (From keys,
// DSDMode auto, AllowDoP false or unset), and while it does, mode auto plays
// no DoP on any DAC until that DAC holds a DoP confirmation a user wrote (an
// app's or the console's confirmDoP) for the firmware it has now. Native, the
// read side's facts and an explicit dop are untouched.

// testUnit is the test DAC as the legacy reader compares it.
func testUnit(fw string) qualification.Identity {
	return qualification.Identity{Vendor: "1234", ProductID: "5678", Manufacturer: "Test", Product: "Test DAC", BCDDevice: fw, Port: "platform-xhci-hcd.0/usb1-1:1.0"}
}

// migratedAuto is the state the migration leaves for 0.2.3's dsdMode auto with
// allowDoP allow (nil: unset), through its own logic (dsdstate.Migrate): with
// no DoP rates, no record, which leaves the defaults and its warning; with
// some, an owner's record of the test DAC on bcd that approved DoP at them, its
// rows: auto, each rate's DoP answer clean, nothing acknowledged for DoP and,
// without allowDoP, no confirmation.
func migratedAuto(t *testing.T, allow *bool, dop ...int) dsdstate.State {
	t.Helper()
	legacy := dsdstate.Legacy{DSDMode: dsdstate.ModeAuto, AllowDoP: allow, DSDQualification: "approved.json"}
	s, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: legacy, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	rec := dsdstate.Record{Outcome: dsdstate.RecordMissing}
	if len(dop) > 0 {
		rec = dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: bcd, QualifiedOn: "2026-11-01", DoP: dop}
	}
	s, _, err = dsdstate.Migrate(s, dsdstate.Input{Legacy: legacy, Bound: &dsdstate.Bound{Key: dacKey(t), Stamp: testStamp, Identity: testUnit(bcd)}, Record: rec, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if m := s.MigratedFrom; m == nil || m.From != dsdstate.FromKeys || m.DSDMode != dsdstate.ModeAuto || m.WaitingFor != "" {
		t.Fatalf("the migration of auto left the marker %+v", s.MigratedFrom)
	}
	return s
}

// heardDAC is the test DAC known by a profile that heard DoP at DSD64 and
// DSD128 and claims no native, native DSD offered at DSD64 alone and DoP's
// carrier at every rate: under auto, DSD64 plays native, and DSD128 plays DoP
// on the profile's claim where nothing keeps DoP out.
func heardDAC(t *testing.T) *rig {
	r := newRig(t)
	r.known = true
	r.profileDoP = &dacprofile.DoP{HeardBitRates: []int{dsd64, dsd128}}
	r.altsets = slices.DeleteFunc(r.altsets, func(a alsa.Altset) bool { return a.DSDFormat() != "" })
	native := nativeAltset()
	native.Rates = []int{88200}
	r.altsets = append(r.altsets, native)
	return r
}

// onState is r's inputs over st.
func onState(r *rig, st dsdstate.State) Inputs { return r.inputs().WithState(st) }

// wantPlans fails t unless the set over in plays DSD64, DSD128 and DSD256 as
// want says.
func wantPlans(t *testing.T, what string, in Inputs, want ...Outcome) {
	t.Helper()
	s := Effective(in)
	for i, rate := range dsdRates {
		wantOutcome(t, what+" at "+rateName(rate), s.At(rate).Plan, want[i])
	}
}

func rateName(rate int) string {
	switch rate {
	case dsd64:
		return "DSD64"
	case dsd128:
		return "DSD128"
	}
	return "DSD256"
}

// chooseAtStored is a write of c, as the app or the console makes one, from
// the revision in's state stores (choose's): the state it leaves.
func chooseAtStored(t *testing.T, in Inputs, c Choice) dsdstate.State {
	t.Helper()
	c.BaseRevision = StoredRevision(in.State, in.Unit.Key, in.Stamp)
	return choose(t, in, c)
}

// TestAMigratedAutoWithoutAllowDoPPlaysNoDoP (B1043): 0.2.3's auto with
// allowDoP false or unset, migrated with no record or with a record whose DoP
// rates are clean answers, plays no DoP under auto on a known DAC whose
// profile heard DoP at DSD128 with no native there: DSD128 converts, as it
// does at DSD256, and DSD64 plays native as before. allowDoP true, a marker of
// no dsdMode and no marker at all play DoP at DSD128 as before.
func TestAMigratedAutoWithoutAllowDoPPlaysNoDoP(t *testing.T) {
	r := heardDAC(t)
	for _, tc := range []struct {
		name string
		st   dsdstate.State
	}{
		{"allowDoP false, no record", migratedAuto(t, ptr(false))},
		{"allowDoP unset, no record", migratedAuto(t, nil)},
		{"allowDoP false, a record that heard DoP at DSD128", migratedAuto(t, ptr(false), dsd128)},
		{"allowDoP unset, a record that heard DoP at DSD64 and DSD128", migratedAuto(t, nil, dsd64, dsd128)},
	} {
		wantPlans(t, tc.name, onState(r, tc.st), OutcomeNative, OutcomeConverted, OutcomeConverted)
	}
	unset := dsdstate.State{DefaultMode: dsdstate.ModeAuto, MigratedFrom: &dsdstate.Marker{From: dsdstate.FromKeys, AllowDoP: ptr(false)}}
	for _, tc := range []struct {
		name string
		st   dsdstate.State
	}{
		{"allowDoP true, no record", migratedAuto(t, ptr(true))},
		{"no marker", dsdstate.Defaults()},
		{"a marker of no dsdMode", unset},
	} {
		wantPlans(t, tc.name, onState(r, tc.st), OutcomeNative, OutcomeDoP, OutcomeConverted)
	}
}

// TestAUsersConfirmationOnThisFirmwareLetsAutoPlayDoP (B1043): the opt-out
// ends with a DoP confirmation a user writes for the DAC's firmware now: a
// write of auto with confirmDoP, the app's or the console's, plays DoP at
// DSD128 on the profile's claim and at DSD256 on the confirmation; a write of
// auto without it, or with firstUseAcknowledged alone (the old design's
// first-use acknowledgement, which is not this consent), keeps DoP out, the
// acknowledgement recording native alone, the one transport auto then plays.
// A consent given under one firmware does not lift it on another, though the
// profile's claim holds there, until a consent there.
func TestAUsersConfirmationOnThisFirmwareLetsAutoPlayDoP(t *testing.T) {
	r := heardDAC(t)
	optOut := migratedAuto(t, ptr(false))
	for _, by := range []string{dsdstate.ByApp, dsdstate.ByConsole} {
		st := chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeAuto, ConfirmDoP: true, By: by})
		if d, _ := st.DAC(dacKey(t), testStamp); d.DoPConfirmed == nil || d.DoPConfirmed.SetBy != by || d.DoPConfirmed.BCDDevice != bcd {
			t.Fatalf("the %s's confirmDoP recorded %+v, want its confirmation under %s", by, d.DoPConfirmed, bcd)
		}
		wantPlans(t, "auto confirmed by the "+by, onState(r, st), OutcomeNative, OutcomeDoP, OutcomeDoP)
	}
	st := chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeAuto, By: dsdstate.ByApp})
	wantPlans(t, "auto written without confirmDoP", onState(r, st), OutcomeNative, OutcomeConverted, OutcomeConverted)

	st = chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeAuto, FirstUseAcknowledged: true, By: dsdstate.ByApp})
	wantPlans(t, "auto written with firstUseAcknowledged alone", onState(r, st), OutcomeNative, OutcomeConverted, OutcomeConverted)
	d, _ := st.DAC(dacKey(t), testStamp)
	if d.DoPConfirmed != nil || len(d.FirstUse) != 1 || d.FirstUse[0].Transport != dsdstate.TransportNative || d.FirstUse[0].BitRates != nil {
		t.Errorf("firstUseAcknowledged alone recorded %+v (confirmed %+v), want native's acknowledgement at every rate and no DoP consent", d.FirstUse, d.DoPConfirmed)
	}

	// A consent under 0100, then the unit on firmware 0200.
	consented := chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeAuto, ConfirmDoP: true, By: dsdstate.ByApp})
	r.unitBCD = "0200"
	wantPlans(t, "the consent under another firmware", onState(r, consented), OutcomeNative, OutcomeConverted, OutcomeConverted)
	again := chooseAtStored(t, onState(r, consented), Choice{Mode: dsdstate.ModeAuto, ConfirmDoP: true, By: dsdstate.ByConsole})
	wantPlans(t, "a consent on the new firmware", onState(r, again), OutcomeNative, OutcomeDoP, OutcomeDoP)
}

// TestAnExplicitDoPAndASwitchBackToAutoUnderTheOptOut (B1043): the rule is
// auto's: an explicit dop plays DoP where its own rules admit it; a confirmed
// dop wrote the user's consent, which a switch back to auto keeps, so auto
// plays DoP; an unconfirmed dop the profile's claims admitted gave none, so
// the switch back to auto restores the opt-out.
func TestAnExplicitDoPAndASwitchBackToAutoUnderTheOptOut(t *testing.T) {
	r := heardDAC(t)
	optOut := migratedAuto(t, ptr(false))
	confirmed := chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeDoP, ConfirmDoP: true, By: dsdstate.ByApp})
	wantPlans(t, "dop confirmed", onState(r, confirmed), OutcomeDoP, OutcomeDoP, OutcomeDoP)
	back := chooseAtStored(t, onState(r, confirmed), Choice{Mode: dsdstate.ModeAuto, By: dsdstate.ByApp})
	wantPlans(t, "auto after a confirmed dop", onState(r, back), OutcomeNative, OutcomeDoP, OutcomeDoP)

	// Carriers at DSD64 and DSD128 alone, both heard: dop needs no
	// confirmation, and its write records none.
	r.altsets = slices.DeleteFunc(r.altsets, func(a alsa.Altset) bool { return slices.Contains(a.Formats, "S32_LE") })
	carrier := s32Altset()
	carrier.Rates = []int{44100, 88200, 176400, 352800}
	r.altsets = append(r.altsets, carrier)
	unconfirmed := chooseAtStored(t, onState(r, optOut), Choice{Mode: dsdstate.ModeDoP, By: dsdstate.ByApp})
	if d, _ := unconfirmed.DAC(dacKey(t), testStamp); d.DoPConfirmed != nil {
		t.Fatalf("an unconfirmed dop recorded a confirmation: %+v", d.DoPConfirmed)
	}
	wantPlans(t, "dop admitted by the profile's claims", onState(r, unconfirmed), OutcomeDoP, OutcomeDoP, OutcomeRefused)
	back = chooseAtStored(t, onState(r, unconfirmed), Choice{Mode: dsdstate.ModeAuto, By: dsdstate.ByApp})
	wantPlans(t, "auto after an unconfirmed dop", onState(r, back), OutcomeNative, OutcomeConverted, OutcomeConverted)
}

// TestTheOptOutHoldsOnAnUnknownDACToo (B1043): auto's unknown branch keeps
// DoP out alike. No migration of auto without allowDoP writes a confirmation,
// so the state here is made by hand: the migration's own confirmation and
// this box's clean answers at every rate, which play DoP on an unknown DAC
// with no native where the marker does not opt out, and convert where it
// does; the user's confirmation plays DoP.
func TestTheOptOutHoldsOnAnUnknownDACToo(t *testing.T) {
	r := newRig(t).withoutNative()
	for _, rate := range dsdRates {
		r.hear(dsdstate.TransportDoP, rate, dsdstate.AnswerClean)
	}
	byMigration := &dsdstate.DoPConfirmation{BCDDevice: bcd, ConfirmedAt: t0, SetBy: dsdstate.ByMigration}
	r.confirmed = byMigration
	in := r.inputs()
	wantPlans(t, "no marker", in, OutcomeDoP, OutcomeDoP, OutcomeDoP)
	in.State.MigratedFrom = &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeAuto, AllowDoP: ptr(false)}
	wantPlans(t, "the opt-out", in, OutcomeConverted, OutcomeConverted, OutcomeConverted)
	r.confirm()
	in = r.inputs()
	in.State.MigratedFrom = &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeAuto, AllowDoP: ptr(false)}
	wantPlans(t, "the opt-out, the user's confirmation", in, OutcomeDoP, OutcomeDoP, OutcomeDoP)
}
