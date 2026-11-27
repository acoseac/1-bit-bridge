package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// B1043 through the route and through serve: a 0.2.3 bridge that ran dsdMode
// auto without allowDoP: true never played DoP, and 0.2.4's auto plays none
// for it until the user confirms DoP on the DAC's firmware (policy's
// dopKeptOut). The read side keeps its meanings: needsConfirmation is the
// rates an explicit dop needs confirmDoP at, and allowDoP a profile's claim or
// a user's confirmation; the rule shows in the plans alone.

// optOutBox is the test DAC's box for B1043: native DSD at DSD64 alone, DoP's
// carrier at DSD64 and DSD128 and, with dsd256, DSD256, and S24_3LE to 176.4
// kHz to convert to. Known by withKnownDAC's profile, which heard DoP at DSD64
// and DSD128, the DAC plays native at DSD64 and, where nothing keeps DoP out,
// DoP at DSD128.
func optOutBox(dsd256 bool) *playbacktest.Box {
	box := playbacktest.NewBox(5)
	carrier := playbacktest.S32Altset()
	if !dsd256 {
		carrier.Rates = []int{44100, 88200, 176400, 352800}
	}
	native := playbacktest.NativeAltset()
	native.Rates = []int{88200}
	box.SetAltsets(5, 0, carrier, playbacktest.S24Altset(), native)
	return box
}

// optedOutState is the state the migration leaves (dsdstate.Migrate) for
// 0.2.3's dsdMode auto with allowDoP false and no owner's record: the marker,
// the defaults and the record's warning.
func optedOutState(t *testing.T) dsdstate.State {
	t.Helper()
	no := false
	l := dsdstate.Legacy{DSDMode: dsdstate.ModeAuto, AllowDoP: &no}
	st, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: l, Now: playbacktest.T0})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err = dsdstate.Migrate(st, dsdstate.Input{Legacy: l, Bound: &dsdstate.Bound{Key: playbacktest.DACKey(), Stamp: playbacktest.DACStamp()},
		Record: dsdstate.Record{Outcome: dsdstate.RecordMissing}, Now: playbacktest.T0})
	if err != nil {
		t.Fatal(err)
	}
	if m := st.MigratedFrom; m == nil || m.DSDMode != dsdstate.ModeAuto || m.WaitingFor != "" {
		t.Fatalf("the migration left the marker %+v", st.MigratedFrom)
	}
	return st
}

// playsAt is what p plays at DSD64, DSD128 and DSD256: native, dop, converted
// or refused with its word.
func playsAt(p api.OutputDSDPlayback) string {
	at := map[int]string{}
	for _, tu := range p.Tuples {
		at[tu.BitRate] = "native"
		if tu.Transport == "dop" {
			at[tu.BitRate] = "dop"
		}
	}
	for _, c := range p.Converted {
		at[c.BitRate] = "converted"
	}
	for _, r := range p.Refused {
		at[r.BitRate] = "refused " + r.Reason
	}
	return fmt.Sprintf("%s, %s, %s", at[dsdstate.DSD64], at[dsdstate.DSD128], at[dsdstate.DSD256])
}

// dopOptionOf is d's dop option.
func dopOptionOf(t *testing.T, d api.OutputDSDDAC) api.OutputDSDOption {
	t.Helper()
	for _, o := range d.Options {
		if o.Mode == dsdstate.ModeDoP {
			return o
		}
	}
	t.Fatalf("the dsdDAC %+v offers no dop option", d)
	return api.OutputDSDOption{}
}

// needsConfirmation is d's dop option's needsConfirmation.
func needsConfirmation(t *testing.T, d api.OutputDSDDAC) bool {
	t.Helper()
	o := dopOptionOf(t, d)
	if o.NeedsConfirmation == nil {
		t.Fatalf("the dop option %+v has no needsConfirmation", o)
	}
	return *o.NeedsConfirmation
}

// firstUseStates is d's firstUse, each transport's state.
func firstUseStates(d api.OutputDSDDAC) string {
	var parts []string
	for _, f := range d.FirstUse {
		parts = append(parts, f.Transport+" "+f.State)
	}
	return strings.Join(parts, ", ")
}

// TestAnOptedOutAutoThroughTheRoute (B1043): on a DAC its profile knows, which
// heard DoP at DSD128 where it offers no native, a migrated 0.2.3 auto with
// allowDoP false publishes DSD128 converted where the same DAC with no marker
// plays it DoP, its needsConfirmation and allowDoP the same in both. Through
// PUT /v1/outputs/{id}/dsd as the app writes, from what GET published: an
// explicit dop without confirmDoP is answered as needsConfirmation says
// (refused where DSD256's carrier has no claim, written where every carrier's
// rate is heard, and then playing DoP, the rule being auto's); auto without
// confirmDoP, and auto with firstUseAcknowledged alone, keep DoP out, the
// latter's firstUse acknowledging native alone; auto with confirmDoP writes
// the app's confirmation under the firmware, and DSD128 plays DoP.
func TestAnOptedOutAutoThroughTheRoute(t *testing.T) {
	t.Run("the read side keeps its meanings", func(t *testing.T) {
		out := newChoiceRig(t, readSideBackend(t, optOutBox(true), playbacktest.NewMemStore(optedOutState(t)), withKnownDAC))
		plain := newChoiceRig(t, readSideBackend(t, optOutBox(true), playbacktest.NewMemStore(dsdstate.Defaults()), withKnownDAC))
		ob, pb := out.bridge(t), plain.bridge(t)
		if got := playsAt(dsdPlaybackOf(t, ob)); got != "native, converted, converted" {
			t.Errorf("the opted-out auto plays %s, want DSD128 converted", got)
		}
		if got := playsAt(dsdPlaybackOf(t, pb)); got != "native, dop, converted" {
			t.Errorf("the same DAC with no marker plays %s, want DSD128 DoP (the control)", got)
		}
		od, pd := dsdDACOf(t, ob), dsdDACOf(t, pb)
		if needsConfirmation(t, od) != needsConfirmation(t, pd) || !needsConfirmation(t, od) || string(ob["allowDoP"]) != string(pb["allowDoP"]) || string(ob["allowDoP"]) != "true" {
			t.Errorf("needsConfirmation %v (%v with no marker), allowDoP %s (%s): want both as the facts give them, true and true",
				needsConfirmation(t, od), needsConfirmation(t, pd), ob["allowDoP"], pb["allowDoP"])
		}
		if od.Choice.DoPConfirmed || pd.Choice.DoPConfirmed {
			t.Errorf("dopConfirmed %v (%v with no marker), want neither: no confirmation is stored", od.Choice.DoPConfirmed, pd.Choice.DoPConfirmed)
		}
	})

	t.Run("an explicit dop is answered as needsConfirmation says", func(t *testing.T) {
		r := newChoiceRig(t, readSideBackend(t, optOutBox(true), playbacktest.NewMemStore(optedOutState(t)), withKnownDAC))
		d := r.dac(t)
		if !needsConfirmation(t, d) {
			t.Fatal("DSD256's carrier, which no claim covers, needs no confirmation")
		}
		wantAnswer(t, "dop without confirmDoP", r.put(t, choiceBody(t, d, dsdstate.ModeDoP)), http.StatusConflict, "dop_confirmation_required", 4)

		every := newChoiceRig(t, readSideBackend(t, optOutBox(false), playbacktest.NewMemStore(optedOutState(t)), withKnownDAC))
		d = every.dac(t)
		if needsConfirmation(t, d) {
			t.Fatal("every carrier's rate is heard, and DoP needs a confirmation")
		}
		if got := playsAt(dsdPlaybackOf(t, every.bridge(t))); got != "native, converted, converted" {
			t.Errorf("the opted-out auto plays %s, want DSD128 converted", got)
		}
		wantAnswer(t, "dop without confirmDoP", every.put(t, choiceBody(t, d, dsdstate.ModeDoP)), http.StatusOK, "", 4)
		if got := playsAt(dsdPlaybackOf(t, every.bridge(t))); got != "dop, dop, refused dop_not_offered" {
			t.Errorf("the explicit dop plays %s, want DoP where its carrier is offered", got)
		}
	})

	t.Run("auto consents only with confirmDoP", func(t *testing.T) {
		store := playbacktest.NewMemStore(optedOutState(t))
		r := newChoiceRig(t, readSideBackend(t, optOutBox(true), store, withKnownDAC))
		wantAnswer(t, "auto without confirmDoP", r.put(t, choiceBody(t, r.dac(t), dsdstate.ModeAuto)), http.StatusOK, "", 4)
		if got := playsAt(dsdPlaybackOf(t, r.bridge(t))); got != "native, converted, converted" {
			t.Errorf("after auto without confirmDoP the DAC plays %s, want DoP kept out", got)
		}
		obj := wantAnswer(t, "auto with firstUseAcknowledged alone", r.put(t, choiceBody(t, r.dac(t), dsdstate.ModeAuto, `"firstUseAcknowledged":true`)), http.StatusOK, "", 4)
		if got := playsAt(dsdPlaybackOf(t, obj)); got != "native, converted, converted" {
			t.Errorf("after auto with firstUseAcknowledged alone the DAC plays %s, want DoP kept out", got)
		}
		if got := firstUseStates(dsdDACOf(t, obj)); got != "native acknowledged, dop pending" {
			t.Errorf("the firstUse read back %q, want native acknowledged and DoP pending: auto's acknowledgement covers what auto plays", got)
		}
		obj = wantAnswer(t, "auto with confirmDoP", r.put(t, choiceBody(t, r.dac(t), dsdstate.ModeAuto, `"confirmDoP":true`)), http.StatusOK, "", 4)
		if got := playsAt(dsdPlaybackOf(t, obj)); got != "native, dop, dop" {
			t.Errorf("after the app's confirmDoP the DAC plays %s, want DoP at DSD128 and DSD256", got)
		}
		d, _ := store.State().DAC(playbacktest.DACKey(), playbacktest.DACStamp())
		if c := d.DoPConfirmed; c == nil || c.SetBy != dsdstate.ByApp || c.BCDDevice != playbacktest.DACBCD {
			t.Errorf("the state holds the confirmation %+v, want the app's under %s", d.DoPConfirmed, playbacktest.DACBCD)
		}
	})
}

// knownGoBar is a profile database that knows the GO bar and heard DoP from
// it at DSD64 and DSD128, which the shipped DAC list does not.
func knownGoBar(p *playback.DSDPolicy) {
	iface, product := 0, "GO bar"
	p.Profiles = &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion, Profiles: []dacprofile.Profile{{
		ID: "test.go-bar", Name: "GO bar",
		Match: []dacprofile.Match{{VendorID: "20b1", ProductID: "3009", Interface: &iface, Product: &product}},
		DoP: &dacprofile.DoP{HeardBitRates: []int{dsdstate.DSD64, dsdstate.DSD128},
			Source: dacprofile.Source{Kind: dacprofile.SourceCapture, Ref: "a test's listening, 2026-11-27"}},
	}}}
}

// goBarDoPOnly lays out the GO bar as goBarDSD does, its stream file listing
// S32_LE alone: DoP's carrier at DSD64 and DSD128, and no native DSD.
func goBarDoPOnly(t *testing.T, trees outputTrees) {
	t.Helper()
	goBarDSD(t, trees)
	stream := "Playback:\n  Status: Stop\n  Interface 1\n    Altset 1\n    Format: S32_LE\n    Channels: 2\n" +
		"    Rates: 44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000\n    Bits: 32\n"
	if err := os.WriteFile(filepath.Join(trees.proc, "asound", "card5", "stream0"), []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAnAutoWithoutAllowDoPKeepsDoPOutInTheRunWhoseSaveFailed (B1043, in
// B1042's windows): 0.2.3's auto without allowDoP: true, set false or unset,
// whose start step's save fails, refused before it lands or landed and not
// known to be durable, keeps DoP out in that run on a GO bar a profile knows
// and heard DoP from at DSD64 and DSD128, with no native: every rate converts,
// from the marker the store holds for the run. With allowDoP: true, the
// control, DoP plays at both.
func TestAnAutoWithoutAllowDoPKeepsDoPOutInTheRunWhoseSaveFailed(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, keys := range []struct {
		name, keys, want string
	}{
		{"allowDoP false", "    dsdMode: auto\n    allowDoP: false\n", `2822400/"converted" 5644800/"converted" 11289600/"converted" `},
		{"allowDoP unset", "    dsdMode: auto\n", `2822400/"converted" 5644800/"converted" 11289600/"converted" `},
		{"allowDoP true", "    dsdMode: auto\n    allowDoP: true\n", `2822400/"dop" 5644800/"dop" 11289600/"converted" `},
	} {
		for _, path := range []string{"refused before it lands", "its barrier fails after it lands"} {
			t.Run(keys.name+"/"+path, func(t *testing.T) {
				box := newDSDBox(t)
				goBarDoPOnly(t, box.trees)
				c := playerServeCase{player: goBarPlayer + keys.keys, standIn: newPlayerStandIn(), with: box.with}
				if path == "refused before it lands" {
					c.seams = refuseTheStartStep(t)
				} else {
					var dataDir atomic.Pointer[string]
					var failed atomic.Bool
					t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
						dir := dataDir.Load()
						if dir == nil {
							return nil
						}
						if _, err := os.Lstat(filepath.Join(*dir, dsdstate.FileName)); err == nil && failed.CompareAndSwap(false, true) {
							return errors.New("the test's barrier fails once the state's file is there")
						}
						return nil
					}))
					c.before = func(d string) { dataDir.Store(&d) }
				}
				ps := bootPlayerServe(t, c)
				availableOutput(t, ps.runningEngine(t))
				b := policyBackendOf(t, wiredPlayback(t, ps.standIn), dopContents{}, &dopOpens{}, knownGoBar)
				if got := planned(b.Policy()); got != keys.want {
					t.Errorf("the effective policy serve wired plays %s(reason %q), want %s", got, b.Policy().Reason, keys.want)
				}
				store := wiredPolicyOf(t, wiredPlayback(t, ps.standIn)).Store
				if m := store.State().MigratedFrom; m == nil || m.DSDMode != dsdstate.ModeAuto {
					t.Errorf("the store holds the marker %+v, want auto's", m)
				}
				if landed := store.(interface{ Landed() error }).Landed(); (landed != nil) != (path != "refused before it lands") {
					t.Errorf("the store's fence %v on the path %q", landed, path)
				}
				stopped(t, ps)
			})
		}
	}
}

// planned is what p plays at each rate: its tuples' transports and its
// converted rates, in rate order.
func planned(p playback.PlayerPolicy) string {
	at := map[int]string{}
	for _, tu := range p.Tuples {
		at[tu.Tuple.BitRate] = "native"
		if tu.Tuple.Transport == dsdstate.TransportDoP {
			at[tu.Tuple.BitRate] = "dop"
		}
	}
	for _, c := range p.Converted {
		at[c.BitRate] = "converted"
	}
	var b strings.Builder
	for _, rate := range []int{dsdstate.DSD64, dsdstate.DSD128, dsdstate.DSD256} {
		fmt.Fprintf(&b, "%d/%q ", rate, at[rate])
	}
	return b.String()
}
