package openhome

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// loopbackInterface is the host's loopback interface, by its flags: the one
// interface a test binds with the restriction, never a LAN one.
func loopbackInterface(t *testing.T) net.Interface {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skipf("the interfaces cannot be listed: %v", err)
	}
	for _, in := range ifs {
		if in.Flags&net.FlagLoopback != 0 && in.Flags&net.FlagUp != 0 {
			return in
		}
	}
	t.Skip("no loopback interface is up")
	return net.Interface{}
}

// TestEachSocketIsRestrictedToItsInterfaceWhereThePlatformLets: Listen,
// production's, restricts a socket to its interface before its bind and
// says how: IP_BOUND_IF on macOS, SO_BINDTOIFINDEX on Linux where the
// kernel lets an unprivileged process set it (and "none: " with the
// kernel's refusal where it does not), nothing elsewhere; the option read
// back from the socket is the interface's index; and the socket listens
// whatever the restriction. It binds loopback, with the loopback
// interface's index.
func TestEachSocketIsRestrictedToItsInterfaceWhereThePlatformLets(t *testing.T) {
	lo := loopbackInterface(t)
	a := lan.Address{Prefix: netip.MustParsePrefix("127.0.0.1/8"), Interface: lo.Name, Index: lo.Index}
	ln, restr, err := Listen(context.Background(), netip.MustParseAddrPort("127.0.0.1:0"), a)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	index, readable := boundInterface(t, ln)
	switch runtime.GOOS {
	case "darwin":
		if restr != "IP_BOUND_IF" || !readable || index != lo.Index {
			t.Errorf("restricted %q, the socket's IP_BOUND_IF %d (%v); want IP_BOUND_IF to %d", restr, index, readable, lo.Index)
		}
	case "linux":
		switch {
		case restr == "SO_BINDTOIFINDEX":
			if readable && index != lo.Index {
				t.Errorf("the socket is bound to interface %d, want %d", index, lo.Index)
			}
		case strings.HasPrefix(restr, "none: the kernel refused SO_BINDTOIFINDEX"):
			t.Logf("this kernel refuses an unprivileged SO_BINDTOIFINDEX: %s", restr)
		default:
			t.Errorf("restricted %q", restr)
		}
	default:
		if !strings.HasPrefix(restr, "none: ") {
			t.Errorf("restricted %q on %s, which has no option", restr, runtime.GOOS)
		}
	}
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("the restricted socket takes no connection: %v", err)
	}
	_ = c.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	// An address whose interface's index is not known is left unrestricted,
	// and says so, rather than bound to whatever index 0 means.
	unknown, restr, err := Listen(context.Background(), netip.MustParseAddrPort("127.0.0.1:0"), lan.Address{Prefix: a.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	_ = unknown.Close()
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && restr != "none: the interface's index is not known" {
		t.Errorf("with no index: restricted %q", restr)
	}
}
