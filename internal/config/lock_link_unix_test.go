//go:build !windows

package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A config named through a link to a directory and a `..` after it is a file
// the OS finds by following the link and then climbing from where the link
// leads, not from where it stands: with cfg/link leading to real/sub,
// cfg/link/../bridge.yaml is real/bridge.yaml. filepath.Dir cleans that name
// to cfg's, so until B506's second round a writer through that spelling took
// its lock in cfg while a writer through real/bridge.yaml took real's, and
// its save staged the new file in cfg and renamed it into real.

// linkedConfig lays out real/bridge.yaml and the link cfg/link to real/sub in
// a directory of its own, and returns the config's plain name and the name
// through the link.
func linkedConfig(t *testing.T) (plain, through string) {
	t.Helper()
	base := realTempDir(t)
	real := filepath.Join(base, "real")
	for _, d := range []string{filepath.Join(real, "sub"), filepath.Join(base, "cfg")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(real, "sub"), filepath.Join(base, "cfg", "link")); err != nil {
		t.Fatal(err)
	}
	plain, _ = loadableConfig(t, real, "Before")
	// Joined by hand: filepath.Join would clean the `..` away with the link.
	through = filepath.Join(base, "cfg", "link") + "/../bridge.yaml"
	a, errA := os.ReadFile(plain)
	b, errB := os.ReadFile(through)
	if errA != nil || errB != nil || !bytes.Equal(a, b) {
		t.Fatalf("the OS does not open one file by both names: %v, %v", errA, errB)
	}
	return plain, through
}

// TestTwoNamesOfOneConfigTakeOneLock: writers of one config file take turns
// whichever name they give it, because the lock is beside the file the OS
// writes, in the directory the name resolves to: an Edit through the link
// waits for a turn taken through the plain name, then reads the file that
// turn saved and changes it, so both changes stay.
func TestTwoNamesOfOneConfigTakeOneLock(t *testing.T) {
	plain, through := linkedConfig(t)
	waited, stop := LockWaitedForTest(plain)
	defer stop()
	other := make(chan error, 1)
	err := Edit(plain, func(cfg *Config) (bool, error) {
		go func() {
			other <- Edit(through, func(c *Config) (bool, error) {
				c.ScanIntervalSec = 7200
				return true, nil
			})
		}()
		select {
		case <-waited:
		case err := <-other:
			other <- err
			t.Errorf("an Edit through %s went through while a turn through %s was held: it took another lock (%v)", through, plain, err)
		case <-time.After(30 * time.Second):
			t.Error("an Edit through the other name neither waited for the turn nor finished")
		}
		cfg.LibraryName = "First Turn"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatalf("the Edit through %s: %v", through, err)
	}
	after, err := Load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if after.LibraryName != "First Turn" || after.ScanIntervalSec != 7200 {
		t.Errorf("libraryName %q, scanIntervalSec %d: want both turns' changes, the second made on the file the first saved",
			after.LibraryName, after.ScanIntervalSec)
	}
}

// A config whose own name is a symbolic link (a dotfiles checkout's, a
// home-manager's) is the file the link leads to: the OS opens the target for
// either name, so the turn locks beside the target, under the target's name
// (alias.yaml leading to actual.yaml locks .actual.yaml.lock), compares the
// target, and renames onto it, leaving the link. Until B506's third round the
// turn resolved the directory alone: a writer through the link locked
// .alias.yaml.lock beside the link while one through the target's own name
// locked beside the target, and a save renamed a file over the link.

// linkedConfigFile lays out base/dot/actual.yaml, a config that loads, and the
// link base/cfg/alias.yaml to it, and returns the target's name and the
// link's.
func linkedConfigFile(t *testing.T) (target, link string) {
	t.Helper()
	base := realTempDir(t)
	dot, cfg := filepath.Join(base, "dot"), filepath.Join(base, "cfg")
	for _, d := range []string{dot, cfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plain, _ := loadableConfig(t, dot, "Before")
	target = filepath.Join(dot, "actual.yaml")
	if err := os.Rename(plain, target); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(cfg, "alias.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return target, link
}

// requireLink fails the test unless link is still a symbolic link to target.
func requireLink(t *testing.T, link, target string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a link (%v, %v): a save renamed a file over it", link, info, err)
	}
	if to, err := os.Readlink(link); err != nil || to != target {
		t.Errorf("%s leads to %q (%v), want %s", link, to, err, target)
	}
}

// TestALinkedConfigsWritersTakeTheLockBesideItsTarget: writers of the config
// through the link and through the target's own name take turns, at the lock
// beside the target named after it, and a save through the link lands in the
// target and leaves the link; a test hook set through either name hears the
// writer.
func TestALinkedConfigsWritersTakeTheLockBesideItsTarget(t *testing.T) {
	target, link := linkedConfigFile(t)
	waited, stop := LockWaitedForTest(link)
	defer stop()
	other := make(chan error, 1)
	err := Edit(link, func(cfg *Config) (bool, error) {
		lockPath := filepath.Join(filepath.Dir(target), ".actual.yaml.lock")
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			t.Errorf("no lock file beside the target, named after it (%s): %v", lockPath, err)
		} else {
			if err := fsutil.TryLock(f); !errors.Is(err, fsutil.ErrLocked) {
				t.Errorf("the lock beside the target is not held by the turn through the link: %v", err)
			}
			_ = f.Close()
		}
		go func() {
			other <- Edit(target, func(c *Config) (bool, error) {
				c.ScanIntervalSec = 7200
				return true, nil
			})
		}()
		select {
		case <-waited:
		case err := <-other:
			other <- err
			t.Errorf("an Edit through %s went through while a turn through %s was held: it took another lock (%v)", target, link, err)
		case <-time.After(30 * time.Second):
			t.Error("an Edit through the target's name neither waited for the turn nor finished")
		}
		cfg.LibraryName = "Through The Link"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatalf("the Edit through %s: %v", target, err)
	}
	requireLink(t, link, target)
	after, err := Load(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.LibraryName != "Through The Link" || after.ScanIntervalSec != 7200 {
		t.Errorf("libraryName %q, scanIntervalSec %d: want both turns' changes in the target", after.LibraryName, after.ScanIntervalSec)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(link), ".alias.yaml.lock")); err == nil {
		t.Errorf("a lock file was made beside the link, under its name")
	}
}

// TestALinkThatLeadsNowhereIsRefused: a config whose name is a link to no
// file is refused by the turn, before anything is made: the link is left as
// it is, nothing is made where it leads, and the refusal says what to do.
// Until B506's third round a writer that found no file there (serve's seed,
// init) saved one over the link.
func TestALinkThatLeadsNowhereIsRefused(t *testing.T) {
	base := realTempDir(t)
	link := filepath.Join(base, "bridge.yaml")
	nowhere := filepath.Join(base, "gone.yaml")
	if err := os.Symlink(nowhere, link); err != nil {
		t.Fatal(err)
	}
	l, err := LockFile(link)
	if err == nil {
		err = (&Config{LibraryName: "Seeded"}).Save(l)
		l.Unlock()
	}
	if !errors.Is(err, fsutil.ErrDanglingLink) {
		t.Errorf("a turn at a link that leads nowhere, and its save = %v, want a refusal of the link", err)
	} else if msg := err.Error(); !strings.Contains(msg, link) || !strings.Contains(msg, "point the link") {
		t.Errorf("refusal %q: want it to name %s and say what to do", msg, link)
	}
	requireLink(t, link, nowhere)
	if _, err := os.Lstat(nowhere); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link's target was made: %v", err)
	}
}

// TestALinkIntoADirectoryThisUserCannotWriteSaysWhatToDo: a config linked
// into a directory this user cannot write (a read-only store, a read-only
// mount) cannot be saved where it is: the lock, or the save's staging, is
// refused there, and the refusal says to point the link at a writable copy or
// put the file in its place.
func TestALinkIntoADirectoryThisUserCannotWriteSaysWhatToDo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not stop root")
	}
	for _, tc := range []struct {
		name string
		// lockMade says whether the lock file is there already, so that the
		// refusal comes from the save's staging rather than the lock.
		lockMade bool
	}{
		{"at the lock", false},
		{"at the save's staging", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, link := linkedConfigFile(t)
			store := filepath.Dir(target)
			if tc.lockMade {
				if err := os.WriteFile(filepath.Join(store, ".actual.yaml.lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(store, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(store, 0o755) })
			err := Edit(link, func(cfg *Config) (bool, error) { cfg.LibraryName = "After"; return true, nil })
			if err == nil {
				t.Fatalf("a save through a link into a directory this user cannot write went through")
			}
			if msg := err.Error(); !strings.Contains(msg, link) || !strings.Contains(msg, "writable copy") {
				t.Errorf("refusal %q: want it to name %s and say to point the link at a writable copy", msg, link)
			}
			requireLink(t, link, target)
		})
	}
}

// TestALockFileThisUserCannotOpenSaysWhatToDo: a lock file that is there and
// that this user may not open to read and write (backlog B537 item 2: root's
// 0600 one beside a config file a `sudo bridge init` made and the service's
// user was given later, or one whose mode was taken away, which mode 0 stands
// for here, a test's user being the only owner it can give a file) refuses
// config.Edit and the holder's Update alike, matching ErrLockInaccessible, in
// words that name the config file and its lock file and give both ways out,
// writing nothing; each way out, the lock file given back its owner's read and
// write or removed, lets the next save land. A directory at the lock's name,
// and no lock file in a directory this user cannot write, keep their words.
func TestALockFileThisUserCannotOpenSaysWhatToDo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file's mode does not stop root")
	}
	rename := func(c *Config) (bool, error) { c.LibraryName = "After"; return true, nil }
	// inaccessible writes a config that loads and a lock file of mode 0 beside
	// it, and returns the config's path, the lock file's and the config's
	// bytes.
	inaccessible := func(t *testing.T) (cfgPath, lock string, before []byte) {
		t.Helper()
		cfgPath, _ = loadableConfig(t, realTempDir(t), "Before")
		lock = filepath.Join(filepath.Dir(cfgPath), LockFileName(cfgPath))
		if err := os.WriteFile(lock, nil, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lock, 0); err != nil {
			t.Fatal(err)
		}
		if f, err := os.OpenFile(lock, os.O_RDWR, 0); !errors.Is(err, fs.ErrPermission) {
			_ = f.Close()
			t.Fatalf("premise: this user opens the lock file of mode 0 (%v)", err)
		}
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfgPath, lock, before
	}
	requireRefusal := func(t *testing.T, err error, cfgPath, lock string, before []byte) {
		t.Helper()
		if !errors.Is(err, ErrLockInaccessible) {
			t.Fatalf("a save beside a lock file this user cannot open = %v, want ErrLockInaccessible", err)
		}
		for _, says := range []string{cfgPath, lock, "give the lock file the config file's owner", "read and write it",
			"or remove it while the bridge is stopped and no bridge command runs"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("refusal %q: want it to say %q", err, says)
			}
		}
		if got, _ := os.ReadFile(cfgPath); !bytes.Equal(got, before) {
			t.Errorf("the refused save wrote the config file:\n%s", got)
		}
	}
	requireRenamed := func(t *testing.T, cfgPath string) {
		t.Helper()
		if cfg, err := Load(cfgPath); err != nil || cfg.LibraryName != "After" {
			t.Errorf("the config after the way out = %+v, %v; want libraryName After", cfg, err)
		}
	}
	t.Run("config.Edit, then the lock file given back its owner's read and write", func(t *testing.T) {
		cfgPath, lock, before := inaccessible(t)
		requireRefusal(t, Edit(cfgPath, rename), cfgPath, lock, before)
		if err := os.Chmod(lock, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Edit(cfgPath, rename); err != nil {
			t.Fatalf("a save once the lock file is its owner's to read and write again: %v", err)
		}
		requireRenamed(t, cfgPath)
	})
	t.Run("the holder's Update, then the lock file removed", func(t *testing.T) {
		cfgPath, lock, before := inaccessible(t)
		cfg, raw, read, err := LoadFileAt(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		rc := NewRuntimeConfig(cfg)
		rc.RecordFile(read, raw)
		ran := false
		requireRefusal(t, rc.Update(cfgPath, func(c *Config) error { ran = true; c.LibraryName = "After"; return nil }),
			cfgPath, lock, before)
		if ran || rc.Load().LibraryName != "Before" {
			t.Errorf("the refused save ran its change (%v) or published it (%q)", ran, rc.Load().LibraryName)
		}
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		if err := rc.Update(cfgPath, func(c *Config) error { c.LibraryName = "After"; return nil }); err != nil {
			t.Fatalf("a save once the lock file is removed: %v", err)
		}
		requireRenamed(t, cfgPath)
		if info, err := os.Lstat(lock); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the lock file after the save = %v, %v; want the save to have made it again", info, err)
		}
	})
	// keepsItsWords requires err to be the lock's open failure as it was said
	// before B537 item 2, and no refusal of a lock file this user cannot open.
	keepsItsWords := func(t *testing.T, err error, cfgPath string) {
		t.Helper()
		if err == nil || errors.Is(err, ErrLockInaccessible) || !strings.Contains(err.Error(), "open the lock beside "+cfgPath) {
			t.Errorf("the save = %v, want the lock's open failure in its own words", err)
		}
	}
	t.Run("a directory at the lock's name", func(t *testing.T) {
		cfgPath, _ := loadableConfig(t, realTempDir(t), "Before")
		if err := os.Mkdir(filepath.Join(filepath.Dir(cfgPath), LockFileName(cfgPath)), 0o700); err != nil {
			t.Fatal(err)
		}
		keepsItsWords(t, Edit(cfgPath, rename), cfgPath)
	})
	t.Run("no lock file in a directory this user cannot write", func(t *testing.T) {
		cfgPath, _ := loadableConfig(t, realTempDir(t), "Before")
		dir := filepath.Dir(cfgPath)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		err := Edit(cfgPath, rename)
		keepsItsWords(t, err, cfgPath)
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the save = %v, want the open's permission error kept", err)
		}
	})
}

// TestARepointedLinkRefusesTheHoldersSave: serve's holder saves only into the
// file it read at boot. A link repointed meanwhile (a dotfiles restow, a
// home-manager switch) names another file, which serve never read, however
// alike the two are, so a console save is refused with a
// *ChangedOutsideError, both files left as they are.
func TestARepointedLinkRefusesTheHoldersSave(t *testing.T) {
	target, link := linkedConfigFile(t)
	cfg, raw, read, err := LoadFileAt(link)
	if err != nil {
		t.Fatal(err)
	}
	if read.target != target {
		t.Fatalf("premise: the boot read through %s opened %s, want %s", link, read.target, target)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(read, raw)
	moved := filepath.Join(filepath.Dir(target), "switched.yaml")
	if err := os.WriteFile(moved, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, link); err != nil {
		t.Fatal(err)
	}
	ran := false
	err = rc.Update(link, func(c *Config) error { ran = true; c.LibraryName = "Renamed"; return nil })
	if !errors.Is(err, ErrChangedOutside) {
		t.Errorf("a console save through a link repointed since boot = %v, want ErrChangedOutside", err)
	}
	if ran {
		t.Error("the refused save ran its change")
	}
	for _, p := range []string{target, moved} {
		if got, _ := os.ReadFile(p); !bytes.Equal(got, raw) {
			t.Errorf("the refused save wrote %s:\n%s", p, got)
		}
	}
	requireLink(t, link, moved)
}

// TestASaveThroughALinkStagesWhereItRenames: a save stages its new file in
// the directory its rename lands in, the one the config's name resolves to,
// never the one filepath.Dir names for it. Here that one takes no new file,
// as a directory the writer cannot write would not, and holds a lock file of
// its own, so a writer that looked for its lock there would get to its
// save: one that staged there failed, and on another filesystem its rename
// would fail across it.
func TestASaveThroughALinkStagesWhereItRenames(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not stop root")
	}
	plain, through := linkedConfig(t)
	lexical := filepath.Dir(through) // cfg, where the cleaned name points
	if err := os.WriteFile(filepath.Join(lexical, LockFileName(through)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lexical, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lexical, 0o755) })
	l, err := LockFile(through)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	cfg, err := Load(through)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LibraryName = "Through The Link"
	if err := cfg.Save(l); err != nil {
		t.Fatalf("a save through %s, which resolves to %s, did not stage beside the file it replaces: %v", through, plain, err)
	}
	if after, err := Load(plain); err != nil || after.LibraryName != "Through The Link" {
		t.Errorf("after the save the file loads as %v, %v", after, err)
	}
	for _, dir := range []string{filepath.Dir(plain), lexical} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".bridge-") {
				t.Errorf("the save left %s staged in %s", e.Name(), dir)
			}
		}
	}
}
