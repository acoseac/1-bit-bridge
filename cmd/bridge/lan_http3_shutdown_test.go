package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// serve drains its LAN HTTP/3 server in two places: the shutdown branch,
// beside the HTTPS server, and a deferred teardown that runs on every exit.
// quic-go runs ServeHTTP inside the wait http3.Server.Shutdown makes past
// its deadline (it calls Close, which waits for every connection's
// handling), so a LAN HTTP/3 handler that ignored its context (a read from
// a hung mount) held both drains, and serve's exit with them, for as long
// as it blocked. These tests boot serve with a route that does exactly that
// (serveOpts.wrapAPIHandler), reach it over HTTP/3 with quic-go's own
// client, and shut serve down with the request in flight.

const (
	msgLANHTTP3GaveUp = "shutdown: the LAN HTTP/3 server did not drain within grace"
	msgPressCtrlC     = "Press Ctrl-C to shut down."
	heldPath          = "/test/held"
)

// TestServeShutdownIsBoundedByALANHTTP3HandlerThatIgnoresItsContext: serve
// is shut down while a LAN HTTP/3 request waits in a handler that never
// asks its context. The shutdown branch drains the LAN servers under one
// grace, and the HTTP/3 drain must give up once that grace, and the
// allowance quic-go's force-close gets past it, have run out: one line,
// the socket closed under it, never a hung exit.
//
// The client must still have been told. quic-go's force-close writes each
// connection's CONNECTION_CLOSE before it waits for the handlers, and a
// socket closed the moment the grace runs out loses it: the client then
// finds out only when its idle timeout fires.
func TestServeShutdownIsBoundedByALANHTTP3HandlerThatIgnoresItsContext(t *testing.T) {
	route := newHeldRoute()
	addr := freeLoopbackTCPAndUDPAddr(t)
	cfgPath, dataDir := writeLANConfig(t, "127.0.0.1:0")
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	// serve prints this once its LAN HTTP/3 server is up; nothing is held.
	up := holdPrint(msgPressCtrlC, alreadyOpen())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: addr, wrapAPIHandler: route.wrap},
			&holdingWriter{buf: stdout, holds: []*printHold{up}}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Registered after the drain, so it runs before it: a failing run lets
	// the handler go, and the drain then finds a serve that can finish.
	t.Cleanup(route.release.open)

	waitForServe(t, up.held, "its LAN listeners", exited, done, stderr)
	told := newLANHTTP3Client(t, dataDir).get("https://" + addr + heldPath)
	waitForServe(t, route.entered, "the held route over HTTP/3", exited, done, stderr)

	cancelled := time.Now()
	cancel()
	code := waitBoundedServeExit(t, exited, done, stderr)
	took := time.Since(cancelled)
	if code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if took < shutdownGrace {
		t.Errorf("runServe returned %v after the cancel, inside the %v grace: the LAN HTTP/3 "+
			"server was not given its grace to drain", took, shutdownGrace)
	}
	mustReportOnce(t, stderr, msgLANHTTP3GaveUp)
	mustHaveBeenToldTheServerClosed(t, told)
	mustBeFreeOnUDP(t, addr)
}

// TestServeErrorExitIsBoundedByALANHTTP3HandlerThatIgnoresItsContext is the
// same request on an error exit (the admin console cannot bind), where the
// shutdown branch never runs and the deferred teardown is the LAN HTTP/3
// server's only drainer.
func TestServeErrorExitIsBoundedByALANHTTP3HandlerThatIgnoresItsContext(t *testing.T) {
	route := newHeldRoute()
	addr := freeLoopbackTCPAndUDPAddr(t)
	cfgPath, dataDir := writeLANConfig(t, takenAddress(t))
	stderr := &safeBuffer{}
	// serve prints its exit reason before its teardown; holding that print
	// until the held route is entered puts the request in flight at the exit.
	exit := holdPrint(msgAdminServer, route.entered)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: addr, wrapAPIHandler: route.wrap},
			&safeBuffer{}, &holdingWriter{buf: stderr, holds: []*printHold{exit}})
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	t.Cleanup(route.release.open)

	// serve reaches its exit only after its LAN HTTP/3 server is up.
	waitForServe(t, exit.held, "its exit on the admin console's bind error", exited, done, stderr)
	told := newLANHTTP3Client(t, dataDir).get("https://" + addr + heldPath)
	waitForServe(t, route.entered, "the held route over HTTP/3", exited, done, stderr)

	inFlight := time.Now()
	code := waitBoundedServeExit(t, exited, done, stderr)
	took := time.Since(inFlight)
	if code != 1 {
		t.Errorf("serve exit code = %d, want 1 (the admin console could not bind); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), msgAdminServer) {
		t.Fatalf("serve did not exit on the admin console's bind error; stderr=%s", stderr.String())
	}
	if took < shutdownGrace {
		t.Errorf("runServe returned %v after the request was in flight, inside the %v grace: "+
			"the LAN HTTP/3 server was not given its grace to drain", took, shutdownGrace)
	}
	mustReportOnce(t, stderr, msgLANHTTP3GaveUp)
	mustHaveBeenToldTheServerClosed(t, told)
	mustBeFreeOnUDP(t, addr)
}

// heldRoute is a route a boot test adds to serve's API handler
// (serveOpts.wrapAPIHandler). A request to it waits until the test opens
// release and never asks its context, like a read from a hung mount.
// Every other request goes to the API handler as usual.
type heldRoute struct {
	path    string
	entered chan struct{} // closed at the first request
	release *gate
	once    sync.Once
}

// newHeldRoute is a heldRoute at heldPath.
func newHeldRoute() *heldRoute { return newHeldRouteAt(heldPath) }

// newHeldRouteAt is a heldRoute at path that nothing has entered, and
// whose release is still shut.
func newHeldRouteAt(path string) *heldRoute {
	return &heldRoute{path: path, entered: make(chan struct{}), release: newGate()}
}

// wrap is the serveOpts.wrapAPIHandler that adds the route in front of
// next.
func (h *heldRoute) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != h.path {
			next.ServeHTTP(w, r)
			return
		}
		h.once.Do(func() { close(h.entered) })
		h.release.wait()
	})
}

// lanHTTP3Client is quic-go's HTTP/3 client, trusting exactly the
// certificate serve minted in dataDir.
type lanHTTP3Client struct {
	tr *http3.Transport
}

// newLANHTTP3Client is a client for the serve whose data dir is dataDir,
// once that serve has minted its certificate.
func newLANHTTP3Client(t *testing.T, dataDir string) *lanHTTP3Client {
	t.Helper()
	certPath := filepath.Join(dataDir, "server.crt")
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read the bridge's cert: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("no certificate in %s", certPath)
	}
	return newHTTP3Client(t, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, NextProtos: []string{http3.NextProtoH3}})
}

// newHTTP3Client is a client with tlsConf, closed when the test ends.
func newHTTP3Client(t *testing.T, tlsConf *tls.Config) *lanHTTP3Client {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: tlsConf}
	t.Cleanup(func() { _ = tr.Close() })
	return &lanHTTP3Client{tr: tr}
}

// get sends one GET, and hands its result over once it has one: nil for a
// response, else the error the request ended with.
func (c *lanHTTP3Client) get(url string) <-chan error {
	result := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			result <- err
			return
		}
		resp, err := c.tr.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	return result
}

// mustHaveBeenToldTheServerClosed fails the test unless the request ended
// with the server's CONNECTION_CLOSE: H3_NO_ERROR, from the remote side.
// Checked once serve has returned, so a request still waiting has not been
// told, and would learn only when its idle timeout fired.
func mustHaveBeenToldTheServerClosed(t *testing.T, told <-chan error) {
	t.Helper()
	select {
	case err := <-told:
		var h3Err *http3.Error
		if !errors.As(err, &h3Err) || !h3Err.Remote || h3Err.ErrorCode != http3.ErrCodeNoError {
			t.Errorf("the held request ended with %v (%T), want the server's CONNECTION_CLOSE "+
				"(H3_NO_ERROR, remote)", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the held request was never told the server closed its connection: the " +
			"socket was closed before quic-go's force-close had written the CONNECTION_CLOSE, " +
			"so the client waits for its idle timeout")
	}
}

// mustReportOnce fails the test unless stderr carries msg exactly once.
func mustReportOnce(t *testing.T, stderr *safeBuffer, msg string) {
	t.Helper()
	if n := strings.Count(stderr.String(), msg); n != 1 {
		t.Errorf("stderr carries %q %d time(s), want once; stderr=%s", msg, n, stderr.String())
	}
}

// mustBeFreeOnUDP fails the test unless nothing holds addr's UDP port:
// serve closed its LAN HTTP/3 socket, which the launcher menu's next
// "Start now" binds again.
func mustBeFreeOnUDP(t *testing.T, addr string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Errorf("serve returned with its LAN HTTP/3 socket still bound: %v", err)
		return
	}
	_ = pc.Close()
}

// waitBoundedServeExit waits for runServe to return, and gives its exit
// code. The bound is the grace, the allowance quic-go's force-close gets
// past it, and the rest of the teardown, with room to spare; a drain that
// waits for a held handler never gets there.
func waitBoundedServeExit(t *testing.T, exited <-chan struct{}, done <-chan int, stderr *safeBuffer) int {
	t.Helper()
	bound := shutdownGrace + 10*time.Second
	select {
	case <-exited:
	case <-time.After(bound):
		t.Fatalf("runServe was still draining %v later, on a LAN HTTP/3 handler that ignores its "+
			"context: the drain is not bounded; stderr=%s", bound, stderr.String())
	}
	return <-done
}

// writeLANConfig drops a bridge.yaml for a loopback serve, HTTP/3 on, beside
// a real library root, and returns its path and the data dir it names.
func writeLANConfig(t *testing.T, adminAddress string) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + dataDir + "\nadminAddress: " + adminAddress + "\n"
	cfgPath = filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// freeLoopbackTCPAndUDPAddr is freeLoopbackPort for an address serve binds
// twice: its LAN HTTPS listener takes the TCP port and its HTTP/3 server
// the UDP port of the same number, and a test that speaks HTTP/3 to serve
// has to know that one, which serve prints nowhere. The gap between the
// release and serve's bind is freeLoopbackPort's, and fails as loudly: a
// TCP port taken meanwhile fails serve's listen, and a UDP one leaves the
// HTTP/3 request unanswered, which waitForServe reports.
func freeLoopbackTCPAndUDPAddr(t *testing.T) string {
	t.Helper()
	for range 20 {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(lis.Addr().(*net.TCPAddr).Port))
		pc, err := net.ListenPacket("udp", addr)
		_ = lis.Close()
		if err != nil {
			continue // that number is taken on UDP; draw another
		}
		_ = pc.Close()
		return addr
	}
	t.Fatal("no loopback port free on both TCP and UDP in 20 draws")
	return ""
}

// alreadyOpen is a channel that is already closed, for a printHold that
// only watches: its held says the print happened, and nothing waits.
func alreadyOpen() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
