package discovery

// Which URLs a device's say-so may send the bridge to (external audit
// 2026-09-23, finding M3: "a discovered UPnP device's URLs are its own
// say-so").
//
// Every URL here comes from an unauthenticated LAN peer: the SSDP LOCATION
// header, and the <controlURL> / <eventSubURL> values inside the description
// fetched from it. Before this file, resolveServiceURL resolved a service URL
// against the description URL with no scheme or host rule at all, so a
// spoofed description could name `file:`, `ftp:`, a host-less URL, or a
// service on ANOTHER machine, and the bridge acted on it three ways:
//
//   - renderer discovery POSTed its own GetProtocolInfo to the
//     ConnectionManager control URL, and relayed the AVTransport and
//     RenderingControl URLs to the phone through /v1/renderers, where the
//     app sends its SOAP and GENA requests to them;
//   - upstream MediaServer discovery (internal/upnp) cached the
//     ContentDirectory control URL, from whose host:port LiveHost derives
//     the target of every byte fetch of that server's routed tracks
//     (/v1/download, the web player and /dlna/file/{trackID} on the
//     unauthenticated DLNA listener), with a path chosen at ingest. A
//     server that re-announced its UDN from a new address with a control
//     URL on the bridge's own loopback console steered all of them there.
//
// The rules mirror the iOS app's (UPnPURLPolicy and
// DeviceDescriptionParser.resolveServiceURL, iOS #1911; and the renderers a
// bridge relays, #1977), so both sides refuse the same shapes.
//
// Those rules bound a description's service URLs to the host that served
// it, and nothing bounded which host THAT is (backlog B14, 2026-09-28): a
// LAN peer answering an M-SEARCH, or re-announcing a known UDN (which the
// move detector re-fetches), with LOCATION http://127.0.0.1:7789/<path> made
// the bridge GET its own no-auth console, and a body that parsed as a
// description there could name loopback service URLs the same-host rule
// then accepts. So a LOCATION may lead the bridge to this machine (loopback
// or the unspecified address) or to a link-local address only when the SSDP
// packet came from that same address. Every device measured announced a
// LOCATION on its own source address (three of three, 2026-09-28), and a
// packet with a loopback source was sent on this machine (RFC 1122 has a
// host discard 127/8 arriving on any other interface). It is enforced
// twice, because a host STRING shows only part of it. LocationFromSource refuses what the string shows (an IP literal, a
// localhost name, a numeric spelling no device writes) before any fetch.
// NewDeviceFetchClient's dial check refuses the rest at the connect, where
// the address a name resolved to is known: measured with Go 1.27.1, a public
// DNS name pointed at 127.0.0.1 reached a loopback listener on macOS and on
// Linux, and so did 127.1, 2130706433 and 0x7f000001 on macOS, whose libc
// resolver takes inet_aton's spellings. The same kinds of host bound a
// service URL, for every source: one naming this machine or a link-local
// address is kept only from a description URL of the same kind, which is
// what bounds the operator's approval of a manual upstream (SourceUserChosen).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// DescriptionSource says where a device description's URL came from, which
// decides how far the service URLs inside it are trusted.
//
// The zero value is SourceDiscovered, the STRICT one, and every value but
// SourceUserChosen is read as strict, so a caller that names no source, or
// one this build does not know, gets the safe behaviour. Mirrors the iOS
// app's DeviceDescriptionParser.DescriptionSource.
type DescriptionSource int

const (
	// SourceDiscovered is a description found through SSDP: an
	// unauthenticated LAN peer's say-so. Every service URL in it must be
	// http(s) with a host AND on the description URL's host, any port, so
	// a spoofed description cannot steer the requests that follow to
	// another machine. Renderer discovery and upstream MediaServer
	// discovery both use it.
	SourceDiscovered DescriptionSource = iota

	// SourceUserChosen is a description URL the operator configured
	// (upnpUpstream.servers[].manualDescriptionURL). That choice is the
	// approval the same-host rule otherwise stands in for, so a service
	// URL on another host is kept: it is the escape hatch for a real
	// device that spans hosts. A URL that is not http(s) with a host is
	// still refused; the approval covers another HOST, never another
	// scheme.
	SourceUserChosen
)

// errServiceURLNotFetchable is resolveServiceURL's refusal of a service URL
// that is not http(s) with a host, whatever the source.
var errServiceURLNotFetchable = errors.New("not an http(s) URL with a host")

// errServiceURLOffHost is resolveServiceURL's refusal of a discovered
// description's service URL on another host than the description itself.
var errServiceURLOffHost = errors.New("on another host than the device description")

// errServiceURLHostLocal is resolveServiceURL's refusal of a service URL that
// names this machine or a link-local address when the description URL does
// not name an address of that kind, whatever the source.
var errServiceURLHostLocal = errors.New("names this machine or a link-local address, and the device description does not")

// errServiceURLNumericHost is resolveServiceURL's refusal of a service URL
// whose host ends in a number without being an IP address (127.1,
// 2130706433): no device writes one, and resolvers disagree about it.
var errServiceURLNumericHost = errors.New("host ends in a number but is not an IP address")

// hostKind says where a URL host can lead a connection, judged from the
// string alone.
type hostKind int

const (
	// hostElsewhere is an address on the LAN, a tailnet or the internet, or
	// a name the string cannot place. A name that resolves to this machine
	// is the dial check's (refuseUnannouncedHostLocal), not this one's.
	hostElsewhere hostKind = iota
	// hostThisMachine is a loopback or unspecified address (a connect to
	// either reaches this machine), or a localhost name (RFC 6761).
	hostThisMachine
	// hostLinkLocal is an IPv4 or IPv6 link-local address, where a cloud
	// VM's metadata service lives (169.254.169.254).
	hostLinkLocal
	// hostNumericSpelling ends in a number without being an IP literal.
	// The WHATWG URL standard reads such a host as IPv4, macOS's resolver
	// accepts it the way inet_aton does (127.1 and 2130706433 both reached a
	// loopback listener there), and Go's own resolver refuses it on Linux.
	// No device writes one, so it is refused wherever it appears.
	hostNumericSpelling
)

// classifyHost reads a URL host (url.URL.Hostname, so an IPv6 literal comes
// without brackets and with its zone) and returns its kind and, for an IP
// literal, its address, unmapped. One trailing dot (the DNS root) is ignored.
func classifyHost(host string) (hostKind, netip.Addr) {
	h := strings.TrimSuffix(host, ".")
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		switch {
		case a.IsLoopback() || a.IsUnspecified():
			return hostThisMachine, a
		case a.IsLinkLocalUnicast():
			return hostLinkLocal, a
		}
		return hostElsewhere, a
	}
	lower := strings.ToLower(h)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return hostThisMachine, netip.Addr{}
	}
	labels := strings.Split(h, ".")
	if endsInANumber(labels[len(labels)-1]) {
		return hostNumericSpelling, netip.Addr{}
	}
	return hostElsewhere, netip.Addr{}
}

// endsInANumber reports whether a host's last label is a number the WHATWG
// URL standard's IPv4 parser would take: decimal digits, or 0x (0X) and hex
// digits, which covers the octal spellings too.
func endsInANumber(label string) bool {
	if len(label) >= 2 && label[0] == '0' && (label[1] == 'x' || label[1] == 'X') {
		for i := 2; i < len(label); i++ {
			c := label[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	}
	return isAllDigits(label)
}

// hostKindAllowed reports whether a URL host of kind k may be used where the
// reference host (the description URL's, for a service URL) is of kind ref:
// always for a host elsewhere, never for a numeric spelling, and for this
// machine or a link-local address only from a reference of the same kind.
func hostKindAllowed(k, ref hostKind) bool {
	switch k {
	case hostElsewhere:
		return true
	case hostNumericSpelling:
		return false
	}
	return k == ref
}

// isFetchableURL reports whether u is http or https (in any case) with a
// non-empty host: the one kind of URL the bridge fetches, or dispatches SOAP
// to, on a device's say-so. Mirrors UPnPURLPolicy.isFetchable.
//
// The host is read with url.URL.Hostname, NEVER url.URL.Host. `http:///x`
// has neither, and Go's client refuses it ("no Host in request URL"). But
// `http://:7789/x` has Host ":7789" and an empty Hostname, and Go's client
// DIALS it on the local host (measured: a GET reached a listener on
// 127.0.0.1), which on a bridge is its own no-auth console. A `Host != ""`
// test would let that through, and so would LiveHost's host:port, which
// reads Host alone.
func isFetchableURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	return u.Hostname() != ""
}

// sharesHost reports whether u names the same host as ref, compared
// case-insensitively. Deliberately the HOST, not the origin: the threat is a
// spoofed description steering later requests to ANOTHER machine, and a
// device that serves its description and its control endpoints on different
// ports of one address is ordinary UPnP. url.URL.Hostname drops the port and
// an IPv6 literal's brackets. Mirrors UPnPURLPolicy.sharesHost.
func sharesHost(u, ref *url.URL) bool {
	a, b := u.Hostname(), ref.Hostname()
	return a != "" && b != "" && strings.EqualFold(a, b)
}

// fetchableLocation returns an SSDP LOCATION value unchanged when it is an
// http(s) URL with a host, and "" otherwise, which both discovery clients
// read as "nothing to fetch". The value is returned as it arrived rather
// than re-serialised, since the move detectors compare it with the Location
// recorded last time and the upstream cache stores it as DescriptionURL.
func fetchableLocation(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !isFetchableURL(u) {
		return ""
	}
	return raw
}

// resolveServiceURL resolves one raw <controlURL> or <eventSubURL> value
// against the description URL base, and refuses one the bridge must not use.
//
//   - An empty value is absent: "" and nil, as it always was.
//   - An unparseable value is an error, as it always was.
//   - A URL that is not http(s) with a host is refused whatever the source
//     (errServiceURLNotFetchable).
//   - Unless source is SourceUserChosen, a URL on another host than base is
//     refused (errServiceURLOffHost). A relative reference resolves onto
//     base's host and passes; a network-path one (`//other/x`) is another
//     host.
//   - Whatever the source, a URL naming this machine or a link-local
//     address is refused unless base names an address of the same kind
//     (errServiceURLHostLocal), and one whose host ends in a number without
//     being an IP address is refused outright (errServiceURLNumericHost).
//     For a discovered description the same-host rule already implies the
//     first; for a manual upstream it is the bound on the operator's
//     approval: a manual URL on another host keeps a service on a third, and
//     does not choose where LiveHost sends every byte fetch of its tracks
//     when that is this machine (the console) or the link (a cloud VM's
//     metadata service). A manual URL on this machine is the operator
//     pointing at a local server on purpose, and keeps its local services.
//
// The one home for these rules: every service URL of every device kind goes
// through it, so the mandatory control URL and the optional ones cannot
// drift apart. The caller decides what a refusal costs: the service for a
// control URL, the URL alone for an eventSubURL. Mirrors
// DeviceDescriptionParser.resolveServiceURL in the iOS app, apart from the
// host-kind rule, which the app does not have.
func resolveServiceURL(base *url.URL, raw string, source DescriptionSource) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", nil
	}
	resolved, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	abs := base.ResolveReference(resolved)
	if !isFetchableURL(abs) {
		return "", errServiceURLNotFetchable
	}
	if source != SourceUserChosen && !sharesHost(abs, base) {
		return "", errServiceURLOffHost
	}
	kind, _ := classifyHost(abs.Hostname())
	if kind == hostNumericSpelling {
		return "", errServiceURLNumericHost
	}
	if baseKind, _ := classifyHost(base.Hostname()); !hostKindAllowed(kind, baseKind) {
		return "", errServiceURLHostLocal
	}
	return abs.String(), nil
}

// announcerAddr is the address an SSDP packet came from as the checks below
// compare it: unmapped, without a zone. The zero Addr when src is nil or
// holds no address, which no check reads as a match.
func announcerAddr(src *net.UDPAddr) netip.Addr {
	if src == nil {
		return netip.Addr{}
	}
	a, ok := netip.AddrFromSlice(src.IP)
	if !ok {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// LocationFromSource returns location, an SSDP LOCATION that ParseSSDPHeaders
// kept, when a discovery client may fetch it on the say-so of a packet from
// src, and "" when it may not. Both SSDP clients call it on every packet and
// read "" as they read an absent LOCATION: a known UDN is refreshed, an
// unknown one is skipped, and nothing is fetched.
//
// It refuses what the host string shows: an IP literal naming this machine
// or a link-local address that is not the address the packet came from (the
// unspecified address never is one), a localhost name unless the packet came
// from a loopback address, and a numeric spelling no device writes. A packet
// with a loopback source was sent on this machine, whose processes can reach
// the console directly. A name the string cannot place is kept: the default
// client's dial check judges the address it resolves to.
func LocationFromSource(location string, src *net.UDPAddr) string {
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	kind, addr := classifyHost(u.Hostname())
	switch kind {
	case hostElsewhere:
		return location
	case hostNumericSpelling:
		return ""
	}
	from := announcerAddr(src)
	if !from.IsValid() {
		return ""
	}
	if !addr.IsValid() { // a localhost name
		if from.IsLoopback() {
			return location
		}
		return ""
	}
	if addr.IsUnspecified() || addr.WithZone("") != from {
		return ""
	}
	return location
}

// announcementSourceKey carries the SSDP packet's source address in the
// context of the fetches it causes, for the dial check.
type announcementSourceKey struct{}

// WithAnnouncementSource returns ctx carrying src, the address of the SSDP
// packet whose LOCATION a fetch follows. NewDeviceFetchClient's dial check
// connects to this machine or a link-local address only when it is that
// address. Both SSDP clients wrap every fetch a packet causes (the
// description, and a renderer's GetProtocolInfo) in it.
func WithAnnouncementSource(ctx context.Context, src *net.UDPAddr) context.Context {
	return context.WithValue(ctx, announcementSourceKey{}, announcerAddr(src))
}

// errUnannouncedHostLocal is the dial check's refusal.
var errUnannouncedHostLocal = errors.New("refusing to connect to this machine or a link-local address " +
	"on the say-so of an SSDP packet from another address")

// refuseUnannouncedHostLocal is NewDeviceFetchClient's net.Dialer
// ControlContext. net passes it the address each connect attempt targets,
// after name resolution (every A and AAAA answer is its own attempt), so it
// judges what a name RESOLVED to, which no check of the URL can: a public DNS
// name pointed at 127.0.0.1, a rebinding answer, macOS's inet_aton spellings.
// A loopback or link-local address is allowed only when it is the address
// the request's context says the SSDP packet came from; the unspecified
// address and an address that does not parse, never. The resolver's own
// connects to a DNS server do not come through here (net's Resolver dials
// with a Dialer of its own), so a stub resolver on 127.0.0.53 keeps working.
func refuseUnannouncedHostLocal(ctx context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("discovery dial check: %q: %w", address, err)
	}
	a := ap.Addr().Unmap()
	if !a.IsLoopback() && !a.IsUnspecified() && !a.IsLinkLocalUnicast() {
		return nil
	}
	from, _ := ctx.Value(announcementSourceKey{}).(netip.Addr)
	if a.IsUnspecified() || !from.IsValid() || a.WithZone("") != from {
		return errUnannouncedHostLocal
	}
	return nil
}

// NewDeviceFetchClient returns the http.Client both SSDP discovery clients
// fetch an announced device with, when their config names no Dispatcher
// (cmd/bridge names none). It follows no redirect (a 3xx comes back as
// itself, so a device cannot redirect the bridge anywhere), and every connect
// goes through refuseUnannouncedHostLocal. Three transport settings keep
// that check whole: no proxy, since through one the connect goes to the
// proxy and the check would judge the proxy's address (and refuse every
// fetch on a host whose HTTP_PROXY is on 127.0.0.1), while a LOCATION names
// a device on the link the packet arrived on, which a proxy cannot stand in
// for; no kept-alive connections, so a request never reuses a connection
// that another packet's source allowed; and no TLS dialer of its own, which
// would connect around the check. A manual upstream is fetched with a client
// of its own (internal/upnp's ManualPoller): its URL is the operator's
// choice, and pointing it at this machine is legitimate.
func NewDeviceFetchClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{ControlContext: refuseUnannouncedHostLocal}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
