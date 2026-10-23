package dlna

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// ssdpGroupUDPAddr is the SSDP group and its port, where every NOTIFY goes and
// where every listener joins.
var ssdpGroupUDPAddr = net.UDPAddrFromAddrPort(netip.MustParseAddrPort(SSDPMulticastAddr))

// ssdpListenerSetup is what opening one listener obtained: the capabilities
// a datagram's metadata carries (SSDPListenerCaps), and why each one it lacks
// is missing.
type ssdpListenerSetup struct {
	caps       SSDPListenerCaps
	dstErr     error
	arrivalErr error
	confineErr error
}

// ssdpReadConn is a listener's socket as its read loop sees it.
type ssdpReadConn interface {
	// readDatagram reads one datagram into buf, with its control data into
	// oob, waiting at most until deadline.
	readDatagram(buf, oob []byte, deadline time.Time) (n int, src netip.AddrPort, m datagramMeta, err error)
	Close() error
}

// ssdpSendConn is an endpoint's NOTIFY sender: unconnected, pinned to the
// endpoint's interface before anything is sent (openNotifySender).
type ssdpSendConn interface {
	WriteToUDP(b []byte, addr *net.UDPAddr) (int, error)
	SetWriteDeadline(t time.Time) error
	Close() error
}

// ssdpReplyConn is the transient socket one burst of M-SEARCH responses is
// written from, connected to the searcher.
type ssdpReplyConn interface {
	Write(b []byte) (int, error)
	SetWriteDeadline(t time.Time) error
	Close() error
}

// ssdpTransport opens the sockets an SSDP subsystem uses. The host's is
// hostSSDPTransport; a test hands the subsystem one of its own, whose sockets
// it drives without a network.
type ssdpTransport interface {
	// listen opens the listener for iface (nil: the OS default interface):
	// bound to the group's port, joined on iface, armed for what it can
	// report. err is a listener that could not be opened at all; a
	// capability it lacks is in the setup.
	listen(ctx context.Context, iface *net.Interface, log *slog.Logger) (ssdpReadConn, ssdpListenerSetup, error)
	// openSender opens an endpoint's NOTIFY sender and pins it to iface (no
	// pin for nil). err is a sender that could not be opened; pinErr a pin
	// that failed on a sender that is open.
	openSender(iface *net.Interface) (conn ssdpSendConn, pinErr error, err error)
	// dialReply opens the socket one burst of responses to to is written
	// from.
	dialReply(to netip.AddrPort) (ssdpReplyConn, error)
}

// hostSSDPTransport is the host's sockets. Each function field, nil in
// production, replaces one call, so a test can make one capability fail on
// real sockets.
type hostSSDPTransport struct {
	armDst     func(*net.UDPConn) error
	armArrival func(*net.UDPConn) error
	pin        func(*net.UDPConn, *net.Interface) error
}

// listen opens a listener with listenSSDP, and arms it to report each
// datagram's destination and arrival interface. A listener that cannot be
// armed for the destination is still a listener: the MediaServer answers on
// it as every listener did before backlog B48, and says so here, once per
// listener. An arrival index it cannot report is the strict registrations'
// concern alone, and is in the setup, not the log.
func (t hostSSDPTransport) listen(ctx context.Context, iface *net.Interface, log *slog.Logger) (ssdpReadConn, ssdpListenerSetup, error) {
	conn, confineErr, err := listenSSDP(ctx, iface, ssdpGroupUDPAddr, log)
	if err != nil {
		return nil, ssdpListenerSetup{}, err
	}
	setup := ssdpListenerSetup{confineErr: confineErr}
	setup.caps.ReceiveConfined = confineErr == nil
	armDst, armArrival := armDatagramDst, armDatagramArrival
	if t.armDst != nil {
		armDst = t.armDst
	}
	if t.armArrival != nil {
		armArrival = t.armArrival
	}
	if err := armDst(conn); err != nil {
		setup.dstErr = err
		log.Warn("SSDP listener cannot read the destination of each datagram: it answers a unicast M-SEARCH "+
			"sent to any address of this host, whether or not the DLNA listener serves it",
			slog.String("interface", interfaceName(iface)),
			slog.String("err", err.Error()))
	} else {
		setup.caps.DestinationReporting = true
	}
	if err := armArrival(conn); err != nil {
		setup.arrivalErr = err
		log.Debug("SSDP listener cannot read the interface each datagram arrived on: no strict registration answers on it",
			slog.String("interface", interfaceName(iface)),
			slog.String("err", err.Error()))
	} else {
		setup.caps.ArrivalReporting = true
	}
	return hostReadConn{conn}, setup, nil
}

// openSender opens an unconnected sender (openNotifySender) and pins it to
// iface (pinMulticastInterface).
func (t hostSSDPTransport) openSender(iface *net.Interface) (ssdpSendConn, error, error) {
	sender, err := openNotifySender()
	if err != nil {
		return nil, nil, err
	}
	if iface == nil {
		return sender, nil, nil
	}
	pin := pinMulticastInterface
	if t.pin != nil {
		pin = t.pin
	}
	return sender, pin(sender, iface), nil
}

// dialReply opens one transient UDP socket connected to the searcher, as
// every advertiser did: the response leaves on the route to the searcher,
// never from the :1900 listener.
func (hostSSDPTransport) dialReply(to netip.AddrPort) (ssdpReplyConn, error) {
	return net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(to))
}

// hostReadConn is a listener's socket on the host.
type hostReadConn struct{ c *net.UDPConn }

// readDatagram reads one datagram and the metadata the kernel reported
// beside it (parseDatagramMeta).
func (r hostReadConn) readDatagram(buf, oob []byte, deadline time.Time) (int, netip.AddrPort, datagramMeta, error) {
	// Wall-clock (time.Now), never an injectable clock: net.Conn deadlines
	// evaluate against the real OS clock.
	_ = r.c.SetReadDeadline(deadline)
	n, oobn, _, src, err := r.c.ReadMsgUDPAddrPort(buf, oob)
	if err != nil {
		return 0, netip.AddrPort{}, datagramMeta{}, err
	}
	return n, netip.AddrPortFrom(src.Addr().Unmap(), src.Port()), parseDatagramMeta(oob[:oobn]), nil
}

// Close closes the socket, which wakes a read in progress.
func (r hostReadConn) Close() error { return r.c.Close() }
