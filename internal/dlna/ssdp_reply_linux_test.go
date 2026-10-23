package dlna

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLinuxStrictReplyCarriesItsInterfaceAndSource checks the actual Linux
// sendmsg ancillary bytes, not a second call to the production marshaler.
func TestLinuxStrictReplyCarriesItsInterfaceAndSource(t *testing.T) {
	c := &round2Unicast{}
	host := hostSSDPTransport{openReply: func(addr *net.UDPAddr) (ssdpUnicastSocket, error) { return c, nil }}
	from := SSDPEndpointSnapshot{Interface: SSDPInterface{Index: 7}, Local: netip.MustParsePrefix("192.168.1.20/24")}
	r, err := host.dialReply(netip.MustParseAddrPort(searchSrc), &from)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	msgs, err := unix.ParseSocketControlMessage(c.oob)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d ancillary messages", len(msgs))
	}
	m := msgs[0]
	if m.Header.Level != unix.IPPROTO_IP || m.Header.Type != unix.IP_PKTINFO || len(m.Data) != 12 {
		t.Fatalf("not in_pktinfo: %+v", m)
	}
	if got := binary.NativeEndian.Uint32(m.Data[:4]); got != 7 {
		t.Errorf("outgoing interface = %d", got)
	}
	if got := [4]byte(m.Data[4:8]); got != [4]byte{192, 168, 1, 20} {
		t.Errorf("ipi_spec_dst = %v", got)
	}
}
