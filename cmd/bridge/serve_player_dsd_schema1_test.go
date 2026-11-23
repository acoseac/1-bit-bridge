package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// A box that ran a main build may hold a schema-1 state, whose DAC entries
// named no maker (B883): serve holds none of them, and an operator who moves
// the file aside has the next start migrate the legacy keys afresh, under the
// bound unit's stamp.

// goBarSchemaOne is a schema-1 state an earlier build wrote on the GO bar's
// box: native chosen, its first use acknowledged at DSD64 and its answer
// clean there, under the record whose digest the marker keeps.
func goBarSchemaOne(recordSHA256 string) string {
	return `{"schemaVersion":1,"defaultMode":"auto","dacs":[{"key":{"vendorId":"20b1","productId":"3009","interface":0,"product":"GO bar"},` +
		`"choice":{"mode":"native","maxBitRate":0,"revision":1,"setAt":"2026-11-05T10:00:00Z","setBy":"migration"},"dopConfirmed":null,` +
		`"firstUse":[{"transport":"native","bcdDevice":"0123","bitRates":[2822400],"acknowledgedAt":"2026-11-05T10:00:00Z"}],` +
		`"listener":[{"transport":"native","bitRate":2822400,"bcdDevice":"0123","answer":"clean"}],"evidence":[]}],` +
		`"migratedFrom":{"from":"keys","dsdMode":"native","dsdQualification":"approved.json","recordSHA256":"` + recordSHA256 + `"},"warnings":[]}` + "\n"
}

// TestAStateMovedAsideMigratesTheBoundUnitAfresh: serve over a schema-1 state
// holds none of its entries, so the GO bar plays what the default auto plays
// on it, native at every rate since B990, with none of the entry's answers
// read (DSD64 not listener confirmed), its record unread, and the file is
// left as it was. With the file moved aside, the next start migrates the
// legacy keys afresh for the GO bar, under its own stamp: where its owner's
// record is still in the data directory, its rows, DSD64 listener confirmed
// again; where it is not (a restore's destination that never held it), the
// fallback's native alone, which plays native at every rate with nothing
// acknowledged.
func TestAStateMovedAsideMigratesTheBoundUnitAfresh(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	const keys = "    dsdMode: native\n    dsdQualification: approved.json\n"
	for _, kept := range []bool{true, false} {
		name := map[bool]string{true: "the record still on the box", false: "the record gone"}[kept]
		t.Run(name, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDSD(t, box.trees)
			id := goBarLiveIdentity(t, box.trees)
			var record qualification.Approval
			var file string
			ps := bootDSD(t, box, keys, func(dataDir string) {
				record = legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
				file = goBarSchemaOne(record.SHA256)
				if err := os.WriteFile(filepath.Join(dataDir, dsdstate.FileName), []byte(file), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			availableOutput(t, ps.runningEngine(t))
			pol := policyBackendOf(t, wiredPlayback(t, ps.standIn), dopContents{}, &dopOpens{}).Policy()
			stopped(t, ps)
			if got, rung := ratesPublished(pol), firstRung(pol); got != everyRateNative || rung == "listener_confirmed" {
				t.Errorf("over the schema-1 state the set publishes %s, DSD64 at %q; want native at every rate, none of the entry's answers read", got, rung)
			}
			statePath := filepath.Join(ps.dataDir, dsdstate.FileName)
			if now := readBytes(t, statePath); string(now) != file {
				t.Fatalf("the start over the schema-1 state wrote it:\n%s", now)
			}

			// The operator's way back before the write route: the file moved
			// aside, the record kept or not.
			if err := os.Rename(statePath, statePath+".moved"); err != nil {
				t.Fatal(err)
			}
			if !kept {
				if err := os.Remove(filepath.Join(ps.dataDir, "approved.json")); err != nil {
					t.Fatal(err)
				}
			}
			ps2 := rebootDSD(t, ps, box, keys)
			availableOutput(t, ps2.runningEngine(t))
			pol2 := policyBackendOf(t, wiredPlayback(t, ps2.standIn), dopContents{}, &dopOpens{}).Policy()
			stopped(t, ps2)
			st, _ := dsdStateIn(t, ps.dataDir)
			d, ok := st.DAC(goBarKey(t), goBarStamp())
			if !ok || d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || d.Choice.SetBy != dsdstate.ByMigration {
				t.Fatalf("the fresh migration left the GO bar's entry %+v (%v), want the migration's native", d, ok)
			}
			if kept {
				if len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) || st.MigratedFrom.RecordSHA256 != record.SHA256 {
					t.Errorf("with its record kept the GO bar holds %+v, marker %+v: want the record's rows", d, st.MigratedFrom)
				}
				if got, rung := ratesPublished(pol2), firstRung(pol2); got != everyRateNative || rung != "listener_confirmed" {
					t.Errorf("with its record kept the set publishes %s, DSD64 at %q; want native at every rate, DSD64 listener confirmed", got, rung)
				}
				return
			}
			if len(d.FirstUse) != 0 || d.DoPConfirmed != nil || len(d.Listener) != 0 {
				t.Errorf("with its record gone the GO bar holds %+v, want the fallback's choice alone", d)
			}
			if got := ratesPublished(pol2); got != everyRateNative || pol2.Reason != "" {
				t.Errorf("with its record gone the set publishes %s (reason %q), want native at every rate", got, pol2.Reason)
			}
		})
	}
}
