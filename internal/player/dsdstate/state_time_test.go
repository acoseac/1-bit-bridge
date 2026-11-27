package dsdstate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A time the state's reader takes and its writer cannot write (B1045's round
// 3): encoding/json reads an RFC 3339 time whose offset is 24 hours or more
// (+24:00, -24:00, +23:60) and refuses to write one, so a state read from a
// file holding such a time could never be written again, and the migration's
// start step over it, which keeps 0.2.3's dsdMode: off, failed with an error
// no hold takes: the run played DSD under auto. The reader refuses such a
// time (writable), so the file is damaged: kept aside and replaced by the
// state that refuses DSD.

// timeFields are the times a state's file holds, each held once by schemaTwo
// and by schemaOne: the text that holds it there, the field a refusal names
// (the GO bar is dacs[0] in both, the Hugo dacs[1]), and the time as it reads.
var timeFields = []struct {
	name, text, field string
	of                func(State) time.Time
}{
	{"choice.setAt", `"setAt": "2026-11-05T10:00:00Z"`, "dacs[0].choice.setAt", func(s State) time.Time { return s.DACs[0].Choice.SetAt }},
	{"dopConfirmed.confirmedAt", `"confirmedAt": "2026-11-05T10:00:00Z"`, "dacs[1].dopConfirmed.confirmedAt", func(s State) time.Time { return s.DACs[1].DoPConfirmed.ConfirmedAt }},
	{"firstUse.acknowledgedAt", `"acknowledgedAt": "2026-11-05T10:00:00Z"`, "dacs[0].firstUse[0].acknowledgedAt", func(s State) time.Time { return s.DACs[0].FirstUse[0].AcknowledgedAt }},
	{"firstUse.observedAt", `"observedAt": "2026-11-05T10:01:00Z"`, "dacs[0].firstUse[0].observedAt", func(s State) time.Time { return s.DACs[0].FirstUse[0].ObservedAt }},
	{"evidence.driverAcceptedAt", `"driverAcceptedAt": "2026-11-05T10:00:00Z"`, "dacs[0].evidence[0].driverAcceptedAt", func(s State) time.Time { return s.DACs[0].Evidence[0].DriverAcceptedAt }},
	{"evidence.streamingObservedAt", `"streamingObservedAt": "2026-11-05T10:01:00Z"`, "dacs[0].evidence[0].streamingObservedAt", func(s State) time.Time { return s.DACs[0].Evidence[0].StreamingObservedAt }},
	{"warnings.at", `"at": "2026-11-05T10:00:00Z"`, "warnings[0]: at", func(s State) time.Time { return s.Warnings[0].At }},
}

// withOffset is text, a time's field ending in "Z", with offset in the Z's
// place.
func withOffset(text, offset string) string {
	return strings.TrimSuffix(text, `Z"`) + offset + `"`
}

// atOffset is laidOut, a file's text, with field's time at offset; it fails
// t unless laidOut holds the field's text once.
func atOffset(t *testing.T, laidOut, text, offset string) string {
	t.Helper()
	if n := strings.Count(laidOut, text); n != 1 {
		t.Fatalf("the file holds %s %d times, want once", text, n)
	}
	return strings.Replace(laidOut, text, withOffset(text, offset), 1)
}

// otherDACsChoiceAt is a markerless state of auto whose one entry, the
// Hugo's, holds a choice set at 09:00 with offset: no migration ran over it.
func otherDACsChoiceAt(offset string) string {
	return `{"schemaVersion":2,"defaultMode":"auto","dacs":[{"key":{"vendorId":"262a","productId":"9302","interface":null,"product":null},"stamp":"` + string(chord) + `",` +
		`"choice":{"mode":"native","maxBitRate":0,"revision":3,"setAt":"2026-11-27T09:00:00` + offset + `","setBy":"app"},"dopConfirmed":null,"firstUse":[],"listener":[],"evidence":[]}],` +
		`"migratedFrom":null,"warnings":[]}` + "\n"
}

// TestTheReaderRefusesATimeItsWriterCannotWrite (B1045's round 3): Decode
// refuses a file holding, at any of its times, one whose offset is 24 hours
// or more, which encoding/json reads and will not write, in schema 2 and in
// schema 1, whose DAC entries fromSchema1 drops: checked before the
// conversion. The refusal names the field and quotes no value.
func TestTheReaderRefusesATimeItsWriterCannotWrite(t *testing.T) {
	for _, schema := range []struct{ name, text string }{{"schema 2", schemaTwo}, {"schema 1", schemaOne}} {
		for _, f := range timeFields {
			for _, offset := range []string{"+24:00", "-24:00", "+23:60"} {
				t.Run(schema.name+"/"+f.name+"/"+offset, func(t *testing.T) {
					_, err := Decode([]byte(atOffset(t, schema.text, f.text, offset)))
					want := f.field + " is a time the state's writer cannot write"
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("Decode = %v, want a refusal saying %q", err, want)
					}
					if strings.Contains(err.Error(), offset) || strings.Contains(err.Error(), "2026") {
						t.Errorf("the refusal %q quotes the value", err)
					}
				})
			}
		}
	}
}

// TestTheReaderTakesATimeItsWriterWritesWithItsOffset (B1045's round 3, the
// control): a time at +23:59, an offset encoding/json writes, reads at every
// field with its offset kept, and the state read writes again as the bytes it
// was read from, the offset with it; in schema 1 the file takes its
// conversion as before, schema_1 and none of its DAC entries.
func TestTheReaderTakesATimeItsWriterWritesWithItsOffset(t *testing.T) {
	const offset, seconds = "+23:59", 23*3600 + 59*60
	for _, f := range timeFields {
		t.Run("schema 2/"+f.name, func(t *testing.T) {
			laidOut := atOffset(t, schemaTwo, f.text, offset)
			s, err := Decode([]byte(laidOut))
			if err != nil {
				t.Fatalf("Decode of a time at %s = %v, want it taken", offset, err)
			}
			if _, off := f.of(s).Zone(); off != seconds {
				t.Fatalf("the time reads at an offset of %d s, want %s's %d", off, offset, seconds)
			}
			out, err := s.encode()
			if err != nil || string(out) != asWritten(t, laidOut) {
				t.Fatalf("the state read writes again as\n%s\n%v; want the bytes it was read from\n%s", out, err, asWritten(t, laidOut))
			}
		})
		t.Run("schema 1/"+f.name, func(t *testing.T) {
			s, err := Decode([]byte(atOffset(t, schemaOne, f.text, offset)))
			if want := (State{DefaultMode: ModeAuto, MigratedFrom: &Marker{From: FromSchema1}}); err != nil || !reflect.DeepEqual(s, want) {
				t.Fatalf("schema 1 with a time at %s reads as %s, %v; want its conversion %s", offset, show(t, s), err, show(t, want))
			}
		})
	}
}

// TestTheReaderTakesAStateWithItsOptionalTimesLeftOut (B1045's round 3): a
// first use's observedAt and an evidence entry's driverAcceptedAt and
// streamingObservedAt, which a file leaves out until they are observed, read
// as the zero time, and the state writes again without them, as it was read.
func TestTheReaderTakesAStateWithItsOptionalTimesLeftOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cuts  []string
		zeros func(State) []time.Time
	}{
		{"observedAt and streamingObservedAt",
			[]string{",\n          \"observedAt\": \"2026-11-05T10:01:00Z\"", ",\n          \"streamingObservedAt\": \"2026-11-05T10:01:00Z\""},
			func(s State) []time.Time {
				return []time.Time{s.DACs[0].FirstUse[0].ObservedAt, s.DACs[0].Evidence[0].StreamingObservedAt}
			}},
		{"driverAcceptedAt with its kernel",
			[]string{"\"driverAcceptedAt\": \"2026-11-05T10:00:00Z\",\n          \"kernel\": \"6.12.47-v8-16k+\",\n          "},
			func(s State) []time.Time { return []time.Time{s.DACs[0].Evidence[0].DriverAcceptedAt} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			laidOut := schemaTwo
			for _, cut := range tc.cuts {
				if n := strings.Count(laidOut, cut); n != 1 {
					t.Fatalf("schema 2 holds %q %d times, want once", cut, n)
				}
				laidOut = strings.Replace(laidOut, cut, "", 1)
			}
			s, err := Decode([]byte(laidOut))
			if err != nil {
				t.Fatalf("Decode with the times left out = %v", err)
			}
			for i, z := range tc.zeros(s) {
				if !z.IsZero() {
					t.Errorf("the time left out #%d reads as %v, want the zero time", i, z)
				}
			}
			if out, err := s.encode(); err != nil || string(out) != asWritten(t, laidOut) {
				t.Fatalf("the state read writes again as\n%s\n%v; want the bytes it was read from\n%s", out, err, asWritten(t, laidOut))
			}
		})
	}
}

// TestAStartStepOverAStateWithAnUnwritableTimeRefusesDSD (B1045's round 3):
// a markerless state of auto whose other DAC's choice was set at +24:00 is
// damaged at its Open, kept aside byte for byte and replaced by the state that
// refuses DSD, and the start step over it, 0.2.3's dsdMode: off, migrates
// nothing over that refusal. The same state with the time at +23:59, the
// control, loads, and the start step writes off and its marker of the keys,
// the other DAC's choice kept as it was, its offset with it, which the file
// read again holds.
func TestAStartStepOverAStateWithAnUnwritableTimeRefusesDSD(t *testing.T) {
	durable(t)
	set := func(offset string) time.Time {
		at, err := time.Parse(time.RFC3339, "2026-11-27T09:00:00"+offset)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	t.Run("+24:00", func(t *testing.T) {
		data := t.TempDir()
		bad := otherDACsChoiceAt("+24:00")
		if err := os.WriteFile(filepath.Join(data, FileName), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		s := openAt(t, data, options{})
		r := s.Report()
		if r.Outcome != Damaged || r.Why == nil || !strings.Contains(r.Why.Error(), "dacs[0].choice.setAt is a time the state's writer cannot write") {
			t.Fatalf("Open = %+v, want the file damaged for its choice's time", r)
		}
		if kept, err := os.ReadFile(r.Aside); err != nil || string(kept) != bad {
			t.Fatalf("the kept copy is %q, %v; want the file's bytes, byte for byte", kept, err)
		}
		if _, err := s.Migrate(data, Legacy{DSDMode: ModeOff}, nil); err != nil {
			t.Fatalf("the start step over the refusal = %v", err)
		}
		if st := s.State(); !reflect.DeepEqual(st, Refusing(t0)) {
			t.Fatalf("after the start step the store holds %s, want the refusal", show(t, st))
		}
		if st := fileState(t, data); !reflect.DeepEqual(st, Refusing(t0)) {
			t.Fatalf("the file holds %s, want the refusal", show(t, st))
		}
	})
	t.Run("+23:59", func(t *testing.T) {
		data := t.TempDir()
		if err := os.WriteFile(filepath.Join(data, FileName), []byte(otherDACsChoiceAt("+23:59")), 0o600); err != nil {
			t.Fatal(err)
		}
		s := openAt(t, data, options{})
		if r := s.Report(); r.Outcome != Loaded {
			t.Fatalf("Open = %+v, want the state loaded", r)
		}
		step, err := s.Migrate(data, Legacy{DSDMode: ModeOff}, nil)
		if err != nil || !step.Changed {
			t.Fatalf("the start step = %+v, %v; want it written", step, err)
		}
		for where, st := range map[string]State{"the store": s.State(), "the file read again": fileState(t, data)} {
			if st.DefaultMode != ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, Marker{From: FromKeys, DSDMode: ModeOff}) {
				t.Fatalf("%s holds defaultMode %q and the marker %+v, want off and the marker of the keys", where, st.DefaultMode, st.MigratedFrom)
			}
			d, ok := st.DAC(hugo(t), chord)
			if !ok || d.Choice == nil || d.Choice.Mode != ModeNative || d.Choice.Revision != 3 || d.Choice.SetBy != ByApp || !d.Choice.SetAt.Equal(set("+23:59")) {
				t.Fatalf("%s holds the other DAC as %+v, want its choice as it was", where, d)
			}
			if _, off := d.Choice.SetAt.Zone(); off != 23*3600+59*60 {
				t.Fatalf("%s holds the other DAC's choice at an offset of %d s, want +23:59's", where, off)
			}
		}
	})
}

// TestARestoreInstallsAStateWithAnUnwritableTimeAsItsBytes (B1045's round 3):
// a snapshot's state holding, at any of its times, one its writer cannot
// write, the observations a restore drops among them, does not decode, so
// ForRestore hands its bytes back as they are, and the store's next Open keeps
// them aside and refuses DSD, as the box it came from would have; the same
// snapshot with the time at +23:59, the control, is installed as a state,
// rewritten with nothing observed, the offset kept.
func TestARestoreInstallsAStateWithAnUnwritableTimeAsItsBytes(t *testing.T) {
	durable(t)
	for _, f := range timeFields {
		t.Run(f.name, func(t *testing.T) {
			snap := []byte(atOffset(t, schemaTwo, f.text, "+24:00"))
			if out, err := ForRestore(snap); err != nil || string(out) != string(snap) {
				t.Fatalf("ForRestore = %d bytes, %v; want the snapshot's %d bytes as they are", len(out), err, len(snap))
			}
			data := t.TempDir()
			if err := WriteRestored(filepath.Join(data, FileName), snap); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, data, options{})
			if r := s.Report(); r.Outcome != Damaged || !reflect.DeepEqual(s.State(), Refusing(t0)) {
				t.Fatalf("the restored bytes open as %+v, holding %s; want them damaged and DSD refused", r, show(t, s.State()))
			}
		})
	}
	t.Run("the control", func(t *testing.T) {
		out, err := ForRestore([]byte(atOffset(t, schemaTwo, timeFields[0].text, "+23:59")))
		if err != nil {
			t.Fatal(err)
		}
		s, err := Decode(out)
		if err != nil {
			t.Fatalf("what the restore installs does not decode: %v", err)
		}
		if _, off := timeFields[0].of(s).Zone(); off != 23*3600+59*60 || len(s.DACs[0].Evidence) != 0 {
			t.Fatalf("the restore installs the choice at an offset of %d s and evidence %+v; want +23:59's and nothing observed", off, s.DACs[0].Evidence)
		}
	})
}
