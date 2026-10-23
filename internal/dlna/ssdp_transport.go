package dlna

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
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

// ssdpOwnedSender stays in the subsystem's shutdown inventory until its
// socket is closed. Close is idempotent even when shutdown interrupts retire.
type ssdpOwnedSender struct {
	ssdpSendConn
	s    *SSDPSubsystem
	once sync.Once
	err  error
}

func (s *SSDPSubsystem) ownSender(c ssdpSendConn) *ssdpOwnedSender {
	sender := &ssdpOwnedSender{ssdpSendConn: c, s: s}
	s.mu.Lock()
	s.senders[sender] = struct{}{}
	expired := s.closed && !time.Now().Before(s.closeDeadline)
	s.mu.Unlock()
	if expired {
		_ = sender.Close()
	}
	return sender
}

func (c *ssdpOwnedSender) Close() error {
	c.once.Do(func() {
		c.err = c.ssdpSendConn.Close()
		c.s.mu.Lock()
		delete(c.s.senders, c)
		c.s.mu.Unlock()
	})
	return c.err
}

// ssdpReplyConn is the transient socket one burst of M-SEARCH responses is
// written through. The MediaServer connects to the searcher; strict replies
// use an unconnected socket with an explicit source address and interface.
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
	// from is nil for the MediaServer's ordinary routed reply. A strict
	// reply must use both the address and interface in from, or fail closed.
	dialReply(to netip.AddrPort, from *SSDPEndpointSnapshot) (ssdpReplyConn, error)
}

// hostSSDPTransport is the host's sockets. Each function field, nil in
// production, replaces one call, so a test can make one capability fail on
// real sockets.
type hostSSDPTransport struct {
	armDst     func(*net.UDPConn) error
	armArrival func(*net.UDPConn) error
	pin        func(*net.UDPConn, *net.Interface) error
	openReply  func(*net.UDPAddr) (ssdpUnicastSocket, error)
	pinReply   func(ssdpUnicastSocket, int) error
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
	setup := t.arm(conn, iface, log)
	setup.confineErr = confineErr
	setup.caps.ReceiveConfined = confineErr == nil
	return hostReadConn{conn}, setup, nil
}

// arm asks the kernel to report each datagram's destination and arrival
// interface on conn, the listener for iface, each by a call of its own, and
// returns what it obtained (confinement aside, which is listenSSDP's).
func (t hostSSDPTransport) arm(conn *net.UDPConn, iface *net.Interface, log *slog.Logger) ssdpListenerSetup {
	var setup ssdpListenerSetup
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
	return setup
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

// dialReply preserves the MediaServer's connected, OS-routed socket. A
// strict reply binds the selected local address and pins its interface
// before sending anything; it never falls back to the default route.
func (t hostSSDPTransport) dialReply(to netip.AddrPort, from *SSDPEndpointSnapshot) (ssdpReplyConn, error) {
	if from == nil {
		return net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(to))
	}
	if err := checkStrictReplyAddress(from.Interface.Index, from.Local); err != nil {
		return nil, err
	}
	open := t.openReply
	if open == nil {
		open = func(addr *net.UDPAddr) (ssdpUnicastSocket, error) { return net.ListenUDP("udp4", addr) }
	}
	c, err := open(net.UDPAddrFromAddrPort(netip.AddrPortFrom(from.Local.Addr(), 0)))
	if err != nil {
		return nil, err
	}
	pin := t.pinReply
	if pin == nil {
		pin = pinStrictReply
	}
	if err := pin(c, from.Interface.Index); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: reply interface: %w", ErrSSDPStrictCapability, err)
	}
	return &ssdpStrictReply{ssdpUnicastSocket: c, to: net.UDPAddrFromAddrPort(to), from: *from}, nil
}

// checkStrictReplyAddress keeps endpoint startup and reply creation on the
// same requirements, so an endpoint that cannot reply never announces itself.
func checkStrictReplyAddress(index int, local netip.Prefix) error {
	if index <= 0 || !local.IsValid() || !local.Addr().Is4() || local.Addr().IsUnspecified() {
		return fmt.Errorf("%w: reply needs an interface and local IPv4 address", ErrSSDPStrictCapability)
	}
	return nil
}

// ssdpUnicastSocket is the unconnected host socket used only by strict replies.
// These seams let tests inspect the bind, pin and ancillary data without LAN I/O.
type ssdpUnicastSocket interface {
	ssdpSendConn
	WriteMsgUDP([]byte, []byte, *net.UDPAddr) (int, int, error)
	SyscallConn() (syscall.RawConn, error)
}

type ssdpStrictReply struct {
	ssdpUnicastSocket
	to   *net.UDPAddr
	from SSDPEndpointSnapshot
}

func (c *ssdpStrictReply) Write(b []byte) (int, error) {
	return writeStrictReply(c.ssdpUnicastSocket, b, c.to, c.from)
}

// setStrictReplyInterface propagates both RawConn.Control and setsockopt
// failures; either means that a strict reply cannot safely be sent.
func setStrictReplyInterface(c ssdpUnicastSocket, index int, set func(uintptr, int) error) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var optionErr error
	if err := raw.Control(func(fd uintptr) { optionErr = set(fd, index) }); err != nil {
		return err
	}
	return optionErr
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
