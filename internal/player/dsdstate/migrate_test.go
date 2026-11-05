package dsdstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// takenRecord is a record the reader took: its digest, the firmware it was
// qualified on, its date and its approved rates.
func takenRecord(native, dop []int, qualifiedOn string) Record {
	return Record{Outcome: RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: "0123", QualifiedOn: qualifiedOn, Native: native, DoP: dop}
}

func refusedRecord(reason string) Record {
	return Record{Outcome: RecordRefused, Reason: reason, Err: errors.New("refused")}
}

var missingRecord = Record{Outcome: RecordMissing}

// migrated is the choice the migration writes.
func migrated(mode string) *Choice {
	return &Choice{Mode: mode, Revision: 1, SetAt: t0, SetBy: ByMigration}
}

func clean(transport string, rate int, on string) Answer {
	return Answer{Transport: transport, BitRate: rate, BCDDevice: "0123", Answer: AnswerClean, On: on}
}

func acknowledged(transport string, rates ...int) FirstUse {
	return FirstUse{Transport: transport, BCDDevice: "0123", BitRates: rates, AcknowledgedAt: t0}
}

var confirmedByMigration = &DoPConfirmation{BCDDevice: "0123", ConfirmedAt: t0, SetBy: ByMigration}

// marker is the migration's marker for the keys, done (nothing waits) with
// the record's digest when digest is set.
func marker(l Legacy, waiting, digest string) *Marker {
	return &Marker{From: FromKeys, DSDMode: l.DSDMode, AllowDoP: l.AllowDoP, DSDQualification: l.DSDQualification, WaitingFor: waiting, RecordSHA256: digest}
}

// TestTheMigrationTable is every row of the design's section 3.6, the legacy
// keys and the owner's record for the bound DAC (the GO bar), migrated in one
// step to what the state then holds: unset and explicit off told apart by the
// raw field; defaultMode written at once; the record's rows with each
// approved rate's answer clean under the record's firmware and date, and an
// acknowledgement of the approved rates alone; the fallback's rows, each
// with its warning, owned by the DAC.
func TestTheMigrationTable(t *testing.T) {
	gb := goBar(t)
	yes, no := ptr(true), ptr(false)
	digest := strings.Repeat("ab", 32)
	warnOf := func(code, reason string) []Warning {
		return []Warning{{Code: code, Reason: reason, DAC: &gb, At: t0}}
	}
	for _, tc := range []struct {
		name   string
		legacy Legacy
		record Record
		want   State
	}{
		{name: "dsdMode unset, no key: nothing, no marker", legacy: Legacy{}, record: missingRecord, want: Defaults()},
		{name: "dsdMode unset, allowDoP set: the defaults, the marker", legacy: Legacy{AllowDoP: yes}, record: takenRecord([]int{DSD64}, nil, ""),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{AllowDoP: yes}, "", "")}},
		{name: "dsdMode unset, a record named: the defaults, the marker", legacy: Legacy{DSDQualification: "approved.json"}, record: takenRecord([]int{DSD64}, nil, ""),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDQualification: "approved.json"}, "", "")}},
		{name: "dsdMode off, written: defaultMode off, migrated_off", legacy: Legacy{DSDMode: ModeOff, DSDQualification: "approved.json"}, record: takenRecord([]int{DSD64}, nil, ""),
			want: State{DefaultMode: ModeOff, MigratedFrom: marker(Legacy{DSDMode: ModeOff, DSDQualification: "approved.json"}, "", ""),
				Warnings: []Warning{{Code: WarningMigratedOff, At: t0}}}},
		{name: "native and a record taken", legacy: Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"},
			record: takenRecord([]int{DSD64, DSD128}, []int{DSD64}, "2026-10-01"),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, "", digest),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeNative),
					FirstUse: []FirstUse{acknowledged(TransportNative, DSD64, DSD128)},
					Listener: []Answer{clean(TransportNative, DSD64, "2026-10-01"), clean(TransportNative, DSD128, "2026-10-01")}}}}},
		{name: "dop, allowDoP true and a record taken", legacy: Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"},
			record: takenRecord([]int{DSD128}, []int{DSD64}, ""),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"}, "", digest),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeDoP), DoPConfirmed: confirmedByMigration,
					FirstUse: []FirstUse{acknowledged(TransportDoP, DSD64)},
					Listener: []Answer{clean(TransportDoP, DSD64, "")}}}}},
		{name: "dop, allowDoP false: defaultMode off, migrated_off", legacy: Legacy{DSDMode: ModeDoP, AllowDoP: no, DSDQualification: "approved.json"},
			record: takenRecord(nil, []int{DSD64}, ""),
			want: State{DefaultMode: ModeOff, MigratedFrom: marker(Legacy{DSDMode: ModeDoP, AllowDoP: no, DSDQualification: "approved.json"}, "", ""),
				Warnings: []Warning{{Code: WarningMigratedOff, At: t0}}}},
		{name: "dop, allowDoP unset, as false", legacy: Legacy{DSDMode: ModeDoP, DSDQualification: "approved.json"},
			record: takenRecord(nil, []int{DSD64}, ""),
			want: State{DefaultMode: ModeOff, MigratedFrom: marker(Legacy{DSDMode: ModeDoP, DSDQualification: "approved.json"}, "", ""),
				Warnings: []Warning{{Code: WarningMigratedOff, At: t0}}}},
		{name: "auto, allowDoP true and a record taken", legacy: Legacy{DSDMode: ModeAuto, AllowDoP: yes, DSDQualification: "approved.json"},
			record: takenRecord([]int{DSD64}, []int{DSD128}, "2026-10-01"),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeAuto, AllowDoP: yes, DSDQualification: "approved.json"}, "", digest),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeAuto), DoPConfirmed: confirmedByMigration,
					FirstUse: []FirstUse{acknowledged(TransportNative, DSD64), acknowledged(TransportDoP, DSD128)},
					Listener: []Answer{clean(TransportNative, DSD64, "2026-10-01"), clean(TransportDoP, DSD128, "2026-10-01")}}}}},
		{name: "auto, allowDoP false and a record taken: DoP heard, not acknowledged", legacy: Legacy{DSDMode: ModeAuto, AllowDoP: no, DSDQualification: "approved.json"},
			record: takenRecord([]int{DSD64}, []int{DSD128}, ""),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeAuto, AllowDoP: no, DSDQualification: "approved.json"}, "", digest),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeAuto),
					FirstUse: []FirstUse{acknowledged(TransportNative, DSD64)},
					Listener: []Answer{clean(TransportNative, DSD64, ""), clean(TransportDoP, DSD128, "")}}}}},
		{name: "native and no record", legacy: Legacy{DSDMode: ModeNative}, record: missingRecord,
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeNative}, "", ""),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeNative)}}, Warnings: warnOf(WarningMigrationRecordMissing, "")}},
		{name: "native and a refused record", legacy: Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, record: refusedRecord(ReasonStaleVersion),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, "", ""),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeNative)}}, Warnings: warnOf(WarningMigrationRecordRefused, ReasonStaleVersion)}},
		{name: "dop, allowDoP true and no record", legacy: Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"}, record: missingRecord,
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"}, "", ""),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeDoP)}}, Warnings: warnOf(WarningMigrationRecordMissing, "")}},
		{name: "dop, allowDoP true and a refused record", legacy: Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"}, record: refusedRecord(ReasonHashMismatch),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeDoP, AllowDoP: yes, DSDQualification: "approved.json"}, "", ""),
				DACs: []DAC{{Key: gb, Choice: migrated(ModeDoP)}}, Warnings: warnOf(WarningMigrationRecordRefused, ReasonHashMismatch)}},
		{name: "auto and no record: the defaults, the warning", legacy: Legacy{DSDMode: ModeAuto}, record: missingRecord,
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeAuto}, "", ""), Warnings: warnOf(WarningMigrationRecordMissing, "")}},
		{name: "auto and a refused record: the defaults, the warning", legacy: Legacy{DSDMode: ModeAuto, DSDQualification: "approved.json"}, record: refusedRecord(ReasonUnreadable),
			want: State{DefaultMode: ModeAuto, MigratedFrom: marker(Legacy{DSDMode: ModeAuto, DSDQualification: "approved.json"}, "", ""),
				Warnings: warnOf(WarningMigrationRecordRefused, ReasonUnreadable)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, step, err := Migrate(Defaults(), Input{Legacy: tc.legacy, Bound: bound(t), Record: tc.record, Now: t0})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.normalized(), tc.want.normalized()) {
				t.Fatalf("Migrate =\n%s\nwant\n%s", show(t, got), show(t, tc.want))
			}
			if step.Changed != !reflect.DeepEqual(tc.want, Defaults()) {
				t.Errorf("Step.Changed = %v", step.Changed)
			}
			if !reflect.DeepEqual(step.Warnings, tc.want.Warnings) {
				t.Errorf("the step reports the warnings %+v, want %+v, each for its Warn line", step.Warnings, tc.want.Warnings)
			}
			if _, err := got.encode(); err != nil {
				t.Fatalf("the migrated state does not encode: %v", err)
			}
		})
	}
}

// show is a state as its file holds it, for a failure's message.
func show(t *testing.T, s State) string {
	t.Helper()
	b, err := s.encode()
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// TestTheBridgesPartIsAtOnceAndADACsWaits: the part of the bridge's is
// written at the first step, with no DAC bound, and the part of a DAC's
// waits for the binding to resolve to one: no DAC is written for, and the
// record is not asked, until then.
func TestTheBridgesPartIsAtOnceAndADACsWaits(t *testing.T) {
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	if name, ask := RecordToRead(Defaults(), l); !ask || name != "approved.json" {
		t.Fatalf("RecordToRead before the migration = %q, %v; want the keys' record asked", name, ask)
	}
	s, step, err := Migrate(Defaults(), Input{Legacy: l, Now: t0})
	if err != nil || !step.Changed {
		t.Fatalf("Migrate with no DAC bound = %v, %v", step, err)
	}
	if want := marker(l, WaitingForBoundDAC, ""); !reflect.DeepEqual(s.MigratedFrom, want) || len(s.DACs) != 0 || len(s.Warnings) != 0 {
		t.Fatalf("with no DAC bound the state is %s, want the marker waiting for a DAC and nothing else", show(t, s))
	}
	if name, ask := RecordToRead(s, Legacy{DSDMode: ModeOff}); !ask || name != "approved.json" {
		t.Fatalf("RecordToRead while the DAC's part waits = %q, %v; want the record the marker named, whatever the keys say now", name, ask)
	}
	s, _, err = Migrate(s, Input{Legacy: Legacy{DSDMode: ModeOff}, Bound: bound(t), Record: takenRecord([]int{DSD64}, nil, ""), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, own := s.ChoiceFor(goBar(t)); mode != ModeNative || !own || s.DefaultMode != ModeAuto {
		t.Fatalf("the bound DAC's part took %q (own %v), defaultMode %q: want the keys the migration took, native", mode, own, s.DefaultMode)
	}
	if _, _, err := Migrate(s, Input{Legacy: l, Bound: bound(t), Now: t0}); err != nil {
		t.Fatalf("a step with nothing waiting needs no record: %v", err)
	}
}

// TestAMigratedAcknowledgementListsItsRecordsRates: a migrated first use
// covers the rates the owner's record approved, and no other (the design's
// sections 2.5 and 3.6), so admission stays per rate: a record of DSD64
// alone covers DSD64 and not DSD128 or DSD256, its file lists [2822400], and a
// transport the record approved no rate of gets no acknowledgement at all
// (an empty list would read as every rate): under auto with allowDoP, a
// native-only record leaves DoP unacknowledged and unconfirmed. A write's
// acknowledgement, with no list, covers every rate; a list present and empty
// does not decode.
func TestAMigratedAcknowledgementListsItsRecordsRates(t *testing.T) {
	gb := goBar(t)
	s, _, err := Migrate(Defaults(), Input{Legacy: Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, Bound: bound(t), Record: takenRecord([]int{DSD64}, nil, ""), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.DAC(gb)
	if len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{DSD64}) {
		t.Fatalf("the migrated first use is %+v, want DSD64 alone", d.FirstUse)
	}
	for rate, want := range map[int]bool{DSD64: true, DSD128: false, DSD256: false} {
		if got := d.FirstUse[0].Covers(rate); got != want {
			t.Errorf("a DSD64 record's acknowledgement covers %d: %v, want %v", rate, got, want)
		}
	}
	b, err := s.encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"bitRates": [`+"\n"+`            2822400`+"\n") {
		t.Fatalf("the file does not list the acknowledgement's one rate:\n%s", b)
	}

	s, _, err = Migrate(Defaults(), Input{Legacy: Legacy{DSDMode: ModeAuto, AllowDoP: ptr(true), DSDQualification: "approved.json"}, Bound: bound(t), Record: takenRecord([]int{DSD64, DSD256}, nil, ""), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	d, _ = s.DAC(gb)
	if len(d.FirstUse) != 1 || d.FirstUse[0].Transport != TransportNative || d.DoPConfirmed != nil {
		t.Fatalf("a native-only record under auto with allowDoP gives %+v, confirmed %+v: want native alone acknowledged and DoP neither acknowledged nor confirmed", d.FirstUse, d.DoPConfirmed)
	}

	every := FirstUse{Transport: TransportNative, BCDDevice: "0123", AcknowledgedAt: t0}
	for _, rate := range []int{DSD64, DSD128, DSD256} {
		if !every.Covers(rate) {
			t.Errorf("a write's acknowledgement does not cover %d", rate)
		}
	}
	file := strings.Replace(string(b), `"bitRates": [`+"\n"+`            2822400`+"\n"+`          ],`, `"bitRates": [],`, 1)
	if file == string(b) {
		t.Fatalf("the fixture's list was not found to empty:\n%s", b)
	}
	if _, err := Decode([]byte(file)); err == nil || !strings.Contains(err.Error(), "bitRates is empty") {
		t.Fatalf("an empty bitRates decodes: %v", err)
	}
}

// TestAMigrationFailureIsAWarningInTheState: a record the migration could
// not carry over is a warning in the state's file, not only a line in the
// log (the design's sections 2.5 and 3.6): migration_record_refused with the
// reader's reason word for each kind of refusal, migration_record_missing
// for no record, and the bridge's migrated_off for an explicit opt-out, each
// read back from the file after a reopen, each owned as it should be, and
// each in the step's report for its Warn line.
func TestAMigrationFailureIsAWarningInTheState(t *testing.T) {
	durable(t)
	gb := goBar(t)
	for _, tc := range []struct {
		name   string
		legacy Legacy
		spec   *recordSpec
		live   func(*Bound)
		want   Warning
	}{
		{name: "stale_version", legacy: Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, spec: &recordSpec{version: 34},
			want: Warning{Code: WarningMigrationRecordRefused, Reason: ReasonStaleVersion, DAC: &gb, At: t0}},
		{name: "identity_mismatch", legacy: Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, spec: &recordSpec{},
			live: func(b *Bound) { b.Identity.Manufacturer = "iFi audio" },
			want: Warning{Code: WarningMigrationRecordRefused, Reason: ReasonIdentityMismatch, DAC: &gb, At: t0}},
		{name: "hash_mismatch", legacy: Legacy{DSDMode: ModeAuto, DSDQualification: "tampered.json"}, spec: &recordSpec{},
			want: Warning{Code: WarningMigrationRecordRefused, Reason: ReasonHashMismatch, DAC: &gb, At: t0}},
		{name: "unreadable", legacy: Legacy{DSDMode: ModeDoP, AllowDoP: ptr(true), DSDQualification: "nothing-holds.json"}, spec: &recordSpec{},
			want: Warning{Code: WarningMigrationRecordRefused, Reason: ReasonUnreadable, DAC: &gb, At: t0}},
		{name: "missing", legacy: Legacy{DSDMode: ModeNative}, want: Warning{Code: WarningMigrationRecordMissing, DAC: &gb, At: t0}},
		{name: "migrated_off", legacy: Legacy{DSDMode: ModeOff}, want: Warning{Code: WarningMigratedOff, At: t0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordDir := t.TempDir()
			if tc.spec != nil {
				recordDir, _ = recordFixture(t, *tc.spec)
				prepare(t, recordDir, tc.legacy.DSDQualification)
			}
			b := bound(t)
			if tc.live != nil {
				tc.live(b)
			}
			data := t.TempDir()
			step, err := openAt(t, data, options{}).Migrate(recordDir, tc.legacy, b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(step.Warnings, []Warning{tc.want}) {
				t.Errorf("the step reports %+v, want %+v", step.Warnings, tc.want)
			}
			reopened := openAt(t, data, options{})
			if r := reopened.Report(); r.Outcome != Loaded {
				t.Fatalf("the reopened state is %+v", r)
			}
			if got := reopened.State().Warnings; !reflect.DeepEqual(got, []Warning{tc.want}) {
				t.Fatalf("the file holds the warnings %+v, want %+v", got, tc.want)
			}
		})
	}
}

// prepare makes the record named name in dir what its case wants of a
// record fixture: a body tampered after its digest for tampered.json, a
// record naming a report that is not there for nothing-holds.json.
func prepare(t *testing.T, dir, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "approved.json"))
	if err != nil {
		t.Fatal(err)
	}
	switch name {
	case "tampered.json":
		b = []byte(strings.Replace(string(b), `"Revision":"r1"`, `"Revision":"r2"`, 1))
	case "nothing-holds.json":
		if err := os.Remove(filepath.Join(dir, "p0-backend-lifecycle.json")); err != nil {
			t.Fatal(err)
		}
	default:
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheMigrationRunsOnce: the marker keeps any migration from running over
// a state once one ran, whatever the keys say since and whoever chose since:
// a later step with other keys changes nothing, and the record is not asked
// again; a state file deleted by hand is migrated again from the keys still
// set (the design's section 3.6); keys that set nothing write no file at
// all, so a fresh install writes none until its first write, and a step over
// a state no migration ran with no key set changes nothing.
func TestTheMigrationRunsOnce(t *testing.T) {
	durable(t)
	recordDir, _ := recordFixture(t, recordSpec{})
	data := t.TempDir()
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	s := openAt(t, data, options{})
	if _, err := s.Migrate(recordDir, l, bound(t)); err != nil {
		t.Fatal(err)
	}
	// The user chooses since: convert, for the GO bar.
	if err := s.Update(func(st *State) error {
		st.entry(goBar(t)).Choice = &Choice{Mode: ModeConvert, Revision: 2, SetAt: t0, SetBy: ByApp}
		st.ClearWarningsFor(goBar(t))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	chosen, err := os.ReadFile(filepath.Join(data, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, again := range []Legacy{l, {DSDMode: ModeOff}, {DSDMode: ModeAuto, AllowDoP: ptr(true), DSDQualification: "approved.json"}} {
		if name, ask := RecordToRead(openAt(t, data, options{}).State(), again); ask {
			t.Fatalf("RecordToRead after the migration asks %q again", name)
		}
		step, err := openAt(t, data, options{}).Migrate(recordDir, again, bound(t))
		if err != nil || step.Changed {
			t.Fatalf("a second migration with keys %+v changed the state: %+v, %v", again, step, err)
		}
		if b, _ := os.ReadFile(filepath.Join(data, FileName)); string(b) != string(chosen) {
			t.Fatalf("a second migration rewrote the state the user chose since:\n%s", b)
		}
	}

	if err := os.Remove(filepath.Join(data, FileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := openAt(t, data, options{}).Migrate(recordDir, l, bound(t)); err != nil {
		t.Fatal(err)
	}
	if mode, _, own := fileState(t, data).ChoiceFor(goBar(t)); mode != ModeNative || !own {
		t.Fatalf("after the state was deleted the migration gave %q (own %v), want the keys migrated again: native", mode, own)
	}

	fresh := t.TempDir()
	if step, err := openAt(t, fresh, options{}).Migrate(recordDir, Legacy{}, bound(t)); err != nil || step.Changed {
		t.Fatalf("a migration of no key = %+v, %v", step, err)
	}
	if _, err := os.Lstat(filepath.Join(fresh, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a fresh install with no legacy key wrote a state: %v", err)
	}
	written := State{DefaultMode: ModeAuto, DACs: []DAC{{Key: brooklyn(t), Choice: &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}}}}
	if got, step, err := Migrate(written, Input{Legacy: Legacy{}, Bound: bound(t), Now: t0}); err != nil || step.Changed || !reflect.DeepEqual(got, written) {
		t.Fatalf("a step with no key over a state its first write made changed it: %+v %v", step, err)
	}
}

// TestADACBoundLaterIsTheRecordsOrTakesTheDefaults: the record's model is
// not the first DAC bound. That DAC gets the fallback's rows, as one the
// reader refused, with identity_mismatch owned by it, and the record waits
// for its own model: binding that first DAC again writes nothing; binding
// the record's model gives it the record's rows; and once the record is
// taken nothing reads it again, so a third DAC bound afterwards takes the
// defaults and is not written for.
func TestADACBoundLaterIsTheRecordsOrTakesTheDefaults(t *testing.T) {
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	first := &Bound{Key: brooklyn(t)}
	s, _, err := Migrate(Defaults(), Input{Legacy: l, Bound: first, Record: refusedRecord(ReasonIdentityMismatch), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, own := s.ChoiceFor(brooklyn(t)); mode != ModeNative || !own {
		t.Fatalf("the first DAC, another model, took %q (own %v), want the fallback's native", mode, own)
	}
	bk := brooklyn(t)
	if want := []Warning{{Code: WarningMigrationRecordRefused, Reason: ReasonIdentityMismatch, DAC: &bk, At: t0}}; !reflect.DeepEqual(s.Warnings, want) {
		t.Fatalf("the warnings are %+v, want %+v", s.Warnings, want)
	}
	if s.MigratedFrom.WaitingFor != WaitingForRecordModel {
		t.Fatalf("the marker is %+v, want the record waiting for its own model", s.MigratedFrom)
	}
	again, step, err := Migrate(s, Input{Legacy: l, Bound: first, Record: refusedRecord(ReasonIdentityMismatch), Now: t0})
	if err != nil || step.Changed || !reflect.DeepEqual(again, s) {
		t.Fatalf("binding the first DAC again changed the state: %+v, %v", step, err)
	}
	s, _, err = Migrate(s, Input{Legacy: l, Bound: bound(t), Record: takenRecord([]int{DSD64}, nil, ""), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := s.DAC(goBar(t))
	if !ok || d.Choice == nil || d.Choice.Mode != ModeNative || len(d.Listener) != 1 || len(d.FirstUse) != 1 {
		t.Fatalf("the record's model, bound later, holds %+v: want the record's rows", d)
	}
	if s.MigratedFrom.WaitingFor != "" || s.MigratedFrom.RecordSHA256 == "" {
		t.Fatalf("the marker is %+v, want the record's digest and nothing waiting", s.MigratedFrom)
	}
	if name, ask := RecordToRead(s, l); ask {
		t.Fatalf("RecordToRead once the record was taken asks %q", name)
	}
	third := &Bound{Key: hugo(t)}
	after, step, err := Migrate(s, Input{Legacy: l, Bound: third, Now: t0})
	if err != nil || step.Changed || !reflect.DeepEqual(after, s) {
		t.Fatalf("a third DAC bound once the record was taken changed the state: %+v, %v", step, err)
	}
	if mode, _, own := after.ChoiceFor(hugo(t)); mode != ModeAuto || own {
		t.Fatalf("the third DAC takes %q (own %v), want the defaults", mode, own)
	}
}

// TestTheMigrationNeverOverwritesAChoice: a DAC that has a choice of its own
// when the migration's part comes to it (a state the app or the console
// wrote before the legacy keys were set) keeps it whole, with no answer,
// acknowledgement, confirmation or warning written for it, and the part is
// done: a user's first choice, at revision 1 as the migration's fallback is,
// and a later one alike, since the writer, not the revision, tells them
// apart (ownChoice).
func TestTheMigrationNeverOverwritesAChoice(t *testing.T) {
	gb := goBar(t)
	for _, choice := range []Choice{
		{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp},
		{Mode: ModeOff, Revision: 1, SetAt: t0, SetBy: ByConsole},
		{Mode: ModeConvert, Revision: 7, SetAt: t0, SetBy: ByApp},
	} {
		own := State{DefaultMode: ModeAuto, DACs: []DAC{{Key: gb, Choice: &choice}}}
		for _, rec := range []Record{takenRecord([]int{DSD64}, []int{DSD128}, ""), missingRecord, refusedRecord(ReasonStaleVersion), refusedRecord(ReasonIdentityMismatch)} {
			got, _, err := Migrate(own, Input{Legacy: Legacy{DSDMode: ModeAuto, AllowDoP: ptr(true), DSDQualification: "approved.json"}, Bound: bound(t), Record: rec, Now: t0})
			if err != nil {
				t.Fatal(err)
			}
			if d, _ := got.DAC(gb); !reflect.DeepEqual(d, own.DACs[0]) || len(got.Warnings) != 0 {
				t.Fatalf("a record %v migrated over the DAC's own choice %+v: %s", rec.Outcome, choice, show(t, got))
			}
			if got.MigratedFrom.RecordSHA256 != "" {
				t.Fatalf("the marker says the record migrated over the DAC's choice: %+v", got.MigratedFrom)
			}
		}
	}
}

// TestAMigrationsChoiceKeepsItsWarningAndAWriteClearsIt: the choice the
// migration writes clears no warning, the one written with it included; a
// choice the app or the console writes for that DAC clears its warnings
// (ClearWarningsFor), never another DAC's or the bridge's migrated_off.
func TestAMigrationsChoiceKeepsItsWarningAndAWriteClearsIt(t *testing.T) {
	s, _, err := Migrate(Defaults(), Input{Legacy: Legacy{DSDMode: ModeNative}, Bound: bound(t), Record: missingRecord, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Warnings) != 1 || s.Warnings[0].Code != WarningMigrationRecordMissing {
		t.Fatalf("the migration's choice took its own warning away: %+v", s.Warnings)
	}
	bk := brooklyn(t)
	s.Warnings = append(s.Warnings, Warning{Code: WarningMigrationRecordMissing, DAC: &bk, At: t0}, Warning{Code: WarningMigratedOff, At: t0})
	s.ClearWarningsFor(goBar(t))
	codes := []string{}
	for _, w := range s.Warnings {
		codes = append(codes, w.Code+"@"+ownerOrder(w))
	}
	if want := []string{WarningMigrationRecordMissing + "@=" + bk.Canonical(), WarningMigratedOff + "@"}; !slices.Equal(codes, want) {
		t.Fatalf("after a write for the GO bar the warnings are %v, want %v", codes, want)
	}
}

// TestALegacyModeTheReaderNeverTakesIsRefused: the table refuses legacy keys
// the config refuses (Problem refuses an unknown dsdMode), never guessing.
func TestALegacyModeTheReaderNeverTakesIsRefused(t *testing.T) {
	if _, _, err := Migrate(Defaults(), Input{Legacy: Legacy{DSDMode: "convert"}, Now: t0}); err == nil {
		t.Fatal("a legacy dsdMode of convert, which bridge.yaml never took, was migrated")
	}
	if _, _, err := Migrate(Defaults(), Input{Legacy: Legacy{DSDMode: ModeNative}, Bound: bound(t), Now: t0}); err == nil {
		t.Fatal("a DAC's part ran with no answer of the reader's")
	}
}
