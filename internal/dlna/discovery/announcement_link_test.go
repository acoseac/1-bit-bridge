package discovery

// The link a discovery client's SSDP answers arrive on (backlog B49): a
// packet's link-local source approves itself only on a zero-configuration
// IPv4 link, judged from the client's own interface.

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// ipNet is an interface address as net.Interface.Addrs returns one on
// Linux and Windows (an IPv4 address in its 16-byte form).
func ipNet(cidr string) *net.IPNet {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

// TestZeroConfIPv4Link pins the predicate on the shapes an interface takes.
// The zero-configuration ones are those measured on 2026-09-30: the dev
// Mac's USB link to an iPhone (a self-assigned 169.254/16 address and fe80) and a Windows
// host's APIPA adapters (169.254/16 alone).
func TestZeroConfIPv4Link(t *testing.T) {
	v4 := func(a, b, c, d byte, bits int) *net.IPNet {
		return &net.IPNet{IP: net.IPv4(a, b, c, d).To4(), Mask: net.CIDRMask(bits, 32)} // the 4-byte form macOS returns
	}
	for _, tc := range []struct {
		name  string
		addrs []net.Addr
		want  bool
	}{
		{"a self-assigned address and fe80 (macOS)", []net.Addr{ipNet("fe80::2/64"), ipNet("169.254.3.4/16")}, true},
		{"a self-assigned address alone (Windows APIPA)", []net.Addr{ipNet("169.254.10.20/16")}, true},
		{"a self-assigned address in its 4-byte form", []net.Addr{v4(169, 254, 3, 4, 16)}, true},
		{"as an IPAddr", []net.Addr{&net.IPAddr{IP: net.ParseIP("169.254.3.4")}}, true},
		{"beside a global IPv6 address", []net.Addr{ipNet("169.254.3.4/16"), ipNet("2001:db8::5/64")}, true},
		{"beside a Tailscale ULA", []net.Addr{ipNet("169.254.3.4/16"), ipNet("fd7a:115c:a1e0::1/128")}, true},
		{"a DHCP address and fe80", []net.Addr{ipNet("fe80::1/64"), ipNet("192.0.2.85/24")}, false},
		{"a DHCP address in its 4-byte form", []net.Addr{v4(192, 168, 0, 85, 24)}, false},
		{"a routable address beside a self-assigned one", []net.Addr{ipNet("169.254.3.4/16"), ipNet("10.0.0.2/8")}, false},
		{"a self-assigned address after a routable one", []net.Addr{ipNet("10.0.0.2/8"), ipNet("169.254.3.4/16")}, false},
		{"a CGNAT address", []net.Addr{ipNet("100.64.1.2/10")}, false},
		{"a public address", []net.Addr{ipNet("203.0.113.9/24")}, false},
		{"fe80 alone: no IPv4 at all", []net.Addr{ipNet("fe80::1/64")}, false},
		{"no address", nil, false},
		{"an address of another type", []net.Addr{&net.UnixAddr{Name: "x", Net: "unix"}}, false},
	} {
		if got := ZeroConfIPv4Link(tc.addrs); got != tc.want {
			t.Errorf("%s: ZeroConfIPv4Link(%v) = %v, want %v", tc.name, tc.addrs, got, tc.want)
		}
	}
}

// switchingAddrs returns an addrs func whose answer the test sets.
type switchingAddrs struct {
	mu    sync.Mutex
	addrs []net.Addr
	err   error
}

func (s *switchingAddrs) set(addrs []net.Addr, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs, s.err = addrs, err
}

func (s *switchingAddrs) read() ([]net.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addrs, s.err
}

// TestAnnouncementLinkFollowsItsInterface pins when the verdict is read: once
// when the link is built, and again at every Refresh (before each M-SEARCH),
// never in between; and an interface whose addresses cannot be read is not a
// zero-configuration link.
func TestAnnouncementLinkFollowsItsInterface(t *testing.T) {
	addrs := &switchingAddrs{}
	addrs.set([]net.Addr{ipNet("169.254.3.4/16")}, nil)
	l := NewAnnouncementLink(slog.New(slog.DiscardHandler), "test", &net.Interface{Name: "en7"}, addrs.read)
	src := udpFrom("169.254.7.7")
	direct := netip.MustParseAddr("169.254.7.7")
	if !l.ZeroConf() || !l.Approval(src).Permits(direct) {
		t.Fatalf("built on a self-assigned address: zero-conf %v, approval %v; want a zero-configuration link", l.ZeroConf(), l.Approval(src))
	}

	addrs.set([]net.Addr{ipNet("10.0.0.2/8")}, nil) // DHCP answered
	if !l.ZeroConf() {
		t.Error("the verdict moved before a Refresh")
	}
	l.Refresh()
	if l.ZeroConf() || l.Approval(src).Permits(direct) {
		t.Errorf("after a Refresh with a routable address: zero-conf %v, approval %v; want a configured link", l.ZeroConf(), l.Approval(src))
	}

	addrs.set([]net.Addr{ipNet("169.254.3.4/16")}, nil)
	l.Refresh()
	if !l.ZeroConf() {
		t.Error("back on a self-assigned address: not a zero-configuration link")
	}
	addrs.set(nil, errors.New("interface went away"))
	l.Refresh()
	if l.ZeroConf() || l.Approval(src).Permits(direct) {
		t.Errorf("addresses unreadable: zero-conf %v; want a link-local source to approve nothing", l.ZeroConf())
	}
}

// TestAnnouncementLinkLogsALinkRefusalOncePerSourceAndBounded pins the one
// Warn: a LOCATION refused only because the link is not a zero-configuration
// one is logged once per source, and a peer sending from a new address every
// packet stops being logged at maxLinkRefusalsLogged. A refusal the link does
// not decide (a loopback LOCATION from a LAN address) and a zero-configuration
// link log nothing at Warn.
func TestAnnouncementLinkLogsALinkRefusalOncePerSourceAndBounded(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.WriteString(string(p))
	}), &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnings := func() int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), "SSDP answer from a link-local address not followed")
	}

	configured := NewAnnouncementLink(log, "test", &net.Interface{Name: "en0"}, configuredLink)
	for range 3 {
		if loc, _ := configured.Location("http://169.254.7.7:8080/d.xml", udpFrom("169.254.7.7")); loc != "" {
			t.Fatalf("a link-local LOCATION from its own address on a configured link was kept: %q", loc)
		}
	}
	if got := warnings(); got != 1 {
		t.Errorf("warnings after three answers from one source = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "hostIPv4=192.0.2.1") || !strings.Contains(buf.String(), "interface=en0") {
		t.Errorf("the warning does not name the interface and this host's address on it:\n%s", buf.String())
	}
	configured.Location("http://127.0.0.1:7789/api/stats", udpFrom("192.0.2.7"))
	configured.Location("http://169.254.7.8:8080/d.xml", udpFrom("169.254.7.7")) // another address than the source
	if got := warnings(); got != 1 {
		t.Errorf("warnings after refusals the link does not decide = %d, want still 1", got)
	}
	for i := range 2 * maxLinkRefusalsLogged {
		ip := fmt.Sprintf("169.254.8.%d", i+1)
		configured.Location("http://"+ip+":8080/d.xml", udpFrom(ip))
	}
	if got := warnings(); got != maxLinkRefusalsLogged {
		t.Errorf("warnings after %d distinct sources = %d, want the bound, %d", 2*maxLinkRefusalsLogged+1, got, maxLinkRefusalsLogged)
	}

	buf.Reset()
	zeroConf := NewAnnouncementLink(log, "test", &net.Interface{Name: "en7"}, zeroConfLink)
	if loc, _ := zeroConf.Location("http://169.254.7.7:8080/d.xml", udpFrom("169.254.7.7")); loc == "" {
		t.Error("a link-local LOCATION from its own address on a zero-configuration link was refused")
	}
	if got := warnings(); got != 0 {
		t.Errorf("warnings on a zero-configuration link = %d, want 0", got)
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestAnnouncedOnApprovesALinkLocalSourceOnlyOnAZeroConfLink pins the
// approval itself: a link-local source covers its own address only on a
// zero-configuration link, and an IPv6 one on none; a loopback source covers
// its own address on either; a GENA subscriber's link-local address counts
// on any link, since the TCP handshake showed it to be the peer's own.
func TestAnnouncedOnApprovesALinkLocalSourceOnlyOnAZeroConfLink(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approval DialApproval
		dial     string
		want     bool
	}{
		{"link-local source, zero-conf link", AnnouncedOn(udpFrom("169.254.7.7"), true), "169.254.7.7", true},
		{"link-local source, configured link", AnnouncedOn(udpFrom("169.254.7.7"), false), "169.254.7.7", false},
		{"link-local source, AnnouncedFrom", AnnouncedFrom(udpFrom("169.254.7.7")), "169.254.7.7", false},
		{"link-local source, zero-conf link, another address", AnnouncedOn(udpFrom("169.254.7.7"), true), "169.254.7.8", false},
		{"IPv6 link-local source, zero-conf link", AnnouncedOn(udpFrom("fe80::7"), true), "fe80::7", false},
		{"loopback source, configured link", AnnouncedOn(udpFrom("127.0.0.1"), false), "127.0.0.1", true},
		{"loopback source, zero-conf link", AnnouncedOn(udpFrom("127.0.0.1"), true), "127.0.0.1", true},
		{"metadata source, zero-conf link", AnnouncedOn(udpFrom("169.254.169.254"), true), "169.254.169.254", false},
		{"LAN source, zero-conf link, a link-local address", AnnouncedOn(udpFrom("192.0.2.7"), true), "169.254.7.7", false},
		{"no source, zero-conf link", AnnouncedOn(nil, true), "169.254.7.7", false},
		{"GENA subscriber, link-local", SubscribedFrom(netip.MustParseAddr("169.254.7.7")), "169.254.7.7", true},
		{"GENA subscriber, IPv6 link-local", SubscribedFrom(netip.MustParseAddr("fe80::7")), "fe80::7", true},
	} {
		if got := tc.approval.Permits(netip.MustParseAddr(tc.dial)); got != tc.want {
			t.Errorf("%s (%v): Permits(%s) = %v, want %v", tc.name, tc.approval, tc.dial, got, tc.want)
		}
	}
	if a, b := AnnouncedOn(udpFrom("169.254.7.7"), true), AnnouncedOn(udpFrom("169.254.7.7"), false); a == b || a.String() == b.String() {
		t.Errorf("the two links' approvals compare or print alike (%q, %q): a cache and a log line could not tell them apart", a, b)
	}
}

// TestTheRendererClientReadsItsLinkBeforeEverySearch pins the wiring of the
// refresh: the running client reads its interface's addresses again before
// each M-SEARCH, so an interface that self-assigned an address after the
// client was built (DHCP gave up) approves its link-local devices from the
// next search on, and one that got a DHCP address stops approving them.
func TestTheRendererClientReadsItsLinkBeforeEverySearch(t *testing.T) {
	addrs := &switchingAddrs{}
	addrs.set([]net.Addr{ipNet("192.0.2.1/24")}, nil)
	cfg := DefaultDiscoveryConfig()
	cfg.Interface = &net.Interface{}
	cfg.InterfaceAddrs = addrs.read
	cfg.MSearchInterval = 20 * time.Millisecond
	cfg.Dispatcher = &stubDispatcher{}
	c, err := NewSSDPDiscoveryClient(cfg, NewRendererCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	sent := make(chan struct{}, 1)
	c.writeMSearch = func(_ *net.UDPConn, b []byte, _ *net.UDPAddr) (int, error) {
		select {
		case sent <- struct{}{}:
		default:
		}
		return len(b), nil
	}
	// awaitVerdict lets searches go out until the link reads want, and fails
	// after a bounded number: a search that began before the addresses
	// changed may still carry the old verdict, the ones after it may not.
	awaitVerdict := func(want bool, what string) {
		t.Helper()
		for range 20 {
			select {
			case <-sent:
			case <-time.After(5 * time.Second):
				t.Fatal("the tick loop sent no M-SEARCH")
			}
			if c.link.ZeroConf() == want {
				return
			}
		}
		t.Errorf("%s: twenty searches later the link still reads zero-conf = %v", what, !want)
	}
	startClient(t, c)
	awaitVerdict(false, "built on a DHCP address")
	addrs.set([]net.Addr{ipNet("169.254.3.4/16")}, nil)
	awaitVerdict(true, "after the interface self-assigned an address")
	addrs.set([]net.Addr{ipNet("192.0.2.1/24")}, nil)
	awaitVerdict(false, "after the interface got a DHCP address")
}
