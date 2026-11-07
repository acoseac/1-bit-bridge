// Package hostname decides what the host name the system reports becomes
// wherever the bridge certifies or advertises it: the certificate's DNS
// names and the want-set they are graded against, the `.local` endpoint
// /v1/health publishes, the pairing QR's URL and the mDNS records (backlog
// B739). ASCII is the one decision, and every one of them goes through it.
//
// A host name is whatever os.Hostname reports, and nothing holds it to the
// rules of a DNS name: macOS reports kern.hostname, which `scutil --set
// HostName` sets to any bytes; Linux the kernel's nodename, which
// `hostname` sets to any bytes; Windows the computer's DNS host name, which
// may be Cyrillic or Chinese. x509.CreateCertificate refuses a DNS name that
// is not ASCII (an IA5String), so on a test Mac whose kern.hostname was
// "Ð\u0091Ä¯U" `bridge init` minted no certificate and installed nothing.
// Converting the certificate's name alone would have turned "cannot install"
// into "installs and cannot connect": a client checks the certificate
// against the name it dials, so the name the bridge advertises and the name
// it certifies have to be one name, decided here once.
//
// The names an operator configures (customEndpoints, the public domain) are
// not reported host names, and take Configured instead (backlog B744): an
// ASCII one is kept as the operator wrote it, and one that is not takes the
// same IDNA ASCII form, where it loads (config.Normalize) and where it is
// typed (`bridge init --domain`, the console's settings save). The mint
// converts no configured name, so one that reaches it without going through
// either still refuses the certificate.
package hostname

import (
	"strings"

	"golang.org/x/net/idna"
)

const (
	// maxLabel is the most bytes one DNS label holds (RFC 1035 2.3.4).
	// idna.Lookup does not check it, so a long enough Unicode label comes
	// back from ToASCII longer than a resolver or the mDNS responder can
	// put on the wire.
	maxLabel = 63
	// maxName is the most bytes a DNS name holds written without its
	// trailing dot (RFC 1035 3.1: 255 on the wire, two of them the
	// length of the first label and the root).
	maxName = 253
)

// ASCII returns the name the bridge certifies and advertises for reported, a
// host name as the system reports it, and false when reported has no form
// that can be: the bridge then has no host name, as when the name cannot be
// read.
//
//   - A name made of ASCII alone is returned as it is, byte for byte, when
//     it is a host name: dot-separated labels of 1 to 63 bytes, each of ASCII
//     letters, digits, hyphens and underscores, 253 bytes at most and one
//     trailing dot at most. Its case and an underscore are kept: idna.Lookup
//     lowercases a name and refuses an underscore, so an ASCII name never
//     goes through it, and a name that works today stays exactly as it is
//     (`MacBook-Pro_2` and its `MacBook-Pro_2.local`).
//   - A name with a byte outside ASCII takes its IDNA ASCII form,
//     idna.Lookup.ToASCII, the profile normalizeSNIHost uses for a server
//     name, held to the same label and length bounds, which Lookup does not
//     check: `Жук` is `xn--f1ai0a`, `müsik` `xn--msik-0ra`, `DESKTOP-日本`
//     `xn--desktop--5y4qg6h`.
//   - Any other name has no valid form: a control character, a space or
//     other punctuation, bytes that are not UTF-8, an empty label, a label
//     over 63 bytes, a name Lookup refuses (an underscore or a C1 control
//     beside a non-ASCII character among them), and the empty name.
//     A test Mac's kern.hostname, `Ð\u0091Ä¯U`, is one.
//
// What it returns for a name it takes, it returns unchanged when given back,
// since that is an ASCII host name.
func ASCII(reported string) (string, bool) {
	if isASCII(reported) {
		if !hostName(reported) {
			return "", false
		}
		return reported, true
	}
	name, err := idna.Lookup.ToASCII(reported)
	if err != nil || !hostName(name) {
		return "", false
	}
	return name, true
}

// Configured returns the form a host name the operator configured (a
// customEndpoints entry's host, autocert.domain) is certified and published
// in, and false when it has none (backlog B744). A host of ASCII alone is
// returned as it is, byte for byte, its case included: the URL parse and the
// checks that took it before decide it, as before. One with a byte outside
// ASCII takes the conversion ASCII gives a reported name: idna.Lookup's
// ToASCII, the lookup profile, which maps case (münchen.de and MÜNCHEN.DE are
// both xn--mnchen-3ya.de), held to a label's 63 bytes and a name's 253, which
// Lookup does not check. One Lookup refuses (an underscore or a leading
// hyphen beside a character that is not ASCII), or whose form passes those
// bounds, has none: x509 would refuse it as written, so every mint failed
// while it was configured.
func Configured(host string) (string, bool) {
	if isASCII(host) {
		return host, true
	}
	return ASCII(host)
}

// isASCII reports whether s holds no byte outside ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// hostName reports whether name is a host name a certificate's DNS name and
// a URL's host can both carry: dot-separated labels of 1 to maxLabel bytes,
// each of ASCII letters, digits, hyphens and underscores, maxName bytes at
// most without the one trailing dot it may end with. The underscore is not
// a host name's character, but Go's TLS name check takes it ("commonly found
// in deployments outside the WebPKI"), and hosts are named with one.
func hostName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > maxName {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > maxLabel {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !hostNameByte(label[i]) {
				return false
			}
		}
	}
	return true
}

// hostNameByte reports whether c may stand in a label of a host name.
func hostNameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}
