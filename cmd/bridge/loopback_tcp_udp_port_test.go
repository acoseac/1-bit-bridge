package main

import (
	"fmt"
	"math/rand/v2"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tcpAndUDPPortLo and tcpAndUDPPortHi bound the numbers
// drawLoopbackTCPAndUDPAddr takes, and tcpAndUDPDraws is how many it tries.
// The range lies below the ephemeral range of every platform the bridge
// targets (Linux hands ports out from 32768, Windows and macOS from 49152),
// so no allocator ever hands out a number in it.
const (
	tcpAndUDPPortLo = 20000
	tcpAndUDPPortHi = 32767
	tcpAndUDPDraws  = 20
)

// freeLoopbackTCPAndUDPAddr is a loopback address for serve to bind twice:
// its LAN HTTPS listener takes the TCP port and its HTTP/3 server the UDP
// port of the same number, and a test that speaks HTTP/3 to serve has to
// know that one, which serve prints nowhere. The number is released before
// serve binds it, as freeLoopbackPort's is, but no allocator hands out
// numbers in tcpAndUDPPortLo..tcpAndUDPPortHi, so only an explicit bind can
// take it meanwhile. That fails as loudly: a TCP port taken fails serve's
// listen, and a UDP one leaves the HTTP/3 request unanswered, which
// waitForServe reports.
func freeLoopbackTCPAndUDPAddr(t *testing.T) string {
	t.Helper()
	addr, err := drawLoopbackTCPAndUDPAddr()
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// drawLoopbackTCPAndUDPAddr is a loopback address whose port was free on
// both TCP and UDP when it was drawn: a random number, bound on both
// protocols at once and released.
//
// It used to take its numbers from the TCP allocator, and it must not take
// them from either allocator. Windows hands ephemeral ports out in
// sequence, TCP and UDP each from a cursor of its own (macOS does the same
// for TCP), so successive draws from one cursor test consecutive numbers on
// the other protocol. On CI's Windows runners WinNAT reserves 200 UDP
// numbers that the TCP cursor still hands out, and 200 TCP numbers that the
// UDP cursor still hands out, and once a cursor reaches the other
// protocol's block every draw is refused (WSAEACCES) until the cursor has
// walked past it. Random numbers are independent draws: a refused run
// costs only the draws that land in it.
func drawLoopbackTCPAndUDPAddr() (string, error) {
	return drawLoopbackTCPAndUDPAddrIn(tcpAndUDPPortLo, tcpAndUDPPortHi, tcpAndUDPDraws)
}

// drawLoopbackTCPAndUDPAddrIn is drawLoopbackTCPAndUDPAddr over [lo, hi],
// with draws tries. It fails naming every address it tried and the error
// each one got.
func drawLoopbackTCPAndUDPAddrIn(lo, hi, draws int) (string, error) {
	refused := make([]string, 0, draws)
	for range draws {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(lo+rand.IntN(hi-lo+1)))
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		pc, err := net.ListenPacket("udp", addr)
		_ = lis.Close()
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		_ = pc.Close()
		return addr, nil
	}
	return "", fmt.Errorf("no loopback port in %d..%d free on both TCP and UDP in %d draws: %s",
		lo, hi, draws, strings.Join(refused, "; "))
}

// TestTheTCPAndUDPPortDrawSurvivesARunOfRefusedUDPNumbers: Windows hands
// out ephemeral ports in sequence, TCP and UDP each from a cursor of its
// own, and macOS does the same for TCP. A draw that took its number from
// the TCP cursor and bound it on UDP tested consecutive numbers, so one run
// of numbers UDP refuses refused every draw while the TCP cursor walked it.
// CI's Windows runners have such a run, a WinNAT reservation of 200 UDP
// numbers, and a draw started in front of it fails as
// TestServeShutdownDrainsTheTailnetBesideTheLAN once did there: twenty
// draws, all refused, in 10 ms.
//
// The test holds a run of UDP numbers directly ahead of the TCP cursor, and
// the draw must still find a number free on both protocols. A platform that
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

// TestTheTCPAndUDPPortDrawsAreIndependent: the draw's own numbers must not
// be consecutive either. With the bottom third of a small range held on
// UDP, a draw that walked up from the bottom would be refused every time;
// independent draws fail only if all twenty land in the held third, about
// one run in 3.5 billion.
func TestTheTCPAndUDPPortDrawsAreIndependent(t *testing.T) {
	const size, held = 60, 20
	lo := tcpAndUDPPortLo + rand.IntN(tcpAndUDPPortHi-tcpAndUDPPortLo-size+2)
	holdUDPRun(t, lo, lo+held-1)

	addr, err := drawLoopbackTCPAndUDPAddrIn(lo, lo+size-1, tcpAndUDPDraws)
	if err != nil {
		t.Fatalf("with UDP %d..%d held, the bottom third of %d..%d: %v", lo, lo+held-1, lo, lo+size-1, err)
	}
	mustBindTCPAndUDP(t, addr)
}

// TestTheTCPAndUDPPortDrawNamesEveryRefusal: a draw that finds nothing
// names every address it tried and the error each one got, which is all a
// one-off failure on CI leaves to diagnose it by. The message used to say
// only that twenty draws had failed.
//
// The range is held on each protocol in turn, so a draw that skipped
// either bind would return a number from it. With TCP held every refusal
// is TCP's, since TCP is bound first. With UDP held a refusal can still be
// TCP's, where something else happens to hold that number on TCP.
func TestTheTCPAndUDPPortDrawNamesEveryRefusal(t *testing.T) {
	for _, tc := range []struct {
		held string
		hold func(t *testing.T, lo, hi int)
	}{
		{"udp", holdUDPRun},
		{"tcp", holdTCPRun},
	} {
		t.Run(tc.held, func(t *testing.T) {
			const size, draws = 4, 6
			lo := tcpAndUDPPortLo + rand.IntN(tcpAndUDPPortHi-tcpAndUDPPortLo-size+2)
			tc.hold(t, lo, lo+size-1)

			addr, err := drawLoopbackTCPAndUDPAddrIn(lo, lo+size-1, draws)
			if err == nil {
				t.Fatalf("drew %s from %d..%d, every number of which is held on %s", addr, lo, lo+size-1, tc.held)
			}
			msg := err.Error()
			tried := regexp.MustCompile(`listen (tcp|udp) 127\.0\.0\.1:(\d+): bind: `).FindAllStringSubmatch(msg, -1)
			if len(tried) != draws {
				t.Fatalf("the failure names %d refused binds, want one per draw (%d): %s", len(tried), draws, msg)
			}
			for _, m := range tried {
				if tc.held == "tcp" && m[1] != "tcp" {
					t.Errorf("a draw from a range held on TCP was refused on %s: %s", m[1], msg)
				}
				if port, _ := strconv.Atoi(m[2]); port < lo || port > lo+size-1 {
					t.Errorf("the failure names port %d, outside the range drawn from (%d..%d): %s", port, lo, lo+size-1, msg)
				}
			}
		})
	}
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

// holdTCPRun is holdUDPRun on TCP: a listener at every number in [lo, hi]
// for the rest of the test.
func holdTCPRun(t *testing.T, lo, hi int) {
	t.Helper()
	for port := lo; port <= hi; port++ {
		lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = lis.Close() })
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
