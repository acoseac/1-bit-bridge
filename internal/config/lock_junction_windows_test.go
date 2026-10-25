//go:build windows

package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A config whose directory is reached through a directory junction is locked,
// compared and replaced where the junction leads, by the path the OS names
// for it (fsutil.ResolveParent over the handle's name), and a junction
// repointed during a turn makes the turn's name opens another file than it
// locked (B506's fourth round). Until then the turn kept the junction's own
// spelling, so a junction repointed after the lock had the turn's reads and
// rename reach the new directory under the old one's lock, and a file link
// inside a junction was refused outright.

// junctionForTest makes link a directory junction to target. `mklink /J`
// needs no privilege, where a symbolic link does.
func junctionForTest(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// symlinkOrSkip makes link a file symbolic link to target, and skips the test
// where Windows refuses this process one (Developer Mode off, not elevated).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no file symbolic link can be made here (Developer Mode, or an elevated process): %v", err)
	}
}

// junctionedTurn lays out real\bridge.yaml, a config that loads, reached
// through the junction cfg, and returns cfg and real as the OS names it.
func junctionedTurn(t *testing.T) (cfg, canonReal string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	loadableConfig(t, real, "Before")
	cfg = filepath.Join(base, "cfg")
	junctionForTest(t, real, cfg)
	canonReal, err := fsutil.ResolveLinks(real)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, canonReal
}

// TestATurnThroughAJunctionLocksBesideItsTarget: a turn of a config reached
// through a junction, by the file's own name and by a file link to it,
// locks beside the file where the junction leads, under the file's name, and
// its save lands in that file, leaving the link a link.
func TestATurnThroughAJunctionLocksBesideItsTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		link bool
	}{
		{"the file's own name", false},
		{"a file link to it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, canonReal := junctionedTurn(t)
			name := filepath.Join(cfg, "bridge.yaml")
			if tc.link {
				symlinkOrSkip(t, filepath.Join(canonReal, "bridge.yaml"), filepath.Join(canonReal, "alias.yaml"))
				name = filepath.Join(cfg, "alias.yaml")
			}
			l, err := LockFile(name)
			if err != nil {
				t.Fatalf("a turn through the junction: %v", err)
			}
			lockPath := filepath.Join(canonReal, ".bridge.yaml.lock")
			f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
			if err != nil {
				t.Errorf("no lock file beside the junction's target, named after the file (%s): %v", lockPath, err)
			} else {
				if err := fsutil.TryLock(f); !errors.Is(err, fsutil.ErrLocked) {
					t.Errorf("the lock beside the junction's target is not held by the turn: %v", err)
				}
				_ = f.Close()
			}
			cfgNow, err := l.LoadFound()
			if err != nil {
				t.Fatal(err)
			}
			cfgNow.LibraryName = "Through The Junction"
			if err := cfgNow.Save(l); err != nil {
				t.Errorf("the save through the junction: %v", err)
			}
			l.Unlock()
			if after, err := Load(filepath.Join(canonReal, "bridge.yaml")); err != nil || after.LibraryName != "Through The Junction" {
				t.Errorf("the file the junction leads to loads as %v, %v", after, err)
			}
			if tc.link {
				if info, err := os.Lstat(filepath.Join(canonReal, "alias.yaml")); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("the file link is no longer a link (%v, %v): the save renamed a file over it", info, err)
				}
			}
		})
	}
}

// TestAJunctionRepointedInATurnRefusesItsSave: a junction at the config's
// directory repointed after the turn took its lock, to a directory whose
// bridge.yaml holds the same bytes, makes the turn's save refuse: the name
// opens another file than the turn locked, and neither file is written.
func TestAJunctionRepointedInATurnRefusesItsSave(t *testing.T) {
	cfg, canonReal := junctionedTurn(t)
	raw, err := os.ReadFile(filepath.Join(canonReal, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(cfg), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "bridge.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LockFile(filepath.Join(cfg, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	junctionForTest(t, other, cfg)
	if err := (&Config{LibraryName: "Mine"}).Save(l); !errors.Is(err, ErrNameMoved) {
		t.Errorf("a save whose junction was repointed in its turn = %v, want ErrNameMoved", err)
	}
	for _, p := range []string{filepath.Join(canonReal, "bridge.yaml"), filepath.Join(other, "bridge.yaml")} {
		if got, err := os.ReadFile(p); err != nil || string(got) != string(raw) {
			t.Errorf("the refused save wrote %s:\n%s (%v)", p, got, err)
		}
	}
}

// TestADanglingLinkInsideAJunctionIsRefused: a config name that is a file link
// to no file, inside a directory reached through a junction, is refused by
// the turn as a link to nothing, before anything is made.
func TestADanglingLinkInsideAJunctionIsRefused(t *testing.T) {
	cfg, canonReal := junctionedTurn(t)
	symlinkOrSkip(t, filepath.Join(canonReal, "gone.yaml"), filepath.Join(canonReal, "dangling.yaml"))
	l, err := LockFile(filepath.Join(cfg, "dangling.yaml"))
	if err == nil {
		l.Unlock()
	}
	if !errors.Is(err, fsutil.ErrDanglingLink) {
		t.Errorf("a turn at a dangling link inside a junction = %v, want a refusal of the link", err)
	}
	if _, err := os.Lstat(filepath.Join(canonReal, "gone.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link's target was made: %v", err)
	}
}
