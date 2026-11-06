package dsdstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestAWriteThatLandsWithoutItsBarrierFencesEveryLaterWrite (B728 B7): a
// write whose rename lands and whose barrier then fails is in the file and
// not in the store, which keeps the state as it was; every write after it,
// an update as playback's evidence makes one and a migration step alike, is
// refused with ErrLandedNotHeld, never made from the store's state, so the
// file keeps the state that landed until the next Open reads it.
func TestAWriteThatLandsWithoutItsBarrierFencesEveryLaterWrite(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := openAt(t, dir, options{})
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if st, err := Decode(b); err == nil && st.DefaultMode == ModeOff && failed.CompareAndSwap(false, true) {
			return errors.New("the test's barrier fails once the first write is there")
		}
		return nil
	}))
	if err := s.Update(func(st *State) error { st.DefaultMode = ModeOff; return nil }); !errors.Is(err, atomicwrite.ErrLanded) {
		t.Fatalf("the first write: %v, want it landed and not known to be durable", err)
	}
	if st := s.State(); st.DefaultMode != ModeAuto || s.Landed() == nil {
		t.Fatalf("the store holds %+v (landed %v), want the state as it was and the landed write kept", st, s.Landed())
	}
	key := brooklyn(t)
	err := s.Update(func(st *State) error {
		st.DACs = append(st.DACs, DAC{Key: key, Evidence: []Evidence{{Transport: TransportNative, Format: "DSD_U32_BE", BitRate: DSD64, BCDDevice: "0100", DriverAcceptedAt: t0, Kernel: "6.12.1"}}})
		return nil
	})
	if !errors.Is(err, ErrLandedNotHeld) {
		t.Errorf("an evidence write after the landed one: %v, want ErrLandedNotHeld", err)
	}
	if _, err := s.Migrate(dir, Legacy{DSDMode: ModeNative}, nil); !errors.Is(err, ErrLandedNotHeld) {
		t.Errorf("a migration step after the landed write: %v, want ErrLandedNotHeld", err)
	}
	if st := fileState(t, dir); st.DefaultMode != ModeOff || len(st.DACs) != 0 || st.MigratedFrom != nil {
		t.Errorf("the file holds %+v, want the state that landed, written over by nothing", st)
	}
	if again := openAt(t, dir, options{}); again.State().DefaultMode != ModeOff || again.Landed() != nil {
		t.Errorf("the next Open reads %+v (landed %v), want the landed state and no fence", again.State(), again.Landed())
	}
}

// TestADamagedStatesReplacementThatLandsIsNotSaidToBeInMemoryAlone (B728 B6
// round 2; Codex): a damaged file's fresh state whose rename lands and whose
// barrier then fails is in the file, not in memory alone: Report.Kept is
// marked atomicwrite.ErrLanded and says the fresh state is in the file, not
// known to be durable; the file decodes as the refusing state, and the next
// Open loads it, setting nothing aside again.
func TestADamagedStatesReplacementThatLandsIsNotSaidToBeInMemoryAlone(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("not a state"), 0o600); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if _, err := Decode(b); err == nil && failed.CompareAndSwap(false, true) {
			return errors.New("the test's barrier fails once the fresh state is there")
		}
		return nil
	}))
	rep := openAt(t, dir, options{}).Report()
	if rep.Outcome != Damaged || rep.Aside == "" || rep.Kept == nil {
		t.Fatalf("the report %+v, want a damaged file kept aside, its replacement's failure in Kept", rep)
	}
	if !errors.Is(rep.Kept, atomicwrite.ErrLanded) || strings.Contains(rep.Kept.Error(), "in memory alone") || !strings.Contains(rep.Kept.Error(), "not known to be durable") {
		t.Errorf("Kept is %q, want it marked ErrLanded and saying the fresh state is in the file, not known to be durable", rep.Kept)
	}
	if st := fileState(t, dir); !st.Unreadable() || st.MigratedFrom == nil || st.MigratedFrom.From != FromStateUnreadable {
		t.Errorf("the file holds %+v, want the refusing state that landed", st)
	}
	if again := openAt(t, dir, options{}).Report(); again.Outcome != Loaded {
		t.Errorf("the next Open found %+v, want the fresh state loaded", again)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, FileName+".bad-*")); len(matches) != 1 {
		t.Errorf("the damaged bytes are kept in %v, want one copy", matches)
	}
}
