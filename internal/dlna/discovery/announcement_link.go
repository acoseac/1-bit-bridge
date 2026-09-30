package discovery

// The link an SSDP client's announcements arrive on, and what a link-local
// source may approve there (backlog B49, 2026-09-30).
//
// #1069 let a LOCATION, and #1074 every later dial, reach a link-local address
// when the SSDP packet came from that very address, for the zero-configuration
// device: a renderer on a direct cable self-assigns 169.254.x.y and has no
// other address to announce from. But a UDP source is not authenticated, so a
// peer on the same segment could send an answer "from" a link-local neighbour
// and the bridge would GET that neighbour, at a port and path the peer chose,
// and dial it again for SOAP and proxied byte fetches whose answers the proxy
// relays. The cloud metadata addresses were already refused whatever the
// source (#1074); this bounds the rest to the link the exception exists for.
//
// A zero-configuration link, as this host sees it, is one where its interface
// holds an IPv4 link-local address and no other IPv4 address: macOS and
// Windows self-assign one when DHCP does not answer (measured on the dev
// Mac's USB link to an iPhone, a 169.254/16 address and fe80 only, and on a
// Windows host's APIPA adapters, 169.254/16 only), and a Linux host has one
// when its connection is configured link-local. On a link where the host
// holds a routable address the devices have one too, and a 169.254 source
// there is either a device that failed DHCP, which UPnP requires to keep
// asking and move once it gets an answer, or a forgery. Measured 2026-09-30:
// a connect to 169.254.7.7 from a host whose LAN interface has a DHCP address
// fails at once on Windows (WSAENETUNREACH, no 169.254 route) and on a Linux
// host with no 169.254 route (routed to the default gateway, which does not
// forward it), and on macOS leaves through the primary interface's 169.254
// route onto the LAN, where a neighbour holding the address would answer.
//
// The verdict is read from the client's OWN interface, the one its M-SEARCH
// goes out on and genuine answers come back from, when the client is built
// and again before every M-SEARCH; not per packet (Interface.Addrs is a
// syscall per call, GetAdaptersAddresses on Windows), and not from the
// interface a packet arrived on, which x/net/ipv4 cannot report on Windows
// (no control messages there). The residual that choice leaves: a peer on a
// configured link sending a forged answer to the ephemeral port of the client
// on ANOTHER interface that is zero-configuration, from the address of a
// device on that other link; both the port and the address are unseen from
// the peer's link.

import (
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// ZeroConfIPv4Link reports whether addrs, one interface's addresses as
// net.Interface.Addrs returns them, make it a zero-configuration IPv4 link
// for this host: at least one IPv4 link-local address (169.254/16) and no
// other IPv4 address. IPv6 addresses do not count either way: the SSDP
// clients are IPv4, and an IPv6 router advertising a global prefix says
// nothing about whether an IPv4 DHCP server answers.
func ZeroConfIPv4Link(addrs []net.Addr) bool {
	linkLocal := false
	for _, a := range addrs {
		ip, ok := ipv4Of(a)
		if !ok {
			continue
		}
		if !ip.IsLinkLocalUnicast() {
			return false
		}
		linkLocal = true
	}
	return linkLocal
}

// ipv4Of returns the IPv4 address an interface address holds, and false for
// an IPv6 one or one of a type net.Interface.Addrs does not return.
func ipv4Of(a net.Addr) (netip.Addr, bool) {
	var ip net.IP
	switch v := a.(type) {
	case *net.IPNet:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	default:
		return netip.Addr{}, false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	return addr, addr.Is4()
}

// maxLinkRefusalsLogged bounds the sources whose link refusal is logged: a
// LAN holds a handful of devices, and a peer that sends from a new address
// every packet reaches the bound and then logs nothing more.
const maxLinkRefusalsLogged = 64

// AnnouncementLink is a discovery client's view of the link its SSDP answers
// arrive on: whether it is a zero-configuration IPv4 link (ZeroConfIPv4Link),
// as the client's interface's addresses said when last read, and what a
// packet's source therefore approves (AnnouncedOn). Safe for concurrent use.
type AnnouncementLink struct {
	log    *slog.Logger
	client string
	iface  string
	addrs  func() ([]net.Addr, error)

	zeroConf atomic.Bool

	mu sync.Mutex
	// routable is the first IPv4 address other than a link-local one that
	// the last read found, named in a refusal's log line; "" when none.
	routable string
	// refused holds the sources whose link refusal has been logged.
	refused map[netip.Addr]struct{}
}

// NewAnnouncementLink returns the link of a discovery client on ifi, reading
// its addresses with addrs (nil reads ifi.Addrs) once before it returns.
// client names the client in log lines ("renderer discovery", "upstream
// server discovery"), as NewSendFailureLog's does.
func NewAnnouncementLink(log *slog.Logger, client string, ifi *net.Interface, addrs func() ([]net.Addr, error)) *AnnouncementLink {
	l := &AnnouncementLink{log: log, client: client, addrs: addrs}
	if ifi != nil {
		l.iface = ifi.Name
		if l.addrs == nil {
			l.addrs = ifi.Addrs
		}
	}
	l.Refresh()
	return l
}

// Refresh reads the interface's addresses again. The clients call it before
// every M-SEARCH, so the verdict describes the link as it was when the search
// the answers reply to went out. An interface whose addresses cannot be read
// is not a zero-configuration link: a link-local source then approves
// nothing until a read succeeds.
func (l *AnnouncementLink) Refresh() {
	var addrs []net.Addr
	if l.addrs != nil {
		if got, err := l.addrs(); err == nil {
			addrs = got
		} else {
			l.log.Debug("SSDP link addresses unreadable; link-local sources approve nothing until they are read",
				"client", l.client, "interface", l.iface, "err", err.Error())
		}
	}
	routable := ""
	for _, a := range addrs {
		if ip, ok := ipv4Of(a); ok && !ip.IsLinkLocalUnicast() {
			routable = ip.String()
			break
		}
	}
	l.mu.Lock()
	l.routable = routable
	l.mu.Unlock()
	l.zeroConf.Store(ZeroConfIPv4Link(addrs))
}

// ZeroConf reports the last verdict: whether the link is a zero-configuration
// IPv4 link.
func (l *AnnouncementLink) ZeroConf() bool { return l.zeroConf.Load() }

// Approval is the approval a packet from src gives on this link:
// AnnouncedOn(src, l.ZeroConf()).
func (l *AnnouncementLink) Approval(src *net.UDPAddr) DialApproval {
	return AnnouncedOn(src, l.ZeroConf())
}

// Location returns what a client may do with the LOCATION a packet from src
// carries on this link: the LOCATION when it may fetch it, "" when it may not
// (LocationPermittedBy), and the approval every request the packet causes
// runs under, the description fetch, a renderer's GetProtocolInfo and every
// later dial of a URL it finds (the approval a cache records beside it).
//
// A LOCATION refused ONLY because the link is not a zero-configuration one
// (it names the packet's own link-local address, which such a link would
// approve) is logged once per source at Warn, bounded: a real device stuck
// on a link-local address on a configured LAN leaves discovery otherwise
// without a word. The other refusals are the client's Debug line.
func (l *AnnouncementLink) Location(raw string, src *net.UDPAddr) (string, DialApproval) {
	zeroConf := l.ZeroConf()
	ap := AnnouncedOn(src, zeroConf)
	location := LocationPermittedBy(raw, ap)
	if location == "" && raw != "" && !zeroConf && LocationPermittedBy(raw, AnnouncedOn(src, true)) != "" {
		l.noteRefusal(announcerAddr(src))
	}
	return location, ap
}

// noteRefusal logs a link refusal of a packet from from, once per source and
// for at most maxLinkRefusalsLogged sources.
func (l *AnnouncementLink) noteRefusal(from netip.Addr) {
	l.mu.Lock()
	if l.refused == nil {
		l.refused = make(map[netip.Addr]struct{})
	}
	_, seen := l.refused[from]
	if seen || len(l.refused) >= maxLinkRefusalsLogged {
		l.mu.Unlock()
		return
	}
	l.refused[from] = struct{}{}
	hostIPv4 := l.routable
	l.mu.Unlock()
	if hostIPv4 == "" {
		hostIPv4 = "none"
	}
	l.log.Warn("SSDP answer from a link-local address not followed: a link-local device is fetched only on a "+
		"zero-configuration link, where this host holds a link-local IPv4 address and no other, and this link "+
		"is not one (a device that failed DHCP, or a forged source)",
		"client", l.client, "interface", l.iface, "source", from.String(), "hostIPv4", hostIPv4)
}
