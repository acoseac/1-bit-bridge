//go:build unix

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
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
// directory below a symbolic link to nothing answers fs.ErrNotExist as a
// directory not made yet does, and the line called it "not there yet;
// bridge serve makes it", ok, as it did a data directory that is such a link
// until B606, which the inspection now tells apart itself. Serve cannot make
// it: its MkdirAll fails on the link (EEXIST,
// TestServeCannotMakeADataDirectoryThroughALinkToNothing), and since B606
// serve refuses to start. From the data directory up, the nearest entry of
// its path that exists is judged: a link to nothing there, whether it is the
// data directory, an ancestor, one reached in the middle of the path through
// a live link, or the head of a chain of links that ends in nothing (named as
// the entry of the data directory's own path), is a fail saying bridge serve
// will not start and why, with the fix; a live link to a directory, with
// only the components below it to make, is still "not there yet", ok, since
// MkdirAll makes them.
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
			"bridge serve will not start: " + link + " is a symbolic link to nothing, and serve cannot make " + link + " through it", fix, doctor.Fail},
		{"an ancestor a link to nothing", filepath.Join(dangling, "data"),
			"bridge serve will not start: " + dangling + " is a symbolic link to nothing, and serve cannot make " + filepath.Join(dangling, "data") + " through it", fix, doctor.Fail},
		{"a link to nothing reached through a live link", filepath.Join(base, "mnt", "link", "sub", "data"),
			"bridge serve will not start: " + filepath.Join(base, "mnt", "link", "sub") + " is a symbolic link to nothing", fix, doctor.Fail},
		{"a chain of links that ends in nothing", filepath.Join(link1, "data"),
			"bridge serve will not start: " + link1 + " is a symbolic link to nothing", fix, doctor.Fail},
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
// of the player-storage line's fail: serve's preparation of its data
// directory (prepareServeDataDir), which makes it before serve takes its
// lock, refuses a data directory that is a link to nothing, and one below
// such a link, with a *dataDirUnusableError whose cause matches fs.ErrExist,
// as this user (os.MkdirAll) and as root (fsutil's own walk, an os.Root on
// each parent, simulated), making nothing; while through a live link it
// makes the directory, and the lock is taken there.
func TestServeCannotMakeADataDirectoryThroughALinkToNothing(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "link")
	linkTo(t, filepath.Join(base, "nothing"), link)
	dangling := filepath.Join(base, "dangling")
	linkTo(t, filepath.Join(base, "nothing"), dangling)
	refused := func(t *testing.T, dir string) {
		t.Helper()
		err := prepareServeDataDir(dir)
		var unusable *dataDirUnusableError
		if !errors.As(err, &unusable) || !errors.Is(err, fs.ErrExist) {
			t.Errorf("prepareServeDataDir(%s) = %v, want a *dataDirUnusableError matching fs.ErrExist", dir, err)
		}
	}
	for _, dir := range []string{link, filepath.Join(dangling, "data")} {
		refused(t, dir)
	}
	t.Run("as root", func(t *testing.T) {
		// An owner no entry here has, so a walk that took the link's entry
		// for a directory it made would give the link that owner.
		changes, restore := fsutil.SimulateRootForTest(4242, 4243)
		defer restore()
		for _, dir := range []string{link, filepath.Join(dangling, "data")} {
			refused(t, dir)
		}
		if made := changes(); len(made) != 0 {
			t.Errorf("serve gave %v an owner through a link to nothing, want nothing made", made)
		}
	})
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(base, "live")
	linkTo(t, target, live)
	if err := prepareServeDataDir(filepath.Join(live, "data")); err != nil {
		t.Fatalf("prepareServeDataDir below a live link = %v, want the directory made", err)
	}
	release, err := lockServeDataDir(filepath.Join(live, "data"))
	release()
	if err != nil {
		t.Fatalf("lockServeDataDir below a live link = %v, want the directory locked", err)
	}
	if info, err := os.Stat(filepath.Join(target, "data")); err != nil || !info.IsDir() {
		t.Errorf("serve did not make the data directory through the live link: %v", err)
	}
}

// TestADataDirectoryThatIsNoDirectoryFailsThePlayersStorageLine pins the
// line on a data directory that is there and is not a directory, which
// serve refuses to start on (B606), through the real inspector: a regular
// file, a link to a regular file and a link to nothing are each a fail
// naming the data directory, what is there and the fix. The inspection tells
// a link to nothing at the data directory apart itself, never answering
// fs.ErrNotExist, a data directory not made yet; it used to, and called a
// regular file there storage it takes.
func TestADataDirectoryThatIsNoDirectoryFailsThePlayersStorageLine(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	toFile := filepath.Join(base, "to-file")
	linkTo(t, file, toFile)
	resolved, err := filepath.EvalSymlinks(toFile)
	if err != nil {
		t.Fatal(err)
	}
	toNothing := filepath.Join(base, "to-nothing")
	linkTo(t, filepath.Join(base, "nothing"), toNothing)
	for _, tc := range []struct{ name, dir, says, hint string }{
		{"a regular file", file,
			"bridge serve will not start: the data directory " + file + " is a regular file, not a directory",
			"move it aside, or set dataDir in bridge.yaml to a directory"},
		{"a link to a regular file", toFile,
			"bridge serve will not start: the data directory " + toFile + " leads to " + resolved + ", a regular file, not a directory",
			"point the link at a directory, or set dataDir in bridge.yaml to one"},
		{"a link to nothing", toNothing,
			"bridge serve will not start: " + toNothing + " is a symbolic link to nothing, and serve cannot make " + toNothing + " through it",
			"point the link at a directory, or set dataDir in bridge.yaml to one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answered error
			inspect := func(root string) (atomicwrite.Medium, error) {
				m, err := atomicwrite.InspectMedium(root)
				answered = err
				return m, err
			}
			c, ok := playerStorageCheck(playerAt(tc.dir), "linux", inspect)
			requireStorageLine(t, c, ok, doctor.Fail, tc.says, tc.hint)
			if errors.Is(answered, fs.ErrNotExist) {
				t.Errorf("the inspection answered %v, a data directory not made yet, for %s", answered, tc.name)
			}
		})
	}
}

// TestBothDoctorsJudgeADataDirectoryThatIsNoDirectoryAsServeDoes runs bridge
// doctor and the console's doctor, on a stand-in Linux box with the real
// inspector, over a config that asks for the player: a data directory that is
// a regular file fails both, word for word, and bridge doctor exits 1; one
// below a directory this user cannot search is not checked, a warn in both,
// since the service's user may read what this one cannot.
func TestBothDoctorsJudgeADataDirectoryThatIsNoDirectoryAsServeDoes(t *testing.T) {
	prevGOOS := playerStorageGOOS
	playerStorageGOOS = "linux"
	t.Cleanup(func() { playerStorageGOOS = prevGOOS })
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	type doctorsCase struct {
		name, dataDir, badge, says string
		code                       int // -1: a warn decides no exit code
	}
	cases := []doctorsCase{
		{"a regular file", file, "[FAIL]", "bridge serve will not start: the data directory " + file + " is a regular file, not a directory", 1},
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(base, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		// The inspection resolves the links above the data directory before
		// it is refused, as macOS's /var is resolved to /private/var.
		resolved, err := filepath.EvalSymlinks(locked)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		dir := filepath.Join(locked, "data")
		cases = append(cases, doctorsCase{"below a directory this user cannot search", dir, "[warn]",
			"not checked: the data directory " + dir + " cannot be inspected: lstat " + filepath.Join(resolved, "data") + ": " + syscall.EACCES.Error(), -1})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.yaml")
			body := "libraryRoots:\n  - " + base + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + tc.dataDir +
				"\noutput:\n  mpd: off\n  player:\n    enabled: true\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut strings.Builder
			code := doctorCmd([]string{"--config", path}, &out, &errOut)
			if want := tc.badge + " player-storage      " + tc.says; !strings.Contains(out.String(), want) {
				t.Errorf("bridge doctor printed\n%s\nwant %q", out.String(), want)
			}
			if tc.code >= 0 && code != tc.code {
				t.Errorf("bridge doctor exited %d, want %d", code, tc.code)
			}
			cli := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(path)), playerStorageCheckName)
			var console *doctor.Check
			for _, c := range adminDoctorRunner(path, nil, nil)(context.Background()).Checks {
				if c.Name == playerStorageCheckName {
					console = &doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
				}
			}
			switch {
			case console == nil:
				t.Fatal("the console's doctor has no player-storage line")
			case *console != cli:
				t.Errorf("the console's doctor says %+v, bridge doctor %+v", *console, cli)
			}
		})
	}
}

// TestThePlayersStorageLineTrustsNoWalkThatFindsNothingInTheWay: where the
// inspection answered a bare ENOTDIR, an entry on the data directory's way
// that is no directory, and the walk up its path finds nothing in the way, the
// path changed in between, and the line says it was not checked, naming the
// inspection's error, never "not there yet" (B659). Unix's: on Windows
// syscall.ENOTDIR is ERROR_PATH_NOT_FOUND, an absence.
func TestThePlayersStorageLineTrustsNoWalkThatFindsNothingInTheWay(t *testing.T) {
	notYet := filepath.Join(t.TempDir(), "data")
	c, ok := playerStorageCheck(playerAt(notYet), "linux", func(string) (atomicwrite.Medium, error) { return atomicwrite.Medium{}, syscall.ENOTDIR })
	requireStorageLine(t, c, ok, doctor.Warn, "not checked: the data directory "+notYet+" cannot be inspected: "+syscall.ENOTDIR.Error(), "mend what the error names on the way to the data directory")
}

// TestNoErrorOfThePlayersStorageLineReadsAsOK pins the line's outcome for
// each error its inspection or its walk up the data directory's path meets
// (B603 round 2, the orchestrator's amendment 3): none is ok. A link cycle,
// which the medium check's resolution answers as too many links, is not
// checked, a warn naming the data directory and the error, whose advice is
// the path's, not the service user's; and where the inspection answers
// fs.ErrNotExist, a real ancestor this user cannot search (an Lstat that
// fails with EACCES on the walk) is not checked either, a warn with the
// service user's advice, never "not there yet". An ancestor that is a regular
// file, whose bare ENOTDIR names no entry, was a warn here until B659, which
// walks the path for the entry in its way and fails it, as serve refuses it
// (TestThePlayersStorageLineSaysWhatTheDataDirLineSays).
func TestNoErrorOfThePlayersStorageLineReadsAsOK(t *testing.T) {
	base := t.TempDir()
	loop1, loop2 := filepath.Join(base, "loop1"), filepath.Join(base, "loop2")
	linkTo(t, loop2, loop1)
	linkTo(t, loop1, loop2)
	const pathFix = "mend what the error names on the way to the data directory"
	for _, tc := range []struct{ name, dir, says string }{
		{"a link cycle", filepath.Join(loop1, "data"), "not checked: the data directory " + filepath.Join(loop1, "data") + " cannot be inspected: "},
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
