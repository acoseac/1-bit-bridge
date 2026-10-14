package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOpenStoreRefusesASymlinkAtTheDatabaseOrASidecar pins that OpenStore
// refuses a symlink planted at the database file or either SQLite sidecar
// (-wal, -shm), or at the migration lock (-migrate.lock, backlog B185),
// rather than opening the link's target. Run as root over an
// install whose data dir another user owns, following such a link writes
// the database wherever that user aimed it (backlog B262). The refusal is
// unconditional — no supported layout symlinks these — so this runs as any
// user. The target lives outside the data dir, in a sibling the data dir's
// "owner" could plant but not write into, so a target that appears proves
// the link was followed.
func TestOpenStoreRefusesASymlinkAtTheDatabaseOrASidecar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs privilege on Windows; the data dir is the bridge's own")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ladderLockSuffix} {
		t.Run("link at bridge.db"+suffix, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "data")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			decoy := filepath.Join(root, "decoy-target")
			link := DefaultDBPath(dir) + suffix
			if err := os.Symlink(decoy, link); err != nil {
				t.Fatal(err)
			}

			s, err := OpenStore(DefaultDBPath(dir))
			if err == nil {
				s.Close()
				t.Fatalf("OpenStore opened through the planted symlink at %s", filepath.Base(link))
			}
			// The refusal is OpenStore's own, before SQLite — not SQLite's
			// later error on a dangling-link sidecar. This distinction is
			// what makes the -wal/-shm cases red before the fix.
			if !errors.Is(err, errStoreSymlink) {
				t.Fatalf("error at %s was %v, want the symlink refusal", filepath.Base(link), err)
			}
			if _, statErr := os.Lstat(decoy); statErr == nil {
				t.Fatalf("the symlink at %s was followed: its target was created", filepath.Base(link))
			}
			// The planted link is left as it was: refused, not replaced.
			if fi, lerr := os.Lstat(link); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the planted symlink at %s was disturbed (err=%v)", filepath.Base(link), lerr)
			}
		})
	}
}

// TestOpenStoreOpensARealExistingDatabase is the companion: a database file
// that is a real file, not a link, still opens. The refusal is for symlinks
// only, so an ordinary store created by an earlier run is untouched.
func TestOpenStoreOpensARealExistingDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := DefaultDBPath(dir)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, lerr := os.Lstat(path); lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the created store is not a real file (err=%v)", lerr)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatalf("reopen of a real database: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenStoreRefusesASymlinkSwappedInBeforeTheOpen plants the link after
// OpenStore's own check and before SQLite opens the files, through the real
// OpenStore. The database, -wal and -shm must come back as the symlink
// refusal, and the decoy must not be created. -journal is not opened in WAL
// mode; that case only requires the decoy to stay absent.
func TestOpenStoreRefusesASymlinkSwappedInBeforeTheOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs privilege on Windows; the data dir is the bridge's own")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		t.Run("link at bridge.db"+suffix, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "data")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			dbPath := DefaultDBPath(dir)
			decoy := filepath.Join(root, "decoy-target")
			link := dbPath + suffix
			restore := setOpenStoreBeforeSQLite(func(p string) {
				if p != dbPath {
					return
				}
				if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("remove %s: %v", filepath.Base(link), err)
					return
				}
				if err := os.Symlink(decoy, link); err != nil {
					t.Errorf("symlink %s: %v", filepath.Base(link), err)
				}
			})
			t.Cleanup(restore)

			s, err := OpenStore(dbPath)
			if s != nil {
				s.Close()
			}
			if _, statErr := os.Lstat(decoy); statErr == nil {
				t.Fatalf("the symlink at %s was followed: its target was created", filepath.Base(link))
			}
			if fi, lerr := os.Lstat(link); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the planted symlink at %s was disturbed (err=%v)", filepath.Base(link), lerr)
			}
			if suffix == "-journal" {
				if err != nil && !errors.Is(err, errStoreSymlink) {
					t.Fatalf("journal link: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("OpenStore opened through the symlink swapped in at %s", filepath.Base(link))
			}
			if !errors.Is(err, errStoreSymlink) {
				t.Fatalf("error at %s was %v, want the symlink refusal", filepath.Base(link), err)
			}
		})
	}
}

// TestOpenStoreOpensADatabaseWhoseParentIsASymlink pins that a data
// directory which is a symlink still opens, and the database file lands in
// the directory the link names.
func TestOpenStoreOpensADatabaseWhoseParentIsASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("directory symlink: " + err.Error())
	}
	path := filepath.Join(link, "bridge.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open through a linked directory: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the database file itself is a symlink")
	}
	atTarget, err := os.Lstat(filepath.Join(real, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(opened, atTarget) {
		t.Fatal("the file opened through the link is not the file in the target directory")
	}
}
