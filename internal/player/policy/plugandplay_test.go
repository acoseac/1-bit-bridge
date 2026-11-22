package policy

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Plug and play (B990, the owner's decision of 2026-11-22): the player plays
// what the DAC supports and converts what it does not, and asks no first-use
// acknowledgement. A unit the DAC list does not know plays native DSD unless
// its USB IDs are a generic board's, one of the list's shared reference IDs,
// where only a listener's clean answer at the rate admits it. The cases take
// the shipped DAC list (dacprofile.Default), whose matcher decides what is
// known.

// shipped is the shipped DAC list, parsed afresh.
func shipped(t *testing.T) *dacprofile.Database {
	t.Helper()
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// modelKey is the model of a unit of vendor:product whose card is interface
// 0, its product string name.
func modelKey(t *testing.T, vendor, product uint16, name string) dacprofile.Key {
	t.Helper()
	k, err := dacprofile.NewKey(vendor, product, ptr(0), ptr(name))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// mytek is the Mytek Brooklyn DAC+, the DAC the owner's box binds, which the
// list knows as mytek.brooklyn.dac.plus.
func mytek(t *testing.T) dacprofile.Key { return modelKey(t, 0x25ce, 0x0025, "Brooklyn DAC+") }

// onCatalog is r's inputs for a unit of k under the DAC list db, r's state
// entry k's: no profile but db's, whatever r.known says.
func (r *rig) onCatalog(k dacprofile.Key, db *dacprofile.Database) Inputs {
	in := r.inputs()
	in.Unit.Key, in.Profiles = k, db
	for i := range in.State.DACs {
		in.State.DACs[i].Key = k
	}
	return in
}

// wantAt fails t unless s gives outcome at each of rates, with reason for a
// refusal.
func wantAt(t *testing.T, what string, s Set, rates []int, outcome Outcome, reason ...any) {
	t.Helper()
	for _, rate := range rates {
		wantOutcome(t, fmt.Sprintf("%s at %d", what, rate), s.At(rate).Plan, outcome, reason...)
	}
}

// migratedOn is the state a migration of the legacy keys, dsdMode mode,
// writes for a unit of key whose owner's record approved native DSD at rates
// alone (dsdstate.Migrate): its choice, clean answers at rates and an
// acknowledgement covering them.
func migratedOn(t *testing.T, key dacprofile.Key, mode string, rates ...int) dsdstate.State {
	t.Helper()
	allow := false
	legacy := dsdstate.Legacy{DSDMode: mode, AllowDoP: &allow, DSDQualification: "approved.json"}
	s, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: legacy, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	product, _ := key.Product()
	id := qualification.Identity{Vendor: usbid.Hex16(key.VendorID()), ProductID: usbid.Hex16(key.ProductID()), Manufacturer: "Test", Product: product,
		BCDDevice: bcd, Port: "platform-xhci-hcd.0/usb1-1:1.0"}
	record := dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: bcd, QualifiedOn: "2026-11-01", Native: rates}
	s, _, err = dsdstate.Migrate(s, dsdstate.Input{Legacy: legacy, Bound: &dsdstate.Bound{Key: key, Stamp: testStamp, Identity: id}, Record: record, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := s.DAC(key, testStamp); !ok || d.Choice == nil || d.Choice.Mode != mode {
		t.Fatalf("the migration of dsdMode %s wrote %+v, want the DAC's migrated choice", mode, s.DACs)
	}
	return s
}

// TestAKnownDACPlaysNativeWithNoAcknowledgement (B990): auto on a DAC the
// list knows plays native DSD wherever the kernel offers it and no listener
// rejected it, with no acknowledgement in the state: the owner's Mytek
// Brooklyn DAC+, which the kernel offers DSD_U32_BE at DSD64, DSD128 and
// DSD256 and whose driver accepted each, plays native at all three, each
// tuple at the driver's acceptance, and publishes no other outcome.
func TestAKnownDACPlaysNativeWithNoAcknowledgement(t *testing.T) {
	r := newRig(t)
	for _, rate := range dsdRates {
		r.evidence = append(r.evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE", BitRate: rate, BCDDevice: bcd, DriverAcceptedAt: t0, Kernel: kernel})
	}
	s := Effective(r.onCatalog(mytek(t), shipped(t)))
	if !s.Known || s.Profile != "mytek.brooklyn.dac.plus" || s.Own || s.Mode != dsdstate.ModeAuto {
		t.Fatalf("the set: known %v as %q, own %v, mode %q; want the Mytek's row under the default auto", s.Known, s.Profile, s.Own, s.Mode)
	}
	all := s.All()
	if len(all) != len(dsdRates) {
		t.Fatalf("the set decides %d rates, want %d", len(all), len(dsdRates))
	}
	for _, at := range all {
		what := fmt.Sprintf("auto on the Mytek at %d", at.Plan.BitRate)
		if at.Native.Acknowledged || at.DoP.Acknowledged {
			t.Fatalf("%s: the state acknowledges a first use", what)
		}
		wantOutcome(t, what, at.Plan, OutcomeNative)
		if at.Plan.Tuple.Format != format.DSDU32BE || at.Plan.Evidence != RungDriverAccepted || at.Plan.EvidenceOn != "2026-11-05" {
			t.Errorf("%s: %s with %q on %q, want DSD_U32_BE with %q on 2026-11-05", what, at.Plan.Tuple.Format, at.Plan.Evidence, at.Plan.EvidenceOn, RungDriverAccepted)
		}
	}
}

// TestAKnownDACPlaysDoPWithNoAcknowledgement (B990): auto on a known DAC
// with no native DSD plays DoP at a rate its row heard DoP at, or where the
// user confirmed DoP, its carrier offered and the gain at unity, with no
// acknowledgement: a row that heard DoP at every rate (one the case adds to
// the list, which holds none with a USB match) plays DoP at every rate, and
// the Mytek, confirmed, plays DoP at DSD64 and DSD128 and converts DSD256,
// which its row refuses.
func TestAKnownDACPlaysDoPWithNoAcknowledgement(t *testing.T) {
	db := shipped(t)
	heard := dacprofile.Profile{ID: "test.dop-heard", Name: "Test DoP DAC",
		Match: []dacprofile.Match{{VendorID: "1234", ProductID: "5678", Product: ptr("Test DAC")}},
		DoP: &dacprofile.DoP{HeardBitRates: []int{dsd64, dsd128, dsd256},
			Source: dacprofile.Source{Kind: dacprofile.SourceListening, Ref: "a listening on a test unit, 2026-11-22"}},
		Provenance: []string{"identity: a capture of a test unit, Linux, 2026-11-22: 1234:5678, the card its audio control interface 0, product string Test DAC",
			"dop: heard DSD64, DSD128, DSD256: a listening on a test unit, 2026-11-22"}}
	db.Profiles = append(db.Profiles, heard)
	if problems := db.Lint(); len(problems) > 0 {
		t.Fatalf("the row the case adds does not lint: %v", problems)
	}
	s := Effective(newRig(t).withoutNative().onCatalog(dacKey(t), db))
	if !s.Known || s.Profile != heard.ID {
		t.Fatalf("the test DAC: known %v as %q, want %s", s.Known, s.Profile, heard.ID)
	}
	wantAt(t, "auto on the row that heard DoP", s, dsdRates, OutcomeDoP)

	m := newRig(t).withoutNative()
	m.confirm()
	s = Effective(m.onCatalog(mytek(t), shipped(t)))
	wantAt(t, "auto on the Mytek, DoP confirmed", s, []int{dsd64, dsd128}, OutcomeDoP)
	wantAt(t, "auto on the Mytek at the rate its row refuses", s, []int{dsd256}, OutcomeConverted)
}

// TestAnUnknownDACPlaysNativeUnlessItsIDsAreAGenericBoards (B990): a DAC the
// list does not know plays native DSD under auto and under an explicit native
// at every rate it is offered, DSD64 to DSD256, with no acknowledgement and no
// listener answer; one whose IDs are one of the list's shared reference IDs,
// a generic board's, whose kernel offer is the board's, converts at every
// rate under auto, and an explicit native refuses it there as
// native_check_required. The Amanero board's newer firmware's IDs, 16d0:0a23,
// are shared, and no row knows them. A row outranks the list: the same IDs
// with a row of their own, by its product string, play native, and so does a
// wildcard row's, a kernel quirk's, which names the IDs alone (the list's
// lint keeps a wildcard row off a shared ID). With no list to ask, the
// bridge cannot tell a board from a DAC, and auto converts.
func TestAnUnknownDACPlaysNativeUnlessItsIDsAreAGenericBoards(t *testing.T) {
	db := shipped(t)
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative} {
		r := newRig(t).withoutCarrier()
		if mode == dsdstate.ModeNative {
			r.mode = mode
		}
		s := Effective(r.onCatalog(dacKey(t), db))
		if s.Known {
			t.Fatalf("the test DAC is known as %s", s.Profile)
		}
		wantAt(t, mode+" on an unknown DAC", s, dsdRates, OutcomeNative)
	}
	board := func(id dacprofile.SharedID) dacprofile.Key {
		v, e1 := usbid.ParseHex16(id.VendorID)
		p, e2 := usbid.ParseHex16(id.ProductID)
		if e1 != nil || e2 != nil {
			t.Fatalf("the shared ID %s:%s does not read", id.VendorID, id.ProductID)
		}
		return modelKey(t, v, p, "Test DAC")
	}
	for _, id := range db.SharedReferenceIDs {
		for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative} {
			r := newRig(t)
			if mode == dsdstate.ModeNative {
				r.mode = mode
			}
			s := Effective(r.onCatalog(board(id), db))
			what := mode + " on the generic board " + id.VendorID + ":" + id.ProductID
			if s.Known {
				t.Errorf("%s: known as %s", what, s.Profile)
			}
			if mode == dsdstate.ModeAuto {
				wantAt(t, what, s, dsdRates, OutcomeConverted)
			} else {
				wantAt(t, what, s, dsdRates, OutcomeRefused, ReasonNativeCheckRequired)
			}
		}
	}
	newer := dacprofile.SharedID{VendorID: "16d0", ProductID: "0a23"}
	if !slices.ContainsFunc(db.SharedReferenceIDs, func(s dacprofile.SharedID) bool {
		return s.VendorID == newer.VendorID && s.ProductID == newer.ProductID
	}) {
		t.Errorf("the list does not hold 16d0:0a23, the Amanero Combo384's newer firmware, as a shared reference ID")
	}
	s := Effective(newRig(t).onCatalog(board(newer), db))
	if s.Known {
		t.Errorf("16d0:0a23 is known as %s: the Amanero board's newer firmware's IDs are a generic board's", s.Profile)
	}
	wantAt(t, "auto on 16d0:0a23", s, dsdRates, OutcomeConverted)

	withRow := shipped(t)
	row := dacprofile.Profile{ID: "test.on-a-board", Name: "Test DAC on a board",
		Match: []dacprofile.Match{{VendorID: "20b1", ProductID: "0010", Product: ptr("Test DAC")}},
		Native: &dacprofile.Native{MaxBitRate: dsd256, Formats: []string{"DSD_U32_BE"},
			Source: dacprofile.Source{Kind: dacprofile.SourceCapture, Ref: "a capture of a test unit, Linux, 2026-11-22"}},
		Provenance: []string{"identity: a capture of a test unit, Linux, 2026-11-22: 20b1:0010, the card its audio control interface 0, product string Test DAC",
			"native: a capture of a test unit, Linux, 2026-11-22: the playback stream lists DSD_U32_BE at frame rates up to 352800, DSD256"}}
	withRow.Profiles = append(withRow.Profiles, row)
	if problems := withRow.Lint(); len(problems) > 0 {
		t.Fatalf("the row the case adds does not lint: %v", problems)
	}
	onBoard := board(dacprofile.SharedID{VendorID: "20b1", ProductID: "0010"})
	s = Effective(newRig(t).withoutCarrier().onCatalog(onBoard, withRow))
	if !s.Known || s.Profile != row.ID {
		t.Fatalf("a unit a row names on a board's IDs: known %v as %q, want %s", s.Known, s.Profile, row.ID)
	}
	wantAt(t, "auto on a row on a board's IDs", s, dsdRates, OutcomeNative)

	furutech := modelKey(t, 0x16d0, 0x0733, "Test DAC")
	s = Effective(newRig(t).withoutCarrier().onCatalog(furutech, db))
	if !s.Known || s.Profile != "kernel.16d0-0733" {
		t.Fatalf("16d0:0733: known %v as %q, want the kernel quirk's row", s.Known, s.Profile)
	}
	wantAt(t, "auto on a wildcard row's IDs", s, dsdRates, OutcomeNative)

	wantAt(t, "auto with no list", Effective(newRig(t).withoutCarrier().onCatalog(dacKey(t), nil)), dsdRates, OutcomeConverted)
}

// TestAListenersCleanAnswerKeepsNativeOnAGenericBoard (B990): on a generic
// board's IDs the latest listener answer for native at a rate, clean, under
// the unit's firmware, admits native there, under auto and an explicit
// native: an owner's record that approved DSD64 alone, migrated, plays
// native at DSD64, and the other rates convert under auto and refuse as
// native_check_required under native; a later noise at DSD64 takes it away,
// auto converting and native refusing it as listener_rejected. DoP there
// stays as it was: confirmed and its latest answer clean, it plays.
func TestAListenersCleanAnswerKeepsNativeOnAGenericBoard(t *testing.T) {
	board := modelKey(t, 0x16d0, 0x071a, "Test DAC")
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative} {
		t.Run(mode, func(t *testing.T) {
			in := newRig(t).withoutCarrier().onCatalog(board, shipped(t))
			in.State = migratedOn(t, board, mode, dsd64)
			s := Effective(in)
			if s.Known {
				t.Fatalf("the board is known as %s", s.Profile)
			}
			wantAt(t, "migrated "+mode, s, []int{dsd64}, OutcomeNative)
			if mode == dsdstate.ModeAuto {
				wantAt(t, "migrated auto past its record's rate", s, []int{dsd128, dsd256}, OutcomeConverted)
			} else {
				wantAt(t, "migrated native past its record's rate", s, []int{dsd128, dsd256}, OutcomeRefused, ReasonNativeCheckRequired)
			}
			i := slices.IndexFunc(in.State.DACs, func(d dsdstate.DAC) bool { return d.Key.Equal(board) })
			d := &in.State.DACs[i]
			for j := range d.Listener {
				if d.Listener[j].Transport == dsdstate.TransportNative && d.Listener[j].BitRate == dsd64 {
					d.Listener[j].Answer, d.Listener[j].On = dsdstate.AnswerNoise, "2026-11-20"
				}
			}
			s = Effective(in)
			if mode == dsdstate.ModeAuto {
				wantAt(t, "migrated auto after a later noise", s, []int{dsd64}, OutcomeConverted)
			} else {
				wantAt(t, "migrated native after a later noise", s, []int{dsd64}, OutcomeRefused, ReasonListenerRejected)
			}
		})
	}
	r := newRig(t).withoutNative()
	r.confirm()
	r.hear(dsdstate.TransportDoP, dsd128, dsdstate.AnswerClean)
	s := Effective(r.onCatalog(board, shipped(t)))
	wantAt(t, "auto on the board, DoP confirmed and heard clean", s, []int{dsd128}, OutcomeDoP)
	wantAt(t, "auto on the board, DoP confirmed with no answer", s, []int{dsd64, dsd256}, OutcomeConverted)
}

// TestAnExplicitChoicePlaysWithNoAcknowledgement (B990): an explicit native
// or dop plays what it plays, with no acknowledgement: the Mytek under native
// plays native at every rate, and under dop, confirmed, DoP at DSD64 and
// DSD128, its row refusing DSD256 as dop_not_offered.
func TestAnExplicitChoicePlaysWithNoAcknowledgement(t *testing.T) {
	n := newRig(t).withoutCarrier()
	n.mode = dsdstate.ModeNative
	wantAt(t, "an explicit native on the Mytek", Effective(n.onCatalog(mytek(t), shipped(t))), dsdRates, OutcomeNative)
	d := newRig(t).withoutNative()
	d.mode = dsdstate.ModeDoP
	d.confirm()
	s := Effective(d.onCatalog(mytek(t), shipped(t)))
	wantAt(t, "an explicit dop on the Mytek, confirmed", s, []int{dsd64, dsd128}, OutcomeDoP)
	wantAt(t, "an explicit dop on the Mytek at the rate its row refuses", s, []int{dsd256}, OutcomeRefused, ReasonDoPNotOffered)
}

// TestARejectionAndTheGainCheckStillRefuse (B990): what no acknowledgement
// ever lifted still refuses. A listener's latest noise for native at DSD128
// takes native out there, auto converting it (the Mytek's DoP unconfirmed)
// and an explicit native refusing it as listener_rejected, the other rates
// playing native; and DoP behind a level control off unity is converted
// under auto and refused as dop_gain_not_unity under dop.
func TestARejectionAndTheGainCheckStillRefuse(t *testing.T) {
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeNative} {
		r := newRig(t)
		if mode == dsdstate.ModeNative {
			r.mode = mode
		}
		r.hear(dsdstate.TransportNative, dsd128, dsdstate.AnswerNoise)
		s := Effective(r.onCatalog(mytek(t), shipped(t)))
		if mode == dsdstate.ModeAuto {
			wantAt(t, "auto after a noise", s, []int{dsd128}, OutcomeConverted)
		} else {
			wantAt(t, "native after a noise", s, []int{dsd128}, OutcomeRefused, ReasonListenerRejected)
		}
		wantAt(t, mode+" at the rates no listener rejected", s, []int{dsd64, dsd256}, OutcomeNative)
	}
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeDoP} {
		r := newRig(t).withoutNative()
		if mode == dsdstate.ModeDoP {
			r.mode = mode
		}
		r.confirm()
		r.mixer = withVolumeUnit(r.mixer, volume("PCM Playback Volume", 200), switchControl("PCM Playback Switch"))
		s := Effective(r.onCatalog(mytek(t), shipped(t)))
		if mode == dsdstate.ModeAuto {
			wantAt(t, "auto with a level control off unity", s, []int{dsd64, dsd128}, OutcomeConverted)
		} else {
			wantAt(t, "dop with a level control off unity", s, []int{dsd64, dsd128}, OutcomeRefused, ReasonGainNotUnity)
		}
	}
}

// TestAnExplicitChoiceIsWrittenWithoutAnAcknowledgement (B990): an explicit
// native or dop is written with no firstUseAcknowledged, whatever the state
// holds of first use (none, a migrated acknowledgement of DSD64 alone, one
// under another firmware), which the write leaves as it was, and the choice
// plays; one that carries it still records an acknowledgement of every rate
// under the firmware, which the read side reports.
func TestAnExplicitChoiceIsWrittenWithoutAnAcknowledgement(t *testing.T) {
	for _, tc := range []struct{ mode, transport string }{
		{dsdstate.ModeNative, dsdstate.TransportNative},
		{dsdstate.ModeDoP, dsdstate.TransportDoP},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			for _, first := range [][]dsdstate.FirstUse{
				nil,
				{{Transport: tc.transport, BCDDevice: bcd, BitRates: []int{dsd64}, AcknowledgedAt: t0}},
				{{Transport: tc.transport, BCDDevice: "0200", AcknowledgedAt: t0}},
			} {
				r := newRig(t)
				r.confirm()
				r.firstUse = first
				st := choose(t, r.inputs(), Choice{Mode: tc.mode})
				d := dacOfState(t, st)
				if d.Choice == nil || d.Choice.Mode != tc.mode || d.Choice.Revision != 1 {
					t.Fatalf("the write of %s wrote %+v", tc.mode, d.Choice)
				}
				if !reflect.DeepEqual(d.FirstUse, first) {
					t.Errorf("a write with no acknowledgement changed first use to %+v, was %+v", d.FirstUse, first)
				}
				in := r.onCatalog(dacKey(t), shipped(t))
				in.State = st
				want := OutcomeNative
				if tc.mode == dsdstate.ModeDoP {
					want = OutcomeDoP
				}
				wantAt(t, "the choice written with no acknowledgement", Effective(in), dsdRates, want)
			}
			r := newRig(t)
			r.confirm()
			st := choose(t, r.inputs(), Choice{Mode: tc.mode, FirstUseAcknowledged: true})
			if !everyRate(firstUseIn(dacOfState(t, st), tc.transport)) {
				t.Errorf("a write that carried the acknowledgement recorded %+v, want %s at every rate under %s", dacOfState(t, st).FirstUse, tc.transport, bcd)
			}
		})
	}
}

// rejectedNative is a rig whose DAC's own choice is mode ("" for none),
// holding a listener's noise for native at DSD64 and DSD256 and a clean
// answer at DSD128 under the unit's firmware, nothing for DoP at DSD64, and
// a noise for native at DSD256 under another firmware.
func rejectedNative(t *testing.T, mode string) *rig {
	r := newRig(t)
	r.mode = mode
	r.hear(dsdstate.TransportNative, dsd64, dsdstate.AnswerNoise)
	r.hear(dsdstate.TransportNative, dsd128, dsdstate.AnswerClean)
	r.hear(dsdstate.TransportNative, dsd256, dsdstate.AnswerNoise)
	r.hear(dsdstate.TransportDoP, dsd64, dsdstate.AnswerNothing)
	r.listener = append(r.listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: dsd256, BCDDevice: "0200", Answer: dsdstate.AnswerNoise})
	return r
}

// sortedAnswers is answers in cmpAnswer's order, in a list of their own.
func sortedAnswers(answers ...dsdstate.Answer) []dsdstate.Answer {
	out := slices.Clone(answers)
	slices.SortFunc(out, cmpAnswer)
	return out
}

// TestARetryClearsTheRejectionsAtTheRatesItsCapAdmits (B990): with no
// acknowledgement asked, the retry of a transport a listener rejected is an
// explicit native or dop write that makes it the DAC's mode, the DAC's own
// choice before it another mode or none. It clears that transport's
// rejections under the unit's firmware at each rate its maxBitRate admits,
// every rate for none, and keeps the rejections above its cap, the
// transport's clean answers, the other transport's answers and those under
// another firmware; a rate it cleared plays again.
func TestARetryClearsTheRejectionsAtTheRatesItsCapAdmits(t *testing.T) {
	answer := func(transport string, rate int, a, bcdDevice, on string) dsdstate.Answer {
		return dsdstate.Answer{Transport: transport, BitRate: rate, BCDDevice: bcdDevice, Answer: a, On: on}
	}
	noise64 := answer(dsdstate.TransportNative, dsd64, dsdstate.AnswerNoise, bcd, "2026-11-05")
	clean128 := answer(dsdstate.TransportNative, dsd128, dsdstate.AnswerClean, bcd, "2026-11-05")
	noise256 := answer(dsdstate.TransportNative, dsd256, dsdstate.AnswerNoise, bcd, "2026-11-05")
	other256 := answer(dsdstate.TransportNative, dsd256, dsdstate.AnswerNoise, "0200", "")
	nothing64 := answer(dsdstate.TransportDoP, dsd64, dsdstate.AnswerNothing, bcd, "2026-11-05")
	for _, from := range []string{"", dsdstate.ModeAuto, dsdstate.ModeConvert, dsdstate.ModeDoP, dsdstate.ModeOff} {
		for _, tc := range []struct {
			max  int
			want []dsdstate.Answer
		}{
			{0, sortedAnswers(clean128, other256, nothing64)},
			{dsd64, sortedAnswers(clean128, noise256, other256, nothing64)},
		} {
			r := rejectedNative(t, from)
			base := uint64(0)
			if from != "" {
				base = 1
			}
			st := choose(t, r.inputs(), Choice{BaseRevision: base, Mode: dsdstate.ModeNative, MaxBitRate: tc.max})
			what := fmt.Sprintf("native capped at %d chosen over %q", tc.max, from)
			if got := sortedAnswers(dacOfState(t, st).Listener...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: the answers %+v, want %+v", what, got, tc.want)
			}
			in := r.onCatalog(dacKey(t), shipped(t))
			in.State = st
			wantAt(t, what, Effective(in), []int{dsd64}, OutcomeNative)
		}
	}
	st := choose(t, rejectedNative(t, dsdstate.ModeAuto).inputs(), Choice{BaseRevision: 1, Mode: dsdstate.ModeDoP, ConfirmDoP: true})
	if got, want := sortedAnswers(dacOfState(t, st).Listener...), sortedAnswers(noise64, clean128, noise256, other256); !reflect.DeepEqual(got, want) {
		t.Errorf("dop chosen over auto: the answers %+v, want DoP's rejection cleared and native's kept, %+v", got, want)
	}
}

// TestAWriteInTheSameModeKeepsEveryRejection (B990): a write that leaves the
// DAC's mode as it was is no retry. An explicit native over the DAC's own
// native that changes only its cap, or carries firstUseAcknowledged, keeps
// every rejection, and the rejected rate stays refused as listener_rejected;
// auto, convert and off, over no choice of the DAC's own or their own mode,
// keep every rejection too.
func TestAWriteInTheSameModeKeepsEveryRejection(t *testing.T) {
	kept := sortedAnswers(rejectedNative(t, "").listener...)
	for _, c := range []Choice{
		{BaseRevision: 1, Mode: dsdstate.ModeNative, MaxBitRate: dsd128},
		{BaseRevision: 1, Mode: dsdstate.ModeNative, MaxBitRate: dsd256},
		{BaseRevision: 1, Mode: dsdstate.ModeNative, FirstUseAcknowledged: true},
	} {
		r := rejectedNative(t, dsdstate.ModeNative)
		st := choose(t, r.inputs(), c)
		what := fmt.Sprintf("%+v over native", c)
		if got := sortedAnswers(dacOfState(t, st).Listener...); !reflect.DeepEqual(got, kept) {
			t.Errorf("%s: the answers %+v, want every one kept, %+v", what, got, kept)
		}
		in := r.onCatalog(dacKey(t), shipped(t))
		in.State = st
		wantAt(t, what, Effective(in), []int{dsd64}, OutcomeRefused, ReasonListenerRejected)
	}
	for _, mode := range []string{dsdstate.ModeAuto, dsdstate.ModeConvert, dsdstate.ModeOff} {
		for _, from := range []string{"", mode} {
			base := uint64(0)
			if from != "" {
				base = 1
			}
			st := choose(t, rejectedNative(t, from).inputs(), Choice{BaseRevision: base, Mode: mode, FirstUseAcknowledged: true})
			if got := sortedAnswers(dacOfState(t, st).Listener...); !reflect.DeepEqual(got, kept) {
				t.Errorf("%s over %q: the answers %+v, want every one kept", mode, from, got)
			}
		}
	}
}

// TestAnExplicitNativeOnAGenericBoardWaitsForACleanAnswer (B990): an
// explicit native on a generic board's IDs is refused as
// native_check_required at every rate no listener's latest answer holds
// clean under the unit's firmware, whatever else the state holds: an
// acknowledgement of native at every rate, the driver's acceptance under the
// running kernel and streaming observed. Auto converts there. A clean answer
// at DSD128 admits DSD128 alone, under either; one under another firmware
// admits nothing.
func TestAnExplicitNativeOnAGenericBoardWaitsForACleanAnswer(t *testing.T) {
	board := modelKey(t, 0x16d0, 0x071a, "Test DAC")
	for _, mode := range []string{dsdstate.ModeNative, dsdstate.ModeAuto} {
		r := newRig(t).withoutCarrier()
		r.mode = mode
		r.acknowledge(dsdstate.TransportNative)
		for _, rate := range dsdRates {
			r.evidence = append(r.evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE", BitRate: rate, BCDDevice: bcd,
				DriverAcceptedAt: t0, Kernel: kernel, StreamingObservedAt: t0.Add(time.Minute)})
		}
		without := OutcomeConverted
		var reason []any
		if mode == dsdstate.ModeNative {
			without, reason = OutcomeRefused, []any{ReasonNativeCheckRequired}
		}
		s := Effective(r.onCatalog(board, shipped(t)))
		if s.Known {
			t.Fatalf("the board is known as %s", s.Profile)
		}
		wantAt(t, mode+" on the board, acknowledged, accepted and observed", s, dsdRates, without, reason...)
		r.listener = append(r.listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: dsd64, BCDDevice: "0200", Answer: dsdstate.AnswerClean})
		r.hear(dsdstate.TransportNative, dsd128, dsdstate.AnswerClean)
		s = Effective(r.onCatalog(board, shipped(t)))
		wantAt(t, mode+" on the board, clean at DSD128", s, []int{dsd128}, OutcomeNative)
		wantAt(t, mode+" on the board, clean under another firmware or nowhere", s, []int{dsd64, dsd256}, without, reason...)
	}
}
