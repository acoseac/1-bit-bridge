//go:build linux

package consolelan

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// absentIndex is an interface index no interface has: a large constant,
// never one past the largest index listed, which an interface made
// meanwhile could take (B527's choice, openhome's measured test).
const absentIndex = 1 << 30

// loopbackIndex is the loopback interface's index.
func loopbackIndex(t *testing.T) int {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range ifs {
		if in.Flags&net.FlagLoopback != 0 {
			return in.Index
		}
	}
	t.Fatal("no loopback interface")
	return 0
}

// TestTheConsolesHoldRefusesASYNFromAnotherInterface qualifies the console's
// own hold on real sockets, unprivileged, in every Linux run (the plan's
// §9.5, D13): a listener on 127.0.0.1:0 that Hold holds to an index that is
// not loopback's refuses a loopback dial (ECONNREFUSED: the SYN arrived on
// loopback and found no listener held to it), held to loopback's own index
// it takes the dial, and the index reads back as set (B527's cases (c) and
// (d), for the console's code). IPv6 is not asked here: a socket held to
// another interface cannot bind [::1] (the plan's §3.3, F4); the root job's
// namespaces qualify a ULA.
func TestTheConsolesHoldRefusesASYNFromAnotherInterface(t *testing.T) {
	listen := func(index int) net.Listener {
		t.Helper()
		lc := net.ListenConfig{Control: Hold(index, false)}
		ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
		if err != nil {
			var re *RefusalError
			if errors.As(err, &re) && re.Reason == ReasonRestrictionRefused {
				t.Skipf("this kernel refuses SO_BINDTOIFINDEX to an unprivileged process (before 5.7): %v", err)
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}
	lo := loopbackIndex(t)

	own := listen(lo)
	got, err := boundIndex(own)
	if err != nil || got != lo {
		t.Fatalf("held to loopback's index %d, the socket reads back %d (%v)", lo, got, err)
	}
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
		t.Fatalf("held to loopback's own index, the socket took no loopback dial: %v", err)
	}
	_ = c.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held to loopback's own index, the socket accepted nothing within 2 s")
	}

	other := listen(absentIndex)
	if got, err := boundIndex(other); err != nil || got != absentIndex {
		t.Fatalf("held to index %d, the socket reads back %d (%v)", absentIndex, got, err)
	}
	c, err = net.DialTimeout("tcp4", other.Addr().String(), 2*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("held to another interface, the socket took a loopback dial")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("held to another interface, a loopback dial was answered %v, not refused", err)
	}
}

// boundIndex reads back the index ln's socket is held to.
func boundIndex(ln net.Listener) (int, error) {
	sc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return 0, err
	}
	var got int
	var gerr error
	if err := sc.Control(func(fd uintptr) { got, gerr = getBindToIfindex(fd) }); err != nil {
		return 0, err
	}
	return got, gerr
}
