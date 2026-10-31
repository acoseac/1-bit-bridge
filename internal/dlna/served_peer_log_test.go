package dlna

import (
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The refusal lines servedAddrs logs, as noteRefused writes them.
const (
	connectionRefusedLine = "DLNA refused a connection to an address it does not serve: it answers only on the LAN-eligible interfaces"
	msearchRefusedLine    = "DLNA refused an M-SEARCH to an address it does not serve: it answers only on the LAN-eligible interfaces"
)

// peerConn is a connection a test hands servedOnlyListener: its two ends,
// and whether it was closed. No other method of a net.Conn is called on it.
type peerConn struct {
	net.Conn
	local, remote net.Addr
	closed        atomic.Bool
}

func (c *peerConn) LocalAddr() net.Addr  { return c.local }
func (c *peerConn) RemoteAddr() net.Addr { return c.remote }
func (c *peerConn) Close() error         { c.closed.Store(true); return nil }

// queuedListener hands out its connections in order, and then answers
// net.ErrClosed.
type queuedListener struct {
	net.Listener
	conns []net.Conn
}

func (l *queuedListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, net.ErrClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

// tcpAddr is s, an address and port, as a connection reports it.
func tcpAddr(s string) *net.TCPAddr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

// requireOneRefusal fails the test unless rec holds exactly one record of
// line, a Warn naming the refused local address and the interface that
// holds it, and no record that names any of peers.
func requireOneRefusal(t *testing.T, rec *loggingtest.Recorder, line, address, iface string, peers ...string) {
	t.Helper()
	got := rec.Lines(line)
	if len(got) != 1 {
		t.Fatalf("%q was logged %d times, want once: %q", line, len(got), rec.All())
	}
	for _, want := range []string{"WARN ", " address=" + address, " interface=" + iface} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the refusal %q does not carry %q", got[0], want)
		}
	}
	if leaked := rec.Carrying(peers...); len(leaked) != 0 {
		t.Errorf("a line names the peer:\n%s", strings.Join(leaked, "\n"))
	}
}

// TestARefusedConnectionNamesItsAddressAndInterfaceAndNeverThePeer: the
// DLNA listener's refusal of a connection to an address it does not serve
// names that address and the interface holding it, the operator's own, and
// never the peer that connected (a tailnet peer, a scanner): no rule lets the
// refusal log a client's address, and the privacy page says client IPs are
// not logged for the phone-facing API. IPv4, IPv6, and an IPv4 peer on a
// dual-stack listener, which reports both ends mapped into IPv6.
func TestARefusedConnectionNamesItsAddressAndInterfaceAndNeverThePeer(t *testing.T) {
	cases := []struct {
		name, local, remote, address, peer string
	}{
		{"IPv4", "100.64.0.7:7790", "100.64.0.99:51000", "100.64.0.7", "100.64.0.99"},
		{"IPv6", "[fd7a:115c:a1e0::7]:7790", "[fd7a:115c:a1e0::99]:51000", "fd7a:115c:a1e0::7", "fd7a:115c:a1e0::99"},
		{"an IPv4 peer on a dual-stack listener", "[::ffff:100.64.0.7]:7790", "[::ffff:100.64.0.99]:51000", "100.64.0.7", "100.64.0.99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := loggingtest.NewRecorder()
			refused := &peerConn{local: tcpAddr(tc.local), remote: tcpAddr(tc.remote)}
			admitted := &peerConn{local: tcpAddr("192.168.1.20:7790"), remote: tcpAddr("192.168.1.30:51001")}
			l := servedOnlyListener{
				Listener: &queuedListener{conns: []net.Conn{refused, refused, admitted}},
				served:   newTestHost(macWithTailscale()...).served(nil),
				log:      rec.Logger(),
			}
			c, err := l.Accept()
			if err != nil || c != admitted {
				t.Fatalf("Accept = %v, %v; want the connection to the LAN address", c, err)
			}
			if !refused.closed.Load() || admitted.closed.Load() {
				t.Errorf("closed: refused %v, admitted %v; want only the refused one", refused.closed.Load(), admitted.closed.Load())
			}
			requireOneRefusal(t, rec, connectionRefusedLine, tc.address, "utun7", tc.peer, "192.168.1.30")
		})
	}
}

// TestARefusedMSearchNamesItsDestinationAndNeverItsSource is the datagram
// caller of the same refusal (ssdpServedAdmission.admits): a unicast
// M-SEARCH to an address the server does not serve, and one whose
// destination the kernel did not report, are logged by the destination and
// the interface holding it, never by the searcher that sent them.
func TestARefusedMSearchNamesItsDestinationAndNeverItsSource(t *testing.T) {
	t.Parallel()
	a := ssdpServedAdmission{served: newTestHost(macWithTailscale()...).served(nil)}
	t.Run("a unicast to an address it does not serve", func(t *testing.T) {
		rec := loggingtest.NewRecorder()
		d := SSDPDatagram{Source: netip.MustParseAddrPort("100.64.0.99:50000"),
			Destination: netip.MustParseAddr("100.64.0.7"), DestinationValid: true}
		if a.admits(rec.Logger(), searchUnicast, d) {
			t.Fatal("a search sent to the tailnet address was admitted")
		}
		requireOneRefusal(t, rec, msearchRefusedLine, "100.64.0.7", "utun7", "100.64.0.99")
	})
	t.Run("a destination the kernel did not report", func(t *testing.T) {
		rec := loggingtest.NewRecorder()
		d := SSDPDatagram{Source: netip.MustParseAddrPort("[fd7a:115c:a1e0::98]:50001")}
		if a.admits(rec.Logger(), searchUnreported, d) {
			t.Fatal("a search whose destination was not reported was admitted")
		}
		requireOneRefusal(t, rec, msearchRefusedLine, "(not reported)", "(none)", "fd7a:115c:a1e0::98")
	})
}
