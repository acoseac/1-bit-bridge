package admin

import (
	"net/netip"
	"strconv"
	"strings"
)

// lanAuthority is a Host's or an Origin's authority as parseLANAuthority
// reads it, canonical: an address, or a name in ASCII lower case without
// its trailing dot, and the port as a number.
type lanAuthority struct {
	addr netip.Addr
	name string
	port int
}

// lanDefaultPort is the port an authority with none, or an empty one, names:
// HTTP's, since the home-network socket speaks plain HTTP (backlog B989's D5).
const lanDefaultPort = 80

// parseLANAuthority reads raw, a Host header's value as it arrived or an
// Origin's after its exact "http://", as RFC 3986 writes an authority with
// no user information, and is the one parser both checks of the
// home-network socket read through (the plan's §4.2, F5), so a spelling is
// taken or refused alike by both. Every byte must be printable ASCII and
// none of @ / ? # % \ or a space, so a user, a path, a query, a fragment, an
// escape and a zone are refused; the host is an IPv6 literal in brackets
// (no zone, not IPv4-mapped: D3), an IPv4 literal netip.ParseAddr takes, or
// a name of letters, digits, - and _ in labels of 1 to 63 bytes with one
// trailing dot at most that does not read as digits and dots alone; the port
// is 1 to 65535 with no leading zero, and an empty one or none is 80 (D5).
func parseLANAuthority(raw string) (lanAuthority, bool) {
	if raw == "" {
		return lanAuthority{}, false
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b <= ' ' || b >= 0x7f || strings.IndexByte("@/?#%\\", b) >= 0 {
			return lanAuthority{}, false
		}
	}
	host, port := raw, ""
	if strings.HasPrefix(raw, "[") {
		end := strings.IndexByte(raw, ']')
		if end < 0 {
			return lanAuthority{}, false
		}
		host, port = raw[1:end], raw[end+1:]
		switch {
		case port == "":
		case port[0] == ':':
			port = port[1:]
			if port == "" {
				port = "80"
			}
		default:
			return lanAuthority{}, false
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return lanAuthority{}, false
		}
		p, ok := parseLANPort(port)
		if !ok {
			return lanAuthority{}, false
		}
		return lanAuthority{addr: a, port: p}, true
	}
	if strings.Count(raw, ":") > 1 {
		// An IPv6 literal without brackets.
		return lanAuthority{}, false
	}
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		host, port = raw[:i], raw[i+1:]
		if port == "" {
			port = "80"
		}
	}
	p, ok := parseLANPort(port)
	if !ok {
		return lanAuthority{}, false
	}
	if host == "" {
		return lanAuthority{}, false
	}
	if digitsAndDots(host) {
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is4() {
			return lanAuthority{}, false
		}
		return lanAuthority{addr: a, port: p}, true
	}
	name, ok := lanName(host)
	if !ok {
		return lanAuthority{}, false
	}
	return lanAuthority{name: name, port: p}, true
}

// parseLANPort is a port's digits, 1 to 65535 with no leading zero; "" is
// the default port.
func parseLANPort(s string) (int, bool) {
	if s == "" {
		return lanDefaultPort, true
	}
	if len(s) > 5 || s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

func digitsAndDots(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}

// lanName is a host name folded to ASCII lower case without its one
// trailing dot, or false for anything that is no host name.
func lanName(host string) (string, bool) {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return "", false
			}
		}
	}
	return strings.ToLower(host), true
}

// lanOriginAuthority is an Origin's authority, read through the one parser
// after its exact, lower-case "http://"; false for anything else, "null"
// and https among them.
func lanOriginAuthority(origin string) (lanAuthority, bool) {
	const prefix = "http://"
	if !strings.HasPrefix(origin, prefix) {
		return lanAuthority{}, false
	}
	return parseLANAuthority(origin[len(prefix):])
}
