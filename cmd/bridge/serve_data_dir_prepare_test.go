package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Backlog B606, the owner's decision of 2026-10-31: serve refuses a data
// directory it cannot prepare (make, or find, as a directory), naming the
// path and what stands in its way, before its first write; a lock that
// cannot be taken on a directory that is there still never stops it. Serve
// used to go on after every lock error that was not contention, MkdirAll's
// included, without its lock, its player's state or its player.

// unusableDataDir is a shape of what stands where a data directory goes,
// which MkdirAll stops at: lay lays it out below base and returns the data
// directory and what a refusal says of the entry in its way, and cause is
// the error MkdirAll answers for it.
type unusableDataDir struct {
	name  string
	cause error
	lay   func(t *testing.T, base string) (dataDir, why string)
}

// unusableDataDirs are the shapes B606 names: a regular file at the data
// directory, a link to nothing there, a link to a regular file there, and an
// ancestor that is a regular file or a link to nothing.
var unusableDataDirs = []unusableDataDir{
	{"a regular file at the data directory", syscall.ENOTDIR, func(t *testing.T, base string) (string, string) {
		file := filepath.Join(base, "data")
		writeFileOrFail(t, file)
		return file, file + " is a regular file, not a directory"
	}},
	{"a link to nothing at the data directory", fs.ErrExist, func(t *testing.T, base string) (string, string) {
		link := filepath.Join(base, "data")
		symlinkOrSkip(t, filepath.Join(base, "nothing"), link)
		return link, link + " is a symbolic link to nothing"
	}},
	{"a link to a regular file at the data directory", syscall.ENOTDIR, func(t *testing.T, base string) (string, string) {
		file := filepath.Join(base, "file")
		writeFileOrFail(t, file)
		link := filepath.Join(base, "data")
		symlinkOrSkip(t, file, link)
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatal(err)
		}
		return link, link + " leads to " + resolved + ", a regular file, not a directory"
	}},
	{"an ancestor that is a regular file", syscall.ENOTDIR, func(t *testing.T, base string) (string, string) {
		file := filepath.Join(base, "config")
		writeFileOrFail(t, file)
		return filepath.Join(file, "data"), file + " is a regular file, not a directory"
	}},
	{"an ancestor that is a link to nothing", fs.ErrExist, func(t *testing.T, base string) (string, string) {
		link := filepath.Join(base, "config")
		symlinkOrSkip(t, filepath.Join(base, "nothing"), link)
		return filepath.Join(link, "data"), link + " is a symbolic link to nothing"
	}},
}

// writeFileOrFail writes a few bytes at path.
func writeFileOrFail(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// symlinkOrSkip makes link a symbolic link to target, or skips the test on
// a host that makes none (Windows without the privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host makes no symlink here: %v", err)
	}
}

// writeServeConfigApart is writeServeConfig's config with the TLS pair kept
// in tlsDir, outside the data directory, so a serve that mints the pair
// before it refuses the data directory is seen to.
func writeServeConfigApart(t *testing.T, path, lib, dataDir, tlsDir string) string {
	t.Helper()
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\n"+
		"tlsCertPath: %s\ntlsKeyPath: %s\ndisableHttp3: true\nmdns:\n    enabled: false\n",
		lib, dataDir, filepath.Join(tlsDir, "server.crt"), filepath.Join(tlsDir, "server.key"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServeRefusesADataDirectoryItCannotPrepare runs serve over each shape
// of a data directory MkdirAll cannot make, and requires it to refuse, exit
// 1, saying the data directory cannot be used and naming the entry in its
// way and what it is, before its first write: the TLS pair, which the config
// keeps outside the data directory, is not minted, and no lock warning is
// printed, since no lock is tried. Serve used to warn that it could not lock
// the data directory, mint the pair and fail further on, or serve on.
func TestServeRefusesADataDirectoryItCannotPrepare(t *testing.T) {
	for _, shape := range unusableDataDirs {
		t.Run(shape.name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			tlsDir := filepath.Join(base, "tls")
			for _, d := range []string{lib, tlsDir} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dataDir, why := shape.lay(t, base)
			cfg := writeServeConfigApart(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, tlsDir)
			code, stdout, stderr := runSecondServe(t, "--config", cfg)
			want := "serve: the data directory " + dataDir + " cannot be used: " + why
			switch {
			case code != 1:
				t.Errorf("serve over %s exited %d, want 1; stderr=%s", shape.name, code, stderr)
			case !strings.Contains(stderr, want):
				t.Errorf("serve's refusal does not say %q; stderr=%s", want, stderr)
			}
			if strings.Contains(stdout, "listening on") {
				t.Errorf("serve served over %s; stdout=%s", shape.name, stdout)
			}
			if strings.Contains(stderr, "could not lock") {
				t.Errorf("serve tried the lock of a data directory it could not prepare; stderr=%s", stderr)
			}
			for _, name := range []string{"server.crt", "server.key"} {
				if _, err := os.Lstat(filepath.Join(tlsDir, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("serve wrote %s before it refused the data directory (lstat: %v)", name, err)
				}
			}
		})
	}
}

// TestServeStartsOnADataDirectoryALinkLeadsTo: a data directory that is a
// link to a directory, as Linux and NAS installs keep one, and one below
// such a link that is not made yet, still serve, the lock taken in the
// directory the link leads to and nothing refused or warned of.
func TestServeStartsOnADataDirectoryALinkLeadsTo(t *testing.T) {
	for _, below := range []bool{false, true} {
		name := "a link to a directory"
		if below {
			name = "a directory not made yet below a link to one"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			target := filepath.Join(base, "target")
			for _, d := range []string{lib, target} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(base, "link")
			symlinkOrSkip(t, target, link)
			dataDir, made := link, target
			if below {
				dataDir, made = filepath.Join(link, "data"), filepath.Join(target, "data")
			}
			cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
			served := bootServe(t, "--config", cfg)
			if s := served.stderr.String(); strings.Contains(s, "cannot be used") || strings.Contains(s, "could not lock") || strings.Contains(s, "gave no lock") {
				t.Errorf("serve refused or warned of a data directory a link leads to; stderr=%s", s)
			}
			if info, err := os.Stat(filepath.Join(made, serveLockFileName)); err != nil || !info.Mode().IsRegular() {
				t.Errorf("serve took no lock in the directory the link leads to (stat: %v)", err)
			}
		})
	}
}

// TestPreparingTheDataDirectoryRefusesWithItsPathAndItsCause: the
// preparation's refusal is a *dataDirUnusableError naming the data
// directory, and errors.Is still reaches the error MkdirAll answered: the
// EEXIST of a link to nothing, the ENOTDIR of a regular file.
func TestPreparingTheDataDirectoryRefusesWithItsPathAndItsCause(t *testing.T) {
	for _, shape := range unusableDataDirs {
		t.Run(shape.name, func(t *testing.T) {
			dataDir, why := shape.lay(t, t.TempDir())
			err := prepareServeDataDir(dataDir)
			var unusable *dataDirUnusableError
			switch {
			case !errors.As(err, &unusable):
				t.Fatalf("prepareServeDataDir(%s) = %v, want a *dataDirUnusableError", dataDir, err)
			case unusable.dataDir != dataDir || !strings.Contains(err.Error(), "the data directory "+dataDir+" cannot be used: "+why):
				t.Errorf("the refusal is %q for %s, want one naming it and saying %q", err, unusable.dataDir, why)
			}
			if !errors.Is(err, shape.cause) {
				t.Errorf("the refusal %v does not carry %v, the error MkdirAll answered", err, shape.cause)
			}
		})
	}
	if err := prepareServeDataDir(filepath.Join(t.TempDir(), "new", "data")); err != nil {
		t.Errorf("prepareServeDataDir refused a data directory it can make: %v", err)
	}
}

// mutationCommand is a command that writes the data directory from a second
// process and takes the data dir's lock first (holdDataDirForMutation).
type mutationCommand struct {
	name string
	// run runs the command over the config at cfgPath, its confirmation
	// skipped, and returns its exit code and everything it printed.
	run func(t *testing.T, cfgPath string) (int, string)
}

// mutationCommands are holdDataDirForMutation's four callers: restore (of
// snap), clear-missing, reset-player and the DAC bind (over root's trees).
func mutationCommands(snap, root string) []mutationCommand {
	return []mutationCommand{
		{"restore", func(t *testing.T, cfgPath string) (int, string) {
			code, out, errOut := runRestoreYes(t, cfgPath, snap)
			return code, out + errOut
		}},
		{cmdNameManifestClearMissing, func(t *testing.T, cfgPath string) (int, string) {
			code, out, errOut := runClearMissingYes(t, cfgPath)
			return code, out + errOut
		}},
		{"reset-player", func(t *testing.T, cfgPath string) (int, string) {
			var out, errOut bytes.Buffer
			code := playerResetCmd(context.Background(), []string{"--config", cfgPath, "--yes"}, strings.NewReader(""), &out, &errOut)
			return code, out.String() + errOut.String()
		}},
		{"output dac bind", func(t *testing.T, cfgPath string) (int, string) {
			out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			return code, out + errOut
		}},
	}
}

// writeMutationConfig writes a config for the commands over dataDir, its
// admin port fixed and closed, so the admin probe finds no bridge.
func writeMutationConfig(t *testing.T, base, dataDir string) string {
	t.Helper()
	lib := filepath.Join(base, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: %s\ndisableHttp3: true\nmdns:\n    enabled: false\n",
		lib, dataDir, closedAdminAddr(t))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEveryCommandThatMutatesTheDataDirRefusesOneItCannotPrepare runs each
// of holdDataDirForMutation's callers over a data directory that is a
// regular file, and requires each to refuse with exit 1 before it writes
// anything, naming the data directory and the file: restore and reset-player
// used to say the lock could not be taken and to fix its permissions,
// clear-missing to go on past its prompt and fail at the store, and the DAC
// bind to write the binding into bridge.yaml and succeed.
func TestEveryCommandThatMutatesTheDataDirRefusesOneItCannotPrepare(t *testing.T) {
	_, _, snap := seedRestorableInstall(t, closedAdminAddr(t))
	for _, c := range mutationCommands(snap, dacTree(t)) {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			dataDir := filepath.Join(base, "data")
			writeFileOrFail(t, dataDir)
			cfgPath := writeMutationConfig(t, base, dataDir)
			before := mustRead(t, cfgPath)
			code, out := c.run(t, cfgPath)
			want := c.name + ": the data directory " + dataDir + " cannot be used: " + dataDir + " is a regular file, not a directory"
			switch {
			case code != 1:
				t.Errorf("%s over a data directory that is a regular file exited %d, want 1:\n%s", c.name, code, out)
			case !strings.Contains(out, want):
				t.Errorf("%s's refusal does not say %q:\n%s", c.name, want, out)
			}
			if after := mustRead(t, cfgPath); after != before {
				t.Errorf("%s changed bridge.yaml over a data directory it could not use:\n%s", c.name, after)
			}
			if got := mustRead(t, dataDir); got != "not a directory" {
				t.Errorf("%s changed the file at the data directory: %q", c.name, got)
			}
		})
	}
}

// TestEveryCommandThatMutatesTheDataDirKeepsItsLockPolicy runs each caller
// over a data directory that is there, with a directory standing where the
// lock file goes, so no lock can be taken: each keeps its own policy, which
// B606 leaves as it was. Restore and reset-player refuse, since each needs
// the exclusive lock; clear-missing and the DAC bind fall back to the admin
// probe, which finds nothing on the closed port, and go through.
func TestEveryCommandThatMutatesTheDataDirKeepsItsLockPolicy(t *testing.T) {
	_, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	refuses := map[string]bool{"restore": true, "reset-player": true}
	for _, c := range mutationCommands(snap, dacTree(t)) {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			ownData := filepath.Join(base, "data")
			copyTree(t, dataDir, ownData)
			blockDataDirLock(t, ownData)
			code, out := c.run(t, writeMutationConfig(t, base, ownData))
			switch {
			case refuses[c.name] && (code != 1 || !strings.Contains(out, "exclusive lock could not be taken")):
				t.Errorf("%s ran without its exclusive lock, exit %d:\n%s", c.name, code, out)
			case !refuses[c.name] && code != 0:
				t.Errorf("%s did not fall back to the admin probe where no lock can be taken, exit %d:\n%s", c.name, code, out)
			}
			if strings.Contains(out, "cannot be used") {
				t.Errorf("%s refused a data directory that is there:\n%s", c.name, out)
			}
		})
	}
}

// copyTree copies the regular files below src into dst, made as needed.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runServeWith runs runServe with opts on a goroutine and returns its exit
// code and output once it has exited, or, once it serves, what it printed
// up to then with served true; a serve that serves is drained on cleanup.
func runServeWith(t *testing.T, opts serveOpts) (code int, served bool, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out, errOut := &safeBuffer{}, &safeBuffer{}
	exitCode := make(chan int, 1)
	exited := make(chan struct{})
	go func(stdout, stderr io.Writer) {
		defer close(exited)
		exitCode <- runServe(ctx, opts, stdout, stderr)
	}(out, errOut)
	drainServeOnCleanup(t, cancel, exited, exitCode, errOut)
	giveUp := serveGiveUp(t)
	for {
		select {
		case <-exited:
			return <-exitCode, false, out.String(), errOut.String()
		case <-giveUp:
			t.Fatalf("serve neither exited nor served before the test's deadline; stderr=%s\nserve's goroutines:\n%s",
				errOut.String(), serveStacks())
		case <-time.After(25 * time.Millisecond):
		}
		if strings.Contains(out.String(), "listening on") {
			return 0, true, out.String(), errOut.String()
		}
	}
}

// lockAnswer is a stand-in for the kernel's answer to the data dir's lock.
func lockAnswer(err error) func(*os.File) error {
	return func(f *os.File) error { return &os.PathError{Op: "flock", Path: f.Name(), Err: err} }
}

// noLockLine and couldNotLockLine are how serve says it took no lock on its
// data directory: where the filesystem's answer is one a filesystem that
// keeps no locks gives (fsutil.KeepsNoLocks), and for any other answer.
const (
	noLockLine       = "serve: the filesystem gave no lock on "
	noLockLineReason = "a filesystem that keeps no locks answers so, and so do an NFS mount whose lock manager is not running " +
		"and a kernel out of lock records, so another bridge serve may run on this data dir all the same; serving without the check that none does"
	couldNotLockLine = "serve: could not lock "
)

// TestServeSaysWhichKindOfLockItCouldNotTake drives serve's lock through
// each kind of answer (B537 item 1): another open holding it (contention,
// fsutil.ErrLocked) refuses; an answer that says no lock can be taken there
// (errors.ErrUnsupported, which fsutil.KeepsNoLocks reads on every platform)
// and any other answer (an I/O error) serve on, each said its own way, the
// first without claiming the filesystem keeps no locks, which ENOLCK, one of
// its answers, does not prove.
func TestServeSaysWhichKindOfLockItCouldNotTake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error
		serves bool
		says   []string
		not    string
	}{
		{"another open holds the lock", fmt.Errorf("%w", fsutil.ErrLocked), false, []string{"already running on the data dir"}, "serving without"},
		{"no lock can be taken there", errors.ErrUnsupported, true, []string{noLockLine, noLockLineReason}, couldNotLockLine},
		{"an I/O error", syscall.EIO, true, []string{couldNotLockLine, "serving without the check that no other bridge serve runs on this data dir"}, noLockLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			if err := os.Mkdir(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			dataDir := filepath.Join(base, "data")
			cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
			code, served, _, stderr := runServeWith(t, serveOpts{configPath: cfg, dataDirTryLock: lockAnswer(tc.answer)})
			switch {
			case served != tc.serves:
				t.Fatalf("serve served %v, want %v (exit %d); stderr=%s", served, tc.serves, code, stderr)
			case !tc.serves && code != 1:
				t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
			}
			for _, s := range tc.says {
				if !strings.Contains(stderr, s) {
					t.Errorf("serve did not say %q; stderr=%s", s, stderr)
				}
			}
			if strings.Contains(stderr, tc.not) {
				t.Errorf("serve said %q, another kind's line; stderr=%s", tc.not, stderr)
			}
		})
	}
}

// TestTheDataDirsLockSortsEachAnswerIntoItsKind pins lockServeDataDir's
// sorting of what the lock answered (B537 item 1), on a data directory that
// is there: a held lock is a *serveDataDirHeldError; every other answer is a
// *dataDirUnlockedError, marked noLocks exactly where fsutil.KeepsNoLocks
// reads the answer (here errors.ErrUnsupported, which it reads on every
// platform; the rest of its answers are platform's own, in the unix and
// Windows halves of this test), and so is a lock file that cannot be opened
// (a directory standing where it goes), which is never marked.
func TestTheDataDirsLockSortsEachAnswerIntoItsKind(t *testing.T) {
	for _, tc := range append([]lockKindCase{
		{"no lock on this platform", errors.ErrUnsupported, true},
		{"an I/O error", syscall.EIO, false},
	}, platformLockKindCases...) {
		t.Run(tc.name, func(t *testing.T) {
			requireLockKind(t, tc)
		})
	}
	t.Run("a held lock", func(t *testing.T) {
		dataDir := t.TempDir()
		release, err := lockServeDataDirWith(dataDir, lockAnswer(fsutil.ErrLocked))
		release()
		var held *serveDataDirHeldError
		if !errors.As(err, &held) {
			t.Errorf("a held lock answered %v, want a *serveDataDirHeldError", err)
		}
	})
	t.Run("a lock file that cannot be opened", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dataDir, serveLockFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		release, err := lockServeDataDir(dataDir)
		release()
		var unlocked *dataDirUnlockedError
		if !errors.As(err, &unlocked) || unlocked.noLocks || unlocked.lockPath != filepath.Join(dataDir, serveLockFileName) {
			t.Errorf("a directory at the lock file answered %v, want an unmarked *dataDirUnlockedError naming %s", err, serveLockFileName)
		}
	})
}

// lockKindCase is one answer of the lock's and whether fsutil.KeepsNoLocks
// reads it.
type lockKindCase struct {
	name    string
	answer  error
	noLocks bool
}

// requireLockKind requires lockServeDataDir, answered tc.answer, to return a
// *dataDirUnlockedError of tc's kind, naming the lock file and carrying the
// answer.
func requireLockKind(t *testing.T, tc lockKindCase) {
	t.Helper()
	dataDir := t.TempDir()
	release, err := lockServeDataDirWith(dataDir, lockAnswer(tc.answer))
	release()
	var unlocked *dataDirUnlockedError
	switch {
	case !errors.As(err, &unlocked):
		t.Fatalf("the lock answered %v, want a *dataDirUnlockedError", err)
	case unlocked.noLocks != tc.noLocks:
		t.Errorf("%v sorted noLocks %v, want %v", tc.answer, unlocked.noLocks, tc.noLocks)
	case unlocked.lockPath != filepath.Join(dataDir, serveLockFileName) || !errors.Is(err, tc.answer):
		t.Errorf("the refusal %v names %s, want %s and the answer %v", err, unlocked.lockPath, filepath.Join(dataDir, serveLockFileName), tc.answer)
	}
}
