package main

import (
	"path/filepath"
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
// what the host's reported name becomes. A name the operator configured (a
// customEndpoints entry, here one whose host is not ASCII) is a name the
// bridge advertises as written, so it still refuses the certificate, as on
// main: leaving it out would publish an endpoint the certificate does not
// cover. The refusal names the configured host. This test passes on main's
// code too; TestAConfiguredNameStillRefusesBesideAConvertedHostName is the
// same on a host whose own name B739 converts.
func TestAConfiguredNameTheCertificateCannotCarryStillRefuses(t *testing.T) {
	assertAConfiguredNameRefuses(t, "musicbox")
}

// assertAConfiguredNameRefuses fails unless, on a host named host, a
// customEndpoints entry whose host is not ASCII loads, reaches the
// certificate's want-set as written, and makes `bridge cert rotate` refuse,
// naming it, with no certificate left behind.
func assertAConfiguredNameRefuses(t *testing.T, host string) {
	t.Helper()
	reportHostName(t, host)
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	appendYAML(t, cfgPath, "customEndpoints:\n  - https://café.example:7788\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.CustomEndpoints, "https://café.example:7788") {
		t.Fatalf("premise: the config keeps the configured endpoint: %q", cfg.CustomEndpoints)
	}
	if !slices.Contains(certSANOptions(cfg).ExtraDNSNames, "café.example") {
		t.Fatalf("the certificate's want-set does not take the configured name as written: %q",
			certSANOptions(cfg).ExtraDNSNames)
	}

	var stdout, stderr strings.Builder
	if code := certRotateCmd([]string{"--config", cfgPath, "--yes"}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("bridge cert rotate exited %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "café.example") {
		t.Errorf("the refusal does not name the configured host: %q", stderr.String())
	}
	if _, err := servertls.Inspect(filepath.Join(dir, "data", servertls.CertFileName)); err == nil {
		t.Error("a refused mint left a certificate behind")
	}
}
