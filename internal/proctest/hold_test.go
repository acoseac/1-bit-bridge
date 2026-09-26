//go:build !windows

package proctest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// heldShell is a /bin/sh this test started, running HoldUntilReleased and
// then writing a marker, the shape of the fakes that use the hold.
type heldShell struct {
	dir, pidFile, release, wrote string

	cmd    *exec.Cmd
	exited chan struct{} // closed once cmd.Wait has returned
	err    error         // cmd.Wait's answer, set before exited is closed
}

// startHeldShell starts a heldShell in a new directory, as
// startHeldShellIn does. The directory's name holds a single quote, so
// every test that uses it also pins the quoting.
func startHeldShell(t *testing.T) *heldShell {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "the fake's dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return startHeldShellIn(t, dir, filepath.Join(dir, "pid"), filepath.Join(dir, "release"))
}

// startHeldShellIn starts a heldShell holding on the pid file and release
// file in dir, which it hands HoldUntilReleased spelled as pidArg and
// releaseArg, and waits until it has recorded its pid. Its cleanup
// releases the shell and waits for it, recreating dir first if a test
// removed it, so whatever a test does, nothing it started is left running.
func startHeldShellIn(t *testing.T, dir, pidArg, releaseArg string) *heldShell {
	t.Helper()
	h := &heldShell{
		dir:     dir,
		pidFile: filepath.Join(dir, "pid"),
		release: filepath.Join(dir, "release"),
		wrote:   filepath.Join(dir, "wrote"),
		exited:  make(chan struct{}),
	}
	h.cmd = exec.Command("/bin/sh", "-c", HoldUntilReleased(pidArg, releaseArg)+": > "+shellQuote(h.wrote)+"\n")
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		h.err = h.cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() { h.stop(t) })

	pid := waitForPIDFile(t, h.pidFile, func() string {
		select {
		case <-h.exited:
			return fmt.Sprintf("the shell exited first (%v)", h.err)
		default:
			return ""
		}
	})
	if pid != h.cmd.Process.Pid {
		t.Fatalf("the held shell recorded pid %d, but it is %d", pid, h.cmd.Process.Pid)
	}
	return h
}

// holds fails the test unless the shell is still waiting 300 ms after it
// recorded its pid. A hold that ended at once would pass every test that
// only asks whether it ends, and would turn each fake built on it into a
// process that is gone before the test can do anything to it.
func (h *heldShell) holds(t *testing.T) {
	t.Helper()
	select {
	case <-h.exited:
		t.Fatalf("the held shell exited (%v) with nothing having released it", h.err)
	case <-time.After(300 * time.Millisecond):
	}
}

// stop releases the shell, from a directory recreated if a test removed
// it, and waits for it. It kills the shell only if the release did not
// end it within 5 s. This test is the shell's parent and has not reaped
// it, so the pid it kills cannot have been reused.
func (h *heldShell) stop(t *testing.T) {
	select {
	case <-h.exited:
		return
	default:
	}
	if err := os.MkdirAll(h.dir, 0o755); err == nil {
		touch(h.release)
	}
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.exited
		t.Errorf("the held shell ignored its release for 5 s and was killed")
	}
}

// touch creates path, empty. Idempotent.
func touch(path string) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

// waitForPIDFile waits up to 10 s for a held shell to record its pid in
// path. early, polled meanwhile, returns a reason to stop waiting, or "".
func waitForPIDFile(t *testing.T, path string, early func() string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if b, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file %q: %v", b, err)
			}
			return pid
		}
		if why := early(); why != "" {
			t.Fatalf("no pid recorded in %s: %s", path, why)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid recorded in %s within 10s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAHeldShellGoesOnOnceReleased pins what the hold is for: it waits
// until released, and then the script goes on with its work and exits 0.
// A fake's write is what its test looks for after a release, so a hold
// that swallowed the release would pass a CLI that survived.
func TestAHeldShellGoesOnOnceReleased(t *testing.T) {
	h := startHeldShell(t)
	h.holds(t)

	touch(h.release)
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the held shell was released and is still waiting 5 s later")
	}
	if h.err != nil {
		t.Fatalf("the released shell exited with %v, want status 0", h.err)
	}
	if _, err := os.Stat(h.wrote); err != nil {
		t.Fatalf("the released shell did not go on to its work: %v", err)
	}
}

// TestAHeldShellEndsWithItsDirectory pins the arm that ends a fake left
// behind by a failing run. That run's cleanups release the fake and then
// remove its directory at once, and the fake almost never looks in
// between. Here the directory goes without the release, so only that arm
// can end the wait. The status is the hold's own, not a failed write's or
// a syntax error's.
func TestAHeldShellEndsWithItsDirectory(t *testing.T) {
	h := startHeldShell(t)
	h.holds(t)

	if err := os.RemoveAll(h.dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the held shell is still waiting 5 s after its directory was removed. A run " +
			"whose cleanups release it and then remove its directory, before it has looked, " +
			"leaves it waiting forever.")
	}
	var exitErr *exec.ExitError
	if !errors.As(h.err, &exitErr) || exitErr.ExitCode() != unreleasedStatus {
		t.Fatalf("the shell exited with %v; want status %d, the hold's own", h.err, unreleasedStatus)
	}
}

// TestAHeldShellGivenBareNamesEndsWithItsDirectory: a bare name's
// directory is ".", which the shell finds for as long as its working
// directory exists, removed or not, so the directory arm would never fire.
// HoldUntilReleased makes the names absolute, against the test's working
// directory, before it writes the script.
func TestAHeldShellGivenBareNamesEndsWithItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "held")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shell inherits it, so the bare names resolve to the same files
	// for it as for this test.
	t.Chdir(dir)
	h := startHeldShellIn(t, dir, "pid", "release")
	h.holds(t)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the held shell, given bare names, is still waiting 5 s after its directory was removed")
	}
	var exitErr *exec.ExitError
	if !errors.As(h.err, &exitErr) || exitErr.ExitCode() != unreleasedStatus {
		t.Fatalf("the shell exited with %v; want status %d, the hold's own", h.err, unreleasedStatus)
	}
}

// holdChildEnv makes TestAHeldShellEndsWithItsTestBinary, in the test
// binary it starts, the child that holds a shell. Its value is the
// directory to hold it in.
const holdChildEnv = "PROCTEST_HOLD_CHILD_DIR"

// TestAHeldShellEndsWithItsTestBinary pins the arm that ends a fake whose
// test binary died before running its cleanups, as one does on a timeout
// or a Ctrl-C. Nothing will release that fake or remove its directory.
//
// The test runs its own binary again as the child that holds the shell,
// because the binary that holds it has to die. The directory stays and no
// release is made, so only that arm can end the wait. The shell is the
// child's child, so this test cannot reap it, and asks Exited instead.
func TestAHeldShellEndsWithItsTestBinary(t *testing.T) {
	if dir := os.Getenv(holdChildEnv); dir != "" {
		holdAsChild(dir)
		return
	}
	dir := t.TempDir()
	pidFile, release := filepath.Join(dir, "pid"), filepath.Join(dir, "release")

	var out bytes.Buffer
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), holdChildEnv+"="+dir)
	child.Stdout, child.Stderr = &out, &out
	// Held open until the child is reaped. If this binary dies first, the
	// child reads EOF and exits too.
	if _, err := child.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-reaped
	})

	pid := waitForPIDFile(t, pidFile, func() string {
		select {
		case <-reaped:
			return "the child exited first; its output: " + out.String()
		default:
			return ""
		}
	})
	// A shell that is not waiting would pass the check below by being gone
	// already.
	for held := time.Now().Add(300 * time.Millisecond); time.Now().Before(held); {
		if exited, why := Exited(pid); exited {
			t.Fatalf("the held shell (pid %d) exited (%s) while its test binary was still running", pid, why)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The child dies as a test binary does on a timeout: at once, with
	// no cleanup run.
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-reaped
	for deadline := time.Now().Add(5 * time.Second); ; {
		exited, why := Exited(pid)
		if exited {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the held shell (pid %d) is still waiting (%s) 5 s after the test binary "+
				"that started it died. A test binary killed before its cleanups releases "+
				"nothing and removes nothing, so it would wait forever.", pid, why)
			releaseAndWait(t, pid, release)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory went too (%v), so the other arm may have ended the wait", err)
	}
}

// releaseAndWait releases a held shell this test cannot reap, and waits up
// to 5 s for it to exit. Releasing alone is not enough: t.TempDir's
// cleanup would remove the release file straight after, and a shell whose
// hold is what failed has nothing else to end it. That is the defect
// HoldUntilReleased exists for, and here the hold is what is under test.
func releaseAndWait(t *testing.T, pid int, release string) {
	t.Helper()
	touch(release)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if exited, _ := Exited(pid); exited {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("the held shell (pid %d) ignored its release as well, and is still running", pid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdAsChild is the child's side of TestAHeldShellEndsWithItsTestBinary:
// it starts a shell holding in dir, whose hold watches this process, and
// then waits to be killed. It never returns. It exits if its stdin reaches
// EOF, which happens when the parent test binary dies without killing it,
// and after an hour, a bound for anything else.
func holdAsChild(dir string) {
	cmd := exec.Command("/bin/sh", "-c", HoldUntilReleased(filepath.Join(dir, "pid"), filepath.Join(dir, "release")))
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start the held shell:", err)
		os.Exit(1)
	}
	// Reaped if it exits while this process lives. Unreaped, a shell whose
	// hold ended at once stays a zombie, which kill(pid, 0) finds on macOS,
	// and the parent's check that it holds would pass it.
	go func() { _ = cmd.Wait() }()
	eof := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(eof)
	}()
	select {
	case <-eof:
	case <-time.After(time.Hour):
	}
	os.Exit(0)
}
