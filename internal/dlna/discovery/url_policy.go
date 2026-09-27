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

import (
	"errors"
	"net/url"
	"strings"
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
//
// The one home for these rules: every service URL of every device kind goes
// through it, so the mandatory control URL and the optional ones cannot
// drift apart. The caller decides what a refusal costs: the service for a
// control URL, the URL alone for an eventSubURL. Mirrors
// DeviceDescriptionParser.resolveServiceURL in the iOS app.
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
	return abs.String(), nil
}
