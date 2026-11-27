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
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
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
// and their acknowledgement, under the record's firmware. Since the cutover
// (B7) the effective policy serve wires reads no record, so nothing compares
// the port, and since B990 the acknowledgement gates no rate: the GO bar,
// which the shipped DAC list does not know and whose IDs are no generic
// board's, plays native at every rate it offers.
func TestAVersion35RecordOnAnotherPortMigrates(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	goBarDSD(t, box.trees)
	var record qualification.Approval
	elsewhere := goBarLiveIdentity(t, box.trees)
	elsewhere.Port = "platform-xhci-hcd.1/usb2-2:1.0"
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		record = legacyRecordIn(t, dataDir, elsewhere, qualification.LegacyVersion, nativeAt(dsdstate.DSD64), nativeAt(dsdstate.DSD128))
	})
	availableOutput(t, ps.runningEngine(t))
	pol := policyBackendOf(t, wiredPlayback(t, ps.standIn), dopContents{}, &dopOpens{}).Policy()
	stopped(t, ps)
	st, _ := dsdStateIn(t, ps.dataDir)
	if m := st.MigratedFrom; m == nil || m.RecordSHA256 != record.SHA256 || m.WaitingFor != "" {
		t.Fatalf("the marker %+v, want the version 35 record taken (%s), nothing waiting", st.MigratedFrom, record.SHA256)
	}
	d, ok := st.DAC(goBarKey(t), goBarStamp())
	if !ok || d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 ||
		!reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64, dsdstate.DSD128}) || len(d.Listener) != 2 {
		t.Fatalf("the GO bar's model holds %+v, want native, both approved rates acknowledged and clean", d)
	}
	if len(st.Warnings) != 0 {
		t.Errorf("a record taken left warnings: %+v", st.Warnings)
	}
	if got := ratesPublished(pol); got != `2822400/"" 5644800/"" 11289600/"" ` || pol.Reason != "" {
		t.Errorf("the effective policy publishes %s(reason %q), want native at every rate", got, pol.Reason)
	}
}

// TestARefusedRecordLeavesAWarning (B728 B6; Gemini on the design's round
// 3): a record the legacy reader refuses for the bound DAC, or none, is a
// warning in the state the app can read, owned by the DAC the part went to,
// beside a Warn line naming it, and the DAC gets the fallback: native's
// choice with nothing acknowledged, which since B990 plays what native plays
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
			d, _ := st.DAC(key, goBarStamp())
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

// TestARefusedStateWriteLeavesAWarnLineAndThePlayerStarts (B728 B6; B1042):
// a migration whose write fails stops nothing, the player starts all the
// same, and a Warn line says so, naming the default the run plays by, in
// words that claim neither that the state is as it was nor that it is
// durable. The store holds the start's step for the run (dsdstate's
// Store.Migrate), so the effective policy serve wired refuses DSD as
// dsd_off_by_choice in that run, before it stops, for an explicit off and for
// dop without allowDoP, set false or unset, as 0.2.3 refused it. What the next
// start does is the file's: a write refused before anything lands (here the
// durable writer's check of the medium, refusing that one write) leaves no
// file, and the next start migrates from the keys and saves them with their
// marker; one whose barrier fails once its rename has landed leaves the
// migration in the file, its fence refusing every later write of the run,
// and the next start reads it and migrates nothing over.
func TestARefusedStateWriteLeavesAWarnLineAndThePlayerStarts(t *testing.T) {
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
		t.Run("refused before it lands/"+tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDSD(t, box.trees)
			log := watchLog(t)
			seams := refuseTheStartStep(t)
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + tc.keys, standIn: newPlayerStandIn(), seams: seams, with: box.with})
			eng := ps.runningEngine(t)
			if eng == nil {
				t.Fatal("the player did not start")
			}
			// The run plays by the step it holds, before it stops: the player
			// starts once its start's step has returned, and the output is
			// available once the watcher has resolved the GO bar.
			availableOutput(t, eng)
			refusesDSDAs(t, ps, "dsd_off_by_choice")
			line := log.await(t, "player: the migration of the legacy DSD keys was not saved, and the player holds it for this run alone")
			if strings.Contains(line, "is not known to be durable") || !strings.Contains(line, "the test's medium refuses this write") || !strings.Contains(line, "defaultMode=off") {
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
			ps2 := rebootDSD(t, ps, box, tc.keys)
			ps2.runningEngine(t)
			stopped(t, ps2)
			if st, there := dsdStateIn(t, ps.dataDir); !there || st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, tc.marker) {
				t.Errorf("the next start left %+v, marker %+v (a file: %v), want the keys migrated with their marker", st, st.MigratedFrom, there)
			}
		})
	}
	t.Run("its barrier fails after it lands", func(t *testing.T) {
		box := newDSDBox(t)
		goBarDSD(t, box.trees)
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
		availableOutput(t, ps.runningEngine(t))
		// The run plays by the step that landed, and its fence refuses every
		// later write of the run.
		refusesDSDAs(t, ps, "dsd_off_by_choice")
		line := log.await(t, "player: the migration of the legacy DSD keys reached the state's file and is not known to be durable")
		if strings.Contains(line, "was not saved") || !strings.Contains(line, "defaultMode=off") {
			t.Errorf("a write that landed is said not to have been saved, or names no default: %s", line)
		}
		for _, claim := range rollbackClaims {
			if strings.Contains(line, claim) {
				t.Errorf("a write that landed is said to have left the state as it was (%q): %s", claim, line)
			}
		}
		if err := wiredPolicyOf(t, wiredPlayback(t, ps.standIn)).Store.Update(func(st *dsdstate.State) error {
			st.DefaultMode = dsdstate.ModeAuto
			return nil
		}); !errors.Is(err, dsdstate.ErrLandedNotHeld) {
			t.Errorf("a write after the landed step: %v, want the fence, ErrLandedNotHeld", err)
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

// refuseTheStartStep refuses the one durable write the migration's start step
// makes, through the strict writer's medium check, armed by the seam just
// before that step on the supervisor: every other write is made.
func refuseTheStartStep(t *testing.T) *servePlayerSeams {
	t.Helper()
	var armed atomic.Bool
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
		if armed.CompareAndSwap(true, false) {
			return errors.New("the test's medium refuses this write")
		}
		return nil
	}))
	return &servePlayerSeams{migrates: func(_ context.Context, b *dsdstate.Bound) {
		if b == nil {
			armed.Store(true)
		}
	}}
}

// refusesDSDAs fails t unless the effective policy serve's player wired, read
// by a backend built on it as serve's reads it now, refuses the GO bar's DSD
// as reason at every rate before the DAC is opened: nothing published or
// converted, and a DSD start refused with no open.
func refusesDSDAs(t *testing.T, ps *playerServe, reason engine.Reason) {
	t.Helper()
	opens := &dopOpens{}
	content := dopContents{"dsd": playbacktest.DSF(dsdstate.DSD64, 0.1, func(ch, i int) byte { return byte(i + ch) })}
	b := policyBackendOf(t, wiredPlayback(t, ps.standIn), content, opens)
	if p := b.Policy(); len(p.Tuples) != 0 || len(p.Converted) != 0 || p.Reason != string(reason) {
		t.Fatalf("the effective policy serve wired publishes %s(converted %+v, reason %q), want nothing playing for %s", ratesPublished(p), p.Converted, p.Reason, reason)
	}
	err := startOn(t, b, 1, "dsd")
	native, pcm := opens.counts()
	if unplayable(err) != reason || len(native) != 0 || len(pcm) != 0 {
		t.Fatalf("a DSD start: %v (refused %q), %d DSD and %d PCM opens; want %s and none", err, unplayable(err), len(native), len(pcm), reason)
	}
}

// TestAStartStepNotSavedIsSavedByTheRunsNextWrite (B1042): a start step whose
// write is refused before it lands is held for the run, and the run's next
// write, a user's choice for the bound DAC on a disk that works again, saves
// the state the store holds, its marker with it: the file holds defaultMode
// off beside the migration's marker, never the default alone. The next start,
// its bridge.yaml no longer setting dsdMode, finds the migration done, as
// after a migration saved at its start: off and the marker kept, no line of
// the migration's, and the file as the run left it.
func TestAStartStepNotSavedIsSavedByTheRunsNextWrite(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	goBarDSD(t, box.trees)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: off\n", standIn: newPlayerStandIn(), seams: refuseTheStartStep(t), with: box.with})
	availableOutput(t, ps.runningEngine(t))
	if _, there := dsdStateIn(t, ps.dataDir); there {
		t.Fatal("the start step's refused write left a state file")
	}
	b := policyBackendOf(t, wiredPlayback(t, ps.standIn), dopContents{}, &dopOpens{})
	view := b.Policy().DSD
	if view == nil || view.DAC == nil {
		t.Fatalf("the read side names no DAC: %+v", b.Policy())
	}
	res := b.ChooseDSD(context.Background(), playback.ChoiceRequest{DACKey: view.DAC.Key, DACToken: view.DAC.Token,
		Choice: policy.Choice{BaseRevision: view.DAC.Choice.Revision, Mode: dsdstate.ModeNative, By: dsdstate.ByApp}})
	if res.Outcome != playback.ChoiceWritten {
		t.Fatalf("the user's choice: %s (%v), want it written", res.Outcome, res.Err)
	}
	if err := b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped(t, ps)
	want := dsdstate.Marker{From: dsdstate.FromKeys, DSDMode: "off"}
	st, there := dsdStateIn(t, ps.dataDir)
	if !there || st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) {
		t.Fatalf("the run's next write left %+v, marker %+v (a file: %v), want defaultMode off beside the marker %+v", st, st.MigratedFrom, there, want)
	}
	if len(st.Warnings) != 1 || st.Warnings[0].Code != dsdstate.WarningMigratedOff {
		t.Errorf("the warnings %+v, want the migration's migrated_off", st.Warnings)
	}
	if d, _ := st.DAC(goBarKey(t), goBarStamp()); d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || d.Choice.SetBy != dsdstate.ByApp {
		t.Errorf("the GO bar holds %+v, want the user's native", d)
	}
	statePath := filepath.Join(ps.dataDir, dsdstate.FileName)
	saved := readBytes(t, statePath)
	log := watchLog(t)
	ps2 := rebootDSD(t, ps, box, "")
	availableOutput(t, ps2.runningEngine(t))
	stopped(t, ps2)
	if lines := migrationLines(log.all()); len(lines) != 0 {
		t.Errorf("the next start logged the migration's lines: %q", lines)
	}
	if again := readBytes(t, statePath); !bytes.Equal(again, saved) {
		t.Errorf("the next start wrote over the state the run saved:\n%s\n%s", saved, again)
	}
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
	d, _ := st.DAC(goBarKey(t), goBarStamp())
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
	if d, _ := st.DAC(goBarKey(t), goBarStamp()); d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 {
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
	d, _ := st.DAC(goBarKey(t), goBarStamp())
	if d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative || len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) {
		t.Errorf("the later start gave the GO bar's model %+v, want the captured keys' native with DSD64 acknowledged", d)
	}
}

// TestARecordOfAnotherUnitWaitsForItsOwnModel (B728 B6; B3's round 2): a
// record the reader refuses for the bound DAC only because it is of another
// model (identity_mismatch: here the manufacturer string, which the key does
// not hold) gives that DAC the fallback and its warning, and waits for its
// own model (record_model); when the binding resolves to a unit the reader
// takes it for, its maker's own, the record's rows are that unit's entry, at
// its first revision, the first unit's fallback left as its own and the key's
// warning dropped, in the same run and at a later start: the record's rows
// are never the first unit's, whose maker it does not name.
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
			if d, _ := st.DAC(key, goBarStamp()); d.Choice == nil || d.Choice.Revision != 1 || len(d.FirstUse) != 0 || len(st.Warnings) != 1 ||
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
			d, _ := st.DAC(key, dsdstate.StampOf("iFi Audio"))
			if c := d.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.Revision != 1 || c.SetBy != dsdstate.ByMigration ||
				len(d.FirstUse) != 1 || !reflect.DeepEqual(d.FirstUse[0].BitRates, []int{dsdstate.DSD64}) {
				t.Errorf("once the record's model came its unit holds %+v, want the record's rows at its first revision", d)
			}
			first, _ := st.DAC(key, goBarStamp())
			if c := first.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.Revision != 1 || c.SetBy != dsdstate.ByMigration ||
				len(first.FirstUse) != 0 || len(first.Listener) != 0 || first.DoPConfirmed != nil {
				t.Errorf("the first unit, of another maker, holds %+v, want its own fallback as it was", first)
			}
			if len(st.DACs) != 2 {
				t.Errorf("the state holds %d entries, want the first unit's and the record's model's", len(st.DACs))
			}
			if len(st.Warnings) != 0 {
				t.Errorf("the record's rows kept the fallback's warning: %+v", st.Warnings)
			}
		})
	}
}
