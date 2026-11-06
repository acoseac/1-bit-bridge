package hostname

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// FuzzASCIIGivesANameEveryConsumerTakes holds ASCII's answer, for any name a
// system could report, to what its consumers need of it (backlog B739). A
// name it takes is ASCII and a host name, and taken again unchanged; an ASCII
// one is returned byte for byte, so no ASCII name is lowercased or refused for
// its underscore on the way; and the name and its `.local` name go into a
// certificate x509.CreateCertificate mints and whose hostname check passes
// for both, into the URL a phone dials (`https://<name>.local:<port>`, whose
// host reads back as written) and onto the wire of a DNS record, as the mDNS
// responder packs it. A name it refuses comes back empty.
func FuzzASCIIGivesANameEveryConsumerTakes(f *testing.F) {
	for _, seed := range []string{
		"Жук", "müsik", "DESKTOP-日本", "Ð\u0091Ä¯U", "MacBook-Pro_2", "MacBook-Pro", "my host",
		strings.Repeat("ж", 57), strings.Repeat("ж", 58), strings.Repeat("a", 64),
		"box.", "a..б", "Ｍａｃ", "\xff\xfe", "", "localhost",
	} {
		f.Add(seed)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, reported string) {
		name, ok := ASCII(reported)
		if !ok {
			if name != "" {
				t.Fatalf("ASCII(%q) refused the name and returned %q", reported, name)
			}
			return
		}
		if !isASCII(name) || !hostName(name) {
			t.Fatalf("ASCII(%q) = %q, which is not an ASCII host name", reported, name)
		}
		if isASCII(reported) && name != reported {
			t.Fatalf("ASCII(%q) = %q: an ASCII name is returned byte for byte", reported, name)
		}
		if again, ok := ASCII(name); again != name || !ok {
			t.Fatalf("ASCII(%q) = (%q, %v): a name it took is not taken again unchanged", name, again, ok)
		}

		short := strings.TrimSuffix(name, ".")
		if i := strings.IndexByte(short, '.'); i > 0 {
			short = short[:i]
		}
		local := short + ".local"
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "1-bit Bridge"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			DNSNames:     []string{name, local},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("a certificate naming %q does not mint: %v", name, err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("a certificate naming %q does not parse: %v", name, err)
		}
		for _, dialled := range []string{name, local} {
			// A name that reads as an IPv4 address (a host named 192.168.1.5)
			// is dialled as that address, which a client checks against the
			// certificate's addresses, the interface walk's, not its names.
			if net.ParseIP(dialled) != nil {
				continue
			}
			if err := leaf.VerifyHostname(dialled); err != nil {
				t.Fatalf("the certificate naming %q does not verify for %q: %v", name, dialled, err)
			}
		}
		u, err := url.Parse("https://" + local + ":7788")
		if err != nil || u.Hostname() != local {
			t.Fatalf("the URL of %q does not read back its host: %v", local, err)
		}
		buf := make([]byte, 512)
		if _, err := dns.PackDomainName(local+".", buf, 0, nil, false); err != nil {
			t.Fatalf("%q does not pack as a DNS name: %v", local, err)
		}
	})
}
