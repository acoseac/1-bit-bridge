package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// On Windows a read of a file just renamed in, or one an indexer or a virus
// scanner holds, is refused for a moment with a sharing violation or access
// denied, which atomicwrite.ReadWithRetryCtx asks again on. Until B506's
// third round the read a turn makes when LockFile takes it, and the one each
// save makes before its rename, were asked once, so such a refusal failed the
// save. These tests stand a portable error in for Windows' (errBusyForTest)
// through two process-global seams, the turn's read and what atomicwrite asks
// again on: none of them calls t.Parallel. Windows' own codes are
// atomicwrite's TestReadRetryWindowsErrors, on CI's Windows leg.

// errBusyForTest stands for Windows' sharing violation on a read.
var errBusyForTest = errors.New("the process cannot access the file because it is being used by another process")

// busyReads makes the reads of the config file at path for which busy answers
// true fail as a read on Windows fails while another process holds the file,
// and atomicwrite ask again on that failure; it returns how many reads of path
// were made.
func busyReads(t *testing.T, path string, busy func(n int) bool) (reads *int) {
	t.Helper()
	n := 0
	t.Cleanup(SetTurnReadForTest(func(p string) ([]byte, error) {
		if p == path {
			n++
			if busy(n) {
				return nil, &fs.PathError{Op: "open", Path: p, Err: errBusyForTest}
			}
		}
		return fsutil.ReadAsFile(p)
	}))
	t.Cleanup(atomicwrite.SetReadRetryableForTest(func(err error) bool { return errors.Is(err, errBusyForTest) }))
	return &n
}

// TestATurnsReadsAskAgainWhileTheFileIsBusy: an unchanged file behind one
// refused read, the turn's own or the one before the rename, is read again
// and saved.
func TestATurnsReadsAskAgainWhileTheFileIsBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy int
	}{
		{"the read when the turn is taken", 1},
		{"the read before the rename", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			path := writeConfigFile(t, dir, "libraryName: Before\n")
			reads := busyReads(t, path, func(n int) bool { return n == tc.busy })
			l, err := LockFile(path)
			if err != nil {
				t.Fatalf("a turn behind one refused read: %v", err)
			}
			defer l.Unlock()
			if err := (&Config{LibraryName: "After"}).Save(l); err != nil {
				t.Fatalf("a save behind one refused read: %v", err)
			}
			if *reads != 3 {
				t.Errorf("the file was read %d times, want the turn's read, the read before the rename, and the refused one again", *reads)
			}
			if got, _ := os.ReadFile(path); !bytes.Contains(got, []byte("libraryName: After")) {
				t.Errorf("the save did not land:\n%s", got)
			}
		})
	}
}

// TestATurnsReadOfAMissingFileAnswersAtOnce: a file that is not there is a
// turn that found none, at once: no read asks again on its absence.
func TestATurnsReadOfAMissingFileAnswersAtOnce(t *testing.T) {
	dir := realTempDir(t)
	path := filepath.Join(dir, "bridge.yaml")
	reads := busyReads(t, path, func(int) bool { return false })
	start := time.Now()
	l, err := LockFile(path)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if !l.absent || *reads != 1 {
		t.Errorf("a missing file: absent %v after %d reads, want absent after one", l.absent, *reads)
	}
	if took >= atomicwrite.RenameRetryBudget()/3 {
		t.Errorf("LockFile of a missing file took %s, as if its read had been asked again", took)
	}
}
