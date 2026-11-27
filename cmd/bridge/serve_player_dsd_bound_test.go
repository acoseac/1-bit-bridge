package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// nearBoundState writes into dir a markerless player's DSD state whose file is
// under bytes short of the 1 MiB its reader reads, one DAC entry whose
// product string makes up the size, through the state's own writer.
func nearBoundState(t *testing.T, dir string, under int) {
	t.Helper()
	write := func(n int) int64 {
		s, err := dsdstate.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		iface, product := 1, strings.Repeat("x", n)
		k, err := dacprofile.NewKey(0x1234, 0x5678, &iface, &product)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(func(st *dsdstate.State) error {
			st.DACs = nil
			st.Entry(k, dsdstate.StampOf("Test")).Choice = &dsdstate.Choice{Mode: dsdstate.ModeNative, Revision: 1, SetAt: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC), SetBy: dsdstate.ByApp}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, dsdstate.FileName))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	target := int64(1<<20 - under)
	if got := write(int(target-write(1)) + 1); got != target {
		t.Fatalf("the fixture's file is %d bytes, want %d", got, target)
	}
}

// TestAStartStepPastTheReadersBoundSaysSo (B1042 round 2): a start whose
// markerless state is 20 bytes under the 1 MiB its reader reads, and dsdMode
// off, whose marker, default and warning take the state past it: the step's
// error is ErrTooLarge, the store holds off and the marker for the run, and the
// start's line says the state with the migration encodes to more than its file
// may hold, that the player holds it for this run alone, and the default it
// plays by.
func TestAStartStepPastTheReadersBoundSaysSo(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	dir := t.TempDir()
	nearBoundState(t, dir, 20)
	log := watchLog(t)
	d := openPlayerDSD(dir, config.PlayerConfig{DSDMode: "off"}, goBarTrees(t), nil)
	if err := d.migrate(context.Background(), nil, nil); !errors.Is(err, dsdstate.ErrTooLarge) {
		t.Fatalf("the start step past the bound: %v, want ErrTooLarge", err)
	}
	if st := d.store.State(); st.DefaultMode != dsdstate.ModeOff || st.MigratedFrom == nil || st.MigratedFrom.DSDMode != dsdstate.ModeOff {
		t.Fatalf("the store holds defaultMode %q and the marker %+v, want off and its marker for the run", st.DefaultMode, st.MigratedFrom)
	}
	line := log.await(t, "player: the migration of the legacy DSD keys was not saved")
	for _, want := range []string{"encodes to more than the 1 MiB", "the player holds it for this run alone", "defaultMode=off"} {
		if !strings.Contains(line, want) {
			t.Errorf("the start's line does not say %q: %.400s", want, line)
		}
	}
}
