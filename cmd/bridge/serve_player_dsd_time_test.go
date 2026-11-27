package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A player's DSD state holding a time its reader takes and its writer cannot
// write (B1045's round 3), through serve's player and bridge output play, each
// on a state of its own: serve's in its data directory, output play's in the
// --state-dir it is given, apart from serve's.

// hugoStamp is the Hugo's maker, as the state keys its entries.
var hugoStamp = dsdstate.StampOf("Chord")

// hugoChoiceAt is a markerless state of auto whose one entry, another DAC's
// than the GO bar (a Hugo), holds a choice set at 09:00 with offset.
func hugoChoiceAt(offset string) string {
	return `{"schemaVersion":2,"defaultMode":"auto","dacs":[{"key":{"vendorId":"262a","productId":"9302","interface":null,"product":null},"stamp":"` + string(hugoStamp) + `",` +
		`"choice":{"mode":"native","maxBitRate":0,"revision":3,"setAt":"2026-11-27T09:00:00` + offset + `","setBy":"app"},"dopConfirmed":null,"firstUse":[],"listener":[],"evidence":[]}],` +
		`"migratedFrom":null,"warnings":[]}` + "\n"
}

// seedDSDState lays state out as the player's DSD state in dir.
func seedDSDState(t *testing.T, dir, state string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, dsdstate.FileName), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAStateWithATimeItsWriterCannotWriteRefusesDSD (B1045's round 3): 0.2.3's
// dsdMode: off beside a markerless state of auto whose other DAC's choice was
// set at +24:00, which encoding/json reads and will not write. The state is
// damaged: serve's player and bridge output play each refuse the GO bar's DSD,
// which the kernel offers natively at every rate, as dsd_state_unreadable
// before any open, nothing published or converted, and leave the file's bytes
// beside it and the state that refuses in its place. With the time at +23:59,
// the control, the state loads and the start step writes off and the marker of
// the keys over it: DSD is refused as dsd_off_by_choice, and the other DAC's
// choice is kept as it was, its offset with it. Output play writes nothing in
// serve's data directory.
func TestAStateWithATimeItsWriterCannotWriteRefusesDSD(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	const keys = "    dsdMode: off\n"
	hugo, err := dacprofile.NewKey(0x262a, 0x9302, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, offset string
		reason       engine.Reason
	}{
		{"an offset of 24 hours", "+24:00", "dsd_state_unreadable"},
		{"an offset of 23:59, the control", "+23:59", "dsd_off_by_choice"},
	} {
		state := hugoChoiceAt(tc.offset)
		left := func(t *testing.T, dir, where string) {
			t.Helper()
			st, there := dsdStateIn(t, dir)
			if !there {
				t.Fatalf("%s holds no DSD state", where)
			}
			if tc.reason == "dsd_state_unreadable" {
				if !st.Unreadable() || len(st.DACs) != 0 || st.MigratedFrom == nil || st.MigratedFrom.From != dsdstate.FromStateUnreadable {
					t.Fatalf("%s holds %+v, want the state that refuses DSD", where, st)
				}
				aside, err := filepath.Glob(filepath.Join(dir, dsdstate.FileName+".bad-*"))
				if err != nil || len(aside) != 1 || string(readBytes(t, aside[0])) != state {
					t.Fatalf("%s keeps %v (%v) beside the state, want one copy of the file's bytes", where, aside, err)
				}
				return
			}
			if st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeOff}) {
				t.Fatalf("%s holds defaultMode %q and the marker %+v, want off and the marker of the keys", where, st.DefaultMode, st.MigratedFrom)
			}
			set, err := time.Parse(time.RFC3339, "2026-11-27T09:00:00+23:59")
			if err != nil {
				t.Fatal(err)
			}
			d, ok := st.DAC(hugo, hugoStamp)
			if !ok || d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || d.Choice.Revision != 3 || d.Choice.SetBy != dsdstate.ByApp || !d.Choice.SetAt.Equal(set) {
				t.Fatalf("%s holds the other DAC as %+v, want its choice as it was", where, d)
			}
			if _, off := d.Choice.SetAt.Zone(); off != 23*3600+59*60 {
				t.Fatalf("%s holds the other DAC's choice at an offset of %d s, want +23:59's", where, off)
			}
		}
		t.Run("serve/"+tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDSD(t, box.trees)
			ps := bootDSD(t, box, keys, func(dataDir string) { seedDSDState(t, dataDir, state) })
			availableOutput(t, ps.runningEngine(t))
			refusesDSDOn(t, wiredPlayback(t, ps.standIn), tc.reason)
			stopped(t, ps)
			left(t, ps.dataDir, "serve's data directory")
		})
		t.Run("output_play/"+tc.name, func(t *testing.T) {
			c := newPlayCase(t, goBarPlayer+keys)
			goBarDSD(t, c.trees)
			stateDir := t.TempDir()
			seedDSDState(t, stateDir, state)
			wired, quit := playWired(t, c, "--state-dir", stateDir)
			refusesDSDOn(t, wired, tc.reason)
			quit()
			left(t, stateDir, "output play's --state-dir")
			if _, there := dsdStateIn(t, c.dataDir); there {
				t.Error("output play wrote a DSD state in serve's data directory")
			}
		})
	}
}
