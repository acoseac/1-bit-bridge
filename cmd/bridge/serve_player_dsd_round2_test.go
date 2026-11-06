package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// Round 2 of B728 B6's review: a part for a DAC asked again after a read that
// failed, a stop before the state is opened, a damaged state's replacement
// that landed, and a DAC part's write that failed, refused or landed.

// asides are the copies of a damaged state the data directory holds.
func asides(t *testing.T, dataDir string) []string {
	t.Helper()
	var out []string
	for _, n := range stateFiles(t, dataDir) {
		if strings.HasPrefix(n, dsdstate.FileName+".bad-") {
			out = append(out, n)
		}
	}
	return out
}

// TestAPendingDACPartIsAskedAgainOnceItsDescriptorsRead (B728 B6 round 2;
// Codex): a part for a DAC whose descriptors cannot be read when the watcher
// first resolves it is asked again on the supervisor's own tick, bounded,
// so once the read succeeds the part is made with no new snapshot: no replug,
// no other change of the box, which an unchanged DAC would never give.
func TestAPendingDACPartIsAskedAgainOnceItsDescriptorsRead(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	id := goBarLiveIdentity(t, box.trees)
	manufacturer := filepath.Join(box.trees.sysfs, qualifyGoBar, "manufacturer")
	if err := os.Remove(manufacturer); err != nil {
		t.Fatal(err)
	}
	log := watchLog(t)
	var record qualification.Approval
	ps := bootDSD(t, box, "    dsdMode: native\n    dsdQualification: approved.json\n", func(dataDir string) {
		record = legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
	})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	log.await(t, "its model could not be read")
	snap, ok := ps.player.feed.Latest()
	if !ok {
		t.Fatal("the watcher published nothing")
	}
	if err := os.WriteFile(manufacturer, []byte("iFi"), 0o600); err != nil {
		t.Fatal(err)
	}
	log.await(t, "player: the legacy DSD keys were migrated into the player's DSD state", "dac=20b1:3009 GO bar")
	if now, _ := ps.player.feed.Latest(); now.Generation() != snap.Generation() {
		t.Errorf("the watcher published generation %d after %d: the part was asked by a reading, not by the supervisor's tick", now.Generation(), snap.Generation())
	}
	stopped(t, ps)
	st, _ := dsdStateIn(t, ps.dataDir)
	if m := st.MigratedFrom; m == nil || m.WaitingFor != "" || m.RecordSHA256 != record.SHA256 {
		t.Errorf("the marker %+v, want the record taken once the descriptors read", st.MigratedFrom)
	}
}

// TestAStopBeforeTheSupervisorRunsOpensNoState (B728 B6 round 2; Codex): a
// stop that began before the player's supervisor ran its start ends the start
// before the state is opened, so a damaged player-dsd.json is left as it is,
// no copy set aside and no fresh state written over it, and the player never
// runs.
func TestAStopBeforeTheSupervisorRunsOpensNoState(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	log := watchLog(t)
	damaged := []byte("not a state")
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n", standIn: newPlayerStandIn(),
		before: func(dataDir string) {
			if err := os.WriteFile(filepath.Join(dataDir, dsdstate.FileName), damaged, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		with: func(o *serveOpts) {
			box.with(o)
			boot := o.playerBoot
			o.playerBoot = func(p *bridgePlayer) {
				boot(p)
				// The player's stop, begun before its supervisor is scheduled: the
				// cancel serve's teardown makes first (bridgePlayer.stop).
				if p.cancel != nil {
					p.cancel()
				}
			}
		}})
	if eng := ps.engine(t); eng != nil {
		t.Error("the player ran after its stop had begun")
	}
	stopped(t, ps)
	if files := stateFiles(t, ps.dataDir); len(files) != 1 || files[0] != dsdstate.FileName {
		t.Errorf("a stop before the start left %v, want the damaged file alone", files)
	}
	if got := readBytes(t, filepath.Join(ps.dataDir, dsdstate.FileName)); !bytes.Equal(got, damaged) {
		t.Errorf("a stop before the start replaced the damaged file: %q", got)
	}
	if strings.Contains(log.all(), "player: the player's DSD state did not read") {
		t.Errorf("a stop before the start opened the state:\n%s", log.all())
	}
}

// TestALandedReplacementOfADamagedStateIsSaidToBeInTheFile (B728 B6 round 2;
// Codex): a damaged player-dsd.json set aside, its fresh state's rename landed
// and the barrier after it failed (atomicwrite.ErrLanded): the Warn line says
// the fresh state took its place in the file and is not known to be durable,
// never that it could not be written or is in memory alone; the file holds
// the refusing state, which the next start loads, setting nothing aside again.
func TestALandedReplacementOfADamagedStateIsSaidToBeInTheFile(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	log := watchLog(t)
	var statePath string
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		if statePath == "" {
			return nil
		}
		b, err := os.ReadFile(statePath)
		if err != nil {
			return nil
		}
		if _, err := dsdstate.Decode(b); err == nil && failed.CompareAndSwap(false, true) {
			return errors.New("the test's barrier fails once the fresh state is there")
		}
		return nil
	}))
	ps := bootDSD(t, box, "    dsdMode: native\n", func(dataDir string) {
		statePath = filepath.Join(dataDir, dsdstate.FileName)
		if err := os.WriteFile(statePath, []byte("not a state"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	ps.runningEngine(t)
	line := log.await(t, "player: the player's DSD state did not read")
	if !strings.Contains(line, "not known to be durable") || strings.Contains(line, "could not be written") || strings.Contains(line, "in memory alone") {
		t.Errorf("a replacement that landed is said not to be in the file: %s", line)
	}
	stopped(t, ps)
	st, there := dsdStateIn(t, ps.dataDir)
	if !there || !st.Unreadable() {
		t.Fatalf("the file holds %+v (there %v), want the refusing state that landed", st, there)
	}
	if got := asides(t, ps.dataDir); len(got) != 1 {
		t.Fatalf("the damaged bytes are kept in %v, want one copy", got)
	}
	log2 := watchLog(t)
	ps2 := rebootDSD(t, ps, box, "    dsdMode: native\n")
	ps2.runningEngine(t)
	stopped(t, ps2)
	if strings.Contains(log2.all(), "did not read") || len(asides(t, ps.dataDir)) != 1 {
		t.Errorf("the next start did not load the fresh state that landed: %v\n%s", asides(t, ps.dataDir), log2.all())
	}
}

// TestADACPartsRefusedWriteIsAskedAgainInThisRun (B728 B6 round 2; the
// Claude API reviewer): a part for a DAC whose write is refused before it
// lands is said not saved, in words for a player that plays on, and asked
// again in this run, on the supervisor's tick, where it is made.
func TestADACPartsRefusedWriteIsAskedAgainInThisRun(t *testing.T) {
	needsDirectorySync(t)
	box := newDSDBox(t)
	log := watchLog(t)
	// Every durable write is refused from the part's first step to its second,
	// a retry's: the part's own write among them.
	var refusing atomic.Bool
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
		if refusing.Load() {
			return errors.New("the test's medium refuses this write")
		}
		return nil
	}))
	var dacSteps atomic.Int32
	seams := &servePlayerSeams{migrates: func(_ context.Context, b *dsdstate.Bound) {
		if b != nil {
			refusing.Store(dacSteps.Add(1) == 1)
		}
	}}
	id := goBarLiveIdentity(t, box.trees)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", standIn: newPlayerStandIn(),
		seams: seams, with: box.with, before: func(dataDir string) { legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64)) }})
	availableOutput(t, ps.runningEngine(t))
	line := log.await(t, "was not saved")
	if !strings.Contains(line, "the DSD migration's part for the bound DAC was not saved") || !strings.Contains(line, "plays on") ||
		!strings.Contains(line, "asked again in this run") || strings.Contains(line, "the player starts") {
		t.Fatalf("a DAC part's refused write is said as %s; want the part named, the player playing on, and it asked again in this run", line)
	}
	log.await(t, "player: the legacy DSD keys were migrated into the player's DSD state", "dac=20b1:3009 GO bar")
	stopped(t, ps)
	if n := dacSteps.Load(); n != 2 {
		t.Errorf("the part was asked %d times, want twice: refused, then made", n)
	}
	if st, _ := dsdStateIn(t, ps.dataDir); st.MigratedFrom == nil || st.MigratedFrom.WaitingFor != "" {
		t.Errorf("after the retry the marker is %+v, want the part done", st.MigratedFrom)
	}
}

// TestADACPartsLandedWriteIsNotMadeAgainInThisRun (B728 B6 round 2; the
// Claude API reviewer): a part for a DAC whose write lands and whose barrier
// then fails is in the file, not in the store's memory, so this run asks it
// no more: a later snapshot, the owner's record put in place meanwhile, makes
// nothing again and writes nothing over the file. Its line is for a player
// that plays on, the warnings it wrote are logged beside it, and the next
// start goes on from the file, where the part is done.
func TestADACPartsLandedWriteIsNotMadeAgainInThisRun(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	id := goBarLiveIdentity(t, box.trees)
	log := watchLog(t)
	var statePath string
	var before atomic.Pointer[[]byte]
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		at := before.Load()
		if at == nil {
			return nil
		}
		now, err := os.ReadFile(statePath)
		if err == nil && !bytes.Equal(now, *at) && failed.CompareAndSwap(false, true) {
			return errors.New("the test's barrier fails once the part's write has landed")
		}
		return nil
	}))
	var dacSteps atomic.Int32
	seams := &servePlayerSeams{migrates: func(_ context.Context, b *dsdstate.Bound) {
		if b != nil && dacSteps.Add(1) == 1 {
			cur, err := os.ReadFile(statePath)
			if err == nil {
				before.Store(&cur)
			}
		}
	}}
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", standIn: newPlayerStandIn(),
		seams: seams, with: box.with, before: func(dataDir string) { statePath = filepath.Join(dataDir, dsdstate.FileName) }})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	line := log.await(t, "is not known to be durable")
	if !strings.Contains(line, "the DSD migration's part for the bound DAC") || !strings.Contains(line, "plays on") ||
		!strings.Contains(line, "this run asks it no more") || strings.Contains(line, "the player starts") {
		t.Fatalf("a DAC part's landed write is said as %s; want the part named, the player playing on, and it asked no more in this run", line)
	}
	log.await(t, "player: the DSD migration recorded a warning in the player's DSD state", "code="+dsdstate.WarningMigrationRecordMissing)
	landed := readBytes(t, statePath)
	// The owner's record put in place, and the DAC plugged in again: a new
	// snapshot, which asks nothing more in this run.
	legacyRecordIn(t, ps.dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64))
	box.unplug(t)
	box.wake()
	awaitEngine(t, eng, "the output gone", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
	box.plug(t, "3", "iFi")
	box.wake()
	availableOutput(t, eng)
	stopped(t, ps)
	if n := dacSteps.Load(); n != 1 {
		t.Errorf("the part was made %d times in one run, want once", n)
	}
	if got := readBytes(t, statePath); !bytes.Equal(got, landed) {
		t.Errorf("the file the landed write left was written over:\n%s\n%s", landed, got)
	}
	log2 := watchLog(t)
	ps2 := rebootDSD(t, ps, box, "    dsdMode: native\n    dsdQualification: approved.json\n")
	availableOutput(t, ps2.runningEngine(t))
	stopped(t, ps2)
	if lines := migrationLines(log2.all()); len(lines) != 0 {
		t.Errorf("the next start migrated again over the file the landed write left: %q", lines)
	}
	if got := readBytes(t, statePath); !bytes.Equal(got, landed) {
		t.Errorf("the next start wrote over the part the file holds")
	}
}
