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
//
// A URL checked once is dialled many times, and a NAME in it is resolved
// again at every dial (backlog B36, 2026-09-28): the upstream ingest's SOAP
// Browse and every byte fetch internal/upnpproxy makes for a routed track
// dial the cached ContentDirectory control URL's host for as long as it is
// cached. A peer that passed discovery with a name resolving to its own LAN
// address could answer 127.0.0.1 for it later, and both requests then
// reached the bridge's console (measured: the proxy relayed the console's
// 200). So what approved a local connect travels with the URL
// (DialApproval: the announcing packet's address, or the kind of host the
// operator's URL named) and every request to a device goes through the same
// dial check (NewDeviceTransport).
//
// No rule here lets a device's say-so, or any approval, lead the bridge to a
// cloud provider's metadata address (cloudMetadataAddrs), not even a packet's
// own: an SSDP source is not authenticated, and a peer on the link can send
// one from 169.254.169.254 (CodeRabbit on #1074).
//
// The app has the host-kind rules too since iOS #1998 (2026-09-29), in
// UPnPURLPolicy: hostKind(of:) and hostKindAllowed bound its
// resolveServiceURL for every source, location(_:announcedFrom:) is
// LocationFromSource rule for rule, and cloudMetadataAddresses holds the
// same 19 addresses as cloudMetadataAddrs. It has no twin of the dial check:
// URLSession offers no hook between resolving a name and connecting.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
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

// errServiceURLCloudMetadata is resolveServiceURL's refusal of a service URL
// on a cloud metadata address (cloudMetadataAddrs), whatever the source.
var errServiceURLCloudMetadata = errors.New("names a cloud metadata address, which no device serves on")

// hostKind says where a URL host can lead a connection, judged from the
// string alone.
type hostKind int

const (
	// hostElsewhere is an address on the LAN, a tailnet or the internet, or
	// a name the string cannot place. A name that resolves to this machine
	// is the dial check's (refuseUnapprovedHostLocal), not this one's.
	hostElsewhere hostKind = iota
	// hostThisMachine is a loopback or unspecified address (a connect to
	// either reaches this machine), or a localhost name (RFC 6761).
	hostThisMachine
	// hostLinkLocal is an IPv4 or IPv6 link-local address: a
	// zero-configuration device's, or a cloud VM's metadata service (whose
	// addresses are hostMetadata instead).
	hostLinkLocal
	// hostNumericSpelling ends in a number without being an IP literal.
	// The WHATWG URL standard reads such a host as IPv4, macOS's resolver
	// accepts it the way inet_aton does (127.1 and 2130706433 both reached a
	// loopback listener there), and Go's own resolver refuses it on Linux.
	// No device writes one, so it is refused wherever it appears.
	hostNumericSpelling
	// hostMetadata is an address a cloud provider serves instance metadata,
	// credentials or platform services on (cloudMetadataAddrs), link-local
	// or not. No device serves on one, so it is refused wherever it appears
	// and whatever approved the request, the announcing packet's own
	// address included.
	hostMetadata
)

// cloudMetadataAddrs are the addresses cloud providers serve instance
// metadata, credentials and platform services on, from each provider's own
// documentation (2026-09-28). A request the bridge sends one on a device's
// say-so can read a cloud VM's credentials, and no UPnP device serves on
// one, so every rule here refuses them (addrKind classifies them as
// hostMetadata): a LOCATION (LocationFromSource), a service URL
// (resolveServiceURL, for every source) and every connect (the dial check),
// whatever the approval. The approval is the case that needs this list: an
// SSDP source is not authenticated, a peer on the same L2 segment can send
// a packet FROM 169.254.169.254, and the same-address exception would
// approve exactly that address, for the description fetch and every later
// dial (CodeRabbit on #1074). Ten of them are not link-local at all, and
// were fetched on any device's say-so, since the other rules bound only
// this machine and the link: the IPv6 ones in fd00::/8 are unique-local
// addresses, Alibaba's 100.100.100.200 is in 100.64/10 (which a tailnet
// node can hold too, one address in four million), and Azure's
// 168.63.129.16 is a Microsoft public address.
//
// Exact addresses, never a range: a direct-cable renderer self-assigns an
// address anywhere in 169.254/16 or fe80::/10, and a /24 around
// 169.254.169.254 would refuse one such device in 254. The one list both
// the string check and the dial check read, through addrKind. Its twin is
// the iOS app's UPnPURLPolicy.cloudMetadataAddresses (iOS #1998), address
// for address: an address added to or dropped from one list belongs in the
// other's change too.
var cloudMetadataAddrs = func() map[netip.Addr]struct{} {
	m := make(map[netip.Addr]struct{})
	for _, s := range []string{
		// Instance metadata on AWS, Azure, Google Cloud, Oracle Cloud,
		// OpenStack, DigitalOcean, Hetzner, IBM Cloud, Linode and others.
		"169.254.169.254",
		"fd00:ec2::254",                    // AWS instance metadata (IPv6, Nitro)
		"169.254.169.253", "fd00:ec2::253", // AWS Route 53 Resolver
		"169.254.169.123", "fd00:ec2::123", // AWS Time Sync Service
		"169.254.170.2",                  // AWS ECS task metadata and credentials
		"169.254.170.23", "fd00:ec2::23", // AWS EKS Pod Identity credentials
		"fd20:ce::254",                 // Google Cloud metadata (IPv6-only instances)
		"fd00:c1::a9fe:a9fe",           // Oracle Cloud instance metadata (IPv6)
		"fe80::a9fe:a9fe",              // OpenStack and Linode metadata (IPv6)
		"fd00:a9fe:a9fe::1",            // Linode metadata (IPv6)
		"169.254.42.42", "fd00:42::42", // Scaleway metadata
		"169.254.0.23", "169.254.10.10", // Tencent Cloud metadata
		"100.100.100.200", // Alibaba Cloud metadata
		"168.63.129.16",   // Azure WireServer (the host's own endpoint)
	} {
		m[netip.MustParseAddr(s)] = struct{}{}
	}
	return m
}()

// isCloudMetadataAddr reports whether a, unmapped and without its zone, is
// one of cloudMetadataAddrs.
func isCloudMetadataAddr(a netip.Addr) bool {
	_, ok := cloudMetadataAddrs[a.Unmap().WithZone("")]
	return ok
}

// classifyHost reads a URL host (url.URL.Hostname, so an IPv6 literal comes
// without brackets and with its zone) and returns its kind and, for an IP
// literal, its address, unmapped. One trailing dot (the DNS root) is ignored.
func classifyHost(host string) (hostKind, netip.Addr) {
	h := strings.TrimSuffix(host, ".")
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		return addrKind(a), a
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

// addrKind is the kind of an address, which must be unmapped: a cloud
// metadata address first (cloudMetadataAddrs, link-local or not), then this
// machine for a loopback or unspecified address (a connect to either reaches
// this machine), link-local for an IPv4 or IPv6 link-local unicast address,
// and elsewhere for any other. classifyHost asks it about an IP literal in a
// URL and the dial check about the address a connect targets, so the string
// and the connect judge an address by one rule.
func addrKind(a netip.Addr) hostKind {
	switch {
	case isCloudMetadataAddr(a):
		return hostMetadata
	case a.IsLoopback() || a.IsUnspecified():
		return hostThisMachine
	case a.IsLinkLocalUnicast():
		return hostLinkLocal
	}
	return hostElsewhere
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
// always for a host elsewhere, never for a numeric spelling or a cloud
// metadata address, and for this machine or a link-local address only from a
// reference of the same kind. Mirrors UPnPURLPolicy.hostKindAllowed in the
// iOS app (iOS #1998).
func hostKindAllowed(k, ref hostKind) bool {
	switch k {
	case hostElsewhere:
		return true
	case hostNumericSpelling, hostMetadata:
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
//     being an IP address (errServiceURLNumericHost) or names a cloud
//     metadata address (errServiceURLCloudMetadata) is refused outright,
//     from a description at that very address too.
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
// DeviceDescriptionParser.resolveServiceURL in the iOS app, host-kind rule
// included since iOS #1998 (UPnPURLPolicy.hostKindAllowed, for every
// source). This docblock said the app had no host-kind rule until
// 2026-09-29: a claim about the other repo goes stale the day that repo
// merges the change (backlog B78).
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
	switch kind {
	case hostNumericSpelling:
		return "", errServiceURLNumericHost
	case hostMetadata:
		return "", errServiceURLCloudMetadata
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
// from a loopback address, a numeric spelling no device writes, and a cloud
// metadata address from any source, that address included. A packet with a
// loopback source was sent on this machine, whose processes can reach the
// console directly; one from a metadata address was spoofed, since the
// metadata service sends no SSDP. A name the string cannot place is kept:
// the default client's dial check judges the address it resolves to.
//
// Mirrors UPnPURLPolicy.location(_:announcedFrom:) in the iOS app (iOS
// #1998), which has no dial check behind it.
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
	case hostNumericSpelling, hostMetadata:
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

// DialApproval is what lets a request the bridge sends a UPnP device connect
// to this machine or a link-local address. The dial check
// (refuseUnapprovedHostLocal, in every NewDeviceTransport) refuses such a
// connect unless the request's context carries an approval that covers it
// (WithDialApproval). The zero DialApproval covers neither kind, so a
// request that carries none reaches other hosts only.
//
// Three things approve such a connect: the two that let a device's URL name
// such an address at all, and the peer a GENA callback came from:
//
//   - AnnouncedFrom: the SSDP packet the URL came from was sent from that
//     very address. It approves that address and no other, and never the
//     unspecified address, which is no packet's source.
//   - OperatorChose: the operator configured a URL whose host names this
//     machine or a link-local address. It approves every address of that
//     kind, as resolveServiceURL keeps a service URL of that kind from such
//     a description.
//   - SubscribedFrom: the GENA SUBSCRIBE whose CALLBACK names the URL came
//     from that very address (internal/dlna's initial NOTIFY). AnnouncedFrom's
//     rule, for a TCP source.
//
// A fourth form is for one request only, a manual upstream's own
// description fetch (ManualDescriptionFetch): every address but a metadata
// one, since the operator typed that URL in full.
//
// None approves a cloud metadata address (cloudMetadataAddrs), not even a
// peer's own: a peer on the link can send from 169.254.169.254.
//
// A URL is checked when it is found and dialled for as long as it is cached,
// and a name in it resolves again at every dial. So the approval is recorded
// beside the URL it came with (internal/upnp's ServerInfo.DialApproval) and
// carried to every later request that dials it: the ingest's SOAP Browse and
// every byte fetch of a routed track. Comparable, so a cache can store it and
// a test can compare it.
type DialApproval struct {
	// source is the approving peer's address (the announcing packet's, or
	// the SUBSCRIBE's), unmapped and without a zone, or the zero Addr for an
	// approval that came from no peer.
	source netip.Addr
	// chosen is the kind of host an operator's URL named: hostThisMachine
	// or hostLinkLocal, or hostElsewhere (the zero value) when it named
	// neither.
	chosen hostKind
	// manualDescription is ManualDescriptionFetch's approval: every address
	// but a cloud metadata one. Never recorded with a URL in a cache; it
	// travels with the one fetch it is for.
	manualDescription bool
}

// AnnouncedFrom is the approval an SSDP packet from src gives the URLs it
// leads to: a connect to this machine or a link-local address at src's own
// address only. A nil src, or one that holds no address, approves none.
func AnnouncedFrom(src *net.UDPAddr) DialApproval {
	return DialApproval{source: announcerAddr(src)}
}

// SubscribedFrom is the approval a GENA SUBSCRIBE from `from` gives the
// callback URL it names, which internal/dlna sends its initial NOTIFY to
// (backlog B39): a connect to this machine or a link-local address at that
// address only, as AnnouncedFrom gives an SSDP packet's LOCATION. from is
// compared unmapped and without its zone; the zero Addr approves none.
func SubscribedFrom(from netip.Addr) DialApproval {
	return DialApproval{source: from.Unmap().WithZone("")}
}

// OperatorChose is the approval the operator's configured URL gives a manual
// upstream (upnpUpstream.servers[].manualDescriptionURL): a connect to any
// address of the kind its host names when that is this machine or a
// link-local address, and to neither otherwise. A URL on a cloud metadata
// address approves nothing, since no media server is one. A NAME approves
// no such address, whatever it resolves to: the operator chose the name, not
// an answer for it that another host on the LAN can give (anyone can answer
// an mDNS query), and a name answered with 127.0.0.1 at a later dial is
// exactly the rebinding the check exists for. So a manual upstream on this
// machine keeps its local services when its URL names localhost or a
// loopback address, and not when it names this host by its host name.
func OperatorChose(rawURL string) DialApproval {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return DialApproval{}
	}
	switch kind, _ := classifyHost(u.Hostname()); kind {
	case hostThisMachine, hostLinkLocal:
		return DialApproval{chosen: kind}
	}
	return DialApproval{}
}

// ManualDescriptionFetch is the approval of a manual upstream's own
// description fetch (internal/upnp's ManualPoller; backlog B54): a connect
// to any address but a cloud metadata one. The operator typed that URL's
// host, path and port, and what it returns is parsed and never relayed, so
// #1069 left it without a dial check, and a URL on this machine, or a name
// that resolves to it, is legitimate there. That left it the one request to
// a device #1074's metadata rule did not reach: every later dial of the
// server it finds runs under OperatorChose, which approves no metadata
// address. So this approves what the fetch reached before, less the
// metadata addresses, whatever the URL names: a literal (which the poller
// refuses before any fetch, NamesCloudMetadataAddr) or a name that resolves
// to one (metadata.google.internal, AWS's instance-data), which only the
// dial check can see. No media server serves on one.
func ManualDescriptionFetch() DialApproval {
	return DialApproval{manualDescription: true}
}

// NamesCloudMetadataAddr reports whether rawURL's host is a cloud metadata
// address written as an IP literal (cloudMetadataAddrs, in any spelling a
// URL can carry one: bracketed, zoned, IPv4-mapped, a trailing dot). A NAME
// answers false: what it resolves to is the dial check's to judge. The
// manual upstream's poller asks it before any fetch, and the console asks
// it of a manual URL an operator types.
func NamesCloudMetadataAddr(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	kind, _ := classifyHost(u.Hostname())
	return kind == hostMetadata
}

// String names what the approval covers, for a log line or a test failure.
func (d DialApproval) String() string {
	switch {
	case d.manualDescription:
		return "an operator's description URL, any address but a cloud metadata one"
	case d.chosen == hostThisMachine:
		return "an operator's URL on this machine"
	case d.chosen == hostLinkLocal:
		return "an operator's link-local URL"
	case d.source.IsValid():
		return "announced from " + d.source.String()
	}
	return "no local address"
}

// Permits reports whether the approval lets a connect reach a, the address a
// dial resolved to: never for a cloud metadata address; always for any other
// address under ManualDescriptionFetch, and for any other address elsewhere
// under every approval; and for this machine or a link-local address when an
// operator's URL named that kind of host, or when a is the approving peer's
// own address (never the unspecified address). The dial check asks it at
// every connect, and internal/dlna's GENA callback guard asks it before it
// sends anything, so the two cannot disagree.
func (d DialApproval) Permits(a netip.Addr) bool {
	a = a.Unmap()
	kind := addrKind(a)
	if kind == hostMetadata {
		return false
	}
	if d.manualDescription {
		return true
	}
	if hostKindAllowed(kind, d.chosen) {
		return true
	}
	return !a.IsUnspecified() && d.source.IsValid() && a.WithZone("") == d.source
}

// dialApprovalKey carries a request's DialApproval in its context, for the
// dial check.
type dialApprovalKey struct{}

// WithDialApproval returns ctx carrying a, the approval of the URL a request
// is sent to. The dial check of every NewDeviceTransport reads it at each
// connect.
func WithDialApproval(ctx context.Context, a DialApproval) context.Context {
	return context.WithValue(ctx, dialApprovalKey{}, a)
}

// WithAnnouncementSource returns ctx carrying AnnouncedFrom(src): the
// approval of the SSDP packet from src whose LOCATION a fetch follows. Both
// SSDP clients wrap every fetch a packet causes (the description, and a
// renderer's GetProtocolInfo) in it.
func WithAnnouncementSource(ctx context.Context, src *net.UDPAddr) context.Context {
	return WithDialApproval(ctx, AnnouncedFrom(src))
}

// errUnapprovedHostLocal is the dial check's refusal of a connect to this
// machine or a link-local address that the request's approval does not
// cover.
var errUnapprovedHostLocal = errors.New("refusing to connect to this machine or a link-local address " +
	"that neither the peer the URL came from (an SSDP packet, a GENA SUBSCRIBE) nor the operator's configured URL named")

// ErrCloudMetadataAddr is the dial check's refusal of a connect to a cloud
// metadata address, which no approval covers. Exported so a caller can tell
// that refusal from a device that did not answer: the manual upstream's
// poller warns about it, where a fetch that merely failed is a Debug line.
var ErrCloudMetadataAddr = errors.New("refusing to connect to a cloud metadata address, which no device serves on")

// refuseUnapprovedHostLocal is the ControlContext of every NewDeviceTransport
// dialer. net passes it the address each connect attempt targets, after name
// resolution (every A and AAAA answer is its own attempt), so it judges what
// a name RESOLVED to, which no check of the URL can: a public DNS name
// pointed at 127.0.0.1, a rebinding answer, macOS's inet_aton spellings. A
// loopback, unspecified or link-local address is allowed only when the
// DialApproval in the request's context permits it; a cloud metadata
// address and an address that does not parse, never. The resolver's own
// connects to a DNS server do not come through here (net's Resolver dials
// with a Dialer of its own), so a stub resolver on 127.0.0.53, or Azure's
// on 168.63.129.16, keeps working.
func refuseUnapprovedHostLocal(ctx context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("device dial check: %q: %w", address, err)
	}
	approval, _ := ctx.Value(dialApprovalKey{}).(DialApproval)
	if approval.Permits(ap.Addr()) {
		return nil
	}
	if isCloudMetadataAddr(ap.Addr()) {
		return ErrCloudMetadataAddr
	}
	return errUnapprovedHostLocal
}

// resolverForTest, when set, is the resolver every NewDeviceTransport dial
// resolves a name with, in place of net.DefaultResolver. Only
// UseResolverForTest sets it.
var resolverForTest atomic.Pointer[net.Resolver]

// UseResolverForTest makes every dial through a NewDeviceTransport resolve
// names with r, until the returned function restores the resolver in place
// before. Tests only, and production code must never call it: it lets a test
// make a name answer one address and then another (internal/dnstest)
// without replacing net.DefaultResolver, which any goroutine in the process
// reads with no synchronisation.
func UseResolverForTest(r *net.Resolver) (restore func()) {
	prev := resolverForTest.Swap(r)
	return func() { resolverForTest.Store(prev) }
}

// NewDeviceTransport returns the http.Transport for every request the bridge
// sends a UPnP device, at a URL a device or the operator's configuration
// supplied: the discovery clients' description and GetProtocolInfo fetches
// (NewDeviceFetchClient), a manual upstream's description fetch
// (internal/upnp's ManualPoller), the upstream ingest's SOAP Browse, and
// every byte fetch internal/upnpproxy makes for a routed track. d is the dialer
// template (timeouts, TCP keep-alive), and its ControlContext is replaced by
// the dial check, which judges every connect against the DialApproval the
// request's context carries.
//
// Three settings keep that check whole, and a caller must not undo them. No
// proxy: through one the connect goes to the proxy, so the check would judge
// the proxy's address (and refuse every request on a host whose HTTP_PROXY
// is on 127.0.0.1), while a device is on the link, which a proxy cannot stand
// in for. No kept-alive connections: a pooled connection carries the next
// request to the same host:port without a dial, so without a check (measured
// on upnpproxy's old pool: a fetch approved only for a LAN address rode an
// idle connection to 127.0.0.1), and net/http also hands a connection it
// dialed for one request to another that is waiting. And no TLS dialer of
// its own, which would connect around the check.
func NewDeviceTransport(d net.Dialer) *http.Transport {
	d.ControlContext = refuseUnapprovedHostLocal
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if r := resolverForTest.Load(); r != nil {
				withResolver := d
				withResolver.Resolver = r
				return withResolver.DialContext(ctx, network, address)
			}
			return d.DialContext(ctx, network, address)
		},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// NewDeviceFetchClient returns an http.Client over NewDeviceTransport that
// follows no redirect (a 3xx comes back as itself, so a device cannot
// redirect the bridge anywhere), with timeout bounding each request. Both
// SSDP discovery clients fetch an announced device with it when their config
// names no Dispatcher (cmd/bridge names none), the upstream ingest sends its
// SOAP with it, and internal/dlna sends its GENA initial NOTIFY with it
// (backlog B39: that NOTIFY once followed a callback's redirect anywhere, the
// bridge's own console included). A manual upstream's description is
// fetched with a client of its own (internal/upnp's ManualPoller) over the
// same transport, under ManualDescriptionFetch: its URL is the operator's
// choice, so a URL on this machine is legitimate there, and only a cloud
// metadata address is refused (backlog B54; until then that fetch had no
// dial check at all).
func NewDeviceFetchClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewDeviceTransport(net.Dialer{}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
