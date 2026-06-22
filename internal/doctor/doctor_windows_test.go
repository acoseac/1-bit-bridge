//go:build windows

package doctor

import (
	"net"
	"os"
	"testing"
)

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
