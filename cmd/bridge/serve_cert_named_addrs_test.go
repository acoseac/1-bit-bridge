package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// heldBackLine is the line serve logs, once, for an IPv6 address of the
// interface walk it does not advertise because its certificate does not
// name it.
const heldBackLine = "address not advertised: the TLS certificate does not name it"

// The walk the tests in this file hand the bridge
// (advertise.SetInterfacesForTest), so that they do not depend on the
// host's own addresses: two LAN IPv4 addresses, a ULA and a global IPv6
// address, on one physical interface.
const (
	walkLANNamed   = "192.168.1.20"
	walkLANUnnamed = "192.168.1.21"
	walkULA        = "fd00:db8:1::2"
	walkGlobal     = "2001:db8::2"
)

// TestServeHoldsBackOnlyTheIPv6AddressesItsCertificateDoesNotName is
// backlog B457, as the owner chose it on 2026-10-18 (option B). A deployed
// bridge picked up an IPv6 ULA from a device on its LAN after its
// certificate was minted, and advertised it in /v1/health, where
// a client that checks the certificate against the URL's host cannot use
// it. The certificate is kept (a rotation makes every phone re-pair), so
// serve holds back an IPv6 address of its interfaces that the certificate
// it minted does not name, and says so once. An IPv4 address is advertised
// whether or not the certificate names it: the app pins the fingerprint and
// does not check the host, and a new DHCP lease must not take the bridge's
// LAN address away. The certificate is stale for such an address, so the
// startup warning and `bridge doctor` say so. `bridge cert rotate` and a
// restart advertise every address again.
//
// The certificate is the one a mint would make now, less one IPv4 address
// and both IPv6 addresses of the walk.
func TestServeHoldsBackOnlyTheIPv6AddressesItsCertificateDoesNotName(t *testing.T) {
	// Registered before the boots, so the cleanup that stops each serve,
	// registered after it, runs first.
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{
		Name:  "en0",
		Flags: net.FlagUp,
		Addrs: []string{walkLANNamed, walkLANUnnamed, walkULA, walkGlobal},
	}}))
	heldV6 := []string{walkULA, walkGlobal}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := certSANOptions(&config.Config{})
	opts.ExtraIPs = withoutIPs(opts.ExtraIPs, walkLANUnnamed, walkULA, walkGlobal)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	if err := servertls.GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatal(err)
	}

	var cfgPath string
	t.Run("held back", func(t *testing.T) {
		// Recorded from before the boot, so the startup's own lines are in
		// it, and put back after serve has returned (registered first).
		rec := loggingtest.Record(t)
		s := bootPairingServeIn(t, dir, "")
		cfgPath = s.cfgPath
		for range 2 {
			eps := healthEndpointIPs(t, s)
			for _, ip := range []string{walkLANNamed, walkLANUnnamed} {
				if !containsIP(eps, ip) {
					t.Errorf("/v1/health dropped %s, an IPv4 address it advertises whatever the certificate names: %v",
						ip, eps)
				}
			}
			for _, ip := range heldV6 {
				if containsIP(eps, ip) {
					t.Errorf("/v1/health advertises %s, an IPv6 address the certificate does not name: %v", ip, eps)
				}
			}
		}
		// The pairing QR reads the same list.
		mint := pairViaAdmin(t, t.Context(), &http.Client{Timeout: 10 * time.Second},
			"http://"+s.admin+"/api/tokens", `{"name":"b457"}`, http.StatusCreated, s.stderr)
		alts := urlIPs(mint.Alternates)
		if !containsIP(alts, walkLANUnnamed) {
			t.Errorf("the pairing QR does not carry %s: %v", walkLANUnnamed, mint.Alternates)
		}
		for _, ip := range heldV6 {
			if containsIP(alts, ip) {
				t.Errorf("the pairing QR carries %s, which the certificate does not name: %v", ip, mint.Alternates)
			}
		}
		lines := rec.All()
		for _, ip := range heldV6 {
			if n := countLines(lines, heldBackLine, ip); n != 1 {
				t.Errorf("serve said %d time(s) that %s is not advertised, want once; lines=%q", n, ip, lines)
			}
		}
		if n := countLines(lines, heldBackLine, walkLANUnnamed); n != 0 {
			t.Errorf("serve held back %s, an IPv4 address; lines=%q", walkLANUnnamed, lines)
		}
		// Serve advertises the IPv4 address and the certificate does not
		// name it, so the startup warning names it, and no held-back one.
		if n := countLines(lines, "cert SANs are stale", walkLANUnnamed); n != 1 {
			t.Errorf("the startup warning named %s %d time(s), want once; lines=%q", walkLANUnnamed, n, lines)
		}
		for _, ip := range heldV6 {
			if n := countLines(lines, "cert SANs are stale", ip); n != 0 {
				t.Errorf("the startup warning called the certificate stale for %s, which serve does not advertise; "+
					"lines=%q", ip, lines)
			}
		}
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "tls-cert-sans")
		if c.Status != doctor.Warn || !strings.Contains(c.Hint, walkLANUnnamed) {
			t.Errorf("tls-cert-sans: status %v summary %q hint %q, want warn naming %s",
				c.Status, c.Summary, c.Hint, walkLANUnnamed)
		}
		for _, ip := range heldV6 {
			if strings.Contains(c.Hint, ip) {
				t.Errorf("tls-cert-sans calls the certificate stale for %s, which serve does not advertise: %q",
					ip, c.Hint)
			}
		}
	})
	if t.Failed() {
		return
	}

	if code := certRotateCmd([]string{"--config", cfgPath, "--yes"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("bridge cert rotate exited %d", code)
	}
	t.Run("a rotation and a restart advertise every address", func(t *testing.T) {
		rec := loggingtest.Record(t)
		s := bootPairingServeIn(t, dir, "")
		eps := healthEndpointIPs(t, s)
		for _, ip := range []string{walkLANNamed, walkLANUnnamed, walkULA, walkGlobal} {
			if !containsIP(eps, ip) {
				t.Errorf("/v1/health does not advertise %s after the rotation: %v", ip, eps)
			}
		}
		if n := countLines(rec.All(), heldBackLine, ""); n != 0 {
			t.Errorf("serve held back an address after the rotation; lines=%q", rec.All())
		}
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(s.cfgPath)), "tls-cert-sans")
		if c.Status != doctor.OK || strings.Contains(c.Summary, "not advertised") {
			t.Errorf("tls-cert-sans after the rotation: status %v summary %q", c.Status, c.Summary)
		}
	})
}

// healthEndpointIPs is the IP literals among the endpoints /v1/health
// advertises.
func healthEndpointIPs(t *testing.T, s pairingServe) []net.IP {
	t.Helper()
	resp, err := pairingPhone().Get(s.base + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, s.stderr.String())
	}
	defer resp.Body.Close()
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /v1/health: %v", err)
	}
	return urlIPs(health.Endpoints)
}

// urlIPs is the IP literals among the hosts of urls.
func urlIPs(urls []string) []net.IP {
	var out []net.IP
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func containsIP(list []net.IP, ip string) bool {
	want := net.ParseIP(ip)
	for _, x := range list {
		if x.Equal(want) {
			return true
		}
	}
	return false
}

func withoutIPs(list []net.IP, drop ...string) []net.IP {
	var out []net.IP
	for _, x := range list {
		kept := true
		for _, d := range drop {
			if x.Equal(net.ParseIP(d)) {
				kept = false
			}
		}
		if kept {
			out = append(out, x)
		}
	}
	return out
}

// countLines counts the lines holding msg and, when it is not empty, addr.
func countLines(lines []string, msg, addr string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, msg) && (addr == "" || strings.Contains(l, addr)) {
			n++
		}
	}
	return n
}
