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
