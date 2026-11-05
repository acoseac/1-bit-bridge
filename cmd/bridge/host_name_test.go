package main

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
)

// The host names B739 is about: what the system reports on a host whose
// name is not ASCII. hostCyrillic is a Windows computer name or a Linux
// hostname in Cyrillic; hostMacBytes is a real test Mac's kern.hostname,
// which holds U+0091, a C1 control character, and has no valid form.
const (
	hostCyrillic = "Жук"
	hostMacBytes = "Ð\u0091Ä¯U"
)

// hostNameWalkLAN is the one address of the interface walk these tests hand
// the bridge (advertise.SetInterfacesForTest), a LAN IPv4 address on one
// physical interface, so that what they certify and advertise does not
// depend on the host's own addresses.
const hostNameWalkLAN = "192.168.1.20"

// reportHostName makes the bridge's host-name read (advertise's seam, which
// the certificate, the endpoints and the pairing QR read) answer host, outside
// a container, and the interface walk hold hostNameWalkLAN alone, for the rest
// of the test.
func reportHostName(t *testing.T, host string) {
	t.Helper()
	t.Cleanup(advertise.SetHostProbeForTest(
		func() (string, error) { return host, nil },
		func() bool { return false },
	))
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{
		Name:  "en0",
		Flags: net.FlagUp,
		Addrs: []string{hostNameWalkLAN},
	}}))
}

// TestInitOnAHostWhoseNameIsNotASCII is backlog B739. On a host whose name is
// not ASCII, `bridge init` failed and installed nothing: "TLS cert: generate
// TLS material: create cert: x509: "Ð\u0091Ä¯U" cannot be encoded as an
// IA5String", then "the config was NOT changed". The host's name went into the
// certificate as the system reported it, and x509.CreateCertificate refuses a
// DNS name that is not ASCII. A real test Mac shows it (its kern.hostname is
// hostMacBytes), and a Windows computer name in Cyrillic or Chinese takes the
// same path.
//
// The run ends in a config that loads and a certificate that parses. A name
// with non-ASCII characters is certified in its IDNA ASCII form, with its
// `.local` name; one with no valid form, as the Mac's, is no host name, and
// the certificate carries localhost and the addresses.
func TestInitOnAHostWhoseNameIsNotASCII(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		// fromHost is the DNS names the certificate carries for the host's
		// name, beside localhost.
		fromHost []string
	}{
		{"a Cyrillic name", hostCyrillic, []string{"xn--f1ai0a", "xn--f1ai0a.local"}},
		{"the test Mac's bytes", hostMacBytes, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reportHostName(t, tc.host)
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), "Test Library")
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("bridge init exited %d", code)
			}
			cfg := loadInstallConfig(t, cfgDir)
			certPath, _ := resolveCertPaths(cfg)
			leaf := readCertificate(t, certPath)

			want := append([]string{"localhost"}, tc.fromHost...)
			for _, extra := range advertise.GatherCertSANDNS(advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}) {
				if !slices.Contains(want, extra) {
					want = append(want, extra)
				}
			}
			if !slices.Equal(leaf.DNSNames, want) {
				t.Errorf("the certificate names %q, want %q", leaf.DNSNames, want)
			}
			for _, ip := range []string{"127.0.0.1", hostNameWalkLAN} {
				if !slices.ContainsFunc(leaf.IPAddresses, func(got net.IP) bool { return got.Equal(net.ParseIP(ip)) }) {
					t.Errorf("the certificate does not name %s: %v", ip, leaf.IPAddresses)
				}
			}
		})
	}
}

// readCertificate parses the first certificate in the PEM file at path, as
// every reader of the bridge's pair does.
func readCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			t.Fatalf("%s holds no certificate", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the certificate at %s does not parse: %v", path, err)
		}
		return leaf
	}
}
