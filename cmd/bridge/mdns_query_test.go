package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// mdnsGroup is the IPv4 mDNS group and port.
var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// mdnsQuerier asks a started mDNS responder questions as a client on the LAN
// asks them (B744's item 4), from a socket bound to one selected interface's
// IPv4 address, sending to the mDNS group out of that interface with
// multicast loopback on, so the bridge's responder, pinned to the same
// interface (pick), hears them on this host. Neither then depends on the
// host's default multicast route (a Docker bridge, a VPN). hashicorp/mdns
// answers every question by unicast to the address that asked
// (Server.sendResponse), which this socket reads.
type mdnsQuerier struct {
	iface *net.Interface
	ip    net.IP
	conn  *net.UDPConn
}

// pick is the interface the responder is pinned to: the querier's.
func (q *mdnsQuerier) pick() (*net.Interface, error) { return q.iface, nil }

// newMDNSQuerier selects the interface and opens the querier, or returns nil
// and why no responder can be asked here. It decides before any responder
// starts: an interface that is up, can multicast, is no loopback or
// point-to-point one and holds an IPv4 address, on which a socket joins the
// mDNS group (the join the responder makes) and hears a question this
// querier sends there (the path the responder's will take). So once a
// responder has started, a question it leaves unanswered is a failure.
// Windows is not asked, as internal/mdns's live tests are not.
func newMDNSQuerier(t *testing.T) (*mdnsQuerier, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return nil, "the mDNS responder is not asked on Windows, as internal/mdns's live tests are not"
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, "the interfaces cannot be listed: " + err.Error()
	}
	var tried []string
	for i := range ifaces {
		iface := &ifaces[i]
		const want = net.FlagUp | net.FlagMulticast
		if iface.Flags&want != want || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		ip := firstIPv4Of(iface)
		if ip == nil {
			continue
		}
		q, why := tryMDNSQuerier(t, iface, ip)
		if q != nil {
			t.Logf("the mDNS responder is pinned to %s and asked from %s", iface.Name, ip)
			return q, ""
		}
		tried = append(tried, iface.Name+": "+why)
	}
	if len(tried) == 0 {
		return nil, "no interface is up, able to multicast, not loopback or point-to-point, and holding an IPv4 address"
	}
	return nil, "no interface carried a question back to a socket joined to the mDNS group (" + strings.Join(tried, "; ") + ")"
}

// firstIPv4Of is the first IPv4 address iface holds, or nil.
func firstIPv4Of(iface *net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip4 := n.IP.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

// tryMDNSQuerier opens a querier on iface at ip and checks that a socket
// joined to the mDNS group there hears a question it sends, or says why not.
func tryMDNSQuerier(t *testing.T, iface *net.Interface, ip net.IP) (*mdnsQuerier, string) {
	listener, err := net.ListenMulticastUDP("udp4", iface, mdnsGroup)
	if err != nil {
		return nil, "the join failed: " + err.Error()
	}
	defer listener.Close()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, "no socket on " + ip.String() + ": " + err.Error()
	}
	p := ipv4.NewPacketConn(conn)
	if err := p.SetMulticastInterface(iface); err != nil {
		conn.Close()
		return nil, "multicast sends cannot be pinned to it: " + err.Error()
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		conn.Close()
		return nil, "multicast loopback cannot be set: " + err.Error()
	}
	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	probe := mdnsQuestion(t, "b744-probe-"+hex.EncodeToString(nonce[:])+".local.", dns.TypeA)
	if _, err := conn.WriteToUDP(probe, mdnsGroup); err != nil {
		conn.Close()
		return nil, "a question cannot be sent: " + err.Error()
	}
	_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65536)
	for {
		n, _, err := listener.ReadFromUDP(buf)
		if err != nil {
			conn.Close()
			return nil, "a question sent was not heard within 2 s: " + err.Error()
		}
		if bytes.Equal(buf[:n], probe) {
			break
		}
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &mdnsQuerier{iface: iface, ip: ip, conn: conn}, ""
}

// mdnsQuestion is one mDNS question for name of type qtype, packed.
func mdnsQuestion(t *testing.T, name string, qtype uint16) []byte {
	t.Helper()
	msg := new(dns.Msg)
	msg.SetQuestion(name, qtype)
	msg.RecursionDesired = false
	out, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertAnswersFor fails unless the started responder answers the A question
// for local (a `.local` host /v1/health advertises) with the selected
// interface's address: the name the bridge advertises is the name its own
// responder publishes. It asks again every half second and takes only an
// answer naming local that carries that address. Another responder on the
// host may answer for the same name with an address of its own:
// internal/mdns's live test publishes xn--f1ai0a.local on loopback, and Linux
// hands a group's datagrams to every socket that joined the group, on any
// interface, so under a parallel run that responder hears this question and
// answers it. The responder has started, so no such answer within 10 s fails
// the test, saying what came back instead, and so does a question that
// cannot be sent.
func (q *mdnsQuerier) assertAnswersFor(t *testing.T, local string) {
	t.Helper()
	name := local + "."
	out := mdnsQuestion(t, name, dns.TypeA)
	buf := make([]byte, 65536)
	var other []net.IP
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, err := q.conn.WriteToUDP(out, mdnsGroup); err != nil {
			t.Fatalf("the A question for %s could not be sent on %s: %v", name, q.iface.Name, err)
		}
		_ = q.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			n, _, err := q.conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			var resp dns.Msg
			if resp.Unpack(buf[:n]) != nil {
				continue
			}
			for _, rr := range append(append([]dns.RR(nil), resp.Answer...), resp.Extra...) {
				if a, ok := rr.(*dns.A); ok && strings.EqualFold(a.Hdr.Name, name) {
					if a.A.Equal(q.ip) {
						return
					}
					other = append(other, a.A)
				}
			}
		}
	}
	if len(other) == 0 {
		t.Fatalf("the started responder left the A question for %s unanswered on %s for 10 s", name, q.iface.Name)
	}
	t.Fatalf("the A question for %s was answered on %s only with %v, want %s, the address of %s",
		name, q.iface.Name, other, q.ip, q.iface.Name)
}
