package doctor

import (
	"net"
	"testing"
)

// TestOwnedPortsShortCircuitsTheProbe pins the in-process path.
//
// An admin console running INSIDE `bridge serve` bound these listeners
// itself, so it does not have to deduce ownership. That matters beyond
// speed: on a binary granted cap_net_bind_service the deduction is
// impossible (dumpable=0 denies port→pid attribution to any unprivileged
// observer), and even where it works the bind probe against our own live
// listener can only fail.
func TestOwnedPortsShortCircuitsTheProbe(t *testing.T) {
	// Bind for real, so without the short-circuit this is a genuine
	// "port in use" that would Fail.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	withPortProbe(t, true)
	d := Deps{APIPort: port, AdminPort: port + 1, OwnedPorts: []int{port}}
	if c := checkAPIPort(t.Context(), d); c.Status != OK {
		t.Errorf("api port claimed as ours: got %v (%s / %s), want ok", c.Status, c.Summary, c.Hint)
	}
}

// TestOwnedPortsDoesNotLeakAcrossChecks is the negative control that
// matters: claiming the API port must NOT excuse the admin port. A
// short-circuit that ignored which port it was asked about would hide a
// real conflict on the other one.
func TestOwnedPortsDoesNotLeakAcrossChecks(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	withPortProbe(t, true)
	// Admin port is the bound one; only the API port is claimed.
	d := Deps{APIPort: port + 1, AdminPort: port, OwnedPorts: []int{port + 1}}
	if c := checkAdminPort(t.Context(), d); c.Status == OK {
		t.Errorf("admin port %d is bound by someone else and unclaimed, but got ok (%s)", port, c.Summary)
	}
}

// TestOwnedPortsEmptyKeepsExistingBehaviour — the CLI passes nothing, and
// must land on exactly the pre-existing probe path.
func TestOwnedPortsEmptyKeepsExistingBehaviour(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	withPortProbe(t, true)
	withPIDAlive(t, false)
	withPortOwner(t, false, nil)
	if c := checkAPIPort(t.Context(), Deps{APIPort: port}); c.Status != Fail {
		t.Errorf("bound port with no OwnedPorts and no pidfile: got %v, want fail", c.Status)
	}
}

// TestOwnedPortsIgnoresZero — a config with no port parsed leaves the
// field at 0, and 0 must never match a claim (it is "unset", not a port).
func TestOwnedPortsIgnoresZero(t *testing.T) {
	if c := checkAPIPort(t.Context(), Deps{APIPort: 0, OwnedPorts: []int{0}}); c.Status == OK {
		t.Errorf("port 0 claimed as owned returned ok (%s); 0 means unset", c.Summary)
	}
}
