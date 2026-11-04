package updater

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheRollbacksRefusalOnItsReadOfTheMarkerIsTheConsoles: RollbackRefusalOn,
// which the console's Updates panel asks of its own read of the install
// marker (backlog B707), is the refusal the operator's rollback makes on its
// read: RollbackRefusal's for a marker it read, and for one it cannot read (a
// directory where the marker goes), a refusal in words that name no path,
// which the rollback's answer carries with the read's own error after them,
// said without its path and kept for code; nothing is written or renamed.
func TestTheRollbacksRefusalOnItsReadOfTheMarkerIsTheConsoles(t *testing.T) {
	opened := State{Status: "installed", TargetVersion: "0.2.3", PreviousVersion: "0.2.1", DataDirOpenedBy: "0.2.3"}
	for _, st := range []State{{}, opened, {DataDirOpenedBy: "0.2.1", PreviousVersion: "0.2.1"}} {
		if got, want := RollbackRefusalOn(st, nil), RollbackRefusal(st); got != want {
			t.Errorf("RollbackRefusalOn(%+v, nil) = %q, want RollbackRefusal's %q", st, got, want)
		}
	}

	l := newRollbackLayout(t)
	if err := os.Remove(StatePath(l.dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(StatePath(l.dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	_, readErr := LoadState(l.dataDir)
	if readErr == nil {
		t.Fatal("a directory where the marker goes reads as a marker; the case needs one that cannot be read")
	}
	why := RollbackRefusalOn(State{}, readErr)
	if why == "" || strings.Contains(why, l.dataDir) {
		t.Fatalf("the refusal of a marker that cannot be read = %q, want one that names no path", why)
	}
	before := l.files()
	err := New(Options{}).Rollback(context.Background(), l.opts())
	if !errors.Is(err, ErrRollbackRefused) || !strings.Contains(err.Error(), why) {
		t.Fatalf("Rollback over a marker it cannot read = %v, want its refusal, saying %q", err, why)
	}
	if strings.Contains(err.Error(), l.dataDir) || strings.Contains(err.Error(), filepath.Dir(l.live)) {
		t.Errorf("the refusal %q names a path", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("the refusal %q does not keep the read's error for code", err)
	}
	l.requireUnchanged(t, before)
}
