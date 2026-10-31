//go:build unix

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// playerAt is a config that asks for the bridge's own player, with its data
// directory at dir.
func playerAt(dir string) *config.Config {
	cfg := &config.Config{DataDir: dir}
	cfg.Output.Player.Enabled = true
	return cfg
}

// linkTo makes link a symbolic link to target.
func linkTo(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// requireStorageLine requires the player-storage line to have status, a
// summary that says says, and a hint that says hint (none for ok).
func requireStorageLine(t *testing.T, c doctor.Check, ok bool, status doctor.Status, says, hint string) {
	t.Helper()
	switch {
	case !ok:
		t.Fatal("no player-storage line for a player the config asks for")
	case c.Status != status || !strings.Contains(c.Summary, says):
		t.Fatalf("the line is %s %q, want %s saying %q", c.Status, c.Summary, status, says)
	case status == doctor.OK && c.Hint != "":
		t.Errorf("an ok line gives the hint %q", c.Hint)
	case status != doctor.OK && !strings.Contains(c.Hint, hint):
		t.Errorf("the hint is %q, want one saying %q", c.Hint, hint)
	}
}

// TestADataDirectoryALinkToNothingStandsInIsNoneServeMakes pins CodeRabbit's
// finding on #1368 (B603 round 2), through the real inspector: the medium
// check resolves every link in the data directory's path, so a data
// directory that is a symbolic link to nothing, or one below such a link,
// answers fs.ErrNotExist as a directory not made yet does, and the line
// called it "not there yet; bridge serve makes it", ok. Serve cannot make it:
// its MkdirAll fails on the link (EEXIST,
// TestServeCannotMakeADataDirectoryThroughALinkToNothing), and serve goes on
// without its lock and without the player. From the data directory up, the
// nearest entry of its path that exists is judged: a link to nothing there,
// whether it is the data directory, an ancestor, one reached in the middle
// of the path through a live link, or the head of a chain of links that ends
// in nothing (named as the entry of the data directory's own path), is a
// fail saying the player will not start and why, with the fix; a live link
// to a directory, with only the components below it to make, is still "not
// there yet", ok, since MkdirAll makes them.
func TestADataDirectoryALinkToNothingStandsInIsNoneServeMakes(t *testing.T) {
	base := t.TempDir()
	nothing := filepath.Join(base, "nothing")
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(filepath.Join(base, "target2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	linkTo(t, nothing, link)
	dangling := filepath.Join(base, "dangling")
	linkTo(t, nothing, dangling)
	live := filepath.Join(base, "live")
	linkTo(t, target, live)
	linkTo(t, filepath.Join(base, "target2"), filepath.Join(base, "mnt", "link"))
	linkTo(t, nothing, filepath.Join(base, "target2", "sub"))
	link1, link2 := filepath.Join(base, "link1"), filepath.Join(base, "link2")
	linkTo(t, link2, link1)
	linkTo(t, nothing, link2)
	const fix = "point the link at a directory, or set dataDir in bridge.yaml to one"
	for _, tc := range []struct {
		name, dir, says, hint string
		status                doctor.Status
	}{
		{"the data directory a link to nothing", link,
			"the player will not start: " + link + " is a symbolic link to nothing, and serve cannot make " + link + " through it", fix, doctor.Fail},
		{"an ancestor a link to nothing", filepath.Join(dangling, "data"),
			"the player will not start: " + dangling + " is a symbolic link to nothing, and serve cannot make " + filepath.Join(dangling, "data") + " through it", fix, doctor.Fail},
		{"a link to nothing reached through a live link", filepath.Join(base, "mnt", "link", "sub", "data"),
			"the player will not start: " + filepath.Join(base, "mnt", "link", "sub") + " is a symbolic link to nothing", fix, doctor.Fail},
		{"a chain of links that ends in nothing", filepath.Join(link1, "data"),
			"the player will not start: " + link1 + " is a symbolic link to nothing", fix, doctor.Fail},
		{"a live link with the data directory below it not made yet", filepath.Join(live, "data"),
			"not checked: the data directory " + filepath.Join(live, "data") + " does not exist yet; bridge serve makes it", "", doctor.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := playerStorageCheck(playerAt(tc.dir), "linux", atomicwrite.InspectMedium)
			requireStorageLine(t, c, ok, tc.status, tc.says, tc.hint)
		})
	}
}

// TestServeCannotMakeADataDirectoryThroughALinkToNothing measures the premise
// of the player-storage line's fail: serve's lock on its data directory
// (lockServeDataDir), which makes the directory first, fails with an error
// matching fs.ErrExist for a data directory that is a link to nothing, and
// for one below such a link, while through a live link it makes the
// directory and takes the lock.
func TestServeCannotMakeADataDirectoryThroughALinkToNothing(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "link")
	linkTo(t, filepath.Join(base, "nothing"), link)
	dangling := filepath.Join(base, "dangling")
	linkTo(t, filepath.Join(base, "nothing"), dangling)
	for _, dir := range []string{link, filepath.Join(dangling, "data")} {
		release, err := lockServeDataDir(dir)
		release()
		if !errors.Is(err, fs.ErrExist) {
			t.Errorf("lockServeDataDir(%s) = %v, want an error matching fs.ErrExist", dir, err)
		}
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(base, "live")
	linkTo(t, target, live)
	release, err := lockServeDataDir(filepath.Join(live, "data"))
	release()
	if err != nil {
		t.Fatalf("lockServeDataDir below a live link = %v, want the directory made and locked", err)
	}
	if info, err := os.Stat(filepath.Join(target, "data")); err != nil || !info.IsDir() {
		t.Errorf("serve did not make the data directory through the live link: %v", err)
	}
}

// TestNoErrorOfThePlayersStorageLineReadsAsOK pins the line's outcome for
// each error its inspection or its walk up the data directory's path meets
// (B603 round 2, the orchestrator's amendment 3): none is ok. A link cycle,
// which the medium check's resolution answers as too many links, and an
// ancestor that is a regular file (a bare ENOTDIR, which names no entry) are
// not checked, a warn naming the data directory and the error, whose advice
// is the path's, not the service user's; and where the inspection answers
// fs.ErrNotExist, a real ancestor this user cannot search (an Lstat that
// fails with EACCES on the walk) is not checked either, a warn with the
// service user's advice, never "not there yet".
func TestNoErrorOfThePlayersStorageLineReadsAsOK(t *testing.T) {
	base := t.TempDir()
	loop1, loop2 := filepath.Join(base, "loop1"), filepath.Join(base, "loop2")
	linkTo(t, loop2, loop1)
	linkTo(t, loop1, loop2)
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pathFix = "mend what the error names on the way to the data directory"
	for _, tc := range []struct{ name, dir, says string }{
		{"a link cycle", filepath.Join(loop1, "data"), "not checked: the data directory " + filepath.Join(loop1, "data") + " cannot be inspected: "},
		{"an ancestor that is a regular file", filepath.Join(file, "data"), "not checked: the data directory " + filepath.Join(file, "data") + " cannot be inspected: not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := playerStorageCheck(playerAt(tc.dir), "linux", atomicwrite.InspectMedium)
			requireStorageLine(t, c, ok, doctor.Warn, tc.says, pathFix)
		})
	}
	t.Run("an ancestor this user cannot search, on the walk", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory of mode 0")
		}
		locked := filepath.Join(base, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		dir := filepath.Join(locked, "data")
		notThere := func(string) (atomicwrite.Medium, error) {
			return atomicwrite.Medium{}, &fs.PathError{Op: "lstat", Path: dir, Err: fs.ErrNotExist}
		}
		c, ok := playerStorageCheck(playerAt(dir), "linux", notThere)
		requireStorageLine(t, c, ok, doctor.Warn, "not checked: the data directory "+dir+" cannot be inspected: lstat "+dir+": permission denied", "the user the service runs as")
	})
}
