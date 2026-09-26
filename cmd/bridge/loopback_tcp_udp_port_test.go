package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// tcpAndUDPDraws is how many numbers drawLoopbackTCPAndUDPAddr tries.
const tcpAndUDPDraws = 20

// freeLoopbackTCPAndUDPAddr is freeLoopbackPort for an address serve binds
// twice: its LAN HTTPS listener takes the TCP port and its HTTP/3 server
// the UDP port of the same number, and a test that speaks HTTP/3 to serve
// has to know that one, which serve prints nowhere. The gap between the
// release and serve's bind is freeLoopbackPort's, and fails as loudly: a
// TCP port taken meanwhile fails serve's listen, and a UDP one leaves the
// HTTP/3 request unanswered, which waitForServe reports.
func freeLoopbackTCPAndUDPAddr(t *testing.T) string {
	t.Helper()
	addr, err := drawLoopbackTCPAndUDPAddr()
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// drawLoopbackTCPAndUDPAddr is a loopback address whose port was free on
// both TCP and UDP when it was drawn. It fails naming every number it
// tried and the error each one got.
func drawLoopbackTCPAndUDPAddr() (string, error) {
	var refused []string
	for range tcpAndUDPDraws {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(lis.Addr().(*net.TCPAddr).Port))
		pc, err := net.ListenPacket("udp", addr)
		_ = lis.Close()
		if err != nil {
			refused = append(refused, err.Error())
			continue // that number is taken on UDP; draw another
		}
		_ = pc.Close()
		return addr, nil
	}
	return "", fmt.Errorf("no loopback port free on both TCP and UDP in %d draws: %s",
		tcpAndUDPDraws, strings.Join(refused, "; "))
}

// TestTheTCPAndUDPPortDrawSurvivesARunOfRefusedUDPNumbers: Windows hands
// out ephemeral ports in sequence, TCP and UDP each from a cursor of its
// own, and macOS does the same for TCP. A draw that takes its number from
// the TCP cursor and binds it on UDP therefore tests consecutive numbers,
// and one run of numbers UDP refuses, a reservation or a block of sockets
// something else holds, refuses every draw while the TCP cursor walks it.
// That is what failed TestServeShutdownDrainsTheTailnetBesideTheLAN once on
// CI's Windows leg, twenty draws in 10 ms.
//
// The test holds a run of UDP numbers directly ahead of the TCP cursor. The
// draw must still find a number free on both protocols. A platform that
// draws TCP ports at random (Linux) cannot place the run in the draw's
// path, so the test skips there rather than pass for nothing.
func TestTheTCPAndUDPPortDrawSurvivesARunOfRefusedUDPNumbers(t *testing.T) {
	const run = 200
	cursor := tcpCursorWithRoomFor(t, run)
	holdUDPRun(t, cursor+1, cursor+run)
	next := drawTCPPort(t)
	if next <= cursor || next > cursor+run {
		t.Skipf("the TCP port drawn after %d was %d: this platform does not hand TCP ports out in "+
			"sequence, so no run of UDP numbers can sit in the draw's path", cursor, next)
	}

	addr, err := drawLoopbackTCPAndUDPAddr()
	if err != nil {
		t.Fatalf("with UDP %d..%d held, just ahead of the TCP cursor: %v", cursor+1, cursor+run, err)
	}
	mustBindTCPAndUDP(t, addr)
}

// drawTCPPort is the number the TCP cursor hands out next, released again.
func drawTCPPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

// tcpCursorWithRoomFor is the TCP port drawn last, once at least n numbers
// lie between it and the top of the port space: a run the cursor would
// wrap out of cannot hold its draws. It draws past the wrap when it has to,
// which on a sequential platform takes at most n+1 draws.
func tcpCursorWithRoomFor(t *testing.T, n int) int {
	t.Helper()
	for range n + 2 {
		if port := drawTCPPort(t); port+n <= 65535 {
			return port
		}
	}
	t.Fatalf("no TCP port with %d numbers above it in %d draws", n, n+2)
	return 0
}

// holdUDPRun binds UDP on 127.0.0.1 at every number in [lo, hi] for the
// rest of the test. A number something else already holds stays refused,
// which is all the run needs.
func holdUDPRun(t *testing.T, lo, hi int) {
	t.Helper()
	for port := lo; port <= hi; port++ {
		pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = pc.Close() })
	}
}

// mustBindTCPAndUDP fails the test unless addr binds on TCP and UDP both,
// at once, as serve binds it.
func mustBindTCPAndUDP(t *testing.T, addr string) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the drawn address %s does not bind on TCP: %v", addr, err)
	}
	defer lis.Close()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("the drawn address %s does not bind on UDP: %v", addr, err)
	}
	_ = pc.Close()
}
