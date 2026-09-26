//go:build linux

package doctor

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// undumpableChildEnv makes this test binary, run again by one of the tests
// below, the process they record as a capability-bound bridge
// (runUndumpable). Its value is the child's mode: "listen" or "idle".
const undumpableChildEnv = "DOCTOR_TEST_UNDUMPABLE_CHILD"

// undumpableReady opens the line the child prints once it is ready, before
// the port it listens on (0 for none).
const undumpableReady = "undumpable child ready, port "

// startUndumpable runs this test binary again as a process of this user that
// makes itself non-dumpable, as a binary granted cap_net_bind_service runs,
// and listens on a loopback port when listen is set. It returns the child's
// pid and that port (0 when it listens on none).
//
// prctl(PR_SET_DUMPABLE, 0) is what the capability does to the process
// here, measured on dido's kernel: /proc/<pid>/fd becomes root's, mode
// 0500, so its own user gets EACCES listing it, and root without
// CAP_SYS_PTRACE (a container's) lists it and reads none of its links. So
// the bridge the NUC runs can be stood in for without setcap, which no test
// can count on.
//
// The child holds until its stdin reaches EOF: the test's cleanup kills it
// first, and if this binary dies before that, the pipe closes and the child
// exits by itself.
func startUndumpable(t *testing.T, listen bool) (pid, port int) {
	t.Helper()
	mode := "idle"
	if listen {
		mode = "listen"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), undumpableChildEnv+"="+mode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The writer is dropped here and still stays open: cmd keeps it
	// (parentIOPipes) and closes it only in Wait, once the child has
	// exited, or on a failed Start. The cleanup below holds cmd, so no
	// finalizer can close it early. internal/proctest's held shells rely
	// on the same thing.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), undumpableReady); ok {
			port, err := strconv.Atoi(p)
			if err != nil {
				t.Fatalf("the child's ready line names no port: %q", sc.Text())
			}
			return cmd.Process.Pid, port
		}
	}
	t.Fatalf("the undumpable child exited before it was ready: %s", stderr.String())
	return 0, 0
}

// runUndumpable is the child's side of startUndumpable. It never returns.
func runUndumpable(mode string) {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "prctl(PR_SET_DUMPABLE, 0):", err)
		os.Exit(1)
	}
	port := 0
	if mode == "listen" {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		port = l.Addr().(*net.TCPAddr).Port
	}
	fmt.Printf("%s%d\n", undumpableReady, port)
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// hiddenFrom reports whether this user cannot read every one of pid's
// descriptors, as the owner probe reads them: where it can (root holding
// CAP_SYS_PTRACE), the probe answers from them and the census is not asked.
func hiddenFrom(t *testing.T, pid int) bool {
	t.Helper()
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if _, err := os.Readlink(filepath.Join(fdDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// TestPortCheckFailsAPortAHiddenBridgeIsRuledOutOfByItsHolder is row L6 of
// #1028's matrix on the real kernel: the recorded bridge runs with
// dumpable=0, as the NUC's bridge granted cap_net_bind_service does, and
// holds no port of ours, and the port its config was edited to is held by
// another process of the same user (this one). The uid arm answered ok for
// that process's listener, `bridge doctor --config` exited 0, and the
// restart could not bind.
//
// No probe can read the bridge's descriptors, but this process's can be
// read, and it holds every socket listening on the port. An inode names one
// socket, and the bridge shares no listener, so none of them is the
// bridge's, and the port FAILs as another process's.
func TestPortCheckFailsAPortAHiddenBridgeIsRuledOutOfByItsHolder(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	bridge, _ := startUndumpable(t, false)
	port := bindPort(t)
	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, bridge))
	if c.Status != Fail {
		t.Fatalf("got %v (%s / %s), want fail", c.Status, c.Summary, c.Hint)
	}
	if !strings.Contains(c.Hint, "stop the process that holds the port") || strings.Contains(c.Hint, "this is expected") {
		t.Errorf("the FAIL must say to stop the holder, and not call the port expected: %s", c.Hint)
	}
	if !hiddenFrom(t, bridge) {
		t.Skip("this user reads the stand-in's descriptors (root with CAP_SYS_PTRACE), so they rule it out, not the census")
	}
	if want := fmt.Sprintf("/proc shows every socket listening on this port held by pid %d", os.Getpid()); !strings.Contains(c.Hint, want) {
		t.Errorf("the hint does not give the census:\n got %s\nwant …%s…", c.Hint, want)
	}
}

// TestPortCheckKeepsAHiddenBridgeOnItsOwnPortOK is row L2 on the real
// kernel, the NUC's ordinary state: the recorded bridge, running with
// dumpable=0, holds the port itself. No probe can read it, no process this
// user can read holds its listener, and the listener runs as this user, so
// the uid arm answers ok, as it has since #640.
func TestPortCheckKeepsAHiddenBridgeOnItsOwnPortOK(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	bridge, port := startUndumpable(t, true)
	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, bridge))
	if c.Status != OK {
		t.Fatalf("got %v (%s / %s), want ok", c.Status, c.Summary, c.Hint)
	}
	if !hiddenFrom(t, bridge) {
		t.Skip("this user reads the stand-in's descriptors (root with CAP_SYS_PTRACE), so the probe names it and the uid arm is not asked")
	}
	if want := "in use by a process running as this user"; !strings.HasPrefix(c.Summary, want) {
		t.Errorf("got %q, want the uid arm's %q…", c.Summary, want)
	}
}

// TestChosenPortRefusalOfAPortAHiddenBridgeIsRuledOutOf is L6's facts over
// an install whose config did not load (checkChosenPort). It refused the
// port before; the hint offered stopping the recorded bridge as a way
// through, which frees nothing when that bridge holds none of the port's
// sockets.
func TestChosenPortRefusalOfAPortAHiddenBridgeIsRuledOutOf(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	bridge, _ := startUndumpable(t, false)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, bridge))
	if c.Status != Fail {
		t.Fatalf("got %v (%s / %s), want fail", c.Status, c.Summary, c.Hint)
	}
	if strings.Contains(c.Hint, "stop that bridge and re-run") || !strings.Contains(c.Hint, "stop the process that holds the port") {
		t.Errorf("the bridge holds none of the port's sockets, so stopping it frees nothing: %s", c.Hint)
	}
}

// TestHiddenListenerOfThisUserOnTheKernel is the uid arm itself against the
// kernel's tables: a listener this user created that a process it can read
// holds (this one's) is that process's, and not counted; one held only by a
// process hidden from it (the stand-in's) is counted. The first is what the
// arm answered ok for in row L6.
func TestHiddenListenerOfThisUserOnTheKernel(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	own := bindPort(t)
	sockets, _, err := listenerSockets(procNetTCPFiles, own)
	if err != nil || len(sockets) != 1 {
		t.Fatalf("the socket tables list %v for this process's listener on :%d (err %v), want one socket", sockets, own, err)
	}
	for s, uid := range sockets {
		if uid != os.Getuid() {
			t.Fatalf("this process's listener %s runs as uid %d in the tables, not this user (%d)", s, uid, os.Getuid())
		}
	}
	if hidden, err := hiddenListenerOfThisUser(own); err != nil || hidden {
		t.Errorf("this process's own listener: hidden %v, err %v; want not hidden, since this user reads its holder", hidden, err)
	}
	bridge, port := startUndumpable(t, true)
	if !hiddenFrom(t, bridge) {
		t.Skip("this user reads the stand-in's descriptors (root with CAP_SYS_PTRACE)")
	}
	if hidden, err := hiddenListenerOfThisUser(port); err != nil || !hidden {
		t.Errorf("the stand-in's listener: hidden %v, err %v; want hidden, since no process this user reads holds it", hidden, err)
	}
}
