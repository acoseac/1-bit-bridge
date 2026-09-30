package upnp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// TestTheServerClientReadsItsLinkBeforeEverySearch pins the upstream client's
// wiring of the link refresh (backlog B49), as the renderer client's twin
// does: the running client reads its interface's addresses again before each
// M-SEARCH, so the verdict on which link-local sources approve themselves
// follows the interface, from a DHCP address to a self-assigned one and back.
func TestTheServerClientReadsItsLinkBeforeEverySearch(t *testing.T) {
	var mu sync.Mutex
	current := []net.Addr{&net.IPNet{IP: net.IPv4(192, 0, 2, 1), Mask: net.CIDRMask(24, 32)}}
	set := func(a net.Addr) {
		mu.Lock()
		defer mu.Unlock()
		current = []net.Addr{a}
	}
	c, err := NewMediaServerDiscoveryClient(DiscoveryConfig{
		Interface: &net.Interface{},
		InterfaceAddrs: func() ([]net.Addr, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]net.Addr(nil), current...), nil
		},
		MSearchInterval: 20 * time.Millisecond,
		Dispatcher:      &recordingDispatcher{},
	}, NewServerCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	sent := make(chan struct{}, 1)
	c.writeMSearch = func(_ *net.UDPConn, b []byte, _ *net.UDPAddr) (int, error) {
		select {
		case sent <- struct{}{}:
		default:
		}
		return len(b), nil
	}
	if err := c.Start(context.Background()); err != nil {
		t.Skipf("cannot bind a UDP socket in this environment: %v", err)
	}
	t.Cleanup(c.Stop)
	awaitVerdict := func(want bool, what string) {
		t.Helper()
		for range 20 {
			select {
			case <-sent:
			case <-time.After(5 * time.Second):
				t.Fatal("the tick loop sent no M-SEARCH")
			}
			if c.link.ZeroConf() == want {
				return
			}
		}
		t.Errorf("%s: twenty searches later the link still reads zero-conf = %v", what, !want)
	}
	awaitVerdict(false, "built on a DHCP address")
	set(&net.IPNet{IP: net.IPv4(169, 254, 3, 4), Mask: net.CIDRMask(16, 32)})
	awaitVerdict(true, "after the interface self-assigned an address")
	set(&net.IPNet{IP: net.IPv4(192, 0, 2, 1), Mask: net.CIDRMask(24, 32)})
	awaitVerdict(false, "after the interface got a DHCP address")
}
