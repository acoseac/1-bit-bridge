//go:build !linux

package dlna

import (
	"context"
	"log/slog"
	"net"
)

// listenSSDP opens an SSDP listener: net.ListenMulticastUDP on group, joined
// on iface. macOS and Windows deliver a group's datagrams only to the
// sockets that joined the group on the interface a datagram arrived on
// (measured on both, 2026-09-29, backlog B71), which is what the Linux
// listener has to ask for (ssdp_listen_linux.go), so this one is net's as it
// stands, its membership is its confinement (confineErr is always nil), and
// log goes unused.
func listenSSDP(_ context.Context, iface *net.Interface, group *net.UDPAddr, _ *slog.Logger) (conn *net.UDPConn, confineErr error, err error) {
	conn, err = net.ListenMulticastUDP("udp4", iface, group)
	return conn, nil, err
}
