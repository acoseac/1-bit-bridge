package atomicwrite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// replaceEvents records, in order, what ReplaceDurable does through the
// strict writer's seams: "fsync <name>" for a staged file's fsync, "rename
// <from> <to>" for a rename (base names), "sync <dir>" for the strict
// barrier on a directory, and "parent <path>" for the best-effort barrier
// after a rename, which ReplaceDurable must never run. fail, when set, is
// asked before each event and its error is the step's.
type replaceEvents struct {
	seen []string
	fail func(event string) error
}

// install sets the seams until the test ends, the medium answering as a disk.
func (r *replaceEvents) install(t *testing.T) {
	t.Helper()
	stubMedium(t, func(string, uint64) error { return nil })
	step := func(event string) error {
		if r.fail != nil {
			if err := r.fail(event); err != nil {
				return err
			}
		}
		r.seen = append(r.seen, event)
		return nil
	}
	prevRename, prevFile, prevDir, prevParent := renameFunc, syncFile, syncDir, syncParentDir
	renameFunc = func(src, dst string) error {
		if err := step("rename " + filepath.Base(src) + " " + filepath.Base(dst)); err != nil {
			return err
		}
		return os.Rename(src, dst)
	}
	syncFile = func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		return step("fsync " + filepath.Base(f.Name()))
	}
	syncDir = func(d *os.File) error { return step("sync " + d.Name()) }
	syncParentDir = func(path string) error { return step("parent " + path) }
	t.Cleanup(func() { renameFunc, syncFile, syncDir, syncParentDir = prevRename, prevFile, prevDir, prevParent })
}

// entrySignature describes the entry at path without following it or opening
// anything but a regular file or a directory: its type and, for a link its
// target, for a directory its names, for a regular file its bytes; "absent"
// for no entry.
func entrySignature(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	kind := st.Mode().Type().String()
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return kind + " -> " + target
	case st.IsDir():
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return kind + " [" + strings.Join(names, " ") + "]"
	case st.Mode().IsRegular():
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return kind + " " + string(b)
	}
	return kind
}

// stagedLeft is every file named by the staging pattern left in dir.
func stagedLeft(t *testing.T, dir string) []string {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, ".replace-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// obstruction plants at path an entry of kind that a rename cannot replace
// in one step or that is no regular file, and returns its signature.
func obstruction(t *testing.T, path, kind string) string {
	t.Helper()
	dir := filepath.Dir(path)
	switch kind {
	case "directory":
		mkdirs(t, path)
	case "directory_with_a_file":
		mkdirs(t, path)
		if err := os.WriteFile(filepath.Join(path, "kept"), []byte("kept"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "dangling_link":
		symlink(t, "nowhere", path)
	case "link_to_a_file":
		if err := os.WriteFile(filepath.Join(dir, "target"), []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		symlink(t, "target", path)
	case "link_to_a_directory":
		mkdirs(t, filepath.Join(dir, "elsewhere"))
		symlink(t, "elsewhere", path)
	case "regular_file":
		if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		obstructionOnThisPlatform(t, path, kind)
	}
	return entrySignature(t, path)
}

// TestReplaceDurable_StagesTheNewFileBeforeTheEntryMoves pins the order a
// replacement takes, which keeps the destination's name from standing empty
// through a write: the new bytes staged beside it and fsynced, then the entry
// renamed aside and the staged file into its place, with nothing synced
// between the two renames, then the directory synced once, after both. No
// best-effort barrier runs: one between the renames would make an empty name
// the state a crash finds. The entry is kept aside as it was.
func TestReplaceDurable_StagesTheNewFileBeforeTheEntryMoves(t *testing.T) {
	root := realTempDir(t)
	r := &replaceEvents{}
	r.install(t)
	path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
	was := obstruction(t, path, "directory_with_a_file")
	if err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp"); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) != 4 || !strings.HasPrefix(r.seen[0], "fsync .replace-") {
		t.Fatalf("events %q, want the staged file's fsync, two renames and one sync", r.seen)
	}
	staged := strings.TrimPrefix(r.seen[0], "fsync ")
	if want := []string{"fsync " + staged, "rename seal.json seal.json.aside", "rename " + staged + " seal.json", "sync " + root}; !slices.Equal(r.seen, want) {
		t.Fatalf("events %q, want %q", r.seen, want)
	}
	if got := entrySignature(t, path); got != "---------- fresh" {
		t.Errorf("the destination holds %q", got)
	}
	if got := entrySignature(t, aside); got != was {
		t.Errorf("the entry kept aside is %q, was %q", got, was)
	}
}

// TestReplaceDurable_ReplacesEveryKindOfEntry: a directory, empty or not, a
// link to nothing, to a file or to a directory, a regular file, and on unix a
// named pipe each give way to a regular file holding the new bytes, and are
// kept aside as they were: a link is moved, never followed, and nothing is
// opened.
func TestReplaceDurable_ReplacesEveryKindOfEntry(t *testing.T) {
	kinds := append([]string{"directory", "directory_with_a_file", "dangling_link", "link_to_a_file", "link_to_a_directory", "regular_file"}, platformObstructions...)
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			root := realTempDir(t)
			(&replaceEvents{}).install(t)
			path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
			was := obstruction(t, path, kind)
			if err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp"); err != nil {
				t.Fatal(err)
			}
			if got := entrySignature(t, path); got != "---------- fresh" {
				t.Errorf("the destination holds %q", got)
			}
			if got := entrySignature(t, aside); got != was {
				t.Errorf("the entry kept aside is %q, was %q", got, was)
			}
			if left := stagedLeft(t, root); len(left) != 0 {
				t.Errorf("staged files left: %v", left)
			}
		})
	}
}

// TestReplaceDurable_LeavesTheEntryWhenAStepBeforeTheSecondRenameFails: the
// staged file's fsync, or the rename of the entry aside, failing returns that
// failure with the entry where it was, nothing kept aside and nothing staged
// left; and the rename of the staged file into the entry's place failing
// renames the entry back and syncs the directory again, so its return is
// durable too.
func TestReplaceDurable_LeavesTheEntryWhenAStepBeforeTheSecondRenameFails(t *testing.T) {
	broken := errors.New("the step failed")
	for _, tc := range []struct {
		step string
		fail func(event string) bool
		want []string // the events after the failure, the staged file's name as STAGED
	}{
		{"staging", func(e string) bool { return strings.HasPrefix(e, "fsync ") }, nil},
		{"aside", func(e string) bool { return e == "rename seal.json seal.json.aside" }, []string{"fsync STAGED"}},
		{
			step: "into_place",
			fail: func(e string) bool {
				return strings.HasPrefix(e, "rename .replace-") && strings.HasSuffix(e, " seal.json")
			},
			want: []string{"fsync STAGED", "rename seal.json seal.json.aside", "rename seal.json.aside seal.json", "sync ROOT"},
		},
	} {
		t.Run(tc.step, func(t *testing.T) {
			root := realTempDir(t)
			r := &replaceEvents{fail: func(e string) error {
				if tc.fail(e) {
					return broken
				}
				return nil
			}}
			r.install(t)
			path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
			was := obstruction(t, path, "directory_with_a_file")
			err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp")
			if !errors.Is(err, broken) {
				t.Fatalf("ReplaceDurable over a failed %s = %v", tc.step, err)
			}
			if got := entrySignature(t, path); got != was {
				t.Errorf("the destination holds %q, not the entry %q", got, was)
			}
			if got := entrySignature(t, aside); got != "absent" {
				t.Errorf("something is kept aside: %q", got)
			}
			if left := stagedLeft(t, root); len(left) != 0 {
				t.Errorf("staged files left: %v", left)
			}
			var got []string
			for _, e := range r.seen {
				if strings.HasPrefix(e, "fsync .replace-") {
					e = "fsync STAGED"
				}
				got = append(got, strings.ReplaceAll(e, root, "ROOT"))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("events %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReplaceDurable_SaysWhereTheEntryIsKeptWhenItCannotBePutBack: the
// staged file refused its place and the entry refused its way back leave the
// destination's name empty and the entry aside, which the error names, with
// both failures in it; nothing staged is left.
func TestReplaceDurable_SaysWhereTheEntryIsKeptWhenItCannotBePutBack(t *testing.T) {
	root := realTempDir(t)
	refused, notBack := errors.New("refused its place"), errors.New("refused its way back")
	r := &replaceEvents{fail: func(e string) error {
		switch {
		case strings.HasPrefix(e, "rename .replace-"):
			return refused
		case e == "rename seal.json.aside seal.json":
			return notBack
		}
		return nil
	}}
	r.install(t)
	path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
	was := obstruction(t, path, "dangling_link")
	err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp")
	if !errors.Is(err, refused) || !errors.Is(err, notBack) || !strings.Contains(err.Error(), aside) {
		t.Fatalf("ReplaceDurable = %v, want both failures and where the entry is kept", err)
	}
	if got := entrySignature(t, path); got != "absent" {
		t.Errorf("the destination holds %q", got)
	}
	if got := entrySignature(t, aside); got != was {
		t.Errorf("the entry kept aside is %q, was %q", got, was)
	}
	if left := stagedLeft(t, root); len(left) != 0 {
		t.Errorf("staged files left: %v", left)
	}
}

// TestReplaceDurable_ABarrierThatFailsLeavesTheWriteNotDurable: the
// directory's sync failing once both renames have landed is the write's
// failure, an ErrBarrierFailed, with the new file in place and the entry
// aside, as WriteBytesDurable's barrier failure leaves its new bytes.
func TestReplaceDurable_ABarrierThatFailsLeavesTheWriteNotDurable(t *testing.T) {
	root := realTempDir(t)
	r := &replaceEvents{fail: func(e string) error {
		if strings.HasPrefix(e, "sync ") {
			return errors.New("the directory would not sync")
		}
		return nil
	}}
	r.install(t)
	path, aside := filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
	was := obstruction(t, path, "directory")
	if err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp"); !errors.Is(err, ErrBarrierFailed) {
		t.Fatalf("ReplaceDurable over a failed barrier = %v, want ErrBarrierFailed", err)
	}
	if got := entrySignature(t, path); got != "---------- fresh" {
		t.Errorf("the destination holds %q", got)
	}
	if got := entrySignature(t, aside); got != was {
		t.Errorf("the entry kept aside is %q, was %q", got, was)
	}
}

// TestReplaceDurable_RefusesBeforeItStages: a platform with no directory
// barrier, an aside that is not a name beside the destination, a path with a
// ".." element or a link below the root, and no entry at the destination are
// each refused before anything is staged, renamed or synced.
func TestReplaceDurable_RefusesBeforeItStages(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, root string) (path, aside string)
		want error
	}{
		{"no_barrier", func(t *testing.T, root string) (string, string) {
			noDirectoryBarrier(t)
			return filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside")
		}, ErrNoDirectoryBarrier},
		{"aside_in_another_directory", func(t *testing.T, root string) (string, string) {
			mkdirs(t, filepath.Join(root, "sub"))
			obstruction(t, filepath.Join(root, "seal.json"), "directory_with_a_file")
			return filepath.Join(root, "seal.json"), filepath.Join(root, "sub", "seal.json.aside")
		}, ErrNotUnderRoot},
		{"aside_the_same_name", func(t *testing.T, root string) (string, string) {
			obstruction(t, filepath.Join(root, "seal.json"), "directory_with_a_file")
			return filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json")
		}, ErrNotUnderRoot},
		{"aside_differing_only_in_case", func(t *testing.T, root string) (string, string) {
			// On a case-insensitive filesystem (APFS's default, NTFS) SEAL.JSON
			// names the entry itself: the first rename would be a case-only
			// rename in place, and the second could not land beside it (Argon
			// on B539).
			obstruction(t, filepath.Join(root, "seal.json"), "directory_with_a_file")
			return filepath.Join(root, "seal.json"), filepath.Join(root, "SEAL.JSON")
		}, ErrNotUnderRoot},
		{"aside_naming_a_directory", func(t *testing.T, root string) (string, string) {
			obstruction(t, filepath.Join(root, "seal.json"), "directory_with_a_file")
			return filepath.Join(root, "seal.json"), filepath.Join(root, "seal.json.aside") + string(filepath.Separator)
		}, ErrNotUnderRoot},
		{"dot_dot", func(t *testing.T, root string) (string, string) {
			mkdirs(t, filepath.Join(root, "sub"))
			sep := string(filepath.Separator)
			return root + sep + "sub" + sep + ".." + sep + "seal.json", filepath.Join(root, "seal.json.aside")
		}, ErrNotUnderRoot},
		{"link_below_the_root", func(t *testing.T, root string) (string, string) {
			mkdirs(t, filepath.Join(root, "real"))
			symlink(t, "real", filepath.Join(root, "link"))
			obstruction(t, filepath.Join(root, "real", "seal.json"), "directory")
			return filepath.Join(root, "link", "seal.json"), filepath.Join(root, "link", "seal.json.aside")
		}, ErrNotUnderRoot},
		{"no_entry", func(t *testing.T, root string) (string, string) {
			return filepath.Join(root, "absent.json"), filepath.Join(root, "absent.json.aside")
		}, fs.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			r := &replaceEvents{}
			r.install(t)
			path, aside := tc.set(t, root)
			before := map[string]string{}
			_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error {
				before[p] = entrySignature(t, p)
				return nil
			})
			if err := ReplaceDurable(root, path, aside, []byte("fresh"), ".replace-*.tmp"); !errors.Is(err, tc.want) {
				t.Fatalf("ReplaceDurable = %v, want %v", err, tc.want)
			}
			if len(r.seen) != 0 {
				t.Errorf("the refusal came after %q", r.seen)
			}
			after := map[string]string{}
			_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error {
				after[p] = entrySignature(t, p)
				return nil
			})
			for p, sig := range before {
				if after[p] != sig {
					t.Errorf("the refusal changed %s from %q to %q", p, sig, after[p])
				}
			}
			for p := range after {
				if _, ok := before[p]; !ok {
					t.Errorf("the refusal made %s", p)
				}
			}
		})
	}
}
