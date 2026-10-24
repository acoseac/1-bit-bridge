//go:build !windows

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
