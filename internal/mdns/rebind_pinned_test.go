package mdns

import (
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// loopbackInterface returns the host's loopback interface, whose
// addresses include 127.0.0.1 on every platform the tests run on, or
// skips the test. hashicorp/mdns can pin a responder to it on macOS and
// on Linux, whose lo carries no multicast flag (measured 2026-09-28).
func loopbackInterface(t *testing.T) *net.Interface {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("net.Interfaces unavailable: %v", err)
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 && ifaces[i].Flags&net.FlagUp != 0 {
			return &ifaces[i]
		}
	}
	t.Skip("no loopback interface available in this environment")
	return nil
}

// TestRebindIgnoresAddressesOffThePinnedInterface drives the real rebind
// path with the responder pinned to the host's loopback interface: an
// address that comes and goes on some other interface (a tunnel's, a
// docker veth's) leaves the running responder alone, since it is in none
// of its records. Until 2026-09-28 the loop compared every interface's
// addresses and rebuilt.
func TestRebindIgnoresAddressesOffThePinnedInterface(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	loopback := loopbackInterface(t)
	var phase atomic.Int32
	a, err := advertiseInternal(Config{
		InstanceName:    "pinned-test",
		Port:            62995,
		ProtocolVersion: 1,
		LibraryName:     "Pinned Test",
		InterfaceSource: func() *net.Interface { return loopback },
	}, func() []net.IP {
		// 127.0.0.1 is on the loopback interface; the other address
		// stands for one on another interface, which changes.
		if phase.Load() == 0 {
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.7")}
		}
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("198.51.100.9")}
	}, time.Hour, false)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	defer a.Close()

	a.rebindMu.Lock()
	firstSrv := a.server
	a.rebindMu.Unlock()

	phase.Store(1)
	a.maybeRebind()

	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.server != firstSrv {
		t.Errorf("the responder was rebuilt for an address on another interface: was %p, now %p", firstSrv, a.server)
	}
}

// TestRebindFollowsTheInterfaceSource drives the real rebind path with
// the host's addresses unchanged and the InterfaceSource's answer
// changing: the responder is rebuilt on the new answer. Until 2026-09-28
// the loop asked the InterfaceSource only when the addresses changed, so
// this rebuilt nothing.
func TestRebindFollowsTheInterfaceSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	loopback := loopbackInterface(t)
	var picked atomic.Pointer[net.Interface]
	picked.Store(loopback)
	stable := []net.IP{net.ParseIP("127.0.0.1")}
	a, err := advertiseInternal(Config{
		InstanceName:    "follow-test",
		Port:            62994,
		ProtocolVersion: 1,
		LibraryName:     "Follow Test",
		InterfaceSource: func() *net.Interface { return picked.Load() },
	}, func() []net.IP { return stable }, time.Hour, false)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	defer a.Close()

	a.rebindMu.Lock()
	firstSrv := a.server
	a.rebindMu.Unlock()

	// The pick is lost (the picker errored): the OS picks from now on.
	picked.Store(nil)
	a.maybeRebind()

	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.server == firstSrv {
		t.Error("the responder was not rebuilt when the InterfaceSource's answer changed")
	}
}
