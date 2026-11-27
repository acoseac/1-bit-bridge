package dsdstate

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A legacy dsdQualification the migration cannot take as a record's name
// (validName: more than 4096 bytes, not UTF-8, or holding a NUL; B1042's round
// 2). bridge.yaml's reader takes such a value, and 0.2.3 kept its explicit
// opt-out whatever the name, so the start step migrates the other keys without
// it: the marker holds no name and keeps the mode and allowDoP, a migrated off
// keeps its migrated_off warning, and the step says it refused the name
// (Step.RecordNameRefused) for its caller's line. The keys bridge.yaml holds at
// a later start are checked only while no migration ran, so a name the
// migration cannot take never blocks the part of a DAC's a saved marker waits
// for.

// unmigratableName is a record name one byte past the bound validName holds a
// name to.
func unmigratableName() string { return strings.Repeat("x", maxNameBytes+1) }

// TestARecordNameTheMigrationCannotTakeKeepsTheOptOut (B1042 round 2): dsdMode
// off, dop with allowDoP false or unset, and auto with allowDoP false or unset,
// each beside a record name the migration cannot take (4097 bytes, or a NUL),
// migrate as they would with no name: the file, read again, holds the marker
// with the keys' mode and allowDoP and no name, off with its migrated_off
// warning, and auto's part of a DAC's waiting; a name alone, which leaves no
// key to migrate, writes nothing and is said all the same.
func TestARecordNameTheMigrationCannotTakeKeepsTheOptOut(t *testing.T) {
	durable(t)
	for _, tc := range []struct {
		name          string
		legacy        Legacy
		mode, waiting string
		off           bool
	}{
		{"dsdMode off", Legacy{DSDMode: ModeOff, DSDQualification: unmigratableName()}, ModeOff, "", true},
		{"dsdMode off, a NUL in the name", Legacy{DSDMode: ModeOff, DSDQualification: "approved\x00.json"}, ModeOff, "", true},
		{"dop with allowDoP false", Legacy{DSDMode: ModeDoP, AllowDoP: ptr(false), DSDQualification: unmigratableName()}, ModeOff, "", true},
		{"dop with allowDoP unset", Legacy{DSDMode: ModeDoP, DSDQualification: unmigratableName()}, ModeOff, "", true},
		{"auto with allowDoP false", Legacy{DSDMode: ModeAuto, AllowDoP: ptr(false), DSDQualification: unmigratableName()}, ModeAuto, WaitingForBoundDAC, false},
		{"auto with allowDoP unset", Legacy{DSDMode: ModeAuto, DSDQualification: unmigratableName()}, ModeAuto, WaitingForBoundDAC, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			step, err := openAt(t, dir, options{}).Migrate(dir, tc.legacy, nil)
			if err != nil || !step.Changed || !step.RecordNameRefused {
				t.Fatalf("the start step: changed %v, name refused %v, %v; want the other keys migrated and the name refused", step.Changed, step.RecordNameRefused, err)
			}
			st := openAt(t, dir, options{}).State()
			want := Marker{From: FromKeys, DSDMode: tc.legacy.DSDMode, AllowDoP: tc.legacy.AllowDoP, WaitingFor: tc.waiting}
			if st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) || st.DefaultMode != tc.mode {
				t.Fatalf("read again the state holds defaultMode %q and the marker %+v, want %q and %+v", st.DefaultMode, st.MigratedFrom, tc.mode, want)
			}
			warned := slices.ContainsFunc(st.Warnings, func(w Warning) bool { return w.Code == WarningMigratedOff && w.DAC == nil })
			if warned != tc.off || len(st.Warnings) != map[bool]int{false: 0, true: 1}[tc.off] {
				t.Errorf("the warnings %+v, want migrated_off alone for an opt-out and none otherwise", st.Warnings)
			}
		})
	}
	t.Run("a name alone", func(t *testing.T) {
		dir := t.TempDir()
		step, err := openAt(t, dir, options{}).Migrate(dir, Legacy{DSDQualification: unmigratableName()}, nil)
		if err != nil || step.Changed || !step.RecordNameRefused {
			t.Fatalf("a name alone: changed %v, name refused %v, %v; want nothing to migrate, the name refused", step.Changed, step.RecordNameRefused, err)
		}
		noFile(t, dir)
	})
}

// TestAPartOfADACsAfterARefusedNameReadsNoRecord (B1042 round 2): native beside
// a record name the migration cannot take waits for its DAC with no name, and
// its part reads no record (RecordToRead asks none by a name it cannot take):
// the fallback, native's choice and migration_record_missing, as for no record
// named.
func TestAPartOfADACsAfterARefusedNameReadsNoRecord(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	l := Legacy{DSDMode: ModeNative, DSDQualification: unmigratableName()}
	s := openAt(t, dir, options{})
	if _, err := s.Migrate(dir, l, nil); err != nil {
		t.Fatal(err)
	}
	if name, ask := RecordToRead(s.State(), l); !ask || name != "" {
		t.Fatalf("RecordToRead after the refused name = %d bytes, %v; want no name, asked", len(name), ask)
	}
	if name, ask := RecordToRead(Defaults(), l); !ask || name != "" {
		t.Fatalf("RecordToRead before any migration = %d bytes, %v; want no name, asked", len(name), ask)
	}
	step, err := s.Migrate(dir, l, bound(t))
	if err != nil || !step.Changed {
		t.Fatalf("the DAC's part: changed %v, %v", step.Changed, err)
	}
	st := fileState(t, dir)
	d, _ := st.DAC(goBar(t), iFi)
	if d.Choice == nil || d.Choice.Mode != ModeNative || d.Choice.SetBy != ByMigration || len(st.Warnings) != 1 || st.Warnings[0].Code != WarningMigrationRecordMissing {
		t.Errorf("the DAC's part left %+v, warnings %+v; want native's fallback and migration_record_missing", d, st.Warnings)
	}
}

// TestASavedMarkerWaitingForItsDACIsMadeWhateverTheKeysSayNow (B1042 round 2;
// the review of the brief): a start that saved its marker, the part of a DAC's
// waiting with a record name that reads, and a later start whose bridge.yaml
// now holds a name the migration cannot take: binding the DAC makes the saved
// migration, by the name the marker keeps, its record's rows and digest; the
// keys bridge.yaml holds now are not asked.
func TestASavedMarkerWaitingForItsDACIsMadeWhateverTheKeysSayNow(t *testing.T) {
	durable(t)
	recordDir, rec := recordFixture(t, recordSpec{})
	dir := t.TempDir()
	if _, err := openAt(t, dir, options{}).Migrate(recordDir, Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, nil); err != nil {
		t.Fatal(err)
	}
	if m := fileState(t, dir).MigratedFrom; m == nil || m.WaitingFor != WaitingForBoundDAC {
		t.Fatalf("the first start left %+v, want the part of a DAC's waiting", m)
	}
	next := openAt(t, dir, options{})
	step, err := next.Migrate(recordDir, Legacy{DSDMode: ModeNative, DSDQualification: unmigratableName()}, bound(t))
	if err != nil || !step.Changed {
		t.Fatalf("the saved part, the keys now holding a name the migration cannot take: changed %v, %v", step.Changed, err)
	}
	st := fileState(t, dir)
	if m := st.MigratedFrom; m == nil || m.WaitingFor != "" || m.RecordSHA256 != rec.SHA256 || m.DSDQualification != "approved.json" {
		t.Fatalf("the marker %+v, want the saved record taken, by the name the marker kept", st.MigratedFrom)
	}
	if d, _ := st.DAC(goBar(t), iFi); d.Choice == nil || d.Choice.Mode != ModeNative || len(d.Listener) != 1 {
		t.Errorf("the GO bar holds %+v, want the record's rows", d)
	}
}
