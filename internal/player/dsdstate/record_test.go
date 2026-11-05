package dsdstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// TestTheMigrationIgnoresThePortAndTakesVersion35: the migration's read of
// the owner's record (ReadRecord, the design's section 3.5) goes through the
// legacy reader with the comparison of the model alone, so it takes a record
// of version 35, the frozen LegacyVersion, made on another port than the
// unit is at now, and one made under another firmware, whose own firmware
// goes with each answer; it takes this build's version, and refuses 34 and
// one past this build's as stale. A unit of another model, or at another PCM
// device, is the record's identity refused. Through the real reader and the
// record's real files; then through the store, whose migration writes the
// record's rows into the state file.
func TestTheMigrationIgnoresThePortAndTakesVersion35(t *testing.T) {
	if qualification.LegacyVersion != 35 {
		t.Fatalf("LegacyVersion = %d; the migration takes records from 35, frozen", qualification.LegacyVersion)
	}
	here := bound(t)
	for _, tc := range []struct {
		name    string
		spec    recordSpec
		live    func(*Bound)
		outcome RecordOutcome
		reason  string
	}{
		{name: "version_35_on_another_port", spec: recordSpec{version: qualification.LegacyVersion}, outcome: RecordTaken},
		{name: "version_35_under_another_firmware", spec: recordSpec{version: qualification.LegacyVersion},
			live: func(b *Bound) { b.Identity.BCDDevice = "0124" }, outcome: RecordTaken},
		{name: "this_builds_version", spec: recordSpec{version: qualification.Version}, outcome: RecordTaken},
		{name: "version_34", spec: recordSpec{version: qualification.LegacyVersion - 1}, outcome: RecordRefused, reason: ReasonStaleVersion},
		{name: "a_version_past_this_builds", spec: recordSpec{version: qualification.Version + 1}, outcome: RecordRefused, reason: ReasonStaleVersion},
		{name: "another_model", spec: recordSpec{}, live: func(b *Bound) { b.Identity.Product = "GO bar 2" }, outcome: RecordRefused, reason: ReasonIdentityMismatch},
		{name: "another_pcm_device", spec: recordSpec{}, live: func(b *Bound) { b.Identity.Device = 1 }, outcome: RecordRefused, reason: ReasonIdentityMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, a := recordFixture(t, tc.spec)
			b := *here
			if tc.live != nil {
				tc.live(&b)
			}
			got := ReadRecord(dir, "approved.json", b)
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("ReadRecord = %v %q (%v), want %v %q", got.Outcome, got.Reason, got.Err, tc.outcome, tc.reason)
			}
			if tc.outcome != RecordTaken {
				return
			}
			want := Record{Outcome: RecordTaken, SHA256: a.SHA256, BCDDevice: "0123", Native: []int{DSD64}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadRecord = %+v, want %+v: the record's digest, its own firmware and its approved rate", got, want)
			}
		})
	}

	t.Run("through_the_store", func(t *testing.T) {
		durable(t)
		recordDir, a := recordFixture(t, recordSpec{version: qualification.LegacyVersion})
		data := t.TempDir()
		s := openAt(t, data, options{})
		if _, err := s.Migrate(recordDir, Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}, here); err != nil {
			t.Fatal(err)
		}
		st := fileState(t, data)
		d, ok := st.DAC(goBar(t))
		if !ok || d.Choice == nil || d.Choice.Mode != ModeNative || len(d.Listener) != 1 || len(d.FirstUse) != 1 {
			t.Fatalf("the state after the migration of a version-35 record on another port holds %+v, want the record's rows", st)
		}
		if st.MigratedFrom == nil || st.MigratedFrom.RecordSHA256 != a.SHA256 || st.MigratedFrom.WaitingFor != "" {
			t.Fatalf("the marker is %+v, want the record's digest and nothing waiting", st.MigratedFrom)
		}
	})
}

// TestARefusedRecordsReasonIsItsKind: every refusal of the record gives the
// warning's reason by the kind the reader marks it with, never by its text:
// a body changed after its digest and a report's byte changed after the
// record named its digest are hash_mismatch; a record that does not exist,
// and an unset dsdQualification, are no record (migration_record_missing),
// where a report the record names that does not exist, an edge no report
// executed and a record a group may write are the record not reading
// (unreadable).
func TestARefusedRecordsReasonIsItsKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, dir string, a *qualification.Approval)
		record  string
		outcome RecordOutcome
		reason  string
	}{
		{name: "a_tampered_body", outcome: RecordRefused, reason: ReasonHashMismatch, change: func(t *testing.T, dir string, a *qualification.Approval) {
			// The body changes after its digest was taken, which stays.
			a.Body.Revision = "r2"
			if err := writeRecordAs(dir, *a); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a_changed_evidence_byte", outcome: RecordRefused, reason: ReasonHashMismatch, change: func(t *testing.T, dir string, a *qualification.Approval) {
			name := filepath.Join(dir, a.Body.Policies[0].Matrix.Path)
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte(strings.Replace(string(b), `"test-build"`, `"test-builD"`, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no_record_at_its_name", record: "elsewhere.json", outcome: RecordMissing},
		{name: "no_record_named", record: "-", outcome: RecordMissing},
		{name: "a_missing_report", outcome: RecordRefused, reason: ReasonUnreadable, change: func(t *testing.T, dir string, a *qualification.Approval) {
			if err := os.Remove(filepath.Join(dir, a.Body.Policies[0].Lifecycle.Path)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "an_unexecuted_edge", outcome: RecordRefused, reason: ReasonUnreadable, change: func(t *testing.T, dir string, a *qualification.Approval) {
			p := a.Body.Policies[0]
			a.Body.Edges = []qualification.Edge{{From: qualification.NativeKey(p), To: qualification.NativeKey(p)}}
			writeRecord(t, dir, a)
		}},
		{name: "a_record_a_group_may_write", outcome: RecordRefused, reason: ReasonUnreadable, change: func(t *testing.T, dir string, _ *qualification.Approval) {
			if err := os.Chmod(filepath.Join(dir, "approved.json"), 0o660); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, a := recordFixture(t, recordSpec{})
			if tc.change != nil {
				tc.change(t, dir, &a)
			}
			name := "approved.json"
			switch tc.record {
			case "-":
				name = ""
			case "":
			default:
				name = tc.record
			}
			got := ReadRecord(dir, name, *bound(t))
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("ReadRecord = %v %q (%v), want %v %q", got.Outcome, got.Reason, got.Err, tc.outcome, tc.reason)
			}
		})
	}
}

// writeRecordAs writes a at approved.json in dir as it is, its digest
// included.
func writeRecordAs(dir string, a qualification.Approval) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "approved.json"), data, 0o600)
}

// TestTheModelComparisonIsTheDesignsFields: the migration compares the
// model alone (the design's section 3.5): the vendor and product IDs,
// manufacturer, product, interface, device and subdevice, each of which
// tells two models apart, and neither the port nor the bcdDevice.
func TestTheModelComparisonIsTheDesignsFields(t *testing.T) {
	recorded := goBarIdentity("platform-1000120000.pcie-usb2-1", "0123")
	if !sameModel(recorded, recorded) {
		t.Fatal("a unit is not its own model")
	}
	for _, tc := range []struct {
		field string
		live  func(*qualification.Identity)
		same  bool
	}{
		{"port", func(i *qualification.Identity) { i.Port = "platform-xhci-hcd.1.auto-usb2-2" }, true},
		{"bcdDevice", func(i *qualification.Identity) { i.BCDDevice = "0124" }, true},
		{"vendor", func(i *qualification.Identity) { i.Vendor = "20b2" }, false},
		{"productId", func(i *qualification.Identity) { i.ProductID = "300a" }, false},
		{"manufacturer", func(i *qualification.Identity) { i.Manufacturer = "iFi audio" }, false},
		{"product", func(i *qualification.Identity) { i.Product = "GO bar 2" }, false},
		{"interface", func(i *qualification.Identity) { i.Interface = 2 }, false},
		{"device", func(i *qualification.Identity) { i.Device = 1 }, false},
		{"subdevice", func(i *qualification.Identity) { i.Subdevice = 1 }, false},
	} {
		live := recorded
		tc.live(&live)
		if got := sameModel(recorded, live); got != tc.same {
			t.Errorf("a unit whose %s differs is the same model: %v, want %v", tc.field, got, tc.same)
		}
	}
}

// TestTheRecordsOwnModelTakesTheRecordOverItsFallback: the model comparison
// reads fields the state's key does not hold, so a unit of the record's own
// key can be refused as another identity (Codex and the Claude API reviewer
// on #1405): bound at another PCM device, or with another manufacturer
// string, it gets the fallback's choice, and the record waits. When the
// record's exact unit is bound, its rows are written over that fallback, the
// choice's revision moved on, the warning the fallback wrote dropped, and the
// record's digest kept: the fallback is the migration's own, its first
// revision, never a choice of the user's that the record must leave alone.
// Through the real reader and the store.
func TestTheRecordsOwnModelTakesTheRecordOverItsFallback(t *testing.T) {
	durable(t)
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	exact := bound(t)
	for _, tc := range []struct {
		name  string
		first func(*Bound)
	}{
		{"another_pcm_device", func(b *Bound) { b.Identity.Device = 1 }},
		{"another_manufacturer_string", func(b *Bound) { b.Identity.Manufacturer = "iFi audio" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordDir, a := recordFixture(t, recordSpec{})
			data := t.TempDir()
			s := openAt(t, data, options{})
			first := *exact
			tc.first(&first)
			if !first.Key.Equal(exact.Key) {
				t.Fatal("the fixture's first unit is not of the record's key")
			}
			if _, err := s.Migrate(recordDir, l, &first); err != nil {
				t.Fatal(err)
			}
			fallback := fileState(t, data)
			d, _ := fallback.DAC(goBar(t))
			if d.Choice == nil || d.Choice.SetBy != ByMigration || d.Choice.Revision != 1 || len(fallback.Warnings) != 1 || fallback.MigratedFrom.WaitingFor != WaitingForRecordModel {
				t.Fatalf("the first unit gave %s: want the fallback's choice, its warning, and the record waiting", show(t, fallback))
			}
			if _, err := s.Migrate(recordDir, l, exact); err != nil {
				t.Fatal(err)
			}
			got := fileState(t, data)
			d, _ = got.DAC(goBar(t))
			wantChoice := &Choice{Mode: ModeNative, Revision: 2, SetAt: t0, SetBy: ByMigration}
			if !reflect.DeepEqual(d.Choice, wantChoice) || len(d.Listener) != 1 || len(d.FirstUse) != 1 {
				t.Fatalf("the record's exact unit, bound after the fallback, holds %s: want the record's rows, the choice at revision 2", show(t, got))
			}
			if len(got.Warnings) != 0 {
				t.Fatalf("the fallback's warning stays beside the record's rows: %+v", got.Warnings)
			}
			if got.MigratedFrom.RecordSHA256 != a.SHA256 || got.MigratedFrom.WaitingFor != "" {
				t.Fatalf("the marker is %+v, want the record's digest and nothing waiting", got.MigratedFrom)
			}
		})
	}
	t.Run("a_users_choice_in_between", func(t *testing.T) {
		recordDir, _ := recordFixture(t, recordSpec{})
		data := t.TempDir()
		s := openAt(t, data, options{})
		first := *exact
		first.Identity.Device = 1
		if _, err := s.Migrate(recordDir, l, &first); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(func(st *State) error {
			d := st.entry(goBar(t))
			d.Choice = &Choice{Mode: ModeConvert, Revision: d.Choice.Revision + 1, SetAt: t0, SetBy: ByApp}
			st.ClearWarningsFor(goBar(t))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(recordDir, l, exact); err != nil {
			t.Fatal(err)
		}
		got := fileState(t, data)
		d, _ := got.DAC(goBar(t))
		if want := (&Choice{Mode: ModeConvert, Revision: 2, SetAt: t0, SetBy: ByApp}); !reflect.DeepEqual(d.Choice, want) || len(d.Listener) != 0 || len(d.FirstUse) != 0 {
			t.Fatalf("the user's choice made after the fallback did not win: %s", show(t, got))
		}
		if got.MigratedFrom.WaitingFor != "" || got.MigratedFrom.RecordSHA256 != "" {
			t.Fatalf("the marker is %+v, want the part done with no record migrated", got.MigratedFrom)
		}
	})
}
