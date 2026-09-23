package handshakelog

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/handshakelog/handshaketest"
)

// Every case runs a client shape against two otherwise identical servers:
// one on the raw listener with no ErrorLog — net/http's own default, and
// so the oracle for what an operator saw before this package — and one
// through Wrap. The oracle is what keeps these tests honest about the
// stdlib: they never restate its wording, they ask it.

// TestTheSilentLoopbackProbeIsNotLogged is the probe itself, over IPv4
// and IPv6 loopback. The oracle first asserts that net/http still logs it
// in exactly the shape isSilentProbe parses.
func TestTheSilentLoopbackProbeIsNotLogged(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			logs := handshaketest.CaptureStdLog(t)

			// The oracle: net/http, left alone, logs the probe in exactly
			// the shape isSilentProbe parses. A Go release that rewords
			// the line fails HERE, rather than silently bringing the
			// flood back.
			plain := startServerOn(t, host, false, 0)
			probe := silentProbe(t, plain.addr)
			plain.waitUntilClosed(t, probe)
			if got, want := linesFrom(logs, probe), handshakeErrorPrefix+probe+silentPeerReason; len(got) != 1 || got[0] != want {
				t.Fatalf("net/http no longer logs a silent close as %q (got %q) — isSilentProbe parses that exact shape", want, got)
			}

			filtered := startServerOn(t, host, true, 0)
			probe = silentProbe(t, filtered.addr)
			filtered.waitUntilClosed(t, probe)
			if got := linesFrom(logs, probe); len(got) != 0 {
				t.Errorf("the silent loopback probe was logged: %q", got)
			}
		})
	}
}

// TestEveryOtherHandshakeFailureIsLoggedAsBefore runs each failing client
// shape against the oracle and the wrapped server, and requires the same
// line from both, addresses aside.
func TestEveryOtherHandshakeFailureIsLoggedAsBefore(t *testing.T) {
	cases := []struct {
		name string
		// run drives one connection and returns the address the server
		// saw it come from.
		run func(t *testing.T, addr string) string
		// The oracle's reason must end with this, so a case cannot drift
		// into testing a different failure than its name says.
		reason           string
		handshakeTimeout time.Duration
	}{
		// The case the text cannot tell from the probe: a peer that sent
		// a whole ClientHello and then went away. The reason is the same
		// bare "EOF", so the only thing keeping it in the log is the
		// per-connection record of what the peer sent.
		{name: "ClientHello then silence", run: helloThenHalfClose, reason: ": EOF"},
		{name: "client rejects the cert", run: handshaketest.RejectTheCert, reason: ": remote error: tls: bad certificate"},
		{name: "plaintext HTTP", run: plaintextHTTP, reason: ": client sent an HTTP request to an HTTPS server"},
		{name: "partial record header", run: partialRecord, reason: ": unexpected EOF"},
		// Silent, but it never CLOSED: a stalled peer, not a probe.
		{name: "silent until the handshake times out", run: stallUntilClosed, reason: ": i/o timeout", handshakeTimeout: 200 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := handshaketest.CaptureStdLog(t)

			plain := startServer(t, false, tc.handshakeTimeout)
			from := tc.run(t, plain.addr)
			plain.waitUntilClosed(t, from)
			oracle := oneLineFrom(t, logs, from)
			if !strings.HasSuffix(oracle, tc.reason) {
				t.Fatalf("fixture drift: net/http logged %q, the case expects a reason ending %q", oracle, tc.reason)
			}

			filtered := startServer(t, true, tc.handshakeTimeout)
			from = tc.run(t, filtered.addr)
			filtered.waitUntilClosed(t, from)
			if got := oneLineFrom(t, logs, from); shape(got) != shape(oracle) {
				t.Errorf("the forwarded line differs from net/http's own:\n got %q\nwant %q", got, oracle)
			}
		})
	}
}

// A silent close from anywhere but this host is a scanner or a load
// balancer's TCP check, and stays in the log. The peer address is faked
// BELOW Wrap, so Wrap sees exactly what it would see from the network.
func TestASilentPeerFromElsewhereIsLogged(t *testing.T) {
	logs := handshaketest.CaptureStdLog(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 40404} // TEST-NET-1
	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 7788}
	s := serveOn(t, elsewhereListener{inner, remote, local}, true, 0)

	silentProbe(t, s.addr)
	s.waitUntilClosed(t, remote.String())
	if got, want := linesFrom(logs, remote.String()), handshakeErrorPrefix+remote.String()+silentPeerReason; len(got) != 1 || got[0] != want {
		t.Errorf("a silent close from %v was not logged as net/http logs it: got %q, want %q", remote, got, want)
	}
	s.lis.local.Range(func(k, _ any) bool {
		t.Errorf("a connection from elsewhere was registered under %v", k)
		return true
	})
}

// `bridge health` dials a listenAddress bound to one specific IP as-is,
// so its source is that IP, not loopback. Source equal to destination
// is still this host, and still the probe.
func TestAProbeOfASpecificBoundIPIsNotLogged(t *testing.T) {
	ip := firstNonLoopbackIPv4(t)
	logs := handshaketest.CaptureStdLog(t)
	inner, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot listen on %v: %v", ip, err)
	}
	s := serveOn(t, inner, true, 0)
	probe := silentProbe(t, s.addr)
	if host, _, _ := net.SplitHostPort(probe); host != ip.String() {
		t.Skipf("the kernel sourced the probe from %s, not %s; nothing to pin here", host, ip)
	}
	s.waitUntilClosed(t, probe)
	if got := linesFrom(logs, probe); len(got) != 0 {
		t.Errorf("a probe of the bound IP from this host was logged: %q", got)
	}
}

// TestFromThisHost pins which peers count as this host: every loopback
// spelling, and a source equal to the destination. A LAN or tailnet peer
// never does.
func TestFromThisHost(t *testing.T) {
	tcp := func(ip string, port int) *net.TCPAddr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: port} }
	cases := []struct {
		name          string
		remote, local net.Addr
		want          bool
	}{
		{"IPv4 loopback", tcp("127.0.0.1", 5000), tcp("127.0.0.1", 7788), true},
		{"anywhere in 127/8", tcp("127.9.9.9", 5000), tcp("127.0.0.1", 7788), true},
		{"IPv6 loopback", tcp("::1", 5000), tcp("::1", 7788), true},
		{"v4-mapped loopback on a dual-stack socket", tcp("::ffff:127.0.0.1", 5000), tcp("::ffff:127.0.0.1", 7788), true},
		{"a specific bound IP, dialled from itself", tcp("192.168.1.5", 5000), tcp("192.168.1.5", 7788), true},
		{"the same, one side v4-mapped", tcp("::ffff:192.168.1.5", 5000), tcp("192.168.1.5", 7788), true},
		{"a LAN peer", tcp("192.168.1.9", 5000), tcp("192.168.1.5", 7788), false},
		{"a tailnet peer", tcp("100.101.102.103", 5000), tcp("100.64.0.1", 7788), false},
		{"not TCP", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5000}, tcp("127.0.0.1", 7788), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fromThisHost(fakeConn{remote: tc.remote, local: tc.local}); got != tc.want {
				t.Errorf("fromThisHost(%v → %v) = %v, want %v", tc.remote, tc.local, got, tc.want)
			}
		})
	}
}

// The registry must drain as connections close, whatever they did —
// otherwise a long-lived bridge accumulates one entry per local
// connection it ever served.
func TestNoRegistrationOutlivesItsConnection(t *testing.T) {
	handshaketest.CaptureStdLog(t)
	s := startServer(t, true, 0)
	var peers []string
	for i := 0; i < 3; i++ {
		peers = append(peers, silentProbe(t, s.addr), handshaketest.RejectTheCert(t, s.addr), verifiedHandshake(t, s.addr))
	}
	for _, p := range peers {
		s.waitUntilClosed(t, p)
	}
	// net/http closes a connection before it reports StateClosed, so
	// every registration is due to be gone by now — no polling.
	s.lis.local.Range(func(k, _ any) bool {
		t.Errorf("a registration outlived its connection: %v", k)
		return true
	})
}

// Once a connection is gone its peer address can come back on a new one.
// A late Close of the old connection must not remove the new one's
// registration, or the next probe from that port is logged after all.
func TestALateCloseLeavesANewerRegistration(t *testing.T) {
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5000}
	server := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7788}
	inner := &queueListener{conns: []net.Conn{
		fakeConn{remote: addr, local: server},
		fakeConn{remote: addr, local: server},
	}}
	wrapped, _ := Wrap(inner)
	l := wrapped.(*listener)

	older, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	newer, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = older.Close()
	if v, ok := l.local.Load(addr.String()); !ok || v != newer {
		t.Fatalf("closing the older connection removed the newer one's registration (got %v, %v)", v, ok)
	}
	_ = newer.Close()
	if _, ok := l.local.Load(addr.String()); ok {
		t.Fatal("closing the newer connection left its registration behind")
	}
}

// ---- servers ----

type server struct {
	addr   string
	lis    *listener // nil for the unfiltered oracle
	closed sync.Map  // peer address → struct{}, once net/http has closed it
}

// startServer serves on an ephemeral IPv4 loopback port.
func startServer(t *testing.T, filtered bool, handshakeTimeout time.Duration) *server {
	t.Helper()
	return startServerOn(t, "127.0.0.1", filtered, handshakeTimeout)
}

// startServerOn serves on an ephemeral port of host, and skips the test
// when this machine cannot bind there (no IPv6, for one).
func startServerOn(t *testing.T, host string, filtered bool, handshakeTimeout time.Duration) *server {
	t.Helper()
	inner, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", host, err)
	}
	return serveOn(t, inner, filtered, handshakeTimeout)
}

// serveOn mirrors the API listener: ServeTLS on the given raw listener,
// through Wrap when filtered. handshakeTimeout 0 means the API's 5 s.
func serveOn(t *testing.T, inner net.Listener, filtered bool, handshakeTimeout time.Duration) *server {
	t.Helper()
	if handshakeTimeout == 0 {
		handshakeTimeout = 5 * time.Second
	}
	s := &server{addr: inner.Addr().String()}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{testCert(t)}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: handshakeTimeout,
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				s.closed.Store(c.RemoteAddr().String(), struct{}{})
			}
		},
	}
	lis := inner
	if filtered {
		lis, srv.ErrorLog = Wrap(inner)
		s.lis = lis.(*listener)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.ServeTLS(lis, "", "")
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return s
}

// waitUntilClosed returns once net/http has finished with the connection
// from peer. It writes a failed handshake's line and closes the
// connection BEFORE it reports StateClosed, on the same goroutine, so
// after this returns the line is in the log or never will be. That makes
// every "was NOT logged" assertion here exact rather than a sleep.
func (s *server) waitUntilClosed(t *testing.T, peer string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s.closed.Load(peer); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never closed the connection from %s", peer)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var (
	certOnce sync.Once
	cert     tls.Certificate
	certErr  error
)

// testCert is the shape internal/tls mints: ECDSA P-256, a server-auth
// leaf that is not a CA, loopback SANs.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			certErr = err
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "handshakelog test"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			DNSNames:              []string{"localhost"},
			IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			certErr = err
			return
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			certErr = err
			return
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	})
	if certErr != nil {
		t.Fatal(certErr)
	}
	return cert
}

// ---- client shapes; each returns the address the server saw ----

// dial connects to addr, failing the test if it cannot.
func dial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.TCPConn)
}

// silentProbe is `bridge health`: connect, close, send nothing.
func silentProbe(t *testing.T, addr string) string {
	t.Helper()
	c := dial(t, addr)
	from := c.LocalAddr().String()
	_ = c.Close()
	return from
}

// halfCloseAfterWrite sends the ClientHello and then shuts the write side.
// A FULL close would race the server's reply: data arriving at a closed
// socket draws an RST, and the server then reads a reset instead of the
// clean EOF this case is about.
type halfCloseAfterWrite struct{ *net.TCPConn }

// Write sends p, then shuts the write side.
func (c halfCloseAfterWrite) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	_ = c.CloseWrite()
	return n, err
}

// helloThenHalfClose is a real client that sends its ClientHello and then
// goes quiet: the one shape whose line reads exactly like the probe's.
func helloThenHalfClose(t *testing.T, addr string) string {
	t.Helper()
	c := dial(t, addr)
	defer c.Close()
	_ = tls.Client(halfCloseAfterWrite{c}, &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}).Handshake()
	return c.LocalAddr().String()
}

// plaintextHTTP speaks HTTP/1.1 to the TLS port and reads the server's
// 400.
func plaintextHTTP(t *testing.T, addr string) string {
	t.Helper()
	c := dial(t, addr)
	defer c.Close()
	if _, err := io.WriteString(c, "GET /v1/health HTTP/1.1\r\nHost: bridge\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, c) // the server's 400, then its close
	return c.LocalAddr().String()
}

// partialRecord sends three of a record header's five bytes, then
// half-closes.
func partialRecord(t *testing.T, addr string) string {
	t.Helper()
	c := dial(t, addr)
	defer c.Close()
	if _, err := c.Write([]byte{0x16, 0x03, 0x01}); err != nil { // 3 of a record header's 5 bytes
		t.Fatal(err)
	}
	_ = c.CloseWrite()
	_, _ = io.Copy(io.Discard, c)
	return c.LocalAddr().String()
}

// stallUntilClosed sends nothing and waits for the server to give up.
func stallUntilClosed(t *testing.T, addr string) string {
	t.Helper()
	c := dial(t, addr)
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.Copy(io.Discard, c)
	return c.LocalAddr().String()
}

// verifiedHandshake completes a handshake that trusts exactly the test
// cert, and returns the address the server saw.
func verifiedHandshake(t *testing.T, addr string) string {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(testCert(t).Leaf)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("a verified handshake failed: %v", err)
	}
	from := c.LocalAddr().String()
	_ = c.Close()
	return from
}

// ---- fakes ----

// elsewhereListener reports every connection as coming from remote.
type elsewhereListener struct {
	net.Listener
	remote, local net.Addr
}

// Accept hands back the next connection with both addresses replaced.
func (l elsewhereListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	return addrConn{c, l.remote, l.local}, nil
}

type addrConn struct {
	net.Conn
	remote, local net.Addr
}

// RemoteAddr is the faked peer.
func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// LocalAddr is the faked destination.
func (c addrConn) LocalAddr() net.Addr { return c.local }

// fakeConn is only ever asked for its addresses, and closed.
type fakeConn struct {
	net.Conn
	remote, local net.Addr
}

// RemoteAddr is the configured peer.
func (c fakeConn) RemoteAddr() net.Addr { return c.remote }

// LocalAddr is the configured destination.
func (c fakeConn) LocalAddr() net.Addr { return c.local }

// Close succeeds: there is nothing underneath to close.
func (fakeConn) Close() error { return nil }

type queueListener struct {
	net.Listener
	conns []net.Conn
}

// Accept hands out the queued connections in order.
func (l *queueListener) Accept() (net.Conn, error) {
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

// firstNonLoopbackIPv4 returns an IPv4 address this machine holds that is
// neither loopback nor link-local, and skips the test when there is none.
func firstNonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			return n.IP.To4()
		}
	}
	t.Skip("this host has no non-loopback IPv4 address to bind")
	return nil
}

// ---- log capture ----

// linesFrom returns the handshake-error lines logged for peer.
func linesFrom(logs *handshaketest.Buffer, peer string) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.HasPrefix(line, handshakeErrorPrefix+peer+": ") {
			out = append(out, line)
		}
	}
	return out
}

// oneLineFrom returns the single handshake-error line for peer. Called
// after waitUntilClosed, so there is nothing left to wait for.
func oneLineFrom(t *testing.T, logs *handshaketest.Buffer, peer string) string {
	t.Helper()
	got := linesFrom(logs, peer)
	if len(got) != 1 {
		t.Fatalf("want exactly one handshake-error line from %s, got %q; the log holds:\n%s", peer, got, logs.String())
	}
	return got[0]
}

var addrPattern = regexp.MustCompile(`(\d{1,3}(\.\d{1,3}){3}|\[[0-9A-Fa-f:.]+\]):\d+`)

// shape is a line with its addresses blanked: two servers on different
// ports, probed from different ports, must otherwise log byte for byte
// the same.
func shape(line string) string { return addrPattern.ReplaceAllString(line, "ADDR") }
