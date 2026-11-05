package main

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
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

// TestEveryNameTheBridgeAdvertisesIsOneItCertifies is B739's cross-check, for
// each host name B739's entry asks about, with mDNS on and with it off. A
// certificate minted now, from the gather init, serve, bridge cert rotate and
// bridge doctor share (certSANOptions), names every host the bridge sends a
// phone to: each endpoint the interface walk and the host's name give
// /v1/health, the `.local` one among them, the pairing QR's primary URL, and,
// while the responder runs, the host= and ips= of its TXT record. A name with
// no valid form is advertised nowhere: no `.local` endpoint, no host= (B739's
// second round: it was localhost.local) and an SRV target made of the
// responder's address, which nothing hands a phone to dial, so the phone is
// sent to the LAN address instead, never to localhost. A converted name is
// one only the bridge's responder answers, so with mDNS off it is advertised
// nowhere either. The want-set is the same either way, so turning mDNS off or
// on makes no certificate stale: the coverage check bridge doctor and the
// startup warning make finds nothing missing under either setting.
func TestEveryNameTheBridgeAdvertisesIsOneItCertifies(t *testing.T) {
	lan := "https://" + hostNameWalkLAN + ":7788"
	for _, tc := range []struct {
		name, host string
		// local is the host the `.local` endpoint and host= name with mDNS
		// on, "" for none; converted says the name is one only the bridge's
		// responder answers, which mDNS off advertises nowhere.
		local     string
		converted bool
	}{
		{"a Cyrillic name", hostCyrillic, "xn--f1ai0a.local", true},
		{"Latin with an umlaut", "müsik", "xn--msik-0ra.local", true},
		{"a Windows computer name with Japanese in it", "DESKTOP-日本", "xn--desktop--5y4qg6h.local", true},
		{"the test Mac's bytes", hostMacBytes, "", false},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2.local", false},
		{"a space", "my host", "", false},
		{"a label over 63 bytes", strings.Repeat("a", 64), "", false},
		{"a Unicode label over 63 bytes in its ASCII form", strings.Repeat("ж", 58), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reportHostName(t, tc.host)
			settings := []struct {
				name string
				mdns bool
			}{{"mDNS on", true}, {"mDNS off", false}}
			configured := func(mdns bool) *config.Config {
				cfg := &config.Config{ListenAddress: ":7788"}
				cfg.MDNS.Enabled = &mdns
				return cfg
			}
			for _, setting := range settings {
				t.Run(setting.name, func(t *testing.T) {
					cfg := configured(setting.mdns)
					certPath, keyPath := servertls.DefaultPaths(t.TempDir())
					if err := servertls.GenerateWithOptions(certPath, keyPath, certSANOptions(cfg)); err != nil {
						t.Fatalf("the mint refused: %v", err)
					}
					leaf := readCertificate(t, certPath)
					local := tc.local
					if tc.converted && !setting.mdns {
						local = ""
					}
					assertEveryHostIsCertified(t, leaf, cfg, tc.host, local, lan)

					for _, other := range settings {
						cov, err := servertls.InspectSANCoverage(certPath, certSANOptions(configured(other.mdns)))
						if err != nil {
							t.Fatal(err)
						}
						if !cov.Covered() {
							t.Errorf("the coverage check calls a certificate minted with %s stale with %s: missing %q and %q",
								setting.name, other.name, cov.MissingDNS, cov.MissingIPStrings())
						}
					}
				})
			}
		})
	}
}

// assertEveryHostIsCertified fails unless every host the bridge would send a
// phone to on cfg, on a host that reports the name reported, is one leaf
// certifies: the endpoints /v1/health lists, whose `.local` one names local
// alone (none for ""), the pairing QR's primary, which is local's URL, or lan
// where there is none, and never localhost, and, while cfg has mDNS on, what
// the responder publishes over the walk's address: host=, which is local, and
// each address of ips=. The SRV target is host='s name with its dot, or, with
// no host=, a name of the responder's own made of that address.
func assertEveryHostIsCertified(t *testing.T, leaf *x509.Certificate, cfg *config.Config, reported, local, lan string) {
	t.Helper()
	var locals []string
	for _, e := range advertise.Endpoints(advertise.Params{Port: 7788, MDNS: cfg.EffectiveMDNSEnabled()}) {
		h := urlHost(t, e.URL)
		if e.Class == advertise.ClassMDNSHost {
			locals = append(locals, h)
		}
		assertCertifies(t, leaf, h)
	}
	if !slices.Equal(locals, nonEmpty(local)) {
		t.Errorf("the .local endpoints name %q, want %q", locals, nonEmpty(local))
	}

	want := lan
	if local != "" {
		want = "https://" + local + ":7788"
	}
	primary := admin.DialURL(cfg)
	if primary != want {
		t.Errorf("the pairing QR dials %q, want %q", primary, want)
	}
	if h := urlHost(t, primary); h == "localhost" {
		t.Errorf("the pairing QR sends the phone to itself: %q", primary)
	} else {
		assertCertifies(t, leaf, h)
	}

	if !cfg.EffectiveMDNSEnabled() {
		return
	}
	// serve's responder reads the host's name itself (Config.Hostname
	// empty), where the walk's seam cannot reach: it is given here.
	rec := bridgemdns.RecordsFor(bridgemdns.Config{ProtocolVersion: 1, Port: 7788, Hostname: reported},
		[]net.IP{net.ParseIP(hostNameWalkLAN)})
	var hosts, ips []string
	for _, s := range rec.TXT {
		if v, ok := strings.CutPrefix(s, "host="); ok {
			hosts = append(hosts, v)
			assertCertifies(t, leaf, v)
		}
		if v, ok := strings.CutPrefix(s, "ips="); ok {
			ips = strings.Split(v, ",")
		}
	}
	if !slices.Equal(hosts, nonEmpty(local)) {
		t.Errorf("the TXT record %q carries host= %q, want %q", rec.TXT, hosts, nonEmpty(local))
	}
	if !slices.Equal(ips, []string{hostNameWalkLAN}) {
		t.Errorf("the TXT record %q carries ips= %q, want %s", rec.TXT, ips, hostNameWalkLAN)
	}
	for _, ip := range ips {
		assertCertifies(t, leaf, ip)
	}
	wantTarget := "onebit-bridge-" + strings.ReplaceAll(hostNameWalkLAN, ".", "-") + ".local."
	if local != "" {
		wantTarget = local + "."
	}
	if rec.Target != wantTarget {
		t.Errorf("the SRV target is %q, want %q", rec.Target, wantTarget)
	}
}

// TestAConfiguredNameStillRefusesBesideAConvertedHostName is
// TestAConfiguredNameTheCertificateCannotCarryStillRefuses on a host named
// Жук: the host's own name now mints in its ASCII form, and the configured
// endpoint whose host is not ASCII is what refuses the certificate, naming
// it, as it did beside an ASCII host name on main.
func TestAConfiguredNameStillRefusesBesideAConvertedHostName(t *testing.T) {
	assertAConfiguredNameRefuses(t, hostCyrillic)
}

// urlHost is the host of the URL raw, without its port.
func urlHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		t.Fatalf("%q is no URL a phone can dial: %v", raw, err)
	}
	return u.Hostname()
}

// assertCertifies fails unless leaf passes a client's check for host: one of
// its addresses for an IP address, its hostname check for a name.
func assertCertifies(t *testing.T, leaf *x509.Certificate, host string) {
	t.Helper()
	if ip := net.ParseIP(host); ip != nil {
		if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
			t.Errorf("the bridge sends a phone to %s, which the certificate does not name: %v", host, leaf.IPAddresses)
		}
		return
	}
	if err := leaf.VerifyHostname(host); err != nil {
		t.Errorf("the bridge sends a phone to %s, which the certificate does not cover: %v", host, err)
	}
}

// reportInstallHostName makes the host-name read firstInstallName makes
// (installHostname, the library name's own seam) answer host, for the rest of
// the test.
func reportInstallHostName(t *testing.T, host string) {
	t.Helper()
	prev := installHostname
	installHostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { installHostname = prev })
}

// TestTheDefaultLibraryNameIsTheHostsNameAsItReads is B739's library name. A
// first install takes the host's name for its library when the run names
// none, and that name is for a reader: a Cyrillic or Chinese one stays as it
// is, never in the IDNA ASCII form the certificate takes. Its control
// characters are removed (a test Mac's kern.hostname holds U+0091, which init
// printed in its prompt and wrote into the config), and a name that is
// nothing else is no name, which DefaultLibraryName stands in for. Both init
// paths take it, --yes and the prompt, where Enter takes it as offered, and
// config.Load serves what was saved as it was saved.
func TestTheDefaultLibraryNameIsTheHostsNameAsItReads(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	for _, tc := range []struct {
		name, host, want string
	}{
		{"a Cyrillic name", hostCyrillic, hostCyrillic},
		{"a Windows computer name with Japanese in it", "DESKTOP-日本", "DESKTOP-日本"},
		{"the test Mac's bytes", hostMacBytes, "ÐÄ¯U"},
		{"control characters alone", "\x01\x1b\u0085", config.DefaultLibraryName},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reportHostName(t, tc.host)
			reportInstallHostName(t, tc.host)
			if got := firstInstallName(); got != tc.want {
				t.Fatalf("firstInstallName = %q, want %q", got, tc.want)
			}

			t.Run("--yes", func(t *testing.T) {
				cfgDir := filepath.Join(t.TempDir(), "cfg")
				code, out := loopbackInit(t, cfgDir, testLibrary(t), "")
				defer logRunOnFailure(t, out)
				if code != 0 {
					t.Fatalf("bridge init exited %d", code)
				}
				assertSavedLibraryName(t, cfgDir, tc.want)
			})
			t.Run("the prompt", func(t *testing.T) {
				cfgDir := filepath.Join(t.TempDir(), "cfg")
				var stdout, stderr strings.Builder
				code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
					strings.NewReader(testLibrary(t)+"\n\n"), &stdout, &stderr)
				out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
				defer logRunOnFailure(t, out)
				if code != 0 {
					t.Fatalf("bridge init exited %d", code)
				}
				if offer := "Library display name [" + tc.want + "]"; !strings.Contains(out, offer) {
					t.Errorf("the prompt does not offer %q", offer)
				}
				for _, r := range tc.host {
					if unicode.IsControl(r) && strings.ContainsRune(out, r) {
						t.Errorf("the run printed the host name's control character %U", r)
					}
				}
				assertSavedLibraryName(t, cfgDir, tc.want)
			})
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
