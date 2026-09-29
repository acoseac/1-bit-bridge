//go:build linux

package dlna

import (
	"context"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// multicastAll reads IP_MULTICAST_ALL off l's socket.
func multicastAll(t *testing.T, l *net.UDPConn) int {
	t.Helper()
	rc, err := l.SyscallConn()
	if err != nil {
		t.Fatalf("raw conn: %v", err)
	}
	var v int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_ALL)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if gerr != nil {
		t.Fatalf("getsockopt IP_MULTICAST_ALL: %v", gerr)
	}
	return v
}

// TestAStartedAdvertisersListenerHearsOnlyItsOwnInterface pins the wiring of
// backlog B71's listener half on Linux, where every CI runner can see it: the
// listener a started advertiser holds has IP_MULTICAST_ALL off, so it hears
// only the memberships it holds itself, the group on its own interface.
// TestAnSSDPListenerHearsOnlyTheInterfaceItJoined pins what that option does
// to the listener listenSSDP opens, and the network-namespace test pins what
// the advertisers then answer.
func TestAStartedAdvertisersListenerHearsOnlyItsOwnInterface(t *testing.T) {
	a := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    "http://127.0.0.1:7790/dlna/description.xml",
		ServerToken: "test",
		Interface:   loopbackInterface(t),
	})
	if err := a.Start(context.Background()); err != nil {
		t.Skipf("multicast unavailable on the loopback interface: %v", err)
	}
	t.Cleanup(a.Stop)
	a.mu.Lock()
	listener := a.listener
	a.mu.Unlock()
	if got := multicastAll(t, listener); got != 0 {
		t.Errorf("the advertiser's listener has IP_MULTICAST_ALL = %d: it hears the group's datagrams "+
			"from every interface any socket on the host joined, and answers their M-SEARCHes", got)
	}
}
