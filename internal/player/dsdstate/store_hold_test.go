package dsdstate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// The migration's start step held for its run when its write fails (B1042,
// and B1043's part 1): the part of the bridge's (Migrate with no DAC bound)
// writes the marker and the default; where that write fails, before it lands
// or once it has landed and is not known to be durable, the store holds the
// state the step made, its marker with it, so the run plays by the opt-out
// the operator set in 0.2.3, and a later write of the run persists that state
// whole. A part of a DAC's that fails is not held.

// failingWrite is a store's writer that refuses each write while fail holds,
// before anything lands (a full disk, an I/O error), and otherwise writes as
// the store's own writer does.
func failingWrite(dir string, fail *atomic.Bool) func(string, []byte) error {
	return func(path string, data []byte) error {
		if fail.Load() {
			return errors.New("the test's disk refuses the write")
		}
		return atomicwrite.WriteBytesDurable(dir, path, data, tmpPrefix)
	}
}

// optOuts are the legacy keys whose start step decides what the run plays
// before any DAC is bound: the explicit opt-outs the migration turns into
// defaultMode off, and auto without allowDoP, whose marker keeps DoP out
// (B1043).
var optOuts = []struct {
	name   string
	legacy Legacy
	mode   string
}{
	{"dsdMode off", Legacy{DSDMode: ModeOff}, ModeOff},
	{"dop with allowDoP false", Legacy{DSDMode: ModeDoP, AllowDoP: ptr(false)}, ModeOff},
	{"dop with allowDoP unset", Legacy{DSDMode: ModeDoP}, ModeOff},
	{"auto with allowDoP false", Legacy{DSDMode: ModeAuto, AllowDoP: ptr(false)}, ModeAuto},
	{"auto with allowDoP unset", Legacy{DSDMode: ModeAuto}, ModeAuto},
}

// startStep is what the start step makes of the keys, through the pure table.
func startStep(t *testing.T, l Legacy) State {
	t.Helper()
	want, step, err := Migrate(Defaults(), Input{Legacy: l, Now: t0})
	if err != nil || !step.Changed || want.MigratedFrom == nil {
		t.Fatalf("the start step of %+v made %s (changed %v, %v), want a marker", l, show(t, want), step.Changed, err)
	}
	return want
}

// noFile fails t unless dir holds no state file.
func noFile(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the state file is there (%v), want none: the write did not land", err)
	}
}

// TestAStartStepWhoseWriteFailsIsHeldForItsRun (B1042, B1043 part 1): the
// start step's write refused before it lands leaves no file and the store
// holding the step's state, its marker and its default included, which the
// effective policy reads at every refresh (so the run refuses DSD under a
// migrated off and keeps DoP out under auto without allowDoP); a later write
// of the run on a disk that works again persists that state with its marker,
// never the default without it; and the next start, whatever the keys say
// then, finds the migration done.
func TestAStartStepWhoseWriteFailsIsHeldForItsRun(t *testing.T) {
	durable(t)
	for _, tc := range optOuts {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var fail atomic.Bool
			fail.Store(true)
			s := openAt(t, dir, options{write: failingWrite(dir, &fail)})
			want := startStep(t, tc.legacy)
			step, err := s.Migrate(dir, tc.legacy, nil)
			if err == nil || errors.Is(err, atomicwrite.ErrLanded) || !step.Changed {
				t.Fatalf("the start step: %v (changed %v), want its write failed before it landed", err, step.Changed)
			}
			noFile(t, dir)
			got := s.State()
			if !reflect.DeepEqual(got.normalized(), want.normalized()) {
				t.Fatalf("the store holds\n%s\nwant the step's state, its marker with it,\n%s", show(t, got), show(t, want))
			}
			if got.DefaultMode != tc.mode || s.Landed() != nil {
				t.Fatalf("defaultMode %q (fence %v), want %q and no fence", got.DefaultMode, s.Landed(), tc.mode)
			}
			// A later write of the run, the disk working again: a user's choice
			// for another DAC.
			fail.Store(false)
			if err := s.Update(func(st *State) error {
				st.Entry(brooklyn(t), mytek).Choice = &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			file := fileState(t, dir)
			if file.DefaultMode != tc.mode || !reflect.DeepEqual(file.MigratedFrom, want.MigratedFrom) || !reflect.DeepEqual(file.Warnings, want.Warnings) {
				t.Fatalf("the later write left\n%s\nwant the held marker, default and warnings with the choice", show(t, file))
			}
			// The next start: the keys no longer set, or set as they were, the
			// migration is done.
			for _, l := range []Legacy{{}, tc.legacy} {
				again := openAt(t, dir, options{})
				if step, err := again.Migrate(dir, l, nil); err != nil || step.Changed {
					t.Fatalf("the next start's step with the keys %+v: changed %v, %v; want nothing, the migration done", l, step.Changed, err)
				}
				if st := again.State(); st.DefaultMode != tc.mode || !reflect.DeepEqual(st.MigratedFrom, want.MigratedFrom) {
					t.Fatalf("the next start holds %s, want the marker and defaultMode %q", show(t, st), tc.mode)
				}
			}
		})
	}
}

// TestAStartStepNotSavedIsMigratedAgainByTheNextStart (B1042): a start step
// held in memory and never saved leaves no file, so the next start migrates
// the keys again, as before B1042, and saves them on a disk that works.
func TestAStartStepNotSavedIsMigratedAgainByTheNextStart(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	var fail atomic.Bool
	fail.Store(true)
	l := Legacy{DSDMode: ModeOff}
	if _, err := openAt(t, dir, options{write: failingWrite(dir, &fail)}).Migrate(dir, l, nil); err == nil {
		t.Fatal("the start step's write was not refused")
	}
	noFile(t, dir)
	next := openAt(t, dir, options{})
	if rep := next.Report(); rep.Outcome != Missing {
		t.Fatalf("the next start found %+v, want no file", rep)
	}
	step, err := next.Migrate(dir, l, nil)
	if err != nil || !step.Changed {
		t.Fatalf("the next start's step: changed %v, %v; want the keys migrated again", step.Changed, err)
	}
	want := startStep(t, l)
	if file := fileState(t, dir); !reflect.DeepEqual(file.normalized(), want.normalized()) {
		t.Fatalf("the next start saved\n%s\nwant\n%s", show(t, file), show(t, want))
	}
}

// TestALandedStartStepIsHeldBehindItsFence (B1042, B1043 part 1): the start
// step's write that lands and whose barrier then fails is in the file and held
// by the store, so the run plays by it; the fence stands as for any landed
// write (ErrLandedNotHeld for every later write, a DAC's part among them), the
// file keeps what landed, and the next start reads it and migrates nothing.
func TestALandedStartStepIsHeldBehindItsFence(t *testing.T) {
	durable(t)
	for _, tc := range optOuts {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			var failed atomic.Bool
			t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
				if _, err := os.Lstat(path); err == nil && failed.CompareAndSwap(false, true) {
					return errors.New("the test's barrier fails once the state's file is there")
				}
				return nil
			}))
			s := openAt(t, dir, options{})
			want := startStep(t, tc.legacy)
			if _, err := s.Migrate(dir, tc.legacy, nil); !errors.Is(err, atomicwrite.ErrLanded) {
				t.Fatalf("the start step: %v, want it landed and not known to be durable", err)
			}
			if got := s.State(); !reflect.DeepEqual(got.normalized(), want.normalized()) {
				t.Fatalf("the store holds\n%s\nwant the step's state that landed\n%s", show(t, got), show(t, want))
			}
			if s.Landed() == nil {
				t.Fatal("no fence after a landed write")
			}
			if err := s.Update(func(st *State) error { st.DefaultMode = ModeAuto; return nil }); !errors.Is(err, ErrLandedNotHeld) {
				t.Errorf("a write after the landed step: %v, want ErrLandedNotHeld", err)
			}
			if _, err := s.Migrate(dir, tc.legacy, bound(t)); !errors.Is(err, ErrLandedNotHeld) {
				t.Errorf("a DAC's part after the landed step: %v, want ErrLandedNotHeld", err)
			}
			if file := fileState(t, dir); !reflect.DeepEqual(file.normalized(), want.normalized()) {
				t.Fatalf("the file holds\n%s\nwant what landed, written over by nothing\n%s", show(t, file), show(t, want))
			}
			again := openAt(t, dir, options{})
			if st := again.State(); !reflect.DeepEqual(st.normalized(), want.normalized()) || again.Landed() != nil {
				t.Fatalf("the next start reads %s (fence %v), want what landed and no fence", show(t, st), again.Landed())
			}
			if step, err := again.Migrate(dir, tc.legacy, nil); err != nil || step.Changed {
				t.Fatalf("the next start's step: changed %v, %v; want nothing", step.Changed, err)
			}
		})
	}
}

// TestADACPartWhoseWriteFailsIsNotHeld (B1042's control): a part of a DAC's
// whose write fails leaves the state as it was, its marker still waiting for
// the DAC, so its caller asks it again (cmd/bridge's playerDSD.retry); and a
// step refused before any write (keys the migration does not take) holds
// nothing either.
func TestADACPartWhoseWriteFailsIsNotHeld(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	var fail atomic.Bool
	s := openAt(t, dir, options{write: failingWrite(dir, &fail)})
	l := Legacy{DSDMode: ModeNative}
	if _, err := s.Migrate(dir, l, nil); err != nil {
		t.Fatal(err)
	}
	before := s.State()
	if before.MigratedFrom == nil || before.MigratedFrom.WaitingFor != WaitingForBoundDAC {
		t.Fatalf("the start step left %s, want the DAC's part waiting", show(t, before))
	}
	fail.Store(true)
	step, err := s.Migrate(dir, l, bound(t))
	if err == nil {
		t.Fatal("the DAC's part's write was not refused")
	}
	if !step.Changed {
		t.Errorf("the DAC's part reports changed %v, want the change its write did not make", step.Changed)
	}
	if got := s.State(); !reflect.DeepEqual(got, before) {
		t.Fatalf("after the DAC's part failed the store holds\n%s\nwant the state as it was\n%s", show(t, got), show(t, before))
	}
	if _, ask := RecordToRead(s.State(), l); !ask {
		t.Error("after the DAC's part failed the record is not asked again")
	}
	if file := fileState(t, dir); !reflect.DeepEqual(file.normalized(), before.normalized()) {
		t.Errorf("the file holds %s, want the start step's alone", show(t, file))
	}

	fresh := openAt(t, t.TempDir(), options{})
	if _, err := fresh.Migrate(dir, Legacy{DSDMode: "loud"}, nil); err == nil {
		t.Fatal("a legacy mode the migration does not take was migrated")
	}
	if st := fresh.State(); !reflect.DeepEqual(st, Defaults()) {
		t.Errorf("a step refused before its write holds %s, want the defaults", show(t, st))
	}
}
