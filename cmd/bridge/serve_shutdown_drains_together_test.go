package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

// On a shutdown serve drains every server it runs: the LAN's HTTPS and
// HTTP/3 servers, and the tailnet's, which tsnetFront.stop drains before it
// closes the node. The shutdown branch used to drain the LAN servers and
// return, and only then did the deferred stop begin on the tailnet's, so
// the two sides drained one after the other. A request held on each side
// cost two graces and two force-close allowances where one would do, and
// the tailnet went on accepting new requests for as long as the LAN
// drained.

const (
	heldPathLAN     = "/test/held-lan"
	heldPathTailnet = "/test/held-tailnet"
)

// TestServeShutdownDrainsTheTailnetBesideTheLAN: serve, in tsnet mode, is
// shut down with an HTTP/3 request held on each side, each in a handler
// that never asks its context. The tailnet's drain must begin with the
// LAN's, not once the LAN's has given up: by the time the LAN drain gives
// up, the tailnet's HTTPS listener is closed, and the tailnet drain gives
// up alongside it, under the same grace. The deferred stop that follows the
// shutdown branch's must do nothing, so each drain reports giving up once,
// both clients are told the server closed, and the node is closed once,
// after use. The tailnet goroutine returns once its servers' drains begin,
// so nothing reports giving up on it.
func TestServeShutdownDrainsTheTailnetBesideTheLAN(t *testing.T) {
	t.Cleanup(func() { metrics.RegisterTsnetProvider(nil) })
	lanRoute, tailnetRoute := newHeldRouteAt(heldPathLAN), newHeldRouteAt(heldPathTailnet)
	serverTLS, clientTLS := loopbackTLSPair(t)
	node := newFakeTsnetNode()
	node.ips = twoTailnetAddrs()[:1]
	node.h3TLS = serverTLS
	node.bind = func(int) (net.PacketConn, error) { return net.ListenPacket("udp", "127.0.0.1:0") }
	node.listen = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	addr := freeLoopbackTCPAndUDPAddr(t)
	cfgPath, dataDir := writeTsnetLANConfig(t)
	stderr := &safeBuffer{}
	// serve prints this once its LAN HTTP/3 server is up; nothing is held.
	up := holdPrint(msgPressCtrlC, alreadyOpen())
	// The LAN drain's line is held until the test has looked at the
	// tailnet side, and the tailnet drain's is only watched.
	looked := newGate()
	lanGaveUp := holdPrint(msgLANHTTP3GaveUp, looked.ch)
	tailnetGaveUp := holdPrint(msgTsnetDrainGaveUp, alreadyOpen())
	wrap := func(next http.Handler) http.Handler { return lanRoute.wrap(tailnetRoute.wrap(next)) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: addr, tsnetNode: node, wrapAPIHandler: wrap},
			&holdingWriter{buf: &safeBuffer{}, holds: []*printHold{up}},
			&holdingWriter{buf: stderr, holds: []*printHold{lanGaveUp, tailnetGaveUp}})
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	t.Cleanup(node.closeHandedOut)
	// Registered after the drain, so they run before it: a failing run lets
	// the held print and the handlers go, and the drain then finds a serve
	// that can finish.
	t.Cleanup(tailnetRoute.release.open)
	t.Cleanup(lanRoute.release.open)
	t.Cleanup(looked.open)

	waitForServe(t, up.held, "its LAN listeners", exited, done, stderr)
	waitForServe(t, node.listenReturned, "the tailnet listen", exited, done, stderr)
	lis, conns := node.listener(), node.handedOutConns()
	if lis == nil || len(conns) != 1 {
		t.Fatalf("precondition: the node handed out a listener (%v) and %d HTTP/3 conn(s), want one of each",
			lis != nil, len(conns))
	}
	waitForServe(t, lis.accepted, "the tailnet's HTTPS server", exited, done, stderr)
	lanTold := newLANHTTP3Client(t, dataDir).get("https://" + addr + heldPathLAN)
	waitForServe(t, lanRoute.entered, "the held route over the LAN's HTTP/3", exited, done, stderr)
	tailnetTold := newHTTP3Client(t, clientTLS).get("https://" + conns[0].LocalAddr().String() + heldPathTailnet)
	waitForServe(t, tailnetRoute.entered, "the held route over the tailnet's HTTP/3", exited, done, stderr)

	cancelled := time.Now()
	cancel()
	waitForServe(t, lanGaveUp.held, "its LAN HTTP/3 drain giving up", exited, done, stderr)
	select {
	case <-lis.closed:
	default:
		t.Error("the LAN HTTP/3 drain gave up with the tailnet's HTTPS listener still open: the " +
			"tailnet side went on accepting new requests for as long as the LAN side drained")
	}
	select {
	case <-tailnetGaveUp.held:
	case <-time.After(shutdownGrace):
		t.Error("the tailnet drain had not given up a grace after the LAN's had: it began only " +
			"once the LAN's was over, so the two sides drained one after the other, each with " +
			"a grace and a force-close allowance of its own")
	}
	looked.open()
	code := waitBoundedServeExit(t, exited, done, stderr)
	took := time.Since(cancelled)
	if code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if took < shutdownGrace {
		t.Errorf("runServe returned %v after the cancel, inside the %v grace: the held requests "+
			"were not given their grace to drain", took, shutdownGrace)
	}
	mustReportOnce(t, stderr, msgLANHTTP3GaveUp)
	mustReportOnce(t, stderr, msgTsnetDrainGaveUp)
	if s := stderr.String(); strings.Contains(s, msgTsnetGaveUp) {
		t.Errorf("shutdown reported giving up on the tailnet goroutine, which returns once its "+
			"servers' drains begin; stderr=%s", s)
	}
	mustHaveBeenToldTheServerClosed(t, lanTold)
	mustHaveBeenToldTheServerClosed(t, tailnetTold)
	mustBeFreeOnUDP(t, addr)
	node.mustHaveBeenClosedOnceAfterUse(t)
}

// writeTsnetLANConfig is writeLANConfig for a serve in tsnet mode, with the
// admin console on an ephemeral port: HTTP/3 on, on the LAN and the tailnet
// both.
func writeTsnetLANConfig(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	cfgPath, dataDir = writeLANConfig(t, "127.0.0.1:0")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, "tailscale:\n  mode: tsnet\n"...)
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}
