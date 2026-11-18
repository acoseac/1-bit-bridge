package dsdstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// The state's entries are each a model's of one maker (B883): an entry is
// keyed by its model's key and its maker's stamp, a schema-1 file, whose
// entries named no maker, is held with none of them, and nothing derives
// them again.

// TestAStampIsTheDigestOfTheManufacturerStringAsItWasRead: a stamp is "s1-"
// and the SHA-256 of the domain, a NUL and the string exactly as the readers
// returned it, here as Python's hashlib takes it, another implementation than
// the bridge's; "" is StampNone. Nothing is redacted, folded or normalized:
// two strings a byte apart, a digit, a case, an inner space, are two stamps,
// "Audio 1 Labs" and "Audio 2 Labs" among them, whose serials 1 and 2 a
// redaction would make one string. Valid takes the two forms StampOf makes
// and nothing else.
func TestAStampIsTheDigestOfTheManufacturerStringAsItWasRead(t *testing.T) {
	for m, want := range map[string]Stamp{
		"iFi":          "s1-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840d",
		"Chord":        "s1-9320b2a37f7de8baed0d59ed555e472cec98e958e92e5570e688b9135ad899c3",
		"Audio 1 Labs": "s1-c2f1da58f3060fb38a01613054586cb4d24f6e5a66aedde8bfa4405ae277eccb",
		"Audio 2 Labs": "s1-6a6fcf1eefcc559303fdd7bf0f88705901de4aaf9bd83ee2effaef9c2180bf89",
		"":             StampNone,
	} {
		if got := StampOf(m); got != want || !got.Valid() {
			t.Errorf("StampOf(%q) = %q (valid %v), want %q", m, got, got.Valid(), want)
		}
	}
	distinct := []string{"Audio 1 Labs", "Audio 2 Labs", "audio 1 labs", "Audio  1 Labs", "Audio 1 Labs.", "Audio [redacted] Labs"}
	seen := map[Stamp]string{}
	for _, m := range distinct {
		if other, ok := seen[StampOf(m)]; ok {
			t.Errorf("%q and %q share a stamp", m, other)
		}
		seen[StampOf(m)] = m
	}
	for _, s := range []Stamp{"", "s1-", "S1-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840d",
		"s1-C382419E7CB5A229735D6ED02A2113D164F6EB67AB60C5DCDF6E0EAD0C50840D", "s1-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840",
		"s1-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840d0", "s2-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840d",
		"iFi", "--", " -"} {
		if s.Valid() {
			t.Errorf("%q is taken for a stamp", s)
		}
	}
}

// TestAnEntryIsItsModelsOfOneMaker: two units that share a model's key and
// are of two makers have two entries, each read and written by its own key
// and stamp alone, through the store and its file: neither's choice, consent
// or evidence is the other's, and a stamp of none (a unit with no
// manufacturer string) reads only an entry of none. Two entries of one key
// and one stamp do not check.
func TestAnEntryIsItsModelsOfOneMaker(t *testing.T) {
	durable(t)
	a, b := StampOf("Audio 1 Labs"), StampOf("Audio 2 Labs")
	dir := t.TempDir()
	s := openAt(t, dir, options{})
	if err := s.Update(func(st *State) error {
		st.Entry(goBar(t), a).Choice = &Choice{Mode: ModeNative, Revision: 3, SetAt: t0, SetBy: ByApp}
		st.Entry(goBar(t), a).FirstUse = []FirstUse{{Transport: TransportNative, BCDDevice: "0123", AcknowledgedAt: t0}}
		st.Entry(goBar(t), StampNone).Choice = &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByConsole}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := fileState(t, dir)
	if len(st.DACs) != 2 {
		t.Fatalf("the file holds %s, want two entries of one key", show(t, st))
	}
	if mode, _, own := st.ChoiceFor(goBar(t), a); mode != ModeNative || !own {
		t.Errorf("the first maker's unit takes %q (own %v), want its own native", mode, own)
	}
	if mode, _, own := st.ChoiceFor(goBar(t), b); mode != ModeAuto || own {
		t.Errorf("the second maker's unit takes %q (own %v), want defaultMode", mode, own)
	}
	if _, ok := st.DAC(goBar(t), b); ok {
		t.Error("the second maker's unit reads an entry")
	}
	if d, ok := st.DAC(goBar(t), StampNone); !ok || d.Choice == nil || d.Choice.Mode != ModeConvert {
		t.Errorf("a unit of no manufacturer string reads %+v (%v), want the entry of none", d, ok)
	}
	if mode, _, own := st.ChoiceFor(goBar(t), StampOf("-")); mode != ModeAuto || own {
		t.Errorf("a unit whose string is a hyphen takes %q (own %v): want defaultMode, its stamp being no entry's, none's either", mode, own)
	}
	if err := s.Update(func(st *State) error {
		st.Entry(goBar(t), b).Choice = &Choice{Mode: ModeOff, Revision: 1, SetAt: t0, SetBy: ByApp}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := fileState(t, dir)
	if d, _ := after.DAC(goBar(t), a); d.Choice == nil || d.Choice.Revision != 3 || len(d.FirstUse) != 1 {
		t.Errorf("a write for the second maker's unit left the first's entry %+v", d)
	}
	twice := after.Clone()
	twice.DACs = append(twice.DACs, after.DACs[0].clone())
	if _, err := twice.encode(); err == nil {
		t.Error("two entries of one key and one stamp were written")
	}
}

// schemaOneFile is st as an earlier build wrote it, schema 1, its entries'
// stamps left out, which that schema did not have: the file's shape, every
// list [], compact, a newline after it.
func schemaOneFile(t *testing.T, st State) []byte {
	t.Helper()
	n := st.normalized()
	f := schema1JSON{SchemaVersion: schema1, DefaultMode: n.DefaultMode, DACs: []schema1DACJSON{}, MigratedFrom: n.MigratedFrom, Warnings: []warningJSON{}}
	for _, d := range n.DACs {
		f.DACs = append(f.DACs, schema1DACJSON{Key: keyOf(d.Key), Choice: d.Choice, DoPConfirmed: d.DoPConfirmed,
			FirstUse: orEmpty(d.FirstUse), Listener: orEmpty(d.Listener), Evidence: orEmpty(d.Evidence)})
	}
	for _, w := range n.Warnings {
		out := warningJSON{Code: w.Code, Reason: w.Reason, At: w.At}
		if w.DAC != nil {
			k := keyOf(*w.DAC)
			out.DAC = &k
		}
		f.Warnings = append(f.Warnings, out)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// The shapes of a schema-1 state an earlier build can leave, each with the
// state this one holds in its place.
type schemaOneCase struct {
	name string
	st   func(t *testing.T) State
	want func(t *testing.T) State
}

func schemaOneCases() []schemaOneCase {
	authorized := func(t *testing.T) DAC {
		return DAC{Key: goBar(t),
			Choice:       &Choice{Mode: ModeNative, Revision: 2, SetAt: t0, SetBy: ByApp},
			DoPConfirmed: &DoPConfirmation{BCDDevice: "0123", ConfirmedAt: t0, SetBy: ByApp},
			FirstUse: []FirstUse{{Transport: TransportNative, BCDDevice: "0123", AcknowledgedAt: t0, ObservedAt: t0},
				{Transport: TransportDoP, BCDDevice: "0123", AcknowledgedAt: t0}},
			Listener: []Answer{{Transport: TransportNative, BitRate: DSD64, BCDDevice: "0123", Answer: AnswerClean},
				{Transport: TransportDoP, BitRate: DSD64, BCDDevice: "0123", Answer: AnswerClean}},
			Evidence: []Evidence{{Transport: TransportNative, Format: "DSD_U32_BE", BitRate: DSD64, BCDDevice: "0123", StreamingObservedAt: t0}}}
	}
	done := &Marker{From: FromKeys, DSDMode: ModeNative, DSDQualification: "approved.json", RecordSHA256: "ab" + string(bytes.Repeat([]byte("cd"), 31))}
	converted := func(mode string, w ...Warning) func(t *testing.T) State {
		return func(*testing.T) State {
			return State{DefaultMode: mode, MigratedFrom: &Marker{From: FromSchema1}, Warnings: w}
		}
	}
	return []schemaOneCase{
		{"an authorized DAC under a migration done", func(t *testing.T) State {
			return State{DefaultMode: ModeAuto, DACs: []DAC{authorized(t)}, MigratedFrom: done}
		}, converted(ModeAuto)},
		{"the fallback's native and its warning", func(t *testing.T) State {
			k := goBar(t)
			return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: k, Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByMigration}}},
				MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeNative, DSDQualification: "approved.json"},
				Warnings:     []Warning{{Code: WarningMigrationRecordMissing, DAC: &k, At: t0}}}
		}, converted(ModeAuto)},
		{"the fallback's dop, refused for its record", func(t *testing.T) State {
			k := goBar(t)
			return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: k, Choice: &Choice{Mode: ModeDoP, Revision: 1, SetAt: t0, SetBy: ByMigration}}},
				MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeDoP, AllowDoP: ptr(true), DSDQualification: "approved.json"},
				Warnings:     []Warning{{Code: WarningMigrationRecordRefused, Reason: ReasonHashMismatch, DAC: &k, At: t0}}}
		}, converted(ModeAuto)},
		{"a DAC's own off", func(t *testing.T) State {
			return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: goBar(t), Choice: &Choice{Mode: ModeOff, Revision: 1, SetAt: t0, SetBy: ByApp}}}}
		}, converted(ModeAuto)},
		{"a migrated off", func(t *testing.T) State {
			return State{DefaultMode: ModeOff, DACs: []DAC{authorized(t)}, MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeOff},
				Warnings: []Warning{{Code: WarningMigratedOff, At: t0}}}
		}, converted(ModeOff, Warning{Code: WarningMigratedOff, At: t0})},
		{"a part waiting for a DAC", func(t *testing.T) State {
			return State{DefaultMode: ModeAuto, MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeNative, DSDQualification: "approved.json", WaitingFor: WaitingForBoundDAC}}
		}, converted(ModeAuto)},
		{"a record waiting for its model", func(t *testing.T) State {
			k := goBar(t)
			return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: k, Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByMigration}}},
				MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeNative, DSDQualification: "approved.json", WaitingFor: WaitingForRecordModel},
				Warnings:     []Warning{{Code: WarningMigrationRecordRefused, Reason: ReasonIdentityMismatch, DAC: &k, At: t0}}}
		}, converted(ModeAuto)},
		{"no marker, evidence alone", func(t *testing.T) State {
			return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: goBar(t), Evidence: authorized(t).Evidence}}}
		}, converted(ModeAuto)},
		{"a state that did not read, a choice written since", func(t *testing.T) State {
			st := Refusing(t0)
			st.DACs = []DAC{authorized(t)}
			st.Warnings = nil
			return st
		}, func(*testing.T) State {
			return State{DefaultMode: ModeOff, MigratedFrom: &Marker{From: FromStateUnreadable}}
		}},
		{"a state that did not read", func(*testing.T) State { return Refusing(t0) }, func(*testing.T) State { return Refusing(t0) }},
	}
}

// TestASchemaOneStateIsHeldWithNoneOfItsEntries: each shape of a schema-1
// state, read through the store from its file, is held as its conversion:
// defaultMode and the bridge's warnings as they were, no DAC entry, none of
// the warnings an entry owned, and the marker schema_1, or state_unreadable
// as it was; the open writes nothing, and a second open holds the same.
func TestASchemaOneStateIsHeldWithNoneOfItsEntries(t *testing.T) {
	durable(t)
	for _, tc := range schemaOneCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			file := schemaOneFile(t, tc.st(t))
			if err := os.WriteFile(path, file, 0o600); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, dir, options{})
			if r := s.Report(); r.Outcome != Loaded {
				t.Fatalf("the schema-1 file opens as %+v, want it read", r)
			}
			if got, want := s.State(), tc.want(t); !reflect.DeepEqual(got.normalized(), want.normalized()) {
				t.Fatalf("a schema-1 state is held as %s, want %s", show(t, got), show(t, want))
			}
			if now := mustRead(t, path); !bytes.Equal(now, file) {
				t.Errorf("the open wrote the schema-1 file:\n%s", now)
			}
			if again := openAt(t, dir, options{}).State(); !reflect.DeepEqual(again, s.State()) {
				t.Errorf("a second open holds %s, the first %s", show(t, again), show(t, s.State()))
			}
		})
	}
}

// TestAWriteOverASchemaOneFileIsMadeOnlyWhereItChangesTheState: an Update that
// leaves the converted state as it was writes nothing, so the schema-1 bytes
// stay and the next open holds the same conversion; one that changes it
// writes schema 2, which the next open reads as written.
func TestAWriteOverASchemaOneFileIsMadeOnlyWhereItChangesTheState(t *testing.T) {
	durable(t)
	tc := schemaOneCases()[0]
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	file := schemaOneFile(t, tc.st(t))
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, dir, options{})
	held := s.State()
	if err := s.Update(func(st *State) error { st.DefaultMode = ModeAuto; return nil }); err != nil {
		t.Fatal(err)
	}
	if now := mustRead(t, path); !bytes.Equal(now, file) {
		t.Fatalf("an Update that changed nothing wrote:\n%s", now)
	}
	if again := openAt(t, dir, options{}).State(); !reflect.DeepEqual(again, held) {
		t.Fatalf("the next open holds %s, want the conversion %s", show(t, again), show(t, held))
	}
	if err := s.Update(func(st *State) error {
		st.Entry(goBar(t), iFi).Choice = &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	written := fileState(t, dir)
	if !bytes.HasPrefix(mustRead(t, path), []byte(`{"schemaVersion":2,`)) || !reflect.DeepEqual(written, s.State()) {
		t.Fatalf("the write left %s, the store holds %s: want schema 2 as held", mustRead(t, path), show(t, s.State()))
	}
	if d, _ := written.DAC(goBar(t), iFi); d.Choice == nil || d.Choice.Mode != ModeConvert || d.DoPConfirmed != nil || len(d.FirstUse) != 0 || len(d.Listener) != 0 {
		t.Errorf("the GO bar's entry once written is %+v: want the write's choice and nothing of the schema-1 entry's", d)
	}
	if m := written.MigratedFrom; m == nil || *m != (Marker{From: FromSchema1}) {
		t.Errorf("the marker once written is %+v, want schema_1's", written.MigratedFrom)
	}
}

// TestAWriteOverASchemaOneFileThatFailsRestartsFromWhatTheFileHolds: a write
// that fails before it lands leaves the schema-1 bytes, which the next open
// converts as before; one that lands and whose barrier fails leaves its
// schema-2 state in the file, which the next open reads.
func TestAWriteOverASchemaOneFileThatFailsRestartsFromWhatTheFileHolds(t *testing.T) {
	durable(t)
	tc := schemaOneCases()[0]
	change := func(st *State) error {
		st.Entry(goBar(t), iFi).Choice = &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}
		return nil
	}
	t.Run("before it lands", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		file := schemaOneFile(t, tc.st(t))
		if err := os.WriteFile(path, file, 0o600); err != nil {
			t.Fatal(err)
		}
		s := openAt(t, dir, options{write: func(string, []byte) error { return errors.New("the test's disk refuses the write") }})
		held := s.State()
		if err := s.Update(change); err == nil {
			t.Fatal("the refused write was answered as made")
		}
		if now := mustRead(t, path); !bytes.Equal(now, file) {
			t.Fatalf("a write refused before it landed changed the file:\n%s", now)
		}
		if again := openAt(t, dir, options{}).State(); !reflect.DeepEqual(again, held) {
			t.Fatalf("the next open holds %s, want the conversion %s", show(t, again), show(t, held))
		}
	})
	t.Run("once it landed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := os.WriteFile(path, schemaOneFile(t, tc.st(t)), 0o600); err != nil {
			t.Fatal(err)
		}
		var landed []byte
		s := openAt(t, dir, options{write: func(p string, data []byte) error {
			if err := os.WriteFile(p, data, 0o600); err != nil {
				return err
			}
			landed = data
			return fmt.Errorf("the test's barrier fails: %w", atomicwrite.ErrLanded)
		}})
		if err := s.Update(change); !errors.Is(err, atomicwrite.ErrLanded) {
			t.Fatalf("the write = %v, want it landed and not known to be durable", err)
		}
		if err := s.Update(func(st *State) error { st.DefaultMode = ModeOff; return nil }); !errors.Is(err, ErrLandedNotHeld) {
			t.Fatalf("a write after the one that landed = %v, want ErrLandedNotHeld", err)
		}
		want, err := Decode(landed)
		if err != nil {
			t.Fatal(err)
		}
		if again := openAt(t, dir, options{}).State(); !reflect.DeepEqual(again, want) {
			t.Fatalf("the next open holds %s, want what landed %s", show(t, again), show(t, want))
		}
		if d, _ := want.DAC(goBar(t), iFi); d.Choice == nil || d.Choice.Mode != ModeConvert || d.DoPConfirmed != nil || len(d.FirstUse) != 0 {
			t.Errorf("what landed holds %+v for the GO bar, want the write's choice alone", d)
		}
	})
}

// TestARestoreInstallsTheSnapshotsConversionWhateverTheBoxHeld: a restore of
// each kind of snapshot over each kind of box state installs what the
// snapshot converts to, the box's own entries never kept: a schema-1 state of
// any marker, held as its conversion; a schema-2 state, with nothing
// observed; and bytes that do not decode, as they are, which the next load
// refuses.
func TestARestoreInstallsTheSnapshotsConversionWhateverTheBoxHeld(t *testing.T) {
	durable(t)
	cases := schemaOneCases()
	byName := func(name string) schemaOneCase {
		for _, c := range cases {
			if c.name == name {
				return c
			}
		}
		t.Fatalf("no case %q", name)
		return schemaOneCase{}
	}
	stamped := func(t *testing.T) State {
		st := byName("an authorized DAC under a migration done").st(t)
		st.DACs[0].Stamp = iFi
		return st
	}
	type snapshot struct {
		name  string
		bytes func(t *testing.T) []byte
		// want is the state the box's next open holds, nil for a refusal.
		want func(t *testing.T) *State
	}
	fromSchemaOne := func(name string) snapshot {
		c := byName(name)
		return snapshot{"schema 1, " + name, func(t *testing.T) []byte { return schemaOneFile(t, c.st(t)) },
			func(t *testing.T) *State { w := c.want(t); return &w }}
	}
	snaps := []snapshot{
		fromSchemaOne("no marker, evidence alone"),
		fromSchemaOne("a part waiting for a DAC"),
		fromSchemaOne("a record waiting for its model"),
		fromSchemaOne("an authorized DAC under a migration done"),
		fromSchemaOne("a state that did not read"),
		{"bytes that do not decode", func(*testing.T) []byte { return []byte(`{"schemaVersion": 1, "defaultMode": "au`) }, func(*testing.T) *State { return nil }},
		{"schema 2", func(t *testing.T) []byte {
			b, err := stamped(t).encode()
			if err != nil {
				t.Fatal(err)
			}
			return b
		}, func(t *testing.T) *State {
			w := stamped(t)
			w.DACs[0].Evidence, w.DACs[0].FirstUse[0].ObservedAt = nil, time.Time{}
			return &w
		}},
	}
	boxes := map[string]func(t *testing.T) []byte{
		"no state": nil,
		"a state that refuses": func(t *testing.T) []byte {
			b, err := Refusing(t0).encode()
			if err != nil {
				t.Fatal(err)
			}
			return b
		},
		"an authorized entry of its own": func(t *testing.T) []byte {
			st := stamped(t)
			st.DACs = append(st.DACs, DAC{Key: hugo(t), Stamp: chord, Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByApp}})
			b, err := st.encode()
			if err != nil {
				t.Fatal(err)
			}
			return b
		},
	}
	for _, snap := range snaps {
		for boxName, box := range boxes {
			t.Run(snap.name+" over "+boxName, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, FileName)
				if box != nil {
					if err := os.WriteFile(path, box(t), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := WriteRestored(path, snap.bytes(t)); err != nil {
					t.Fatal(err)
				}
				s := openAt(t, dir, options{})
				want := snap.want(t)
				if want == nil {
					if r := s.Report(); r.Outcome != Damaged || !s.State().Unreadable() {
						t.Fatalf("the next load of bytes that do not decode is %+v (unreadable %v), want them refused", r, s.State().Unreadable())
					}
					return
				}
				if r := s.Report(); r.Outcome != Loaded {
					t.Fatalf("the restored state opens as %+v", r)
				}
				if got := s.State(); !reflect.DeepEqual(got.normalized(), want.normalized()) {
					t.Fatalf("the restore left %s, want the snapshot's %s", show(t, got), show(t, *want))
				}
				if b := mustRead(t, path); !bytes.HasPrefix(b, []byte(`{"schemaVersion":2,`)) {
					t.Errorf("the restore installed %s, want schema 2", b)
				}
			})
		}
	}
}

// TestAMigrationGoesOnOverASchemaTwoStateAcrossARestart: a migration this
// build began goes on at a later start, from the file: a part waiting for a
// DAC, and a record waiting for its own model, file under the bound unit's
// own entry; and a migration starts over a schema-2 state with no marker, one
// that evidence alone was written into while no key was set.
func TestAMigrationGoesOnOverASchemaTwoStateAcrossARestart(t *testing.T) {
	durable(t)
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	t.Run("a part waiting for a DAC", func(t *testing.T) {
		recordDir, a := recordFixture(t, recordSpec{})
		dir := t.TempDir()
		if _, err := openAt(t, dir, options{}).Migrate(recordDir, l, nil); err != nil {
			t.Fatal(err)
		}
		if m := fileState(t, dir).MigratedFrom; m == nil || m.WaitingFor != WaitingForBoundDAC {
			t.Fatalf("the first start left the marker %+v, want its part waiting", m)
		}
		if _, err := openAt(t, dir, options{}).Migrate(recordDir, l, bound(t)); err != nil {
			t.Fatal(err)
		}
		st := fileState(t, dir)
		if d, _ := st.DAC(goBar(t), iFi); d.Choice == nil || d.Choice.Mode != ModeNative || len(d.FirstUse) != 1 || st.MigratedFrom.RecordSHA256 != a.SHA256 {
			t.Fatalf("the later start left %s, want the record's rows in the bound unit's entry", show(t, st))
		}
	})
	t.Run("a record waiting for its own model", func(t *testing.T) {
		recordDir, a := recordFixture(t, recordSpec{})
		dir := t.TempDir()
		other := *bound(t)
		other.Identity.Manufacturer, other.Stamp = "iFi Audio", StampOf("iFi Audio")
		if _, err := openAt(t, dir, options{}).Migrate(recordDir, l, &other); err != nil {
			t.Fatal(err)
		}
		if m := fileState(t, dir).MigratedFrom; m == nil || m.WaitingFor != WaitingForRecordModel {
			t.Fatalf("the first start left the marker %+v, want the record waiting for its model", m)
		}
		if _, err := openAt(t, dir, options{}).Migrate(recordDir, l, bound(t)); err != nil {
			t.Fatal(err)
		}
		st := fileState(t, dir)
		if d, _ := st.DAC(goBar(t), iFi); d.Choice == nil || d.Choice.Revision != 1 || len(d.FirstUse) != 1 || st.MigratedFrom.RecordSHA256 != a.SHA256 {
			t.Fatalf("the later start left %s, want the record's rows in its own maker's entry", show(t, st))
		}
		if d, _ := st.DAC(goBar(t), other.Stamp); d.Choice == nil || d.Choice.Revision != 1 || len(d.FirstUse) != 0 {
			t.Errorf("the first unit's entry is %+v, want its fallback as it was", d)
		}
	})
	t.Run("no marker, evidence alone", func(t *testing.T) {
		recordDir, a := recordFixture(t, recordSpec{})
		dir := t.TempDir()
		if err := openAt(t, dir, options{}).Update(func(st *State) error {
			st.Entry(goBar(t), iFi).Evidence = []Evidence{{Transport: TransportNative, Format: "DSD_U32_BE", BitRate: DSD64, BCDDevice: "0123", StreamingObservedAt: t0}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if st := fileState(t, dir); st.MigratedFrom != nil || len(st.DACs) != 1 {
			t.Fatalf("the evidence write left %s, want an entry and no marker", show(t, st))
		}
		if _, err := openAt(t, dir, options{}).Migrate(recordDir, l, bound(t)); err != nil {
			t.Fatal(err)
		}
		st := fileState(t, dir)
		d, _ := st.DAC(goBar(t), iFi)
		if m := st.MigratedFrom; m == nil || m.From != FromKeys || m.RecordSHA256 != a.SHA256 || d.Choice == nil || len(d.Evidence) != 1 {
			t.Fatalf("the migration over a state with no marker left %s, want it run, the evidence kept", show(t, st))
		}
	})
}

// TestNoMigrationRunsOverASchemaOneState: whatever the legacy keys and the
// owner's record say, a state read from schema 1 is migrated by no step, the
// bridge's part or a DAC's, and its file is not written: no record is asked.
func TestNoMigrationRunsOverASchemaOneState(t *testing.T) {
	durable(t)
	for _, tc := range schemaOneCases() {
		t.Run(tc.name, func(t *testing.T) {
			recordDir, _ := recordFixture(t, recordSpec{})
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			file := schemaOneFile(t, tc.st(t))
			if err := os.WriteFile(path, file, 0o600); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, dir, options{})
			held := s.State()
			if name, ask := RecordToRead(held, Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}); ask {
				t.Errorf("a schema-1 state asks the record %q", name)
			}
			for _, b := range []*Bound{nil, bound(t)} {
				step, err := s.Migrate(recordDir, Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, b)
				if err != nil || step.Changed {
					t.Fatalf("a migration step over a schema-1 state = %+v, %v; want none", step, err)
				}
			}
			if now := mustRead(t, path); !bytes.Equal(now, file) || !reflect.DeepEqual(s.State(), held) {
				t.Errorf("the steps changed the state %s, or the file %s", show(t, s.State()), now)
			}
		})
	}
}

// TestAnEntryWithoutAStampIsADamagedState: a schema-2 file whose entry's
// stamp is missing, null, empty or of no form StampOf makes does not read,
// whichever way it reaches the box: opened where it lies, it is kept aside
// and the state refuses DSD; restored from a snapshot, it is installed as its
// bytes, and the next open refuses it alike. No stamp is supplied for it.
func TestAnEntryWithoutAStampIsADamagedState(t *testing.T) {
	durable(t)
	good, err := State{DefaultMode: ModeAuto, DACs: []DAC{{Key: goBar(t), Stamp: iFi,
		Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByApp}}}}.encode()
	if err != nil {
		t.Fatal(err)
	}
	stamp := `"stamp":"` + string(iFi) + `",`
	if !bytes.Contains(good, []byte(stamp)) {
		t.Fatalf("the file %s holds no stamp to take away", good)
	}
	for name, with := range map[string]string{"missing": ``, "null": `"stamp":null,`, "empty": `"stamp":"",`,
		"malformed": `"stamp":"s1-` + string(bytes.Repeat([]byte("g"), 64)) + `",`, "a manufacturer string": `"stamp":"iFi",`} {
		file := bytes.Replace(good, []byte(stamp), []byte(with), 1)
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(file); err == nil {
				t.Fatalf("a file whose stamp is %s read", name)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), file, 0o600); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, dir, options{})
			if r := s.Report(); r.Outcome != Damaged || !s.State().Unreadable() {
				t.Fatalf("opened where it lies: %+v (unreadable %v), want it kept aside and DSD refused", r, s.State().Unreadable())
			}
			restored := t.TempDir()
			path := filepath.Join(restored, FileName)
			if err := WriteRestored(path, file); err != nil {
				t.Fatal(err)
			}
			if now := mustRead(t, path); !bytes.Equal(now, file) {
				t.Fatalf("the restore installed %s, want the snapshot's bytes as they are", now)
			}
			if s := openAt(t, restored, options{}); s.Report().Outcome != Damaged || !s.State().Unreadable() {
				t.Fatalf("restored and opened: %+v, want it kept aside and DSD refused", s.Report())
			}
		})
	}
}
