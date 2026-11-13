package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// TestTheRestoresWordsSpellTheSnapshotAsItWasGiven (backlog B826's third
// round): spelledAsGiven, which gives what a restore answers once it holds its
// snapshot words that name the snapshot as the restore was given it, spells
// each path below the snapshot where it resolves below the path given, as
// filepath.Join spells it (a given "." as the name alone), keeps the error's
// chain for errors.Is and errors.As, and answers the error itself where its
// words name nothing below the snapshot (a neighbour whose name begins with
// the snapshot's among them), or where the snapshot was given as it resolves,
// so that the words would not change. A snapshot at a root is spelled as any
// is since backlog B871 (TestTheRestoresWordsSpellAQuotedPathAndARootAsGiven).
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

// asQuoted is p as %q writes it, without its quotes.
func asQuoted(p string) string {
	q := strconv.Quote(p)
	return q[1 : len(q)-1]
}

// TestTheRestoresWordsSpellAQuotedPathAndARootAsGiven is backlog B871's items
// 11 and 13 at spelledAsGiven: a path below the snapshot that the words quote
// (%q, which on Windows doubles every separator, and elsewhere escapes a
// backslash a name holds) is spelled below the path given as %q writes that,
// a separator after the given path or none alike, with exactly one separator
// between it and the name; a snapshot at a root (Windows' E:\, or /) is
// spelled as any snapshot is, where until then its words were left as they
// were; and a match of the snapshot's spelling that begins no path (inside
// another path, or inside words such as "input/output") is left as it is.
func TestTheRestoresWordsSpellAQuotedPathAndARootAsGiven(t *testing.T) {
	// Paths %q writes otherwise than as they are.
	snap, given := `/real/back\up/2026-11-12T00-00-00Z`, `/link/le\ft/2026-11-12T00-00-00Z`
	root, givenRoot := "/", "/recovery"
	if runtime.GOOS == "windows" {
		snap, given = `C:\Users\runneradmin\AppData\Local\Temp\001\backups\2026-11-12T00-00-00Z`, `C:\Users\RUNNER~1\AppData\Local\Temp\001\backups\2026-11-12T00-00-00Z`
		root, givenRoot = `E:\`, `C:\recovery`
	}
	if asQuoted(snap) == snap || asQuoted(given) == given {
		t.Fatalf("premise: %q and %q are written by %%q as they are", snap, given)
	}
	file := filepath.Join(snap, PlayerUUIDFileName)
	for _, as := range []string{given, given + string(filepath.Separator)} {
		err := spelledAsGiven(fmt.Errorf("restore %s: open %q: %w; copy %s", PlayerUUIDFileName, file, fs.ErrPermission, file), snap, as)
		want := fmt.Sprintf("restore %s: open %q: %v; copy %s", PlayerUUIDFileName, filepath.Join(given, PlayerUUIDFileName), fs.ErrPermission,
			filepath.Join(given, PlayerUUIDFileName))
		if err == nil || err.Error() != want || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the quoted and plain paths given as %q: %v, want %q", as, err, want)
		}
	}

	for _, as := range []string{givenRoot, givenRoot + string(filepath.Separator)} {
		inRoot := filepath.Join(root, PlayerUUIDFileName)
		err := spelledAsGiven(fmt.Errorf("read %s: input/output error; open %q", inRoot, inRoot), root, as)
		want := fmt.Sprintf("read %s: input/output error; open %q", filepath.Join(givenRoot, PlayerUUIDFileName), filepath.Join(givenRoot, PlayerUUIDFileName))
		if err == nil || err.Error() != want {
			t.Errorf("a snapshot at the root %q given as %q: %v, want %q", root, as, err, want)
		}
	}

	if runtime.GOOS != "windows" {
		inside := fmt.Errorf("open /elsewhere%s: %w", filepath.Join(snap, PlayerUUIDFileName), fs.ErrNotExist)
		if got := spelledAsGiven(inside, snap, given); got != inside {
			t.Errorf("a path that holds the snapshot's spelling inside it was spelled again: %v", got)
		}
	}
}
