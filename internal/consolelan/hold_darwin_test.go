//go:build darwin

package consolelan

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// TestTheConsolesHoldScopesItsRepliesOnMacOS qualifies the console's hold
// on the macos-latest leg (the plan's §9.5): 127.0.0.1:0 held to an
// interface that is not lo0 does not connect on a loopback dial within 2 s,
// since the kernel scopes the SYN-ACK to that interface; held to lo0 it
// connects at once; IP_BOUND_IF reads back. It skips locally where the box
// has no interface but lo0, and fails where CI is set (build-ci-tests.md,
// the host-address bullet).
func TestTheConsolesHoldScopesItsRepliesOnMacOS(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	lo, other := 0, 0
	for _, in := range ifs {
		switch {
		case in.Flags&net.FlagLoopback != 0:
			lo = in.Index
		case other == 0 && in.Flags&net.FlagUp != 0:
			other = in.Index
		}
	}
	if lo == 0 || other == 0 {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI: no loopback interface (%d) or none beside it (%d) to hold to", lo, other)
		}
		t.Skip("this box has no interface beside lo0 to hold a socket to")
	}
	listen := func(index int) net.Listener {
		t.Helper()
		lc := net.ListenConfig{Control: Hold(index, false)}
		ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}
	own := listen(lo)
	if got, err := readBoundIf(own); err != nil || got != lo {
		t.Fatalf("held to lo0's index %d, the socket reads back %d (%v)", lo, got, err)
	}
	go func() {
		if c, err := own.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp4", own.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("held to lo0, the socket took no loopback dial: %v", err)
	}
	_ = c.Close()

	scoped := listen(other)
	if got, err := readBoundIf(scoped); err != nil || got != other {
		t.Fatalf("held to index %d, the socket reads back %d (%v)", other, got, err)
	}
	c, err = net.DialTimeout("tcp4", scoped.Addr().String(), 2*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("held to another interface, the socket completed a loopback handshake")
	}
}

// readBoundIf reads back the index ln's socket is held to.
func readBoundIf(ln net.Listener) (int, error) {
	sc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return 0, err
	}
	var got int
	var gerr error
	if err := sc.Control(func(fd uintptr) { got, gerr = getBoundIf(fd) }); err != nil {
		return 0, err
	}
	return got, gerr
}
