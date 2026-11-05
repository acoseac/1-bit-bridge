package mdns

import (
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// TestTheMDNSHostIsTheNameTheCertificateCarries is backlog B739. The SRV
// target and the TXT record's host= named the host as the system reported it,
// and the certificate cannot carry a name that is not ASCII, while the app
// builds `https://<host>:<port>` from host= and checks the certificate
// against it. The responder publishes the name in the form the certificate
// carries: an ASCII name as it is, byte for byte; a name with non-ASCII
// characters in its IDNA ASCII form; and a name with no valid form (a test
// Mac's kern.hostname) as no host name, which is localhost.local, as before.
func TestTheMDNSHostIsTheNameTheCertificateCarries(t *testing.T) {
	for _, tc := range []struct {
		name, host, want string
	}{
		{"a Cyrillic name", "Жук", "xn--f1ai0a.local"},
		{"the test Mac's bytes", "Ð\u0091Ä¯U", "localhost.local"},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2.local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ProtocolVersion: 1, Port: 7788, Hostname: tc.host}
			if got := cfg.advertisedHost(); got != tc.want {
				t.Errorf("advertisedHost = %q, want %q", got, tc.want)
			}
			if txt := buildTXTRecords(cfg, nil); !slices.Contains(txt, "host="+tc.want) {
				t.Errorf("the TXT record is %q, want host=%s", txt, tc.want)
			}
		})
	}
}

// TestAClientResolvesTheASCIINameThroughTheBridgesResponder is B739's mDNS
// half, measured: the bridge's own responder publishes the name it advertises
// (rebuildLocked hands hashicorp/mdns advertisedHost as the SRV target), and
// hashicorp/mdns answers an A or AAAA question whose name is exactly that
// target. So a client given `https://xn--f1ai0a.local:<port>` (a host named
// Жук), from the TXT record or /v1/health, resolves it through the bridge
// itself. A live responder on the loopback interface is asked, as a client
// asks, for the service and then for the host's address: the SRV target and
// host= are the ASCII form, and the address question for that name is
// answered with the responder's address.
func TestAClientResolvesTheASCIINameThroughTheBridgesResponder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mdns live test skipped on windows")
	}
	loopback := loopbackInterface(t)
	const instance, host = "b739-ascii-host", "xn--f1ai0a.local."
	a, err := advertiseInternal(Config{
		InstanceName:    instance,
		Port:            62991,
		ProtocolVersion: 1,
		LibraryName:     "B739 Test",
		Hostname:        "Жук",
		InterfaceSource: func() *net.Interface { return loopback },
	}, func() []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} }, time.Hour, false)
	if err != nil {
		t.Skipf("mdns unavailable in this env: %v", err)
	}
	defer a.Close()

	q := newLoopbackQuerier(t, loopback)
	browse := q.ask(t, Service+".local.", dns.TypePTR, func(rr dns.RR) bool {
		srv, ok := rr.(*dns.SRV)
		return ok && strings.HasPrefix(srv.Hdr.Name, instance+".")
	})
	var target string
	var txt []string
	for _, rr := range browse {
		switch v := rr.(type) {
		case *dns.SRV:
			if strings.HasPrefix(v.Hdr.Name, instance+".") {
				target = v.Target
			}
		case *dns.TXT:
			if strings.HasPrefix(v.Hdr.Name, instance+".") {
				txt = v.Txt
			}
		}
	}
	if target != host {
		t.Errorf("the SRV target is %q, want %q", target, host)
	}
	if !slices.Contains(txt, "host="+strings.TrimSuffix(host, ".")) {
		t.Errorf("the TXT record is %q, want host=%s", txt, strings.TrimSuffix(host, "."))
	}

	addr := q.ask(t, host, dns.TypeA, func(rr dns.RR) bool {
		a, ok := rr.(*dns.A)
		return ok && a.Hdr.Name == host
	})
	found := false
	for _, rr := range addr {
		if a, ok := rr.(*dns.A); ok && a.Hdr.Name == host && a.A.Equal(net.ParseIP("127.0.0.1")) {
			found = true
		}
	}
	if !found {
		t.Errorf("no answer named %s carries 127.0.0.1: %v", host, addr)
	}
}

// loopbackQuerier asks multicast DNS questions on the loopback interface from
// a socket of its own, and reads the answers sent back to it: hashicorp/mdns
// answers every question by unicast to the address that asked
// (Server.sendResponse).
type loopbackQuerier struct {
	conn *net.UDPConn
}

// newLoopbackQuerier opens the querier's socket on 127.0.0.1 and pins its
// multicast sends to the loopback interface, or skips the test where the
// interface takes no pin.
func newLoopbackQuerier(t *testing.T, loopback *net.Interface) *loopbackQuerier {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no UDP socket on loopback: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p := ipv4.NewPacketConn(conn)
	if err := p.SetMulticastInterface(loopback); err != nil {
		t.Skipf("multicast sends cannot be pinned to %s here: %v", loopback.Name, err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Skipf("multicast loopback cannot be set here: %v", err)
	}
	return &loopbackQuerier{conn: conn}
}

// ask sends one question for name of type qtype to the mDNS group and returns
// the answer records of the first response holding a record match takes,
// failing the test when none comes within the deadline. Other responders on
// the host may answer too, about names of their own, so a response is taken
// only by match.
func (q *loopbackQuerier) ask(t *testing.T, name string, qtype uint16, match func(dns.RR) bool) []dns.RR {
	t.Helper()
	msg := new(dns.Msg)
	msg.SetQuestion(name, qtype)
	msg.RecursionDesired = false
	out, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 65536)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if _, err := q.conn.WriteToUDP(out, group); err != nil {
			t.Skipf("no multicast send on loopback here: %v", err)
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
			records := append(append([]dns.RR(nil), resp.Answer...), resp.Extra...)
			if slices.ContainsFunc(records, match) {
				return records
			}
		}
	}
	t.Fatalf("no answer to the %s question for %s came back", dns.TypeToString[qtype], name)
	return nil
}
