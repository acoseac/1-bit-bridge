package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification/approvaltest"
)

// The migration of the legacy DSD keys at serve's player start (B728 B6;
// ops/plan-b728-dsd-capability-design.md, sections 2.5 and 3.6), through a
// real serve: the state these tests read is the file serve persisted in its
// data directory, decoded through dsdstate as the next start reads it.

// dsdStateIn is the player's DSD state serve left in dataDir, decoded through
// dsdstate, and whether there is a file: none is the defaults. It reads the
// file's bytes and writes nothing (dsdstate.Open would set a damaged file
// aside).
func dsdStateIn(t *testing.T, dataDir string) (dsdstate.State, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, dsdstate.FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return dsdstate.Defaults(), false
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := dsdstate.Decode(raw)
	if err != nil {
		t.Fatalf("the player's DSD state serve left does not decode: %v\n%s", err, raw)
	}
	return st, true
}

// goBarKey is the GO bar's model as the state keys it: its USB IDs, the
// interface its card is in the Pi 5 tree (1-1:1.0, interface 0) and its
// product string as goBarIdentityFiles lays it out.
func goBarKey(t *testing.T) dacprofile.Key {
	t.Helper()
	iface, product := 0, "GO bar"
	k, err := dacprofile.NewKey(0x20b1, 0x3009, &iface, &product)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// goBarLiveIdentity is the GO bar in trees as the legacy reader compares it,
// production's every field and the migration's model alike: the identity a
// record made on this unit, at this port, holds.
func goBarLiveIdentity(t *testing.T, trees outputTrees) qualification.Identity {
	t.Helper()
	q, err := (alsa.SysfsReader{Root: trees.sysfs}).Qualification(5)
	if err != nil {
		t.Fatalf("the GO bar's descriptors: %v", err)
	}
	return qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice,
		Port: q.Port, Interface: q.Interface}
}

// nativeAt is a native DSD tuple at rate, as an owner's record approves one.
func nativeAt(rate int) qualification.Tuple {
	return qualification.Tuple{Format: "DSD_U32_BE", BitRate: rate, Pause: "kernel"}
}

// legacyRecordIn writes in dir, the state directory a record is named within
// (serve's data directory), an owner's approval record of version for the
// unit id, approving tuples, at approved.json, with each policy's matrix and
// lifecycle report as the harness writes them, and returns it. The directory
// is made private first, as the reader requires.
func legacyRecordIn(t *testing.T, dir string, id qualification.Identity, version int, tuples ...qualification.Tuple) qualification.Approval {
	t.Helper()
	approvaltest.SecureDir(t, dir)
	environment := qualification.Digest([]string{"linux/arm64", "test-go", "test-kernel"})
	var policies []qualification.ApprovedPolicy
	for i, tuple := range tuples {
		p := qualification.ApprovedPolicy{ID: fmt.Sprintf("p%d", i), Tuple: tuple, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30}
		for _, scenario := range []string{"matrix", "backend-lifecycle"} {
			r := map[string]any{"QualificationVersion": version, "Tuple": tuple, "NativeTuple": tuple, "NativeCompleted": 1, "Identity": id,
				"ManifestHash": qualification.Digest("fixtures"), "Environment": environment, "Scenario": scenario, "End": "complete", "Completed": []bool{true},
				"PauseReleaseSeconds": 30, "BuildSHA": "test-build", "FeatureUnit": false, "Before": map[string]any{}, "After": map[string]any{},
				"LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("%s-%s.json", p.ID, scenario)
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			ev := qualification.Evidence{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
			if scenario == "matrix" {
				p.Matrix = ev
			} else {
				p.Lifecycle = ev
			}
		}
		policies = append(policies, p)
	}
	a := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: 1, QualificationVersion: version, Confirmation: qualification.ApprovalPhrase,
		Revision: "r1", ManifestHash: qualification.Digest("fixtures"), Environment: environment, Identity: id, Policies: policies}}
	a.SHA256 = qualification.Digest(a.Body)
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "approved.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

// wiredAuthorization is the DSD authorization serve's player wired into the
// backend it built (the stand-in keeps it): what production authorizes DSD
// by until the cutover.
func wiredAuthorization(t *testing.T, standIn *playerStandIn) playback.Authorization {
	t.Helper()
	standIn.mu.Lock()
	defer standIn.mu.Unlock()
	if !standIn.built || standIn.cfg.Authorization == nil {
		t.Fatal("serve's player wired no DSD authorization")
	}
	return *standIn.cfg.Authorization
}

// TestAnExplicitOffSurvivesAsTheDefault (B728 B6, the design's section 3.6):
// an explicit dsdMode: off, and dop with allowDoP false, which refuses all DSD
// today as well, are migrated at serve's player start into the player's DSD
// state as defaultMode off, every DAC with no choice of its own, with the
// bridge's migrated_off warning and the marker holding the keys as
// bridge.yaml held them; an unset dsdMode, which refuses DSD today just as
// off does, migrates nothing: no file, the defaults. The migration is the
// bridge's part alone, made before the player runs. Production still reads
// the keys until the cutover: the authorization serve wires is theirs, DSD off
// or DoP held back, as before.
func TestAnExplicitOffSurvivesAsTheDefault(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	no := false
	for _, tc := range []struct {
		name, keys string
		// mode and allowDoP are what serve wires into the authorization, as
		// before B6.
		mode     string
		allowDoP bool
		// marker is the migration's, nil for none and no file.
		marker *dsdstate.Marker
	}{
		{"explicit off", "    dsdMode: off\n", "off", false, &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "off"}},
		{"dop without allowDoP", "    dsdMode: dop\n    allowDoP: false\n", "dop", false, &dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "dop", AllowDoP: &no}},
		{"unset", "", "off", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standIn := newPlayerStandIn()
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + tc.keys, standIn: standIn})
			ps.runningEngine(t)
			st, there := dsdStateIn(t, ps.dataDir)
			if tc.marker == nil {
				if there || st.DefaultMode != dsdstate.ModeAuto || len(st.Warnings) != 0 || st.MigratedFrom != nil {
					t.Fatalf("an unset dsdMode left the state %+v (a file: %v), want the defaults and no file", st, there)
				}
			} else {
				if !there {
					t.Fatalf("serve's player persisted no DSD state for %q: an explicit opt-out is lost", tc.keys)
				}
				if st.DefaultMode != dsdstate.ModeOff {
					t.Errorf("defaultMode %q, want off: the explicit opt-out applies to every DAC with no choice of its own", st.DefaultMode)
				}
				if len(st.Warnings) != 1 || st.Warnings[0].Code != dsdstate.WarningMigratedOff || st.Warnings[0].DAC != nil {
					t.Errorf("the warnings %+v, want the bridge's migrated_off alone", st.Warnings)
				}
				if st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, *tc.marker) {
					t.Errorf("the marker %+v, want %+v", st.MigratedFrom, *tc.marker)
				}
				if len(st.DACs) != 0 {
					t.Errorf("an opt-out gave a DAC something: %+v", st.DACs)
				}
			}
			if a := wiredAuthorization(t, standIn); a.Mode != tc.mode || a.AllowDoP != tc.allowDoP {
				t.Errorf("serve wired dsdMode %q and allowDoP %v into the authorization, want %q and %v: production reads the keys until the cutover", a.Mode, a.AllowDoP, tc.mode, tc.allowDoP)
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
			}
		})
	}
}

// TestAMigratedApprovalCoversOnlyItsRates (B728 B6; Codex's P1 on the
// design's round 3): an owner's record that approved native DSD64 alone,
// under dsdMode native, is migrated once the watcher finds the GO bar, before
// its output is reported to the engine: a choice of native for the GO bar's
// model, set by the migration, DSD64's listener answer clean on the record's
// firmware (undated: a schema-1 record), and a first-use acknowledgement of
// native that lists DSD64 alone, so it covers DSD64 and not DSD128, which an
// acknowledgement of the whole transport would. The marker keeps the
// record's digest and waits for nothing. Production's read of the record is
// unchanged: the backend over the authorization serve wired still publishes
// the record's DSD64 tuple through the legacy reader.
func TestAMigratedApprovalCoversOnlyItsRates(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	trees := goBarTrees(t)
	goBarIdentityFiles(t, trees)
	standIn := newPlayerStandIn()
	var record qualification.Approval
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", standIn: standIn,
		before: func(dataDir string) {
			record = legacyRecordIn(t, dataDir, goBarLiveIdentity(t, trees), qualification.Version, nativeAt(dsdstate.DSD64))
		},
		with: func(o *serveOpts) { o.playerTrees = trees }})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	st, there := dsdStateIn(t, ps.dataDir)
	if !there {
		t.Fatal("serve's player persisted no DSD state: the owner's approval was not migrated")
	}
	want := dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "native", DSDQualification: "approved.json", RecordSHA256: record.SHA256}
	if st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) {
		t.Fatalf("the marker %+v, want %+v: the record taken, nothing waiting", st.MigratedFrom, want)
	}
	if st.DefaultMode != dsdstate.ModeAuto || len(st.Warnings) != 0 {
		t.Errorf("defaultMode %q, warnings %+v; want auto and none", st.DefaultMode, st.Warnings)
	}
	d, ok := st.DAC(goBarKey(t))
	if !ok {
		t.Fatalf("the state holds nothing for the GO bar's model: %+v", st.DACs)
	}
	if c := d.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.MaxBitRate != 0 || c.Revision != 1 || c.SetBy != dsdstate.ByMigration {
		t.Errorf("the GO bar's choice %+v, want native at revision 1, set by the migration", d.Choice)
	}
	if wantAnswers := []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: dsdstate.DSD64, BCDDevice: "0123", Answer: dsdstate.AnswerClean}}; !reflect.DeepEqual(d.Listener, wantAnswers) {
		t.Errorf("the listener answers %+v, want DSD64's clean on the record's firmware, undated", d.Listener)
	}
	if len(d.FirstUse) != 1 {
		t.Fatalf("the first-use acknowledgements %+v, want native's alone", d.FirstUse)
	}
	f := d.FirstUse[0]
	if f.Transport != dsdstate.TransportNative || f.BCDDevice != "0123" || !reflect.DeepEqual(f.BitRates, []int{dsdstate.DSD64}) {
		t.Errorf("the acknowledgement %+v, want native's under 0123 at DSD64 alone", f)
	}
	if !f.Covers(dsdstate.DSD64) || f.Covers(dsdstate.DSD128) || f.Covers(dsdstate.DSD256) {
		t.Errorf("the acknowledgement covers DSD64 %v, DSD128 %v, DSD256 %v; want DSD64 alone, the rate the record approved",
			f.Covers(dsdstate.DSD64), f.Covers(dsdstate.DSD128), f.Covers(dsdstate.DSD256))
	}
	if d.DoPConfirmed != nil {
		t.Errorf("a native record confirmed DoP: %+v", d.DoPConfirmed)
	}
	// The legacy reader's verdict, unchanged by the migration: the backend over
	// what serve wired, its control reader standing in for the box, which has
	// no control node, reads the record and publishes its tuple.
	auth := wiredAuthorization(t, standIn)
	if auth.Mode != "native" || auth.Record != "approved.json" || auth.StateDir != ps.dataDir {
		t.Fatalf("serve wired %+v, want the keys' native mode and record in its data directory", auth)
	}
	auth.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }}
	auth.Healthy = nil
	standIn.mu.Lock()
	locator := standIn.cfg.Locator
	standIn.mu.Unlock()
	b, err := playback.New(playback.Config{Authorization: &auth, Locator: locator, Resolver: dopContents{}})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshAuthorization(context.Background())
	if pol := b.Policy(); len(pol.Tuples) != 1 || pol.Tuples[0].Tuple.BitRate != dsdstate.DSD64 || pol.Tuples[0].Tuple.Transport != "" {
		t.Errorf("the legacy reader's epoch after the migration: %+v (reason %q), want the record's DSD64 native tuple", pol.Tuples, pol.Reason)
	}
}
