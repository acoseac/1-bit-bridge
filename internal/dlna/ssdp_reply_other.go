//go:build !linux && !darwin && !windows

package dlna

import "net"

// pinStrictReply refuses platforms without a proven unprivileged interface pin.
func pinStrictReply(_ ssdpUnicastSocket, _ int) error { return ErrSSDPStrictCapability }

func writeStrictReply(_ ssdpUnicastSocket, _ []byte, _ *net.UDPAddr, _ SSDPEndpointSnapshot) (int, error) {
	return 0, ErrSSDPStrictCapability
}
