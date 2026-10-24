package dlna

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// SSDPNetwork supplies an instance's socket operations. Nil uses the host.
// Implementations must honor deadlines and Close, including a write already
// underway. This seam leaves registration, dispatch and retirement unchanged.
type SSDPNetwork struct {
	Listen     func(context.Context, *net.Interface, *slog.Logger) (SSDPReader, SSDPListenerCaps, error)
	OpenSender func(*net.Interface) (SSDPSender, error, error)
	Reply      func(netip.AddrPort, *SSDPEndpointSnapshot) (SSDPReply, error)
	// Delay and IntervalFloor let an isolated test control MX and repeats.
	// Nil/zero retain production timing.
	Delay         func(time.Duration) time.Duration
	IntervalFloor time.Duration
}

// SSDPReader is a datagram listener with explicit destination/arrival evidence.
type SSDPReader interface {
	ReadDatagram([]byte, []byte, time.Time) (int, SSDPDatagram, error)
	Close() error
}

// SSDPSender is an unconnected, interface-pinned NOTIFY socket.
type SSDPSender interface {
	WriteToUDP([]byte, *net.UDPAddr) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
}

// SSDPReply is a response socket pinned to the selected endpoint when strict.
type SSDPReply interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
}

type injectedSSDPNetwork struct{ network SSDPNetwork }
type injectedSSDPReader struct{ SSDPReader }

func (r injectedSSDPReader) readDatagram(b, oob []byte, deadline time.Time) (int, netip.AddrPort, datagramMeta, error) {
	n, d, err := r.ReadDatagram(b, oob, deadline)
	return n, d.Source, datagramMeta{dst: d.Destination, dstOK: d.DestinationValid, ifIndex: d.ArrivalIndex, ifOK: d.ArrivalValid}, err
}
func (n injectedSSDPNetwork) listen(ctx context.Context, iface *net.Interface, log *slog.Logger) (ssdpReadConn, ssdpListenerSetup, error) {
	r, caps, err := n.network.Listen(ctx, iface, log)
	if err != nil {
		return nil, ssdpListenerSetup{}, err
	}
	return injectedSSDPReader{r}, ssdpListenerSetup{caps: caps}, nil
}
func (n injectedSSDPNetwork) openSender(iface *net.Interface) (ssdpSendConn, error, error) {
	return n.network.OpenSender(iface)
}
func (n injectedSSDPNetwork) dialReply(to netip.AddrPort, from *SSDPEndpointSnapshot) (ssdpReplyConn, error) {
	return n.network.Reply(to, from)
}
