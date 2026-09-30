//go:build !linux

package dlna

import (
	"context"
	"log/slog"
	"net"
)

// listenSSDP opens an advertiser's M-SEARCH listener: net.ListenMulticastUDP
// on group, joined on iface. macOS and Windows deliver a group's datagrams
// only to the sockets that joined the group on the interface a datagram
// arrived on (measured on both, 2026-09-29, backlog B71), which is what the
// Linux listener has to ask for (ssdp_listen_linux.go), so this one is net's
// as it stands and log goes unused.
func listenSSDP(_ context.Context, iface *net.Interface, group *net.UDPAddr, _ *slog.Logger) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp4", iface, group)
}
