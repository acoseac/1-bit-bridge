package atomicwrite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// failRenames makes every rename the strict writer tries fail with err, on a
// schedule of one attempt, until the test ends.
func failRenames(t *testing.T, err error) {
	t.Helper()
	prevRename, prevBackoff := renameFunc, renameBackoff
	renameFunc = func(string, string) error { return err }
	renameBackoff = []time.Duration{0}
	t.Cleanup(func() { renameFunc, renameBackoff = prevRename, prevBackoff })
}

// fileSyncs answers ret for the fsync of the file named dst, and fsyncs every
// other file, until the test ends.
func fileSyncs(t *testing.T, dst string, ret error) {
	t.Helper()
	prev := syncFile
	syncFile = func(f *os.File) error {
		if f.Name() == dst {
			return ret
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = prev })
}

// TestWriteBytesDurable_MarksWhatFailsOnceTheWriteHasLanded pins ErrLanded
// (B728 B3 round 4): an error that comes once the write has landed, its
// bytes at the name, is marked, and none before it, the destination then as
// it was. Once landed: the directory's fsync failing after the rename
// (ErrBarrierFailed too); the directory that cannot be opened for its sync
// after the rename, which no fsync marks; and a failed rename's fallback that
// found the bytes there, whose barrier or own fsync then fails. Before: a
// platform with no barrier; the barrier on a directory the write created,
// before it stages its file (ErrBarrierFailed with nothing landed); and a
// failed rename whose destination holds other bytes.
func TestWriteBytesDurable_MarksWhatFailsOnceTheWriteHasLanded(t *testing.T) {
	broken := errors.New("the step failed")
	const was, data = "the box's", "the snapshot's"
	for _, tc := range []struct {
		name string
		// file is the destination below the root; seed its bytes before the
		// write, "" for none, and after its bytes once the write failed.
		file, seed, after string
		set               func(t *testing.T, root, dst string)
		landed, barrier   bool
	}{
		{"the_fsync_after_the_rename", "seal.json", was, data, func(t *testing.T, _, _ string) { stubSyncDir(t, broken) }, true, true},
		{"an_open_after_the_rename", "seal.json", was, data, func(t *testing.T, root, _ string) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("a directory's mode keeps neither Windows nor root from opening it")
			}
			stubSyncDir(t, nil)
			prev := renameFunc
			renameFunc = func(src, dst string) error {
				if err := os.Rename(src, dst); err != nil {
					return err
				}
				return os.Chmod(root, 0o300)
			}
			t.Cleanup(func() { renameFunc = prev; _ = os.Chmod(root, 0o700) })
		}, true, false},
		{"the_same_bytes_whose_barrier_fails", "seal.json", data, data, func(t *testing.T, _, dst string) {
			failRenames(t, broken)
			fileSyncs(t, dst, nil)
			stubSyncDir(t, broken)
		}, true, true},
		{"the_same_bytes_whose_fsync_fails", "seal.json", data, data, func(t *testing.T, _, dst string) {
			failRenames(t, broken)
			fileSyncs(t, dst, broken)
			stubSyncDir(t, nil)
		}, true, false},
		{"no_barrier", "seal.json", was, was, func(t *testing.T, _, _ string) { noDirectoryBarrier(t) }, false, false},
		{"the_barrier_on_a_directory_it_created", filepath.Join("made", "seal.json"), "", "", func(t *testing.T, _, _ string) { stubSyncDir(t, broken) }, false, true},
		{"a_rename_that_fails", "seal.json", was, was, func(t *testing.T, _, dst string) {
			failRenames(t, broken)
			fileSyncs(t, dst, nil)
			stubSyncDir(t, nil)
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			dst := filepath.Join(root, tc.file)
			if tc.seed != "" {
				if err := os.WriteFile(dst, []byte(tc.seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tc.set(t, root, dst)
			err := WriteBytesDurable(root, dst, []byte(data), ".durable-*.tmp")
			if err == nil {
				t.Fatal("the write answered nil")
			}
			if got := errors.Is(err, ErrLanded); got != tc.landed {
				t.Errorf("errors.Is(%v, ErrLanded) = %v, want %v", err, got, tc.landed)
			}
			if got := errors.Is(err, ErrBarrierFailed); got != tc.barrier {
				t.Errorf("errors.Is(%v, ErrBarrierFailed) = %v, want %v", err, got, tc.barrier)
			}
			if tc.name == "an_open_after_the_rename" && !errors.Is(err, fs.ErrPermission) {
				t.Errorf("the open that failed after the rename is %v, want its permission error", err)
			}
			got, rerr := os.ReadFile(dst)
			switch {
			case tc.after == "" && !errors.Is(rerr, fs.ErrNotExist):
				t.Errorf("the destination holds %q, %v, want none", got, rerr)
			case tc.after != "" && (rerr != nil || string(got) != tc.after):
				t.Errorf("the destination holds %q, %v, want %q", got, rerr, tc.after)
			}
		})
	}
}

// TestReplaceDurable_ABarrierThatFailsOnceTheEntryIsBackIsNotLanded: the
// staged file refused its place, the entry put back, and the directory's sync
// after that failing is the replacement's failure with nothing landed: the
// rename's and the barrier's errors, never ErrLanded, and the entry where it
// was.
func TestReplaceDurable_ABarrierThatFailsOnceTheEntryIsBackIsNotLanded(t *testing.T) {
	root := realTempDir(t)
	refused, unsynced := errors.New("refused its place"), errors.New("the directory would not sync")
	r := &replaceEvents{fail: func(e string) error {
		switch {
		case strings.HasPrefix(e, "rename .replace-"):
			return refused
		case strings.HasPrefix(e, "sync "):
			return unsynced
		}
		return nil
	}}
	r.install(t)
	path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
	was := obstruction(t, path, "directory_with_a_file")
	err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp")
	if !errors.Is(err, refused) || !errors.Is(err, ErrBarrierFailed) || errors.Is(err, ErrLanded) {
		t.Fatalf("ReplaceDurable = %v, want the rename's and the barrier's failures, not ErrLanded", err)
	}
	if got := entrySignature(t, path); got != was {
		t.Errorf("the destination holds %q, not the entry %q", got, was)
	}
}
