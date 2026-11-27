package dsdstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// The migration's start step at the bounds a state is held to (B1042's round
// 2): a markerless state that read, just under the 1 MiB its reader reads,
// which the step's marker, default and warning take past it; and one at the
// 512 warnings a state holds, to which the step would add migrated_off. Either
// way the run keeps the opt-out, and no state that fails its own check is held.

// TestAStartStepPastTheReadersBoundIsHeldForItsRun (B1042 round 2): the start
// step's state whose encoding would pass the reader's bound is refused with
// ErrTooLarge, nothing written, the file's bytes as they were, and held for the
// run, its marker and off with it, as a write that did not land is; a later
// write that does not make it smaller is refused alike, the held state kept,
// and one that does saves it, marker and off with it, which the next Open reads.
func TestAStartStepPastTheReadersBoundIsHeldForItsRun(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	near := sizedState(t, maxFileBytes-20)
	s := openAt(t, dir, options{})
	if err := s.Update(func(st *State) error { *st = near.Clone(); return nil }); err != nil {
		t.Fatalf("the markerless state under the bound does not write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil || len(before) != maxFileBytes-20 {
		t.Fatalf("the fixture's file is %d bytes, %v; want %d", len(before), err, maxFileBytes-20)
	}
	l := Legacy{DSDMode: ModeOff}
	if migrated, _, err := Migrate(near, Input{Legacy: l, Now: t0}); err != nil {
		t.Fatal(err)
	} else if _, err := migrated.encode(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the migrated fixture encodes (%v): the case pins nothing", err)
	}
	step, err := s.Migrate(dir, l, nil)
	if !errors.Is(err, ErrTooLarge) || !step.Changed {
		t.Fatalf("the start step past the bound: changed %v, %v; want ErrTooLarge", step.Changed, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatal("the refused start step changed the file's bytes")
	}
	want := Marker{From: FromKeys, DSDMode: ModeOff}
	held := s.State()
	if held.DefaultMode != ModeOff || held.MigratedFrom == nil || !reflect.DeepEqual(*held.MigratedFrom, want) {
		t.Fatalf("the store holds defaultMode %q and the marker %+v, want off and %+v for the run", held.DefaultMode, held.MigratedFrom, want)
	}
	if err := s.Update(func(st *State) error {
		st.Entry(brooklyn(t), mytek).Choice = &Choice{Mode: ModeConvert, Revision: 1, SetAt: t0, SetBy: ByApp}
		return nil
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a write that keeps the state past the bound: %v, want ErrTooLarge", err)
	}
	if !reflect.DeepEqual(s.State(), held) {
		t.Fatal("a refused write changed the held state")
	}
	if err := s.Update(func(st *State) error { st.DACs = nil; return nil }); err != nil {
		t.Fatalf("a write that makes the state smaller: %v", err)
	}
	st := openAt(t, dir, options{}).State()
	if st.DefaultMode != ModeOff || st.MigratedFrom == nil || !reflect.DeepEqual(*st.MigratedFrom, want) ||
		!slices.ContainsFunc(st.Warnings, func(w Warning) bool { return w.Code == WarningMigratedOff }) {
		t.Fatalf("the next Open reads defaultMode %q, the marker %+v and the warnings %+v; want off, %+v and migrated_off", st.DefaultMode, st.MigratedFrom, st.Warnings, want)
	}
}

// TestAStartStepAtTheWarningsCapKeepsTheOptOut (B1042 round 2; the review of
// the brief): a markerless state holding the 512 warnings a state may hold,
// and dsdMode off: the start step writes its marker and off and leaves its
// migrated_off out, so the state stays one its check takes, is written and is
// read again, where a 513th warning would have failed the write and lost the
// opt-out for the run.
func TestAStartStepAtTheWarningsCapKeepsTheOptOut(t *testing.T) {
	durable(t)
	dir := t.TempDir()
	full := State{DefaultMode: ModeAuto}
	for i := 0; len(full.Warnings) < maxWarnings; i++ {
		k := newKey(t, 0x1234, uint16(0x100+i), nil, nil)
		full.Warnings = append(full.Warnings,
			Warning{Code: WarningMigrationRecordMissing, DAC: &k, At: t0},
			Warning{Code: WarningMigrationRecordRefused, Reason: ReasonUnreadable, DAC: &k, At: t0})
	}
	s := openAt(t, dir, options{})
	if err := s.Update(func(st *State) error { *st = full.Clone(); return nil }); err != nil {
		t.Fatalf("the fixture at the warnings' cap does not write: %v", err)
	}
	step, err := s.Migrate(dir, Legacy{DSDMode: ModeOff}, nil)
	if err != nil || !step.Changed {
		t.Fatalf("the start step at the warnings' cap: changed %v, %v; want it written", step.Changed, err)
	}
	st := openAt(t, dir, options{}).State()
	if st.DefaultMode != ModeOff || st.MigratedFrom == nil || st.MigratedFrom.DSDMode != ModeOff || len(st.Warnings) != maxWarnings {
		t.Fatalf("read again: defaultMode %q, the marker %+v, %d warnings; want off, the marker and %d", st.DefaultMode, st.MigratedFrom, len(st.Warnings), maxWarnings)
	}
	if slices.ContainsFunc(st.Warnings, func(w Warning) bool { return w.Code == WarningMigratedOff }) {
		t.Error("migrated_off is in a state already at the cap")
	}
}
