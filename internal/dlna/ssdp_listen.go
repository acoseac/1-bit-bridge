package dlna

import (
	"context"
	"log/slog"
	"net"
)

// listenSSDP opens an advertiser's M-SEARCH listener: net.ListenMulticastUDP
// on group, joined on iface.
func listenSSDP(_ context.Context, iface *net.Interface, group *net.UDPAddr, _ *slog.Logger) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp4", iface, group)
}
