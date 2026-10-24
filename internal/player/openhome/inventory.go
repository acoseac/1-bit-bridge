package openhome

import (
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// DefaultTrackPort is the port 1-bit names in every URI of a bridge track it
// inserts, whatever dlna.listenAddress says (its BridgeDLNAURLResolver's
// defaultDLNAPort): the DLNA listener's default port.
const DefaultTrackPort = 7790

// Inventory is the hosts and ports an Insert's URI may name (Hosts): what
// 1-bit can derive, plus what the DLNA server itself hands other controllers
// (B470 PR 5a4). A host is one of the hosts in the endpoints /v1/health
// serves, its very snapshot (Advertised: the same provider and its cache, so
// an address that disappears stops being taken when that snapshot refreshes,
// and an interface listing that fails in part gives the partial set health
// shows), or an address the DLNA listener has bound now (Served); a port is
// one of Ports. Nothing is resolved: a name is compared as a string, both
// sides in their canonical form (trackref.CanonicalName: ASCII, asked before
// anything is folded, lower-cased byte by byte, never by Unicode's rules,
// which fold the Kelvin sign to k and a long s to s; B470 PR 5a4, round 2),
// so a name advertised in its Unicode form names no URI, and an address is
// compared by its value.
type Inventory struct {
	// Advertised is the endpoint URLs /v1/health serves now
	// (api.Server.HealthEndpoints); nil is none.
	Advertised func() []string
	// Served reports whether the DLNA listener takes a connection on the
	// local address a now (dlna.Server.AcceptsOn); nil is no DLNA listener.
	Served func(a netip.Addr) bool
	// Ports are the ports a URI may name: DefaultTrackPort and the port of
	// dlna.listenAddress.
	Ports []uint16
}

// Names reports whether u's host and port name this bridge now.
func (inv Inventory) Names(u trackref.URI) bool {
	if !slices.Contains(inv.Ports, u.Port) {
		return false
	}
	if u.Addr.IsValid() && inv.Served != nil && inv.Served(u.Addr) {
		return true
	}
	if inv.Advertised == nil {
		return false
	}
	for _, raw := range inv.Advertised() {
		p, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := p.Hostname()
		if a, err := netip.ParseAddr(host); err == nil {
			if u.Addr.IsValid() && a.Unmap() == u.Addr {
				return true
			}
			continue
		}
		if name, ok := trackref.CanonicalName(strings.TrimSuffix(host, ".")); ok && u.Name != "" && name == u.Name {
			return true
		}
	}
	return false
}
