// Package trackref is the one parser of what names one of the bridge's own
// tracks to its player (B470 PR 5a4): the URI of the track's /dlna/file/
// route, as an OpenHome controller inserts it, and the reference the
// player's engine keeps for an entry, that URI as it was sent or the bare
// track id serve's player and bridge output play wrote before it.
//
// A URI is taken only in its canonical form, byte for byte but for the case
// of a name:
//
//	"http://" + host + ":" + port + "/dlna/file/" + id
//
// where an IP host is netip.Addr.String() (an IPv6 literal in brackets, with
// no zone; an IPv4-mapped IPv6 address is no host), a name is ASCII letters,
// digits and hyphens in dot-separated labels, with no trailing dot and a
// last label that is not all digits, in any case, the port is decimal, 1 to
// 65535, with no leading zero, and id is 16 lower-case hexadecimal digits,
// the track id (dlna.TrackID with an empty root). So a scheme in capitals, an
// empty or zero-padded port, a zero-padded or abbreviated address, a
// trailing dot, user information, a query, a fragment, percent-encoding, a
// trailing slash and a rendition's path below the id are each refused, and
// nothing is ever resolved: the host is compared in its canonical form, by
// the caller (internal/player/openhome checks it against the hosts the bridge
// advertises and binds; the library resolver, at an occurrence's open, does
// not look at it, so a queue keeps playing after the bridge's address
// changes).
//
// A name is ASCII, asked of every byte of it before any is folded (B470 PR
// 5a4, round 2: CanonicalName): Unicode's lower case of U+0130 (İ) is i and
// of U+212A (the Kelvin sign) k, so a host lower-cased first by Unicode's
// rules read as an ASCII name it is not. An internationalized name is taken
// in its punycode form alone; the rest of the form (the scheme, an address,
// the port, the path) is ASCII by its grammar, so every URI taken is ASCII.
// URI.String is the canonical form, the URI with its name lower-cased byte by
// byte, in which two spellings of one URI compare. A URI taken differs from
// it in the ASCII case of its name alone (Parse folds nothing else: the
// scheme, the path and an address are matched exactly), so it names the same
// host and track as sent, and an Insert stores it as sent (B470 PR 5a4,
// round 3), which is what Read returns.
//
// The length is checked first: a string past MaxLength bytes, the engine's
// longest reference, is ErrTooLong whatever its shape.
package trackref

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// MaxLength is the longest string that can name a track: the engine's
// longest reference (engine.MaxReferenceBytes; TestMaxLengthIsTheEngines).
const MaxLength = 128

// The fixed parts of a canonical URI.
const (
	scheme     = "http://"
	filePrefix = "/dlna/file/" // dlna.FilePathPrefix (TestTheFilePrefixIsDLNAs)
	idLength   = 16
)

// The refusals. Each is wrapped with what is not so, which quotes nothing of
// the string.
var (
	// ErrTooLong is a string past MaxLength bytes.
	ErrTooLong = errors.New("trackref: longer than a track's reference")
	// ErrNotCanonical is a string that is not a track's canonical URI (or,
	// for ParseStored, its id).
	ErrNotCanonical = errors.New("trackref: not the canonical URI of a bridge track")
)

// URI is a track's canonical URI, parsed.
type URI struct {
	// Addr is the host when it is an IP literal (the zero Addr for a name).
	Addr netip.Addr
	// Name is the host when it is a name, in its canonical form: ASCII,
	// lower-cased (CanonicalName); empty for an IP literal.
	Name string
	// Port is the port the URI names.
	Port uint16
	// ID is the track id, 16 lower-case hexadecimal digits.
	ID string
}

// String is the URI's canonical form: the URI as it was sent, but for a name
// in capitals, which it writes lower-cased; Parse takes it back to the same
// URI. An Insert stores the URI as sent, not this (B470 PR 5a4, round 3).
func (u URI) String() string {
	host := u.Name
	if u.Addr.IsValid() {
		host = u.Addr.String()
		if u.Addr.Is6() {
			host = "[" + host + "]"
		}
	}
	return scheme + host + ":" + strconv.Itoa(int(u.Port)) + filePrefix + u.ID
}

// refused is ErrNotCanonical, saying what is not so.
func refused(why string) error { return fmt.Errorf("%w: %s", ErrNotCanonical, why) }

// Parse parses s as a track's canonical URI, and nothing else: a bare id is
// refused (an OpenHome Insert's Uri is a URI).
func Parse(s string) (URI, error) {
	if len(s) > MaxLength {
		return URI{}, fmt.Errorf("%w: %d bytes, more than %d", ErrTooLong, len(s), MaxLength)
	}
	rest, ok := strings.CutPrefix(s, scheme)
	if !ok {
		return URI{}, refused("it does not begin with http:// as written")
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return URI{}, refused("it has no path")
	}
	authority, path := rest[:slash], rest[slash:]
	id, ok := strings.CutPrefix(path, filePrefix)
	if !ok || !IsID(id) {
		return URI{}, refused("its path is not /dlna/file/ and a track id, with nothing after it")
	}
	if strings.ContainsAny(authority, "@%") {
		return URI{}, refused("its host carries user information or percent-encoding")
	}
	u := URI{ID: id}
	var port string
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return URI{}, refused("an IPv6 literal with no closing bracket")
		}
		lit, after := authority[1:end], authority[end+1:]
		addr, err := netip.ParseAddr(lit)
		switch {
		case err != nil, !addr.Is6():
			return URI{}, refused("a bracketed host that is no IPv6 address")
		case addr.Is4In6():
			return URI{}, refused("an IPv4-mapped IPv6 address")
		case addr.String() != lit:
			return URI{}, refused("an IPv6 address not in its canonical form")
		}
		p, ok := strings.CutPrefix(after, ":")
		if !ok {
			return URI{}, refused("no port after the address")
		}
		u.Addr, port = addr, p
	} else {
		colon := strings.LastIndexByte(authority, ':')
		if colon < 0 {
			return URI{}, refused("no port")
		}
		host := authority[:colon]
		port = authority[colon+1:]
		if addr, err := netip.ParseAddr(host); err == nil {
			// An IPv4 literal: ParseAddr refuses one with a zero-padded field,
			// and a host here holds no colon but the port's.
			if !addr.Is4() || addr.String() != host {
				return URI{}, refused("an address not in its canonical form")
			}
			u.Addr = addr
		} else {
			name, ok := CanonicalName(host)
			if !ok {
				return URI{}, refused("a host that is neither an address in its canonical form nor a name")
			}
			u.Name = name
		}
	}
	n, ok := portOf(port)
	if !ok {
		return URI{}, refused("a port that is empty, zero-padded or outside 1 to 65535")
	}
	u.Port = n
	return u, nil
}

// ParseStored parses s as an entry's reference and returns the track id it
// names: a canonical URI (Parse), whatever host and port it names, or a bare
// track id, the form serve's player and bridge output play wrote before PR
// 5a4.
func ParseStored(s string) (string, error) {
	if len(s) > MaxLength {
		return "", fmt.Errorf("%w: %d bytes, more than %d", ErrTooLong, len(s), MaxLength)
	}
	if IsID(s) {
		return s, nil
	}
	u, err := Parse(s)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// IsID reports whether s is a track id: 16 lower-case hexadecimal digits.
func IsID(s string) bool {
	if len(s) != idLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isASCII reports whether every byte of s is ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// CanonicalName is host as the canonical form writes a name, and whether it
// is one: ASCII, asked of every byte before any is folded, lower-cased byte
// by byte (never by Unicode's case mapping, which takes U+0130 to i and
// U+212A to k), and a name validName takes. An internationalized name in its
// Unicode form, or any other host that is not ASCII, is no name: its punycode
// form is. internal/player/openhome compares the names the bridge advertises
// through it too (B470 PR 5a4, round 2).
func CanonicalName(host string) (string, bool) {
	if !isASCII(host) {
		return "", false
	}
	b := []byte(host)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	name := string(b)
	if !validName(name) {
		return "", false
	}
	return name, true
}

// validName reports whether n, lower-cased, is a host name the canonical form
// takes: dot-separated labels of letters, digits and hyphens, each 1 to 63
// bytes and neither beginning nor ending with a hyphen, 253 bytes at most, no
// trailing dot, and a last label that is not all digits (so no spelling of an
// address that is not its canonical form, 127.1 or 192.168.001.020, reads as a
// name).
func validName(n string) bool {
	if n == "" || len(n) > 253 {
		return false
	}
	labels := strings.Split(n, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// portOf is a port as the canonical form writes it: decimal digits, no leading
// zero, 1 to 65535.
func portOf(p string) (uint16, bool) {
	if p == "" || len(p) > 5 || p[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return uint16(n), true
}
