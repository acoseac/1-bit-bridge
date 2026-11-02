package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// errBarrier is the directory barrier's failure in these tests.
var errBarrier = errors.New("the directory would not sync")

// failMarkerBarrier makes the marker's directory barrier fail for the rest of
// the test, and records the paths it was asked to sync.
func failMarkerBarrier(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	prev := syncMarkerDir
	syncMarkerDir = func(path string) error {
		asked = append(asked, path)
		return errBarrier
	}
	t.Cleanup(func() { syncMarkerDir = prev })
	return &asked
}

// TestSaveStateReportsADirectoryBarrierThatFails: the marker drives both
// rollbacks, and a write whose directory entry may not outlive a power loss
// is not a write the updater can count on, as a file whose own fsync failed
// is not (backlog B613's round 2, Codex's finding 4).
func TestSaveStateReportsADirectoryBarrierThatFails(t *testing.T) {
	dir := t.TempDir()
	asked := failMarkerBarrier(t)
	err := SaveState(dir, State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1"})
	if !errors.Is(err, errBarrier) {
		t.Fatalf("SaveState = %v, want the barrier's failure", err)
	}
	if len(*asked) != 1 || filepath.Dir((*asked)[0]) != dir {
		t.Errorf("the barrier was asked of %q, want the marker in %s", *asked, dir)
	}
}

func TestClearStateDurablyReportsADirectoryBarrierThatFails(t *testing.T) {
	dir := t.TempDir()
	if err := SaveState(dir, State{Status: "installing", TargetVersion: "0.2.3"}); err != nil {
		t.Fatal(err)
	}
	asked := failMarkerBarrier(t)
	if err := ClearStateDurably(dir); !errors.Is(err, errBarrier) {
		t.Fatalf("ClearStateDurably = %v, want the barrier's failure", err)
	}
	if len(*asked) != 1 || filepath.Dir((*asked)[0]) != dir {
		t.Errorf("the barrier was asked of %q, want the marker in %s", *asked, dir)
	}
	if _, err := os.Lstat(StatePath(dir)); !os.IsNotExist(err) {
		t.Errorf("the marker is still there (lstat %v)", err)
	}
}
