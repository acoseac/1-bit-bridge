package policy

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// B1045: a migrated 0.2.3 approval keeps its DoP rates. 0.2.3's record held
// the rates it approved, and 0.2.3 refused DoP at any other; the migration
// writes a clean DoP answer at each approved rate and, for any, one DoP
// confirmation with no rate, tied to the firmware. That confirmation counts at
// a rate only where a clean DoP answer for the rate holds on this firmware;
// the catalog's claims and a user's confirmation count as B990 set them.

// migratedDoP is the state the migration leaves (dsdstate.Migrate) for 0.2.3's
// dsdMode mode with allowDoP: true and an owner's record of the test DAC, on
// its firmware, that approved DoP at rates alone: the mode's choice at
// revision 1, each rate's DoP answer clean, DoP acknowledged at those rates,
// and the migration's own confirmation.
func migratedDoP(t *testing.T, mode string, rates ...int) dsdstate.State {
	t.Helper()
	allow := true
	legacy := dsdstate.Legacy{DSDMode: mode, AllowDoP: &allow, DSDQualification: "approved.json"}
	s, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: legacy, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	rec := dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: bcd, QualifiedOn: "2026-11-01", DoP: rates}
	s, _, err = dsdstate.Migrate(s, dsdstate.Input{Legacy: legacy, Bound: &dsdstate.Bound{Key: dacKey(t), Stamp: testStamp, Identity: testUnit(bcd)}, Record: rec, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.DAC(dacKey(t), testStamp)
	if d.Choice == nil || d.Choice.Mode != mode || d.Choice.Revision != 1 || d.DoPConfirmed == nil || d.DoPConfirmed.SetBy != dsdstate.ByMigration || len(d.Listener) != len(rates) {
		t.Fatalf("the migration of %s with a DoP record wrote %+v, want its choice, its answers and its own confirmation", mode, d)
	}
	return s
}

// TestAMigratedConfirmationCountsOnlyAtItsCleanRates (B1045): dop with
// allowDoP: true and a record that approved DoP at DSD64 alone, migrated onto
// a DAC that offers every carrier and whose catalog neither hears nor states
// DoP (none, or a profile with no DoP claim), plays DoP at DSD64 on the
// migration's confirmation and its clean answer there, and refuses DSD128 and
// DSD256 as dop_confirmation_required, where the confirmation counted at every
// rate before; the facts say it, Confirmed at DSD64 alone.
func TestAMigratedConfirmationCountsOnlyAtItsCleanRates(t *testing.T) {
	for _, known := range []bool{false, true} {
		r := newRig(t).withoutNative()
		r.known = known
		s := Effective(onState(r, migratedDoP(t, dsdstate.ModeDoP, dsd64)))
		what := map[bool]string{false: "an unknown DAC", true: "a known DAC with no DoP claim"}[known]
		wantOutcome(t, what+" at DSD64", s.At(dsd64).Plan, OutcomeDoP)
		for _, rate := range []int{dsd128, dsd256} {
			wantOutcome(t, what+" at "+rateName(rate), s.At(rate).Plan, OutcomeRefused, ReasonConfirmationRequired)
			if s.At(rate).DoP.Confirmed {
				t.Errorf("%s: the migration's confirmation counts at %s, where no clean answer holds", what, rateName(rate))
			}
		}
		if !s.At(dsd64).DoP.Confirmed {
			t.Errorf("%s: the migration's confirmation does not count at DSD64, its clean rate", what)
		}
	}
}

// TestAMigratedAutoApprovalKeepsItsDoPRates (B1045): auto with allowDoP: true
// and the same record, on a known DAC with no native and no DoP claim, plays
// DoP at DSD64 and converts DSD128 and DSD256.
func TestAMigratedAutoApprovalKeepsItsDoPRates(t *testing.T) {
	r := newRig(t).withoutNative()
	r.known = true
	wantPlans(t, "the migrated auto", onState(r, migratedDoP(t, dsdstate.ModeAuto, dsd64)), OutcomeDoP, OutcomeConverted, OutcomeConverted)
}

// TestAUsersConfirmationReplacesTheMigrations (B1045): over the migrated dop
// of a DSD64 record, the console's write of dop without confirmDoP is refused
// as dop_confirmation_required, the state handed back as it was; with
// confirmDoP it replaces the migration's confirmation with the console's under
// the firmware, and DSD128 and DSD256 play DoP. A state an earlier 0.2.4
// build could leave, the user's confirmed dop beside the confirmation still
// marked the migration's (only development builds held one), needs DSD128
// confirmed again, and the write of it replaces that one.
func TestAUsersConfirmationReplacesTheMigrations(t *testing.T) {
	r := newRig(t).withoutNative()
	in := onState(r, migratedDoP(t, dsdstate.ModeDoP, dsd64))
	refusedWith(t, in, Choice{BaseRevision: 1, Mode: dsdstate.ModeDoP, By: dsdstate.ByConsole}, ChoiceConfirmationRequired)
	st := choose(t, in, Choice{BaseRevision: 1, Mode: dsdstate.ModeDoP, ConfirmDoP: true, By: dsdstate.ByConsole})
	if c := dacOfState(t, st).DoPConfirmed; c == nil || c.SetBy != dsdstate.ByConsole || c.BCDDevice != bcd {
		t.Fatalf("the console's confirmDoP left %+v, want the console's confirmation under %s", c, bcd)
	}
	wantPlans(t, "dop confirmed by the console", onState(r, st), OutcomeDoP, OutcomeDoP, OutcomeDoP)

	// What an earlier 0.2.4 build's confirmed write left: the user's dop, the
	// confirmation kept as the migration's.
	dev := migratedDoP(t, dsdstate.ModeDoP, dsd64)
	d := dev.Entry(dacKey(t), testStamp)
	d.Choice = &dsdstate.Choice{Mode: dsdstate.ModeDoP, Revision: 2, SetAt: t0, SetBy: dsdstate.ByApp}
	in = onState(r, dev)
	wantPlans(t, "an earlier build's confirmed write", in, OutcomeDoP, OutcomeRefused, OutcomeRefused)
	refusedWith(t, in, Choice{BaseRevision: 2, Mode: dsdstate.ModeDoP, By: dsdstate.ByApp}, ChoiceConfirmationRequired)
	st = choose(t, in, Choice{BaseRevision: 2, Mode: dsdstate.ModeDoP, ConfirmDoP: true, By: dsdstate.ByApp})
	if c := dacOfState(t, st).DoPConfirmed; c == nil || c.SetBy != dsdstate.ByApp {
		t.Fatalf("the app's confirmDoP over an earlier build's state left %+v, want the app's", c)
	}
	wantPlans(t, "confirmed again", onState(r, st), OutcomeDoP, OutcomeDoP, OutcomeDoP)
}

// TestTheCatalogAndTheUsersConfirmationKeepTheirAuthority (B1045's controls,
// the same before and after it): after the same migration, a row that states
// DoP to DSD256 plays DSD128 and DSD256 DoP under dop, and a row that heard it
// at DSD128 plays DSD128 DoP under auto; an independent clean answer at DSD128
// plays DSD128 DoP; and a user's confirmation counts at every rate, a clean
// answer at none. The rates these leave to the migration's confirmation alone
// are the narrowing's (TestAMigratedConfirmationCountsOnlyAtItsCleanRates).
func TestTheCatalogAndTheUsersConfirmationKeepTheirAuthority(t *testing.T) {
	stated := newRig(t).withoutNative()
	stated.known, stated.profileDoP = true, &dacprofile.DoP{StatedMaxBitRate: dsd256}
	wantPlans(t, "dop on a row stating DoP to DSD256", onState(stated, migratedDoP(t, dsdstate.ModeDoP, dsd64)), OutcomeDoP, OutcomeDoP, OutcomeDoP)

	heard := newRig(t).withoutNative()
	heard.known, heard.profileDoP = true, &dacprofile.DoP{HeardBitRates: []int{dsd128}}
	s := Effective(onState(heard, migratedDoP(t, dsdstate.ModeAuto, dsd64)))
	wantOutcome(t, "auto on a row that heard DoP at DSD128, at DSD128", s.At(dsd128).Plan, OutcomeDoP)

	r := newRig(t).withoutNative()
	st := migratedDoP(t, dsdstate.ModeDoP, dsd64)
	d := st.Entry(dacKey(t), testStamp)
	d.Listener = append(d.Listener, dsdstate.Answer{Transport: dsdstate.TransportDoP, BitRate: dsd128, BCDDevice: bcd, Answer: dsdstate.AnswerClean, On: "2026-11-20"})
	s = Effective(onState(r, st))
	wantOutcome(t, "an independent clean answer at DSD128, at DSD128", s.At(dsd128).Plan, OutcomeDoP)
	if !s.At(dsd128).DoP.Confirmed {
		t.Error("an independent clean answer at DSD128 does not count the migration's confirmation there")
	}

	user := newRig(t).withoutNative()
	user.mode = dsdstate.ModeDoP
	user.confirm()
	wantPlans(t, "a user's confirmation", user.inputs(), OutcomeDoP, OutcomeDoP, OutcomeDoP)
}
