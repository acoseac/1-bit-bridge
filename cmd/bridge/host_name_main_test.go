package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// The tests in this file use nothing B739 added, so each runs on main's code
// as well (with host_name_test.go's helpers as main's red commit has them),
// and says there what it says here: what B739 leaves exactly as main made it.

// TestAnASCIIHostNameIsCertifiedAndAdvertisedAsOnMain is the other half of
// B739: a plain ASCII host name never goes through idna.Lookup, which would
// lowercase `MacBook-Pro_2` and refuse it for its underscore. For such a host
// the certificate's DNS names and addresses, the endpoints /v1/health builds
// from the interface walk, and the pairing QR's primary URL are byte for byte
// what main makes; this test passes on main's code too.
func TestAnASCIIHostNameIsCertifiedAndAdvertisedAsOnMain(t *testing.T) {
	const host = "MacBook-Pro_2"
	reportHostName(t, host)
	cfg := &config.Config{ListenAddress: ":7788"}
	opts := certSANOptions(cfg)
	if opts.Hostname != host {
		t.Errorf("the certificate's want-set names the host %q, want %q", opts.Hostname, host)
	}
	certPath, keyPath := servertls.DefaultPaths(t.TempDir())
	if err := servertls.GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatal(err)
	}
	leaf := readCertificate(t, certPath)

	wantDNS := []string{"localhost", "MacBook-Pro_2", "MacBook-Pro_2.local"}
	for _, extra := range advertise.GatherCertSANDNS(advertise.CertSANConfig{}) {
		if !slices.Contains(wantDNS, extra) {
			wantDNS = append(wantDNS, extra)
		}
	}
	if !slices.Equal(leaf.DNSNames, wantDNS) {
		t.Errorf("the certificate names %q, want %q", leaf.DNSNames, wantDNS)
	}
	wantIPs := []string{"127.0.0.1", "::1", "0.0.0.0"}
	for _, ip := range advertise.GatherCertSANIPs(advertise.CertSANConfig{}) {
		if !slices.Contains(wantIPs, ip.String()) {
			wantIPs = append(wantIPs, ip.String())
		}
	}
	var gotIPs []string
	for _, ip := range leaf.IPAddresses {
		gotIPs = append(gotIPs, ip.String())
	}
	if !slices.Equal(gotIPs, wantIPs) || !slices.Contains(gotIPs, hostNameWalkLAN) {
		t.Errorf("the certificate names the addresses %q, want %q", gotIPs, wantIPs)
	}

	want := []advertise.Endpoint{
		{URL: "https://" + hostNameWalkLAN + ":7788", Class: advertise.ClassLANv4},
		{URL: "https://MacBook-Pro_2.local:7788", Class: advertise.ClassMDNSHost},
	}
	if got := advertise.Endpoints(advertise.Params{Port: 7788}); !slices.Equal(got, want) {
		t.Errorf("the endpoints are %v, want %v", got, want)
	}
	if got := admin.DialURL(cfg); got != "https://MacBook-Pro_2.local:7788" {
		t.Errorf("the pairing QR dials %q, want https://MacBook-Pro_2.local:7788", got)
	}
}

// TestAConfiguredNameTheCertificateCannotCarryStillRefuses: B739 changes only
// what the host's reported name becomes, and the mint converts nothing
// configured (tls.dnsNames converts the reported name alone). A configured
// name reaches it in the form the load gives it: since B744 every
// customEndpoints entry and autocert.domain loads in its IDNA ASCII form or
// not at all (TestAConfiguredNameThatIsNotASCIIIsCertifiedInItsIDNAForm). So
// a config never loaded, built in memory, whose customEndpoints hold a host
// that is not ASCII still refuses the mint, naming the host, with no
// certificate left behind: certifying another name than the one the bridge
// advertises would publish an endpoint the certificate does not cover. This
// test passes on main's code too;
// TestAConfiguredNameStillRefusesBesideAConvertedHostName is the same on a
// host whose own name B739 converts.
func TestAConfiguredNameTheCertificateCannotCarryStillRefuses(t *testing.T) {
	assertAConfiguredNameRefuses(t, "musicbox")
}

// assertAConfiguredNameRefuses fails unless, on a host named host, a config
// never loaded whose customEndpoints hold a host that is not ASCII hands the
// certificate's want-set that host as written, and the mint refuses it, naming
// it, with no certificate left behind.
func assertAConfiguredNameRefuses(t *testing.T, host string) {
	t.Helper()
	reportHostName(t, host)
	cfg := &config.Config{CustomEndpoints: []string{"https://café.example:7788"}}
	opts := certSANOptions(cfg)
	if !slices.Contains(opts.ExtraDNSNames, "café.example") {
		t.Fatalf("the certificate's want-set does not take the configured name as written: %q", opts.ExtraDNSNames)
	}
	certPath, keyPath := servertls.DefaultPaths(t.TempDir())
	err := servertls.GenerateWithOptions(certPath, keyPath, opts)
	if err == nil || !strings.Contains(err.Error(), "café.example") {
		t.Fatalf("the mint of a name it cannot carry answered %v, want a refusal naming café.example", err)
	}
	if _, err := servertls.Inspect(certPath); err == nil {
		t.Error("a refused mint left a certificate behind")
	}
}
