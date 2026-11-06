package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// TestServeOnAHostWhoseNameIsNotASCII is B739 through the real bridge, on an
// install `bridge init` made on such a host: the host-name seams report the
// name, and the interface walk one LAN address. `bridge serve` starts twice
// on it. Each time the certificate it serves covers every host /v1/health
// sends a phone to, and the console's pairing link dials a host it covers,
// with the library named as init named it, for a reader. A name with no form
// is advertised nowhere and pairs at the LAN address. A name with non-ASCII
// characters takes its IDNA ASCII form, which nothing but the bridge's own
// mDNS responder answers, so with mDNS off its `.local` endpoint is not
// listed and it pairs at the LAN address too (B739's second round: the
// pairing link dialled https://xn--f1ai0a.local, which nothing answered).
// With mDNS on, the `.local` endpoint is listed and the pairing link dials
// it: the responder publishes that name (internal/mdns's tests resolve it
// through one), here pinned to loopback so it answers nothing on the LAN.
// Neither start logs the stale-SAN warning, the second presents the
// fingerprint the first did, since an existing pair is loaded and never
// minted again, and bridge doctor's certificate and pairing lines are quiet,
// the pairing line saying why the QR dials an address and not the name.
func TestServeOnAHostWhoseNameIsNotASCII(t *testing.T) {
	for _, tc := range []struct {
		name, host, library string
		mdns                bool
		// local is the host the `.local` endpoint names, "" for none,
		// primary the host the pairing link's URL dials, and why what
		// bridge doctor's pairing-url line says beside it, "" for nothing.
		local, primary, why string
	}{
		{"a Cyrillic name, mDNS off", hostCyrillic, hostCyrillic, false, "", hostNameWalkLAN, "mdns.enabled is false"},
		{"a Cyrillic name, mDNS on", hostCyrillic, hostCyrillic, true, "xn--f1ai0a.local", "xn--f1ai0a.local", ""},
		{"the test Mac's bytes", hostMacBytes, "ÐÄ¯U", false, "", hostNameWalkLAN, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Registered before the boots, so the cleanups that stop serve,
			// registered after them, run first.
			reportHostName(t, tc.host)
			reportInstallHostName(t, tc.host)
			rec := loggingtest.Record(t)
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			if code, out := loopbackInit(t, cfgDir, testLibrary(t), ""); code != 0 {
				t.Fatalf("bridge init exited %d:\n%s", code, out)
			}
			appendToConfig(t, cfgDir, fmt.Sprintf("mdns:\n  enabled: %t\n", tc.mdns))
			var pick func() (*net.Interface, error)
			if tc.mdns {
				pick = loopbackPick(t)
			}

			first := serveInstall(t, cfgDir, pick)
			first.assertAdvertisesWhatItCertifies(t, tc.local, tc.primary, tc.library)
			first.stop()
			second := serveInstall(t, cfgDir, pick)
			second.assertAdvertisesWhatItCertifies(t, tc.local, tc.primary, tc.library)
			if second.fingerprint != first.fingerprint {
				t.Errorf("the second start presents %s, the first %s: the pair was minted again",
					second.fingerprint, first.fingerprint)
			}
			if n := countLines(rec.All(), "cert SANs are stale", ""); n != 0 {
				t.Errorf("serve called a certificate it minted for this host stale %d time(s): %q", n, rec.All())
			}

			rep := doctor.Run(context.Background(), buildDoctorDeps(filepath.Join(cfgDir, "bridge.yaml")))
			if c := findCheck(t, rep, "tls-cert-sans"); c.Status != doctor.OK {
				t.Errorf("tls-cert-sans: %v %q\n%s", c.Status, c.Summary, c.Hint)
			}
			if c := findCheck(t, rep, "pairing-url"); c.Status != doctor.OK ||
				!strings.Contains(c.Summary, tc.primary) || !strings.Contains(c.Summary, tc.why) {
				t.Errorf("pairing-url: %v %q, want ok naming %s and saying %q", c.Status, c.Summary, tc.primary, tc.why)
			}
		})
	}
}

// installServe is a `bridge serve` serveInstall started on an install.
type installServe struct {
	// base is the API's https URL, admin the console's host:port, and
	// fingerprint the one the startup banner prints.
	base, admin, fingerprint string
	stderr                   *safeBuffer
	// stop shuts serve down and waits for it to return.
	stop func()
}

// serveInstall runs `bridge serve` on the install at cfgDir, on listeners of
// its own (the config's admin address moved onto the console's), its mDNS
// responder pinned by pick (nil: serve's own pick), and returns once the API
// and the console answer.
func serveInstall(t *testing.T, cfgDir string, pick func() (*net.Interface, error)) installServe {
	t.Helper()
	adminLn := holdLoopback(t)
	setConfigKey(t, cfgDir, "adminAddress", "adminAddress: "+adminLn.addr)
	// On every address, as an install a phone pairs with is: a listener bound
	// to loopback has no LAN address to pair at (lanPairingURL).
	lan, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lan.Close() })
	port := lan.Addr().(*net.TCPAddr).Port
	everyAddress := lan.Addr().String()
	setConfigKey(t, cfgDir, "listenAddress", `listenAddress: "`+everyAddress+`"`)
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{
			configPath: filepath.Join(cfgDir, "bridge.yaml"), addrOverride: everyAddress,
			lanListener: lan, adminListener: adminLn.ln, tailscaleCLI: noTailscaleCLI(),
			mdnsPick: pick,
		}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	_, fingerprint := waitForListening(t, stdout, exited, done, stderr)
	waitForAdminReady(t, adminLn.addr, done, stderr)
	return installServe{
		base: fmt.Sprintf("https://127.0.0.1:%d", port), admin: adminLn.addr, fingerprint: fingerprint, stderr: stderr,
		stop: func() {
			cancel()
			<-exited
		},
	}
}

// assertAdvertisesWhatItCertifies fails unless the certificate s serves is
// the one its banner names and covers every host /v1/health advertises, the
// `.local` endpoints are local's host alone (none for ""), the pairing link
// the console mints dials primary, a host the certificate covers, and both
// name the library library.
func (s installServe) assertAdvertisesWhatItCertifies(t *testing.T, local, primary, library string) {
	t.Helper()
	leaf := s.servedLeaf(t)
	if fp := servertls.FingerprintFromDER(leaf.Raw); fp != s.fingerprint {
		t.Errorf("serve presents %s and printed %s", fp, s.fingerprint)
	}

	resp, err := pairingPhone().Get(s.base + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, s.stderr.String())
	}
	defer resp.Body.Close()
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /v1/health: %v", err)
	}
	if health.LibraryName != library {
		t.Errorf("/v1/health names the library %q, want %q", health.LibraryName, library)
	}
	var locals []string
	for _, e := range health.Endpoints {
		h := urlHost(t, e)
		if strings.HasSuffix(h, ".local") {
			locals = append(locals, h)
		}
		assertCertifies(t, leaf, h)
	}
	if want := strings.Join(nonEmpty(local), ","); strings.Join(locals, ",") != want {
		t.Errorf("/v1/health's .local endpoints name %q, want %q", locals, want)
	}

	mint := pairViaAdmin(t, t.Context(), &http.Client{Timeout: 10 * time.Second},
		"http://"+s.admin+"/api/tokens", `{"name":"b739"}`, http.StatusCreated, s.stderr)
	items := linkQueryItems(t, mint.PairURL)
	if items["url"] != mint.URL {
		t.Errorf("the pairing link dials %q beside the console's %q", items["url"], mint.URL)
	}
	if h := urlHost(t, mint.URL); h != primary {
		t.Errorf("the pairing link dials %q, want the host %s", mint.URL, primary)
	} else {
		assertCertifies(t, leaf, h)
	}
	if items["name"] != library {
		t.Errorf("the pairing link names the library %q, want %q", items["name"], library)
	}
}

// servedLeaf is the certificate s presents on its API listener.
func (s installServe) servedLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp",
		strings.TrimPrefix(s.base, "https://"), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS to %s: %v", s.base, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatalf("%s presented no certificate", s.base)
	}
	return certs[0]
}

// loopbackPick is a pick that pins the bridge's mDNS responder to the host's
// loopback interface, so a serve test that leaves mDNS on answers nothing on
// the LAN. A host with no loopback interface skips the test: its responder
// would take the LAN's.
func loopbackPick(t *testing.T) func() (*net.Interface, error) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			return func() (*net.Interface, error) { return &iface, nil }
		}
	}
	t.Skip("no loopback interface to pin the mDNS responder to")
	return nil
}

// nonEmpty is s as a list: itself, or nothing for "".
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
