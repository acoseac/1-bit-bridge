package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification/approvaltest"
)

// The migration's cases through a real serve (B728 B6): the record's
// refusals and their warnings, a write refused before and after it lands, a
// migration that runs once and again once the state is deleted, and one that
// waits for a DAC, in the same run and across a restart.

// The GO bar's unit in the Pi 5 tree, and the lines that plug it back in after
// an unplug, as another enumeration (devnum) of the same unit at the same port.
const goBarUnit = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"

func goBarReplug(devnum string) string {
	return "usbdev " + goBarUnit + " vendor=20b1 product=3009 serial=GOBAR-7Q2X-91KZ-4471 devnum=" + devnum + "\n" +
		"usbcard 5 " + goBarUnit + "/1-1:1.0\n" +
		"proccard 5 Audio \"iFi (by AMR) HD USB Audio\" usb=20b1:3009 pcm=0.0\n"
}

// dsdBox is the Pi 5 with the GO bar the watcher reads, its descriptors laid
// out, and a source the test wakes, so an unplug or a replug is read at once.
type dsdBox struct {
	trees  outputTrees
	source *dacEventSource
}

func newDSDBox(t *testing.T) *dsdBox {
	t.Helper()
	trees := goBarTrees(t)
	goBarIdentityFiles(t, trees)
	source := &dacEventSource{notes: make(chan struct{}, 1), done: make(chan struct{})}
	close(source.done)
	return &dsdBox{trees: trees, source: source}
}

// with is the serve options that read the box.
func (b *dsdBox) with(o *serveOpts) {
	o.playerTrees = b.trees
	o.hotplugSource = func(context.Context) hotplug.Source { return b.source }
}

// unplug takes the GO bar out of the box, and plug puts it back as
// enumeration devnum, its manufacturer string the one given; wake has the
// watcher read the box at once.
func (b *dsdBox) unplug(t *testing.T) {
	t.Helper()
	sysfstest.Apply(t, filepath.Dir(b.trees.proc), "unplug "+goBarUnit+"\n")
}

func (b *dsdBox) plug(t *testing.T, devnum, manufacturer string) {
	t.Helper()
	// The descriptors first, in the unit's directory made ahead of its card,
	// so no reading of the watcher's finds the card before them.
	if err := os.MkdirAll(filepath.Join(b.trees.sysfs, qualifyGoBar), 0o755); err != nil {
		t.Fatal(err)
	}
	goBarIdentityFiles(t, b.trees)
	if manufacturer != "iFi" {
		if err := os.WriteFile(filepath.Join(b.trees.sysfs, qualifyGoBar, "manufacturer"), []byte(manufacturer), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sysfstest.Apply(t, filepath.Dir(b.trees.proc), goBarReplug(devnum))
}

func (b *dsdBox) wake() {
	b.source.lost.Store(true)
	select {
	case b.source.notes <- struct{}{}:
	default:
	}
}

// bootDSD boots serve on the box with goBarPlayer and keys, a stand-in
// backend, and before run once the directories exist.
func bootDSD(t *testing.T, box *dsdBox, keys string, before func(dataDir string)) *playerServe {
	t.Helper()
	return bootPlayerServe(t, playerServeCase{player: goBarPlayer + keys, standIn: newPlayerStandIn(), before: before, with: box.with})
}

// rebootDSD boots serve again on ps's directories and the box, the config
// written anew with keys: the next start of the same bridge.
func rebootDSD(t *testing.T, ps *playerServe, box *dsdBox, keys string) *playerServe {
	t.Helper()
	return bootPlayerServeAt(t, playerServeCase{player: goBarPlayer + keys, standIn: newPlayerStandIn(), with: box.with},
		filepath.Dir(ps.configPath), ps.music, ps.dataDir)
}

// stopped stops ps and requires a clean exit.
func stopped(t *testing.T, ps *playerServe) {
	t.Helper()
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// migrationLines are the lines of log a start's migration writes: what it
// migrated, each warning it recorded, a step not saved, and a DAC's part
// asked again or no more.
func migrationLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "player: the legacy DSD keys were migrated") || strings.Contains(l, "player: the DSD migration recorded a warning") ||
			strings.Contains(l, "player: the migration of the legacy DSD keys") || strings.Contains(l, "player: the DSD migration's part") {
			out = append(out, l)
		}
	}
	return out
}

// TestAVersion35RecordOnAnotherPortMigrates (B728 B6; the design's sections
// 3.5 and 3.6): an owner's record made at version 35, the oldest the legacy
// reader takes, on another port of the GO bar's model, is the GO bar's at the
// migration, which compares the model and not the port: its rates' answers
// and their acknowledgement, under the record's firmware. Production's reader,
// which compares the port until the cutover, does not take it.
func TestAVersion35RecordOnAnotherPortMigrates(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	var record qualification.Approval
	elsewhere := goBarLiveIdentity(t, box.trees)
	elsewhere.Port = "platform-xhci-hcd.1/usb2-2:1.0"
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		record = legacyRecordIn(t, dataDir, elsewhere, qualification.LegacyVersion, nativeAt(dsdstate.DSD64), nativeAt(dsdstate.DSD128))
	})
	availableOutput(t, ps.runningEngine(t))
	auth := wiredAuthorization(t, ps.standIn)
	stopped(t, ps)
	st, _ := dsdStateIn(t, ps.dataDir)
	if m := st.MigratedFrom; m == nil || m.RecordSHA256 != record.SHA256 || m.WaitingFor != "" {
		t.Fatalf("the marker %+v, want the version 35 record taken (%s), nothing waiting", st.MigratedFrom, record.SHA256)
	}
	d, ok := st.DAC(goBarKey(t))
	if !ok || d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 ||
		!reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64, dsdstate.DSD128}) || len(d.Listener) != 2 {
		t.Fatalf("the GO bar's model holds %+v, want native, both approved rates acknowledged and clean", d)
	}
	if len(st.Warnings) != 0 {
		t.Errorf("a record taken left warnings: %+v", st.Warnings)
	}
	// Production still compares every field, the port among them.
	auth.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }}
	auth.Healthy = nil
	ps.standIn.mu.Lock()
	locator := ps.standIn.cfg.Locator
	ps.standIn.mu.Unlock()
	b, err := playback.New(playback.Config{Authorization: &auth, Locator: locator, Resolver: dopContents{}})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshAuthorization(context.Background())
	if pol := b.Policy(); len(pol.Tuples) != 0 {
		t.Errorf("production's reader took a record of another port: %+v", pol.Tuples)
	}
}

// TestARefusedRecordLeavesAWarning (B728 B6; Gemini on the design's round
// 3): a record the legacy reader refuses for the bound DAC, or none, is a
// warning in the state the app can read, owned by the DAC the part went to,
// beside a Warn line naming it, and the DAC gets the fallback: native's
// choice with nothing acknowledged, which refuses until the app acknowledges
// and never converts. The migration is done: nothing waits.
func TestARefusedRecordLeavesAWarning(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct {
		name, keys   string
		record       func(t *testing.T, dataDir string, id qualification.Identity)
		code, reason string
	}{
		{"a stale version", "    dsdQualification: approved.json\n", func(t *testing.T, dataDir string, id qualification.Identity) {
			legacyRecordIn(t, dataDir, id, qualification.LegacyVersion-1, nativeAt(dsdstate.DSD64))
		}, dsdstate.WarningMigrationRecordRefused, dsdstate.ReasonStaleVersion},
		{"a tampered body", "    dsdQualification: approved.json\n", func(t *testing.T, dataDir string, id qualification.Identity) {
			a := legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
			a.Body.Revision = "r2"
			data, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataDir, "approved.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, dsdstate.WarningMigrationRecordRefused, dsdstate.ReasonHashMismatch},
		{"a record that does not parse", "    dsdQualification: approved.json\n", func(t *testing.T, dataDir string, _ qualification.Identity) {
			approvaltest.SecureDir(t, dataDir)
			if err := os.WriteFile(filepath.Join(dataDir, "approved.json"), []byte("{not a record"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, dsdstate.WarningMigrationRecordRefused, dsdstate.ReasonUnreadable},
		{"no record at its name", "    dsdQualification: approved.json\n", nil, dsdstate.WarningMigrationRecordMissing, ""},
		{"no record named", "", nil, dsdstate.WarningMigrationRecordMissing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			log := watchLog(t)
			ps := bootDSD(t, box, "    dsdMode: native\n"+tc.keys, func(dataDir string) {
				if tc.record != nil {
					tc.record(t, dataDir, goBarLiveIdentity(t, box.trees))
				}
			})
			availableOutput(t, ps.runningEngine(t))
			stopped(t, ps)
			st, _ := dsdStateIn(t, ps.dataDir)
			key := goBarKey(t)
			if len(st.Warnings) != 1 || st.Warnings[0].Code != tc.code || st.Warnings[0].Reason != tc.reason || st.Warnings[0].DAC == nil || !st.Warnings[0].DAC.Equal(key) {
				t.Fatalf("the warnings %+v, want %s (%q) owned by the GO bar's model", st.Warnings, tc.code, tc.reason)
			}
			d, _ := st.DAC(key)
			if c := d.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.Revision != 1 || c.SetBy != dsdstate.ByMigration ||
				len(d.FirstUse) != 0 || len(d.Listener) != 0 || d.DoPConfirmed != nil {
				t.Errorf("the GO bar's model holds %+v, want native's fallback, nothing acknowledged", d)
			}
			if m := st.MigratedFrom; m == nil || m.WaitingFor != "" || m.RecordSHA256 != "" {
				t.Errorf("the marker %+v, want a migration done with no record taken", st.MigratedFrom)
			}
			line := log.await(t, "player: the DSD migration recorded a warning", "code="+tc.code)
			if tc.reason != "" && !strings.Contains(line, "reason="+tc.reason) {
				t.Errorf("the warning's line does not name its reason %s: %s", tc.reason, line)
			}
			if !strings.Contains(line, "dac=20b1:3009 GO bar") || strings.Contains(line, goBarSerial) || strings.Contains(line, "usb2-1") {
				t.Errorf("the warning's line names its DAC by anything but the model: %s", line)
			}
		})
	}
}

// rollbackClaims are words a failed write's line must not say: that the file
// is as it was, which a write that failed may not have left it (dsdstate's
// Store.Update), or that it is durable.
var rollbackClaims = []string{"is not written", "nothing was written", "unchanged", "as it was", "rolled back", "is durable"}

// TestARefusedStateWriteLeavesAWarnLineAndThePlayerStarts (B728 B6): a
// migration whose write fails stops nothing, the player starts all the same,
// and a Warn line says so, in words that claim neither that the state is as
// it was nor that it is durable. What the next start does is the file's: a
// write refused before anything lands (here the durable writer's check of
// the medium, refusing that one write) leaves no file, and the next start
// migrates from the keys; one whose barrier fails once its rename has landed
// leaves the migration in the file, which the next start reads, and migrates
// nothing over.
func TestARefusedStateWriteLeavesAWarnLineAndThePlayerStarts(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	t.Run("refused before it lands", func(t *testing.T) {
		box := newDSDBox(t)
		log := watchLog(t)
		// The one write the step makes is refused: the seam arms the refusal
		// just before the step, on the supervisor, and nothing else writes
		// durably until the step has returned.
		var armed atomic.Bool
		t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
			if armed.CompareAndSwap(true, false) {
				return errors.New("the test's medium refuses this write")
			}
			return nil
		}))
		seams := &servePlayerSeams{migrates: func(context.Context, *dsdstate.Bound) { armed.Store(true) }}
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: off\n", standIn: newPlayerStandIn(), seams: seams, with: box.with})
		if ps.runningEngine(t) == nil {
			t.Fatal("the player did not start")
		}
		line := log.await(t, "player: the migration of the legacy DSD keys was not saved, and the player starts without it")
		if strings.Contains(line, "is not known to be durable") || !strings.Contains(line, "the test's medium refuses this write") {
			t.Errorf("a write refused before it landed: %s", line)
		}
		for _, claim := range rollbackClaims {
			if strings.Contains(line, claim) {
				t.Errorf("a write refused is said to have left the state as it was (%q): %s", claim, line)
			}
		}
		stopped(t, ps)
		if _, there := dsdStateIn(t, ps.dataDir); there {
			t.Fatal("a write refused before anything landed left a state file")
		}
		if lines := migrationLines(log.all()); len(lines) != 1 {
			t.Errorf("the refused step said %q, want its Warn line alone", lines)
		}
		// The next start reads no file, and migrates the keys.
		ps2 := rebootDSD(t, ps, box, "    dsdMode: off\n")
		ps2.runningEngine(t)
		stopped(t, ps2)
		if st, there := dsdStateIn(t, ps.dataDir); !there || st.DefaultMode != dsdstate.ModeOff {
			t.Errorf("the next start left %+v (a file: %v), want the keys migrated", st, there)
		}
	})
	t.Run("its barrier fails after it lands", func(t *testing.T) {
		box := newDSDBox(t)
		log := watchLog(t)
		var dataDir string
		var failed atomic.Bool
		t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
			if dataDir == "" {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(dataDir, dsdstate.FileName)); err == nil && failed.CompareAndSwap(false, true) {
				return errors.New("the test's barrier fails once the state's file is there")
			}
			return nil
		}))
		ps := bootDSD(t, box, "    dsdMode: off\n", func(d string) { dataDir = d })
		ps.runningEngine(t)
		line := log.await(t, "player: the migration of the legacy DSD keys reached the state's file and is not known to be durable")
		if strings.Contains(line, "was not saved") {
			t.Errorf("a write that landed is said not to have been saved: %s", line)
		}
		for _, claim := range rollbackClaims {
			if strings.Contains(line, claim) {
				t.Errorf("a write that landed is said to have left the state as it was (%q): %s", claim, line)
			}
		}
		stopped(t, ps)
		st, there := dsdStateIn(t, ps.dataDir)
		if !there || st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil {
			t.Fatalf("the file holds %+v (there %v), want the migration its write landed", st, there)
		}
		before := readBytes(t, filepath.Join(ps.dataDir, dsdstate.FileName))
		log2 := watchLog(t)
		ps2 := rebootDSD(t, ps, box, "    dsdMode: off\n")
		availableOutput(t, ps2.runningEngine(t))
		stopped(t, ps2)
		if after := readBytes(t, filepath.Join(ps.dataDir, dsdstate.FileName)); !bytes.Equal(before, after) {
			t.Errorf("the next start wrote over the migration it read:\n%s\n%s", before, after)
		}
		if lines := migrationLines(log2.all()); len(lines) != 0 {
			t.Errorf("the next start migrated again over the file that holds the migration: %q", lines)
		}
	})
}

// TestTheMigrationRunsOnceAndAgainAfterTheStateIsDeleted (B728 B6; the
// design's section 3.6): once done, the migration never runs again, however
// the legacy input changes (a key flipped, the record replaced): the next
// start leaves the state byte for byte as it was, with no line of the
// migration's, where a deterministic migration run again would have passed.
// With the state deleted by hand, the next start migrates again, from the
// keys and the record as they are then.
func TestTheMigrationRunsOnceAndAgainAfterTheStateIsDeleted(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	id := goBarLiveIdentity(t, box.trees)
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
	})
	availableOutput(t, ps.runningEngine(t))
	stopped(t, ps)
	statePath := filepath.Join(ps.dataDir, dsdstate.FileName)
	first := readBytes(t, statePath)

	// Another input: auto with DoP, a record that approves DSD128 too.
	replaced := legacyRecordIn(t, ps.dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64), nativeAt(dsdstate.DSD128))
	keys := "    dsdMode: auto\n    allowDoP: true\n    dsdQualification: approved.json\n"
	log := watchLog(t)
	ps2 := rebootDSD(t, ps, box, keys)
	availableOutput(t, ps2.runningEngine(t))
	stopped(t, ps2)
	if again := readBytes(t, statePath); !bytes.Equal(first, again) {
		t.Fatalf("a migration done ran again over a changed input:\n%s\n%s", first, again)
	}
	if lines := migrationLines(log.all()); len(lines) != 0 {
		t.Errorf("the start after a migration done logged the migration's lines: %q", lines)
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	ps3 := rebootDSD(t, ps, box, keys)
	availableOutput(t, ps3.runningEngine(t))
	stopped(t, ps3)
	st, there := dsdStateIn(t, ps.dataDir)
	yes := true
	want := dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "auto", AllowDoP: &yes, DSDQualification: "approved.json", RecordSHA256: replaced.SHA256}
	if !there || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) {
		t.Fatalf("after the state was deleted the marker is %+v, want the input as it is now, %+v", st.MigratedFrom, want)
	}
	d, _ := st.DAC(goBarKey(t))
	if d.Choice == nil || d.Choice.Mode != dsdstate.ModeAuto || len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64, dsdstate.DSD128}) {
		t.Errorf("after the state was deleted the GO bar's model holds %+v, want auto, the new record's two rates acknowledged", d)
	}
}

// TestAMigrationWaitingForTheDACIsResolvedWhenItArrives (B728 B6): with no
// DAC at the binding when the player starts, the migration's part of the
// bridge's is made and its part for a DAC waits (bound_dac), which the state
// tells from a migration done; once the watcher finds the DAC, in the same
// run, the part is made for it, with the keys the start captured, before the
// output is reported available.
func TestAMigrationWaitingForTheDACIsResolvedWhenItArrives(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	var record qualification.Approval
	id := goBarLiveIdentity(t, box.trees)
	box.unplug(t)
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		record = legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
	})
	eng := ps.runningEngine(t)
	awaitEngine(t, eng, "the output known and missing", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
	st, _ := dsdStateIn(t, ps.dataDir)
	if m := st.MigratedFrom; m == nil || m.WaitingFor != dsdstate.WaitingForBoundDAC || m.DSDMode != "native" || m.RecordSHA256 != "" || len(st.DACs) != 0 {
		t.Fatalf("with no DAC the state is %+v, marker %+v: want its part for a DAC waiting, nothing for any DAC", st, st.MigratedFrom)
	}
	box.plug(t, "3", "iFi")
	box.wake()
	availableOutput(t, eng)
	st, _ = dsdStateIn(t, ps.dataDir)
	if m := st.MigratedFrom; m == nil || m.WaitingFor != "" || m.RecordSHA256 != record.SHA256 {
		t.Fatalf("once the DAC came the marker is %+v, want the record taken, nothing waiting", st.MigratedFrom)
	}
	if d, _ := st.DAC(goBarKey(t)); d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 {
		t.Errorf("once the DAC came its model holds %+v, want the record's rows", d)
	}
	stopped(t, ps)
}

// TestAMigrationWaitingForTheDACIsResolvedAtALaterStartWithTheKeysItCaptured
// (B728 B6; the design's section 3.6): a part for a DAC still waiting when
// serve stops is made at a later start that finds the DAC, with the keys and
// the record name the first start captured in the marker, never the ones
// bridge.yaml holds then: here none.
func TestAMigrationWaitingForTheDACIsResolvedAtALaterStartWithTheKeysItCaptured(t *testing.T) {
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
	ps2 := rebootDSD(t, ps, box, "")
	availableOutput(t, ps2.runningEngine(t))
	stopped(t, ps2)
	st, _ := dsdStateIn(t, ps.dataDir)
	want := dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "native", DSDQualification: "approved.json", RecordSHA256: record.SHA256}
	if st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) {
		t.Fatalf("the later start left the marker %+v, want the captured keys' migration done, %+v", st.MigratedFrom, want)
	}
	d, _ := st.DAC(goBarKey(t))
	if d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) {
		t.Errorf("the later start gave the GO bar's model %+v, want the captured keys' native with DSD64 acknowledged", d)
	}
}

// TestARecordOfAnotherUnitWaitsForItsOwnModel (B728 B6; B3's round 2): a
// record the reader refuses for the bound DAC only because it is of another
// model (identity_mismatch: here the manufacturer string, which the key does
// not hold) gives that DAC the fallback and its warning, and waits for its
// own model (record_model); when the binding resolves to a unit the reader
// takes it for, the record's rows replace the fallback at the next revision,
// its warning dropped, in the same run and at a later start.
func TestARecordOfAnotherUnitWaitsForItsOwnModel(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, later := range []bool{false, true} {
		name := "in the same run"
		if later {
			name = "at a later start"
		}
		t.Run(name, func(t *testing.T) {
			box := newDSDBox(t)
			id := goBarLiveIdentity(t, box.trees)
			id.Manufacturer = "iFi Audio"
			var record qualification.Approval
			ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
				record = legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
			})
			eng := ps.runningEngine(t)
			availableOutput(t, eng)
			key := goBarKey(t)
			st, _ := dsdStateIn(t, ps.dataDir)
			if m := st.MigratedFrom; m == nil || m.WaitingFor != dsdstate.WaitingForRecordModel {
				t.Fatalf("the marker %+v, want the record waiting for its own model", st.MigratedFrom)
			}
			if d, _ := st.DAC(key); d.Choice == nil || d.Choice.Revision != 1 || len(d.FirstUse) != 0 || len(st.Warnings) != 1 ||
				st.Warnings[0].Reason != dsdstate.ReasonIdentityMismatch {
				t.Fatalf("the GO bar holds %+v, warnings %+v; want the fallback and identity_mismatch", d, st.Warnings)
			}
			box.unplug(t)
			box.wake()
			awaitEngine(t, eng, "the output gone", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
			if later {
				stopped(t, ps)
				box.plug(t, "3", "iFi Audio")
				ps = rebootDSD(t, ps, box, "    dsdMode: native\n    dsdQualification: approved.json\n")
				eng = ps.runningEngine(t)
			} else {
				box.plug(t, "3", "iFi Audio")
				box.wake()
			}
			availableOutput(t, eng)
			stopped(t, ps)
			st, _ = dsdStateIn(t, ps.dataDir)
			if m := st.MigratedFrom; m == nil || m.WaitingFor != "" || m.RecordSHA256 != record.SHA256 {
				t.Fatalf("once the record's model came the marker is %+v, want the record taken", st.MigratedFrom)
			}
			d, _ := st.DAC(key)
			if c := d.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.Revision != 2 || c.SetBy != dsdstate.ByMigration ||
				len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) {
				t.Errorf("once the record's model came the GO bar holds %+v, want the record's rows at revision 2", d)
			}
			if len(st.Warnings) != 0 {
				t.Errorf("the record's rows kept the fallback's warning: %+v", st.Warnings)
			}
		})
	}
}
