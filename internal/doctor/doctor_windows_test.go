//go:build windows

package doctor

import (
	"net"
	"os"
	"os/exec"
	"testing"
)

// TestPIDAlive_WindowsLiveForeignProcess pins the direction the Windows
// implementation actually promises, and the one checkPort leans on: a live
// process that is NOT us must read as alive.
//
// That is the home-pc install shape — the bridge runs as a scheduled task
// under another account, so doctor opens a handle across integrity levels.
// Getting it wrong reports a perfectly healthy bridge as dead and turns its
// own listening port into a "conflict", which is the whole reason
// PROCESS_QUERY_LIMITED_INFORMATION and the ACCESS_DENIED-means-alive branch
// exist. pidAlive(os.Getpid()) in the shared test cannot catch a regression
// there: our own process is openable with any access mask.
//
// Deliberately only this direction. The mirror assertion ("a reaped child
// reads dead") is unix-only, in pidalive_notwindows_test.go — see the note
// there; on Windows a terminated process stays openable while any handle
// survives, and asserting otherwise is how this flaked across four PRs.
func TestPIDAlive_WindowsLiveForeignProcess(t *testing.T) {
	// ping is present on every Windows SKU and self-terminates, so a
	// failed Kill leaks a process for seconds rather than forever.
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not spawn a probe process on this host: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !pidAlive(pid) {
		t.Errorf("pidAlive(%d) = false for a running child; a live bridge under another account "+
			"must not read as dead, or doctor calls its own port a conflict", pid)
	}
}

// TestIsPIDListeningOnPort_Windows binds a loopback listener and asserts
// the native iphlpapi probe attributes the port to our own PID. Runs only
// on Windows (CI is linux/darwin), so this is the home-pc / local-Windows
// verification gate for the GetExtendedTcpTable path.
func TestIsPIDListeningOnPort_Windows(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	found, perr := isPIDListeningOnPort(port, os.Getpid())
	if perr != nil {
		t.Fatalf("probe errored: %v", perr)
	}
	if !found {
		t.Errorf("our PID %d should own the listener on port %d", os.Getpid(), port)
	}
}

// TestIsPIDListeningOnPort_WindowsFreePort grabs then releases a port so
// nothing is listening on it, and asserts the probe reports not-found
// (cleanly, no error). Mildly racy on the freed port — matches the
// accepted raciness of mustFreePort in the shared test file.
func TestIsPIDListeningOnPort_WindowsFreePort(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	found, perr := isPIDListeningOnPort(port, os.Getpid())
	if perr != nil {
		t.Fatalf("probe errored: %v", perr)
	}
	if found {
		t.Errorf("no listener should be attributed on freed port %d", port)
	}
}
