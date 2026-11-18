package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Each unit reads its own model's entry of its own maker (B883): what the
// state holds for another maker's unit of the model, and what a schema-1 file
// held for a model whose maker it never recorded, authorizes nothing.

// schemaOneFile is st as an earlier build wrote it, schema 1: each entry its
// model's key alone, which named no maker.
func schemaOneFile(t *testing.T, st dsdstate.State) []byte {
	t.Helper()
	keyOf := func(k dacprofile.Key) map[string]any {
		j := map[string]any{"vendorId": usbid.Hex16(k.VendorID()), "productId": usbid.Hex16(k.ProductID()), "interface": nil, "product": nil}
		if i, ok := k.Interface(); ok {
			j["interface"] = i
		}
		if p, ok := k.Product(); ok {
			j["product"] = p
		}
		return j
	}
	orEmpty := func(n int, v any) any {
		if n == 0 {
			return []any{}
		}
		return v
	}
	dacs := []any{}
	for _, d := range st.DACs {
		dacs = append(dacs, map[string]any{"key": keyOf(d.Key), "choice": d.Choice, "dopConfirmed": d.DoPConfirmed,
			"firstUse": orEmpty(len(d.FirstUse), d.FirstUse), "listener": orEmpty(len(d.Listener), d.Listener), "evidence": orEmpty(len(d.Evidence), d.Evidence)})
	}
	warnings := []any{}
	for _, w := range st.Warnings {
		j := map[string]any{"code": w.Code, "at": w.At}
		if w.Reason != "" {
			j["reason"] = w.Reason
		}
		if w.DAC != nil {
			j["dac"] = keyOf(*w.DAC)
		}
		warnings = append(warnings, j)
	}
	b, err := json.Marshal(map[string]any{"schemaVersion": 1, "defaultMode": st.DefaultMode, "dacs": dacs, "migratedFrom": st.MigratedFrom, "warnings": warnings})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// durableStore gives the state's real store the durable writer over a barrier
// and a medium of the test's (process-global seams: never in parallel).
func durableStore(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

// throughPersistence is what each way a schema-1 file reaches the box holds of
// it: opened where it lies; opened, written by a change that leaves the test
// DAC's entries alone, and opened again; and restored from a snapshot holding
// it, then opened.
func throughPersistence(t *testing.T, file []byte) map[string]dsdstate.State {
	t.Helper()
	out := map[string]dsdstate.State{}
	open := func(dir string) *dsdstate.Store {
		s, err := dsdstate.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if r := s.Report(); r.Outcome != dsdstate.Loaded {
			t.Fatalf("the state opens as %+v", r)
		}
		return s
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dsdstate.FileName), file, 0o600); err != nil {
		t.Fatal(err)
	}
	out["opened"] = open(dir).State()
	if err := open(dir).Update(func(st *dsdstate.State) error {
		st.Entry(otherModel(t), dsdstate.StampOf("Other")).Evidence = []dsdstate.Evidence{{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE",
			BitRate: dsdstate.DSD64, BCDDevice: bcd, StreamingObservedAt: t0}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out["written and opened again"] = open(dir).State()
	restored := t.TempDir()
	if err := dsdstate.WriteRestored(filepath.Join(restored, dsdstate.FileName), file); err != nil {
		t.Fatal(err)
	}
	out["restored"] = open(restored).State()
	return out
}

// TestNoSchemaOneEntryAuthorizesNativeOrDoP: a schema-1 file's entry for the
// bound DAC's model, its maker never recorded, authorizes neither native nor
// DoP at any rate, whichever way the file reaches the box (opened, written
// over, restored), with every live input offering both: a choice with its
// first use acknowledged, its answers clean and DoP confirmed is dropped, and
// so is a refusal, the migration's fallback pending its acknowledgement or a
// DAC's own off, which falls to the default kept: under auto, DSD converted
// where a conversion is offered and refused where none is, and under off,
// refused; a state that did not read refuses every rate as before.
func TestNoSchemaOneEntryAuthorizesNativeOrDoP(t *testing.T) {
	durableStore(t)
	key := dacKey(t)
	authorized := dsdstate.DAC{Key: key,
		Choice:       &dsdstate.Choice{Mode: dsdstate.ModeNative, Revision: 2, SetAt: t0, SetBy: dsdstate.ByApp},
		DoPConfirmed: &dsdstate.DoPConfirmation{BCDDevice: bcd, ConfirmedAt: t0, SetBy: dsdstate.ByApp},
		FirstUse: []dsdstate.FirstUse{{Transport: dsdstate.TransportNative, BCDDevice: bcd, AcknowledgedAt: t0},
			{Transport: dsdstate.TransportDoP, BCDDevice: bcd, AcknowledgedAt: t0}},
		Listener: []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: dsd64, BCDDevice: bcd, Answer: dsdstate.AnswerClean},
			{Transport: dsdstate.TransportDoP, BitRate: dsd64, BCDDevice: bcd, Answer: dsdstate.AnswerClean}}}
	fallback := func(mode string) dsdstate.DAC {
		return dsdstate.DAC{Key: key, Choice: &dsdstate.Choice{Mode: mode, Revision: 1, SetAt: t0, SetBy: dsdstate.ByMigration}}
	}
	missing := []dsdstate.Warning{{Code: dsdstate.WarningMigrationRecordMissing, DAC: &key, At: t0}}
	for _, tc := range []struct {
		name string
		st   dsdstate.State
		// refusedAlways is the word every rate is refused with whatever is
		// offered; "" is the default auto's outcome: converted where a
		// conversion is offered, conversion_not_offered where none is.
		refusedAlways engine.Reason
	}{
		{name: "an authorized choice", st: dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{authorized}}},
		{name: "the fallback's native pending its acknowledgement", st: dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{fallback(dsdstate.ModeNative)},
			MigratedFrom: &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeNative}, Warnings: missing}},
		{name: "the fallback's dop pending its acknowledgement", st: dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{fallback(dsdstate.ModeDoP)},
			MigratedFrom: &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeDoP, AllowDoP: ptr(true)}, Warnings: missing}},
		{name: "a DAC's own off", st: dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{{Key: key,
			Choice: &dsdstate.Choice{Mode: dsdstate.ModeOff, Revision: 1, SetAt: t0, SetBy: dsdstate.ByApp}}}}},
		{name: "the fallback under a migrated off", st: dsdstate.State{DefaultMode: dsdstate.ModeOff, DACs: []dsdstate.DAC{authorized},
			MigratedFrom: &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: dsdstate.ModeOff}, Warnings: []dsdstate.Warning{{Code: dsdstate.WarningMigratedOff, At: t0}}},
			refusedAlways: ReasonOffByChoice},
		{name: "a state that did not read", st: func() dsdstate.State {
			st := dsdstate.Refusing(t0)
			st.DACs = []dsdstate.DAC{authorized}
			return st
		}(), refusedAlways: ReasonStateUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for way, st := range throughPersistence(t, schemaOneFile(t, tc.st)) {
				if len(st.DACs) > 1 || len(st.DACs) == 1 && st.DACs[0].Key.Equal(key) {
					t.Fatalf("%s: the state holds the test DAC's schema-1 entry: %+v", way, st.DACs)
				}
				for offered, r := range map[bool]*rig{true: newRig(t).everything(dsdstate.ModeNative), false: newRig(t).everything(dsdstate.ModeNative)} {
					if !offered {
						r.altsets = []alsa.Altset{nativeAltset()}
					}
					s := Effective(r.inputs().WithState(st))
					for _, rate := range s.All() {
						p := rate.Plan
						switch {
						case p.Outcome == OutcomeNative || p.Outcome == OutcomeDoP:
							t.Fatalf("%s (a conversion offered: %v): %d plays %s on the schema-1 entry", way, offered, p.BitRate, p.Outcome)
						case tc.refusedAlways != "":
							if p.Outcome != OutcomeRefused || p.Reason != tc.refusedAlways {
								t.Errorf("%s (a conversion offered: %v): %d is %s %s, want refused %s", way, offered, p.BitRate, p.Outcome, p.Reason, tc.refusedAlways)
							}
						case offered && p.Outcome != OutcomeConverted:
							t.Errorf("%s: %d is %s %s, want converted under the default kept", way, p.BitRate, p.Outcome, p.Reason)
						case !offered && (p.Outcome != OutcomeRefused || p.Reason != ReasonConversionNotOffered):
							t.Errorf("%s with no conversion offered: %d is %s %s, want refused conversion_not_offered", way, p.BitRate, p.Outcome, p.Reason)
						}
					}
				}
			}
		})
	}
}

// twoMakers are a state holding, for the test DAC's model, the same choice
// and the same consents for two makers' units, each in its own entry, and the
// inputs of each unit bound: one model, firmware and live capabilities.
func twoMakers(t *testing.T) (a, b Inputs) {
	r := newRig(t).everything(dsdstate.ModeNative)
	in := r.inputs()
	first := in.State.DACs[0]
	second := first
	second.Stamp = dsdstate.StampOf("Audio 2 Labs")
	first.Stamp = dsdstate.StampOf("Audio 1 Labs")
	in.State.DACs = []dsdstate.DAC{first, second}
	a, b = in, in
	a.Stamp, b.Stamp = first.Stamp, second.Stamp
	return a, b
}

// TestAReconciliationTellsTwoMakersOfOneModelApart: two units of one model,
// key and firmware alike, independently authorized for the same plans, are
// two identities by their makers' stamps alone, so a set computed for one
// does not keep what the other's sink holds: an occurrence heard on the first
// unit, its released pause and its frames buffered in the first unit's sink
// are revoked once the set is the second's, while a successor that has never
// had a sink is kept, as a successor is under any set that plays its plan.
func TestAReconciliationTellsTwoMakersOfOneModelApart(t *testing.T) {
	ina, inb := twoMakers(t)
	a, b := Effective(ina), Effective(inb)
	for _, rate := range dsdRates {
		if pa, pb := a.At(rate).Plan, b.At(rate).Plan; pa.Outcome != OutcomeNative || !pa.Same(pb) {
			t.Fatalf("at %d the first unit's plan %+v and the second's %+v: the case needs one native plan for both", rate, pa, pb)
		}
	}
	if ia, ib := a.Identity(), b.Identity(); ia.Stamp == ib.Stamp || !ia.Key.Equal(ib.Key) || ia.BCDDevice != ib.BCDDevice {
		t.Fatalf("the identities %+v and %+v: the case needs one key and firmware, two makers", ia, ib)
	}
	opened := a.Identity()
	plan := a.At(dsd64).Plan
	for _, tc := range []struct {
		name string
		o    Occurrence
		want Action
	}{
		{"heard on the first unit", Occurrence{Plan: plan, Submitted: 1000, Written: 800, Played: 400, Audible: true, Opened: &opened}, Revoke},
		{"its pause released", Occurrence{Plan: plan, Submitted: 1000, Played: 400, Audible: true, Opened: &opened}, Revoke},
		{"buffered in the first unit's sink", Occurrence{Plan: plan, Submitted: 1000, Opened: &opened}, Revoke},
		{"a successor that never had a sink", Occurrence{Plan: plan}, Keep},
	} {
		if v := Reconcile(b, tc.o); v.Action != tc.want {
			t.Errorf("%s: %+v under the second unit's set, want %v", tc.name, v, tc.want)
		}
		if v := Reconcile(a, tc.o); v.Action != Keep {
			t.Errorf("%s: %+v under the first unit's own set, want it kept", tc.name, v)
		}
	}
}

// TestAChoiceIsTheBoundUnitsOwnEntrysAlone: a choice's write for one maker's
// unit compares the base with that unit's own revision and writes its own
// entry alone, never the entry another maker's unit of the model holds, both
// ways: the second unit's first write is at its revision 0 whatever the
// first's is, the first's base is stale for it, and the first's entry is as it
// was; an auto write's acknowledgements are read and written on the bound
// unit's entry.
func TestAChoiceIsTheBoundUnitsOwnEntrysAlone(t *testing.T) {
	ina, inb := twoMakers(t)
	ina.State.DACs = ina.State.DACs[:1]
	ina.State.DACs[0].Choice.Revision = 3
	inb.State = ina.State
	if got := StoredRevision(inb.State, inb.Unit.Key, inb.Stamp); got != 0 {
		t.Fatalf("the second unit's stored revision is %d, want 0: it has no entry", got)
	}
	if _, err := Choose(inb, Choice{BaseRevision: 3, Mode: dsdstate.ModeConvert, By: dsdstate.ByApp}, t0); err == nil {
		t.Fatal("a write for the second unit on the first's revision was taken")
	}
	st, err := Choose(inb, Choice{BaseRevision: 0, Mode: dsdstate.ModeAuto, FirstUseAcknowledged: true, By: dsdstate.ByApp}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := st.DAC(ina.Unit.Key, ina.Stamp); !ok || d.Choice.Revision != 3 || d.Choice.Mode != dsdstate.ModeNative {
		t.Errorf("the first unit's entry is %+v once the second's was written, want it as it was", d)
	}
	d, ok := st.DAC(inb.Unit.Key, inb.Stamp)
	if !ok || d.Choice == nil || d.Choice.Revision != 1 || d.Choice.Mode != dsdstate.ModeAuto || len(d.FirstUse) == 0 {
		t.Fatalf("the second unit's entry is %+v, want its own auto at revision 1, acknowledged", d)
	}
	for _, f := range d.FirstUse {
		if !f.AcknowledgedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("the second unit's acknowledgement %+v is not the write's: it was read from another entry", f)
		}
	}
}

// TestAUnitWithNoManufacturerStringReadsOnlyAnEntryOfNone: StampNone, a
// unit's stamp with no manufacturer string, is a maker like any other: an
// entry of none authorizes a unit of none, and neither a unit of a maker nor
// an entry of one is taken for the other.
func TestAUnitWithNoManufacturerStringReadsOnlyAnEntryOfNone(t *testing.T) {
	in := newRig(t).everything(dsdstate.ModeNative).inputs()
	in.State.DACs[0].Stamp, in.Stamp = dsdstate.StampNone, dsdstate.StampNone
	if p := Effective(in).At(dsd64).Plan; p.Outcome != OutcomeNative {
		t.Fatalf("a unit of none on an entry of none plays %s %s, want native", p.Outcome, p.Reason)
	}
	maker := in
	maker.Stamp = dsdstate.StampOf("Test")
	if p := Effective(maker).At(dsd64).Plan; p.Outcome == OutcomeNative || p.Outcome == OutcomeDoP {
		t.Errorf("a unit of a maker plays %s on an entry of none", p.Outcome)
	}
	none := maker
	none.State = maker.State.Clone()
	none.State.DACs[0].Stamp, none.Stamp = dsdstate.StampOf("Test"), dsdstate.StampNone
	if p := Effective(none).At(dsd64).Plan; p.Outcome == OutcomeNative || p.Outcome == OutcomeDoP {
		t.Errorf("a unit of none plays %s on an entry of a maker", p.Outcome)
	}
}
