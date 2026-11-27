package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// B1045 through the route: a migrated 0.2.3 approval of DoP at DSD64 alone
// keeps DoP at its rate, the migration's confirmation counting only where its
// clean answer holds, and a user's confirmDoP replaces that confirmation.

// migratedDoP64 is the state the migration leaves (dsdstate.Migrate) for
// 0.2.3's dsdMode dop with allowDoP: true and an owner's record of the test
// DAC, on its firmware, that approved DoP at DSD64 alone: dop at revision 1,
// DSD64's DoP answer clean, DoP acknowledged at DSD64, and the migration's one
// confirmation, with no rate.
func migratedDoP64(t *testing.T) dsdstate.State {
	t.Helper()
	yes := true
	l := dsdstate.Legacy{DSDMode: dsdstate.ModeDoP, AllowDoP: &yes, DSDQualification: "approved.json"}
	st, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: l, Now: playbacktest.T0})
	if err != nil {
		t.Fatal(err)
	}
	rec := dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: playbacktest.DACBCD, QualifiedOn: "2026-10-01", DoP: []int{dsdstate.DSD64}}
	st, _, err = dsdstate.Migrate(st, dsdstate.Input{Legacy: l, Bound: &dsdstate.Bound{Key: playbacktest.DACKey(), Stamp: playbacktest.DACStamp()}, Record: rec, Now: playbacktest.T0})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := st.DAC(playbacktest.DACKey(), playbacktest.DACStamp())
	if d.Choice == nil || d.Choice.Mode != dsdstate.ModeDoP || d.Choice.Revision != 1 || d.DoPConfirmed == nil || d.DoPConfirmed.SetBy != dsdstate.ByMigration {
		t.Fatalf("the migration wrote %+v, want dop at revision 1 and its own confirmation", d)
	}
	return st
}

// TestAMigratedDoPApprovalThroughTheRoute (B1045): from the migrated dop of a
// DSD64 record, on a DAC the profile database does not know that offers every
// carrier, GET publishes DoP at DSD64 alone, DSD128 and DSD256 refused as
// dop_confirmation_required, needsConfirmation true and dopConfirmed true (a
// stored confirmation counts on this firmware: its meaning, unchanged). A dop
// PUT without confirmDoP, from the dacKey, dacToken and revision GET published,
// is 409 dop_confirmation_required; one with it is written, the confirmation
// the app's, and DSD128 and DSD256 play DoP. On a DAC that offers DoP's carrier
// at DSD64 alone, the confirmation counts at its only rate: needsConfirmation
// false, and a dop PUT without confirmDoP is written.
func TestAMigratedDoPApprovalThroughTheRoute(t *testing.T) {
	t.Run("every carrier offered", func(t *testing.T) {
		store := playbacktest.NewMemStore(migratedDoP64(t))
		r := newChoiceRig(t, readSideBackend(t, playbacktest.NewBox(5), store))
		d := r.dac(t)
		if !needsConfirmation(t, d) || !d.Choice.DoPConfirmed || d.Choice.Mode != dsdstate.ModeDoP || d.Choice.Revision != 1 {
			t.Fatalf("GET publishes needsConfirmation %v, the choice %+v: want true, and dop at revision 1 with its confirmation stored", needsConfirmation(t, d), d.Choice)
		}
		if got := playsAt(dsdPlaybackOf(t, r.bridge(t))); got != "dop, refused dop_confirmation_required, refused dop_confirmation_required" {
			t.Errorf("the migrated approval plays %s, want DoP at DSD64 alone", got)
		}
		wantAnswer(t, "dop without confirmDoP", r.put(t, choiceBody(t, d, dsdstate.ModeDoP)), http.StatusConflict, "dop_confirmation_required", 4)
		obj := wantAnswer(t, "dop with confirmDoP", r.put(t, choiceBody(t, d, dsdstate.ModeDoP, `"confirmDoP":true`)), http.StatusOK, "", 4)
		e, _ := store.State().DAC(playbacktest.DACKey(), playbacktest.DACStamp())
		if c := e.DoPConfirmed; c == nil || c.SetBy != dsdstate.ByApp || c.BCDDevice != playbacktest.DACBCD {
			t.Fatalf("the confirmed write left the confirmation %+v, want the app's under %s", e.DoPConfirmed, playbacktest.DACBCD)
		}
		if got := playsAt(dsdPlaybackOf(t, obj)); got != "dop, dop, dop" {
			t.Errorf("after the app's confirmation the DAC plays %s, want DoP at every rate", got)
		}
		if needsConfirmation(t, dsdDACOf(t, obj)) {
			t.Error("after the app's confirmation a rate still needs one")
		}
	})
	t.Run("DoP offered at DSD64 alone", func(t *testing.T) {
		box := playbacktest.NewBox(5)
		carrier := playbacktest.S32Altset()
		carrier.Rates = []int{44100, 88200, 176400}
		box.SetAltsets(5, 0, carrier, playbacktest.S24Altset(), playbacktest.NativeAltset())
		r := newChoiceRig(t, readSideBackend(t, box, playbacktest.NewMemStore(migratedDoP64(t))))
		d := r.dac(t)
		if needsConfirmation(t, d) {
			t.Fatal("the migration's confirmation does not count at DSD64, its clean rate and the only one DoP is offered at")
		}
		if got := playsAt(dsdPlaybackOf(t, r.bridge(t))); got != "dop, refused dop_not_offered, refused dop_not_offered" {
			t.Errorf("the migrated approval plays %s, want DoP at DSD64", got)
		}
		wantAnswer(t, "dop without confirmDoP", r.put(t, choiceBody(t, d, dsdstate.ModeDoP)), http.StatusOK, "", 4)
	})
}
