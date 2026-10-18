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

// heldBackLine is the line serve logs, once, for an address of the
// interface walk it does not advertise because its certificate does not
// name it.
const heldBackLine = "address not advertised: the TLS certificate does not name it"

// TestServeAdvertisesOnlyTheWalkAddressesItsCertificateNames is backlog
// B457. The operator bridge picked up an IPv6 ULA from a device on its LAN
// after its certificate was minted, and advertised it in /v1/health, where a
// client that checks the certificate against the URL's host cannot use it.
// The owner keeps the certificate (a rotation makes every phone re-pair), so
// serve publishes a walk address only while the certificate it serves names
// it, says so once, and `bridge doctor` does not call the certificate stale
// for it. `bridge cert rotate` and a restart publish it again.
//
// The certificate here is the one a mint would make now, less one address
// the walk publishes: the NUC's shape. The test needs a host with such an
// address (a CI runner's eth0 is one).
func TestServeAdvertisesOnlyTheWalkAddressesItsCertificateNames(t *testing.T) {
	walk := walkAddrsHealthPublishes(t)
	held := walk[0]
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := certSANOptions(&config.Config{})
	opts.ExtraIPs = withoutIP(opts.ExtraIPs, held)
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
			if containsIP(eps, held) {
				t.Errorf("/v1/health advertises %s, which the certificate does not name: %v", held, eps)
			}
			for _, ip := range walk[1:] {
				if !containsIP(eps, ip) {
					t.Errorf("/v1/health dropped %s, which the certificate names: %v", ip, eps)
				}
			}
		}
		// The pairing QR reads the same list, so it carries the address no
		// more than health does.
		mint := pairViaAdmin(t, t.Context(), &http.Client{Timeout: 10 * time.Second},
			"http://"+s.admin+"/api/tokens", `{"name":"b457"}`, http.StatusCreated, s.stderr)
		for _, alt := range mint.Alternates {
			if ip := urlIP(alt); ip != nil && ip.Equal(held) {
				t.Errorf("the pairing QR carries %s, which the certificate does not name: %v", held, mint.Alternates)
			}
		}
		lines := rec.All()
		if n := countLines(lines, heldBackLine, held.String()); n != 1 {
			t.Errorf("serve said %d time(s) that %s is not advertised, want once; lines=%q", n, held, lines)
		}
		if n := countLines(lines, "cert SANs are stale", held.String()); n != 0 {
			t.Errorf("serve called the certificate stale for %s, which it does not advertise; lines=%q",
				held, lines)
		}
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "tls-cert-sans")
		if c.Status != doctor.OK || !strings.Contains(c.Summary, held.String()) {
			t.Errorf("tls-cert-sans: status %v summary %q hint %q, want ok naming %s as not advertised",
				c.Status, c.Summary, c.Hint, held)
		}
	})
	if t.Failed() {
		return
	}

	if code := certRotateCmd([]string{"--config", cfgPath, "--yes"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("bridge cert rotate exited %d", code)
	}
	t.Run("a rotation and a restart advertise it again", func(t *testing.T) {
		rec := loggingtest.Record(t)
		s := bootPairingServeIn(t, dir, "")
		eps := healthEndpointIPs(t, s)
		for _, ip := range walk {
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

// walkAddrsHealthPublishes is every address /v1/health takes from this
// host's interface walk: a LAN or public class, never Tailscale's, which
// the provider publishes. It skips a host that has none.
func walkAddrsHealthPublishes(t *testing.T) []net.IP {
	t.Helper()
	var out []net.IP
	for _, e := range advertise.Endpoints(advertise.Params{Port: 7788}) {
		switch e.Class {
		case advertise.ClassLANv4, advertise.ClassLANv6, advertise.ClassPublic:
		default:
			continue
		}
		if ip := urlIP(e.URL); ip != nil {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		t.Skip("this host's interfaces hold no address /v1/health would publish")
	}
	return out
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
	var out []net.IP
	for _, raw := range health.Endpoints {
		if ip := urlIP(raw); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func urlIP(raw string) net.IP {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return net.ParseIP(u.Hostname())
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, x := range list {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

func withoutIP(list []net.IP, ip net.IP) []net.IP {
	var out []net.IP
	for _, x := range list {
		if !x.Equal(ip) {
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
