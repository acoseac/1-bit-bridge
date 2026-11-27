package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// A legacy dsdQualification the migration cannot take as a record's name
// (B1042's round 2), through serve's player and bridge output play: bridge.yaml's
// reader takes a 4097-byte dsdQualification, and 0.2.3 kept DSD off, or DoP
// out, whatever its record's name. The start migrates the other keys without
// the name, says it refused the name without quoting it, and a saved
// migration waiting for its DAC is made whatever name bridge.yaml holds by the
// time the DAC is bound.

// unmigratableName is a dsdQualification one byte past the 4096 bytes a record
// name may have.
func unmigratableName() string { return strings.Repeat("x", 4097) }

// refusedNameLine is the start's line saying it refused the record's name.
const refusedNameLine = "player: the legacy dsdQualification is no name an owner's record can have"

// refusesDSDOn fails t unless the effective policy a route wired, read by a
// backend built on it, refuses the GO bar's DSD as reason at every rate before
// the DAC is opened: nothing published or converted, and a DSD start refused
// with no open.
func refusesDSDOn(t *testing.T, wired playback.Config, reason engine.Reason) {
	t.Helper()
	opens := &dopOpens{}
	content := dopContents{"dsd": playbacktest.DSF(dsdstate.DSD64, 0.1, func(ch, i int) byte { return byte(i + ch) })}
	b := policyBackendOf(t, wired, content, opens)
	if p := b.Policy(); len(p.Tuples) != 0 || len(p.Converted) != 0 || p.Reason != string(reason) {
		t.Fatalf("the effective policy the route wired publishes %s(converted %+v, reason %q), want nothing playing for %s", ratesPublished(p), p.Converted, p.Reason, reason)
	}
	err := startOn(t, b, 1, "dsd")
	native, pcm := opens.counts()
	if unplayable(err) != reason || len(native) != 0 || len(pcm) != 0 {
		t.Fatalf("a DSD start: %v (refused %q), %d DSD and %d PCM opens; want %s and none", err, unplayable(err), len(native), len(pcm), reason)
	}
}

// wantNameRefusedLine fails t unless the start said it refused the record's
// name, in a line that does not quote it.
func wantNameRefusedLine(t *testing.T, log *logWatch) {
	t.Helper()
	if line := log.await(t, refusedNameLine); strings.Contains(line, strings.Repeat("x", 64)) {
		t.Errorf("the start's line quotes the refused name: %.200s…", line)
	}
}

// TestARecordNameTheMigrationCannotTakeKeepsDSDOff (B1042 round 2): dsdMode
// off, and dop with allowDoP false or unset, beside a 4097-byte
// dsdQualification: serve's player and bridge output play each refuse the GO
// bar's DSD as dsd_off_by_choice before any open, the start saying it refused
// the name; serve's file, read again, holds the marker with the keys' mode and
// allowDoP and no name, defaultMode off and migrated_off, and output play's
// store the same.
func TestARecordNameTheMigrationCannotTakeKeepsDSDOff(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	no := false
	for _, tc := range []struct {
		name, keys string
		marker     dsdstate.Marker
	}{
		{"dsdMode off", "    dsdMode: off\n", dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "off"}},
		{"dop with allowDoP false", "    dsdMode: dop\n    allowDoP: false\n", dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "dop", AllowDoP: &no}},
		{"dop with allowDoP unset", "    dsdMode: dop\n", dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "dop"}},
	} {
		keys := tc.keys + "    dsdQualification: " + unmigratableName() + "\n"
		wantOff := func(t *testing.T, st dsdstate.State, where string) {
			t.Helper()
			if st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, tc.marker) {
				t.Fatalf("%s holds defaultMode %q and the marker %+v, want off and %+v", where, st.DefaultMode, st.MigratedFrom, tc.marker)
			}
			if len(st.Warnings) != 1 || st.Warnings[0].Code != dsdstate.WarningMigratedOff {
				t.Errorf("%s holds the warnings %+v, want migrated_off alone", where, st.Warnings)
			}
		}
		t.Run("serve/"+tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDSD(t, box.trees)
			log := watchLog(t)
			ps := bootDSD(t, box, keys, nil)
			availableOutput(t, ps.runningEngine(t))
			refusesDSDOn(t, wiredPlayback(t, ps.standIn), "dsd_off_by_choice")
			wantNameRefusedLine(t, log)
			stopped(t, ps)
			st, there := dsdStateIn(t, ps.dataDir)
			if !there {
				t.Fatal("serve's player persisted no DSD state")
			}
			wantOff(t, st, "the file, read again,")
		})
		t.Run("output_play/"+tc.name, func(t *testing.T) {
			c := newPlayCase(t, goBarPlayer+keys)
			goBarDSD(t, c.trees)
			log := watchLog(t)
			wired, quit := playWired(t, c)
			defer quit()
			refusesDSDOn(t, wired, "dsd_off_by_choice")
			wantNameRefusedLine(t, log)
			wantOff(t, wiredPolicyOf(t, wired).Store.State(), "output play's state")
		})
	}
}

// TestARecordNameTheMigrationCannotTakeKeepsDoPOut (B1042 round 2; B1043, the
// review of the brief): auto with allowDoP false or unset beside a 4097-byte
// dsdQualification keeps its marker, so on a GO bar a profile knows, which
// heard DoP from it at DSD64 and DSD128 and offers a DoP carrier and no native
// DSD, every rate converts on both routes; with allowDoP true, the control,
// DoP plays at both.
func TestARecordNameTheMigrationCannotTakeKeepsDoPOut(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct {
		name, keys, want string
	}{
		{"allowDoP false", "    dsdMode: auto\n    allowDoP: false\n", `2822400/"converted" 5644800/"converted" 11289600/"converted" `},
		{"allowDoP unset", "    dsdMode: auto\n", `2822400/"converted" 5644800/"converted" 11289600/"converted" `},
		{"allowDoP true", "    dsdMode: auto\n    allowDoP: true\n", `2822400/"dop" 5644800/"dop" 11289600/"converted" `},
	} {
		keys := tc.keys + "    dsdQualification: " + unmigratableName() + "\n"
		t.Run("serve/"+tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDoPOnly(t, box.trees)
			ps := bootDSD(t, box, keys, nil)
			availableOutput(t, ps.runningEngine(t))
			if got := planned(policyBackendOf(t, wiredPlayback(t, ps.standIn), dopContents{}, &dopOpens{}, knownGoBar).Policy()); got != tc.want {
				t.Errorf("serve's player plays %s, want %s", got, tc.want)
			}
			stopped(t, ps)
		})
		t.Run("output_play/"+tc.name, func(t *testing.T) {
			c := newPlayCase(t, goBarPlayer+keys)
			goBarDoPOnly(t, c.trees)
			wired, quit := playWired(t, c)
			defer quit()
			if got := planned(policyBackendOf(t, wired, dopContents{}, &dopOpens{}, knownGoBar).Policy()); got != tc.want {
				t.Errorf("output play plays %s, want %s", got, tc.want)
			}
		})
	}
}

// TestASavedMigrationWaitingForItsDACIsMadeAfterANameTheMigrationCannotTake
// (B1042 round 2; the review of the brief): a start with dsdMode native and a
// record name that reads, the GO bar unplugged, saves its marker with the part
// of a DAC's waiting; the next start's bridge.yaml holds a 4097-byte
// dsdQualification in that name's place, and the GO bar is plugged in: the
// saved part is made, by the name the marker kept, with the record's rows and
// digest.
func TestASavedMigrationWaitingForItsDACIsMadeAfterANameTheMigrationCannotTake(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	var record qualification.Approval
	id := goBarLiveIdentity(t, box.trees)
	box.unplug(t)
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		record = legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
	})
	awaitEngine(t, ps.runningEngine(t), "the output known and missing", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
	stopped(t, ps)
	if st, _ := dsdStateIn(t, ps.dataDir); st.MigratedFrom == nil || st.MigratedFrom.WaitingFor != dsdstate.WaitingForBoundDAC {
		t.Fatalf("the first start left %+v, want its part for a DAC waiting", st.MigratedFrom)
	}
	box.plug(t, "3", "iFi")
	ps2 := rebootDSD(t, ps, box, "    dsdMode: native\n    dsdQualification: "+unmigratableName()+"\n")
	availableOutput(t, ps2.runningEngine(t))
	stopped(t, ps2)
	st, _ := dsdStateIn(t, ps.dataDir)
	want := dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "native", DSDQualification: "approved.json", RecordSHA256: record.SHA256}
	if st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) {
		t.Fatalf("the later start left the marker %+v, want the saved migration made, %+v", st.MigratedFrom, want)
	}
	if d, _ := st.DAC(goBarKey(t), goBarStamp()); d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) {
		t.Errorf("the GO bar holds %+v, want the record's native with DSD64 acknowledged", d)
	}
}
