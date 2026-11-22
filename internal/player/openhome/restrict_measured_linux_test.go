//go:build linux

package openhome

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// absentIndex is an interface index no interface has: a large constant,
// never one past the largest index listed, which an interface made
// meanwhile could take.
const absentIndex = 1 << 30

// TestALoopbackSocketRestrictedToAnotherIndexRefusesALoopbackConnection: a
// loopback socket production's Listen restricts to an index no interface has
// binds and listens, and a loopback dial to it is refused, where restricted
// to the loopback interface's own index it takes the same dial. Measured on
// a Linux test box (kernel 7.0.0-38-generic, Ubuntu 26.04.1 LTS, unprivileged,
// 2026-11-22, B527): the kernel stored the absent index without asking
// whether an interface has it (setsockopt, bind and listen each answered nil)
// and answered each of 50 loopback dials with a reset, ECONNREFUSED, within
// 0.1 ms, so the refused dial's 200 ms bound is room, never a wait. It binds
// loopback and lists the loopback interface alone (openhome.md), and skips
// only where Listen says the kernel left the socket unrestricted ("none: "),
// which leaves no restriction to measure.
func TestALoopbackSocketRestrictedToAnotherIndexRefusesALoopbackConnection(t *testing.T) {
	lo := loopbackInterface(t)
	listen := func(index int) net.Listener {
		t.Helper()
		ln, restr, err := Listen(context.Background(), netip.MustParseAddrPort("127.0.0.1:0"),
			lan.Address{Prefix: netip.MustParsePrefix("127.0.0.1/8"), Interface: lo.Name, Index: index})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		if strings.HasPrefix(restr, "none: ") {
			t.Skipf("the socket listens unrestricted, so there is no restriction to measure: %s", restr)
		}
		if restr != "SO_BINDTOIFINDEX" {
			t.Fatalf("restricted %q", restr)
		}
		return ln
	}

	// The premise: restricted to the loopback interface's own index, the
	// socket takes a loopback dial.
	own := listen(lo.Index)
	accepted := make(chan error, 1)
	go func() {
		c, err := own.Accept()
		if err == nil {
			_ = c.Close()
		}
		accepted <- err
	}()
	c, err := net.DialTimeout("tcp4", own.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("restricted to the loopback interface's own index, the socket took no loopback connection: %v", err)
	}
	_ = c.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}

	// Restricted to an index no interface has, it is handed no connection:
	// the kernel answers the SYN with a reset.
	other := listen(absentIndex)
	c, err = net.DialTimeout("tcp4", other.Addr().String(), 200*time.Millisecond)
	if err == nil {
		_ = c.Close()
		t.Fatal("restricted to an index no interface has, the socket took a loopback connection")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("restricted to an index no interface has, a loopback dial was answered %v, not refused", err)
	}
}
