package dlna

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// errMsgForeignHost is ownHostOnly's refusal body.
const errMsgForeignHost = "dlna refused: this server answers only to its own addresses " +
	"(the address in its SSDP LOCATION, or another address of this host)"

// hostRefusedSeenCap bounds the set noteForeignHost logs from. A renderer
// that sends a name it was never given is one entry; a page that rebinds a
// fresh name per request must not grow the set, or the journal, without
// limit.
const hostRefusedSeenCap = 16

// maxLoggedHostLen cuts a refused Host before it reaches the journal: the
// value is the requester's to choose.
const maxLoggedHostLen = 100

// ownHosts is what the listener's own names and addresses are known to be
// when it starts: the host of its ServerURL, of every advertise endpoint's
// LOCATION, and of a listen address pinned to one host. A name is kept
// folded (lowercase, no trailing dot); an address unmapped and without a
// zone.
type ownHosts struct {
	names map[string]struct{}
	addrs map[netip.Addr]struct{}
}

// knownOwnHosts gathers the ownHosts of this server's configuration.
func (s *Server) knownOwnHosts() ownHosts {
	known := ownHosts{names: map[string]struct{}{}, addrs: map[netip.Addr]struct{}{}}
	add := func(host string) {
		host = foldHostName(host)
		if host == "" {
			return
		}
		if a, err := netip.ParseAddr(host); err == nil {
			a = a.Unmap().WithZone("")
			if !a.IsUnspecified() {
				known.addrs[a] = struct{}{}
			}
			return
		}
		known.names[host] = struct{}{}
	}
	addURL := func(raw string) {
		if u, err := url.Parse(raw); err == nil {
			add(u.Hostname())
		}
	}
	addURL(s.cfg.ServerURL)
	for _, ep := range s.cfg.AdvertiseEndpoints {
		addURL(ep.ServerURL)
	}
	if host, _, err := net.SplitHostPort(s.cfg.ListenAddress); err == nil {
		add(host)
	}
	return known
}

// foldHostName is the form a host name is compared in: lowercase, without
// the root's trailing dot, as url.URL.Hostname leaves it (no port, no IPv6
// brackets).
func foldHostName(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// ownHostOnly refuses, with 421 Misdirected Request, a request whose Host
// names anything but this host (backlog B170).
//
// The listener has no authentication; what keeps it to the LAN is that it
// is reached on the LAN. A page a browser on that LAN loads from a name its
// author controls can re-point the name at this host's address (DNS
// rebinding): the browser then sends the page's requests here and hands the
// page the answers, because to the browser they are the page's own origin.
// That is a Browse of the whole library and a GET of any file, from a page
// on the internet. The name is what gives it away: the browser sends it in
// Host, and nothing the page can do changes that. It is also what the
// ContentDirectory builds every `<res>` and albumArtURI from, so an
// unchecked Host was handed back as the URL of every track.
//
// What passes, with any port or none:
//
//   - an EMPTY Host: every browser sends one, and an HTTP/1.0 renderer may
//     not (the ContentDirectory then builds its URLs on ServerURL);
//   - "localhost" and a loopback literal;
//   - the host of a LOCATION this server advertises, of its ServerURL, or of
//     a listen address pinned to one host, name or literal (knownOwnHosts):
//     what a renderer that found us by SSDP dials, and what our own DIDL
//     hands it;
//   - any other address of this host, asked of its interfaces at the
//     request (Server.interfaceAddrs): a wildcard listener answers on every
//     one, a Tailscale or second-subnet address included, and an address
//     that came up after Start is one too.
//
// A NAME not in that list is refused, this host's own included. A
// rebinding page's name is exactly such a name, and a name cannot be judged
// by resolving it, since resolving is what the page controls. The cost is a
// control point pointed at this server by a name by hand, or the iOS app's
// fallback that composes a renderer URL on the paired host when
// /v1/health.endpoints names no RFC 1918 address (BridgeDLNAURLResolver):
// both must use an address.
func (s *Server) ownHostOnly(next http.Handler) http.Handler {
	known := s.knownOwnHosts()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostIsOwn(r.Host, known) {
			s.noteForeignHost(r.Host)
			http.Error(w, errMsgForeignHost, http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsOwn reports whether a Host header value names this host, by
// ownHostOnly's list.
func (s *Server) hostIsOwn(hostport string, known ownHosts) bool {
	if hostport == "" {
		return true
	}
	name := foldHostName((&url.URL{Host: hostport}).Hostname())
	if name == "" {
		return false
	}
	if name == "localhost" {
		return true
	}
	if _, ok := known.names[name]; ok {
		return true
	}
	a, err := netip.ParseAddr(name)
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	if a.IsLoopback() {
		return true
	}
	if _, ok := known.addrs[a]; ok {
		return true
	}
	if a.IsUnspecified() {
		return false
	}
	return s.isInterfaceAddr(a)
}

// isInterfaceAddr reports whether a is assigned to one of this host's
// interfaces now. Asked only for a literal the configuration did not
// name, so a renderer on the advertised address never pays for it.
func (s *Server) isInterfaceAddr(a netip.Addr) bool {
	list := s.interfaceAddrs
	if list == nil {
		list = net.InterfaceAddrs
	}
	addrs, err := list()
	if err != nil {
		return false
	}
	for _, ia := range addrs {
		var ip net.IP
		switch v := ia.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if b, ok := netip.AddrFromSlice(ip); ok && b.Unmap().WithZone("") == a {
			return true
		}
	}
	return false
}

// noteForeignHost logs a Host ownHostOnly refused, once per name (bounded
// by hostRefusedSeenCap): how a renderer that dials a name, or a page that
// tried to reach the listener under one, shows up in the journal without a
// line per request.
func (s *Server) noteForeignHost(hostport string) {
	name := (&url.URL{Host: hostport}).Hostname()
	if name == "" {
		name = hostport
	}
	if len(name) > maxLoggedHostLen {
		name = strings.ToValidUTF8(name[:maxLoggedHostLen], "")
	}
	s.hostRefusedMu.Lock()
	if s.hostRefusedSeen == nil {
		s.hostRefusedSeen = make(map[string]struct{})
	}
	_, seen := s.hostRefusedSeen[name]
	full := len(s.hostRefusedSeen) >= hostRefusedSeenCap
	if !seen && !full {
		s.hostRefusedSeen[name] = struct{}{}
	}
	s.hostRefusedMu.Unlock()
	if seen || full {
		return
	}
	s.log.Warn("DLNA refused a request that names another host",
		slog.String("host", name),
		slog.String("hint", "the DLNA listener answers only to this host's addresses; "+
			"point a control point or renderer at the address in its SSDP LOCATION"))
}
