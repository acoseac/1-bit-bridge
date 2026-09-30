//go:build linux

package dlna

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// listenSSDP opens an advertiser's M-SEARCH listener: a UDP socket on the
// group's port, joined to group on iface (nil: the interface the kernel
// routes the group through), that hears only the group's datagrams arriving
// on iface (backlog B71).
//
// net.ListenMulticastUDP binds the socket to the WILDCARD address and the
// group's port (net's listenDatagram rewrites a multicast address to
// 0.0.0.0 before the bind, so it also takes unicast to that port), and
// Linux's default IP_MULTICAST_ALL = 1 then delivers to it every datagram
// for that port and ANY group that arrives on an interface where some
// socket on the host joined the group: when the socket holds no membership
// for the arrival interface, the kernel's ip_mc_sf_allow answers with that
// flag. The DLNA server runs one advertiser per LAN interface, each
// announcing its own interface's LOCATION, so on Linux every advertiser also
// answered the M-SEARCHes that arrived on the others. Measured with the real
// binary in three network namespaces on 2026-09-29: every M-SEARCH from either
// subnet got two answers, one naming the other subnet, in either order.
// IP_MULTICAST_ALL = 0 confines the socket to
// the memberships it holds itself, (group, interface) pairs, which is what
// macOS and Windows do anyway (ssdp_listen_other.go). It does nothing to a
// unicast datagram, which the kernel hands to one socket on the port.
//
// The option is set in the ListenConfig's Control, BEFORE the bind: from the
// bind on the socket receives other interfaces' group datagrams, so an option
// set after ListenMulticastUDP returns would leave a window in which a
// foreign M-SEARCH is queued and answered. The rest is ListenMulticastUDP's:
// ListenPacket on a multicast address takes the same wildcard bind with
// SO_REUSEADDR, and the join is x/net's JoinGroup, by interface index, where
// net joins by the interface's first IPv4 address; that is the same
// membership for every interface the wiring hands an advertiser, which all
// carry one. ListenMulticastUDP also sets IP_MULTICAST_IF and clears
// IP_MULTICAST_LOOP on the listener; on Linux both govern only what a socket
// sends, and the listener sends nothing.
//
// A kernel that refuses the option (Linux has had it since 2.6.31; a
// sandbox that emulates the socket API may not) leaves the listener as it
// was, with a Warn: an advertiser that also answers the searches of other
// interfaces is what every Linux bridge did before, and one that does not
// start at all is worse.
func listenSSDP(ctx context.Context, iface *net.Interface, group *net.UDPAddr, log *slog.Logger) (*net.UDPConn, error) {
	return listenSSDPConfined(ctx, iface, group, log, multicastAllOff)
}

// multicastAllOff turns IP_MULTICAST_ALL off on fd.
func multicastAllOff(fd uintptr) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0)
}

// listenSSDPConfined is listenSSDP with the confining setsockopt as a
// parameter, so a test can drive a kernel that refuses it.
func listenSSDPConfined(ctx context.Context, iface *net.Interface, group *net.UDPAddr, log *slog.Logger,
	confine func(fd uintptr) error) (*net.UDPConn, error) {
	var confineErr error
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { confineErr = confine(fd) })
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", group.String())
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("dlna: SSDP listener is a %T, not a UDP socket", pc)
	}
	if err := ipv4.NewPacketConn(conn).JoinGroup(iface, &net.UDPAddr{IP: group.IP}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if confineErr != nil {
		log.Warn("SSDP listener cannot be limited to its own interface: on a host with more than one "+
			"advertiser it also answers the multicast M-SEARCHes that arrive on the others, with this interface's LOCATION",
			slog.String("interface", interfaceName(iface)),
			slog.String("err", confineErr.Error()))
	}
	return conn, nil
}
