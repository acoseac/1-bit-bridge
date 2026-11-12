package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
)

// TestTheRestoresWordsSpellTheSnapshotAsItWasGiven (backlog B826's third
// round): spelledAsGiven, which gives what a restore answers once it holds its
// snapshot words that name the snapshot as the restore was given it, spells
// each path below the snapshot where it resolves below the path given, as
// filepath.Join spells it (a given "." as the name alone), keeps the error's
// chain for errors.Is and errors.As, and answers the error itself where its
// words name nothing below the snapshot (a neighbour whose name begins with
// the snapshot's among them), where the snapshot was given as it resolves, so
// that the words would not change, or where the snapshot is a root, below
// which every path is.
func TestTheRestoresWordsSpellTheSnapshotAsItWasGiven(t *testing.T) {
	snap := filepath.FromSlash("/real/backups/2026-11-12T00-00-00Z")
	given := filepath.FromSlash("/link/2026-11-12T00-00-00Z")
	inSnap := filepath.Join(snap, PlayerUUIDFileName)

	pathErr := &fs.PathError{Op: "open", Path: inSnap, Err: fs.ErrNotExist}
	err := spelledAsGiven(fmt.Errorf("restore %s: %w; nothing was restored", PlayerUUIDFileName, pathErr), snap, given)
	want := "restore " + PlayerUUIDFileName + ": open " + filepath.Join(given, PlayerUUIDFileName) + ": file does not exist; nothing was restored"
	if err == nil || err.Error() != want {
		t.Errorf("spelled as given: %v, want %q", err, want)
	}
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) || pe != pathErr {
		t.Errorf("the respelled error lost its chain: %v", err)
	}

	if err := spelledAsGiven(pathErr, snap, "."); err == nil || err.Error() != "open "+PlayerUUIDFileName+": file does not exist" {
		t.Errorf("spelled as given %q: %v, want the name alone", ".", err)
	}

	neighbour := &fs.PathError{Op: "open", Path: filepath.Join(snap+"-1", ManifestFile), Err: fs.ErrNotExist}
	for name, tc := range map[string]struct {
		err  error
		snap string
	}{
		"nothing below the snapshot":  {fmt.Errorf("%w; nothing was restored", ErrBackupsBusy), snap},
		"a neighbour of the snapshot": {neighbour, snap},
		"a snapshot at a root":        {pathErr, filepath.Dir(filepath.Dir(filepath.Dir(snap)))},
		"no snapshot":                 {pathErr, ""},
	} {
		if got := spelledAsGiven(tc.err, tc.snap, given); got != tc.err {
			t.Errorf("%s: spelledAsGiven answered %v, want the error itself", name, got)
		}
	}
	for name, as := range map[string]string{"as it resolves": snap, "with a separator after it": snap + string(filepath.Separator)} {
		if got := spelledAsGiven(pathErr, snap, as); got != error(pathErr) {
			t.Errorf("%s: spelledAsGiven answered %v, want the error itself", name, got)
		}
	}
	if spelledAsGiven(nil, snap, given) != nil {
		t.Error("spelledAsGiven(nil) is not nil")
	}
}
