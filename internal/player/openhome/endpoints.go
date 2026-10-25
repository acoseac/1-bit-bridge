package openhome

import (
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// UDN is the Source's unique device name for the player's UUID id: what
// discovery advertises (its USNs and its registration) and GET /v1/outputs
// lists as the Source's (B470 PR 5b).
func UDN(id string) string { return "uuid:" + id }

// Authority is the host and port of a socket the surface bound at ap, as a
// URL's authority writes them: what every LOCATION the Source advertises
// names (sourceLocation), and what GET /v1/outputs lists as the Source's
// endpoints (Status.Endpoints, B470 PR 5b), so a client compares a
// discovered Source's LOCATION with them byte for byte. An IPv4 address,
// IPv4-mapped or not, is its dotted quad; an IPv6 one is bracketed, its zone
// percent-encoded as net/url encodes a URL's host (fe80::1%eth0 is
// [fe80::1%25eth0]); then the port. The surface binds IPv4 addresses alone
// today.
func Authority(ap netip.AddrPort) string {
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	u := url.URL{Scheme: "http", Host: ap.String()}
	return strings.TrimPrefix(u.String(), "http://")
}

// sourceLocation is the LOCATION the Source advertises for a socket bound at
// ap: its device description there.
func sourceLocation(ap netip.AddrPort) string {
	return "http://" + Authority(ap) + "/openhome/description.xml"
}

// Endpoints is the Authority of each socket the surface listens on, each
// once, in address order: one per bound address, each with the port the
// kernel picked for it when openhomePort is 0 (B470 PR 5b). Discovery
// registers bound listeners alone, so every LOCATION the Source advertises
// names one of them. A surface whose stop has begun (Stopped: every request
// refused, discovery withdrawn) lists none.
func (st Status) Endpoints() []string {
	if st.Stopped {
		return nil
	}
	bound := make([]netip.AddrPort, 0, len(st.Listening))
	for _, l := range st.Listening {
		bound = append(bound, netip.AddrPortFrom(l.Bound.Addr().Unmap(), l.Bound.Port()))
	}
	slices.SortFunc(bound, func(a, b netip.AddrPort) int { return a.Compare(b) })
	bound = slices.Compact(bound)
	out := make([]string, 0, len(bound))
	for _, ap := range bound {
		out = append(out, Authority(ap))
	}
	return out
}
