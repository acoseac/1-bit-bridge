//go:build linux

package dlna

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
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

// TestAListenerTheKernelWillNotConfineStillListensAndSaysSo pins the soft
// failure of listenSSDP: a kernel (or a sandbox emulating the socket API)
// that refuses IP_MULTICAST_ALL gets the listener every Linux bridge had
// before, joined and bound, with one Warn naming the interface, never a
// refusal that would take DLNA down. The positive control is the same
// listener with the option accepted: confined, and no Warn.
func TestAListenerTheKernelWillNotConfineStillListensAndSaysSo(t *testing.T) {
	lo := loopbackInterface(t)
	group := testMulticastGroup(t)
	for _, tc := range []struct {
		name     string
		confine  func(fd uintptr) error
		wantAll  int
		wantWarn int
	}{
		{"the kernel refuses the option", func(uintptr) error { return unix.ENOPROTOOPT }, 1, 1},
		{"the kernel takes the option", multicastAllOff, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			l, err := listenSSDPConfined(context.Background(), lo, group, log, tc.confine)
			if err != nil {
				t.Fatalf("listenSSDPConfined: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if got := multicastAll(t, l); got != tc.wantAll {
				t.Errorf("IP_MULTICAST_ALL = %d, want %d", got, tc.wantAll)
			}
			if got := strings.Count(buf.String(), "SSDP listener cannot be limited to its own interface"); got != tc.wantWarn {
				t.Errorf("logged %d Warns about the listener, want %d:\n%s", got, tc.wantWarn, buf.String())
			}
			if tc.wantWarn > 0 && !strings.Contains(buf.String(), "interface="+lo.Name) {
				t.Errorf("the Warn does not name the interface %s:\n%s", lo.Name, buf.String())
			}
		})
	}
}
