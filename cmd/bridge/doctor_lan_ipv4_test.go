package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestTheDoctorAsksOnlyALoopbackBridgeForALANIPv4 is backlog B457's option
// B: `bridge doctor`'s tls-cert-sans warns when the bridge advertises no
// LAN IPv4 address, and both builders of its Deps, the doctor's own and
// `bridge init`'s preflight over the install that is there, ask it of a
// loopback bridge only. A public-mode bridge advertises its declared
// endpoints alone, never its interfaces, so it is not asked. The walk here
// holds one IPv6 ULA and no IPv4 address, and no certificate is minted
// yet, which the warning does not wait for.
func TestTheDoctorAsksOnlyALoopbackBridgeForALANIPv4(t *testing.T) {
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{
		Name:  "en0",
		Flags: net.FlagUp,
		Addrs: []string{walkULA},
	}}))
	for _, tc := range []struct {
		name   string
		public bool
	}{{"loopback", false}, {"public", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "Music")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") + "\n"
			if tc.public {
				body += "listenAddress: \"0.0.0.0:7788\"\nadminAddress: \"0.0.0.0:7789\"\n" +
					"deployment:\n  mode: public\n  adminTLSTerminatedByProxy: true\n" +
					"autocert:\n  domain: bridge.example.com\n"
			}
			cfgPath := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var preflight doctor.Deps
			if !withExistingInstallDeps(&preflight, cfgPath) {
				t.Fatalf("the %s fixture config did not load", tc.name)
			}
			for name, d := range map[string]doctor.Deps{
				"bridge doctor":    buildDoctorDeps(cfgPath),
				"init's preflight": preflight,
			} {
				c := findCheck(t, doctor.Run(context.Background(), d), "tls-cert-sans")
				asked := strings.Contains(c.Summary, "no LAN IPv4 address is advertised")
				if asked == tc.public || (asked && c.Status != doctor.Warn) {
					t.Errorf("%s over a %s bridge: status %v summary %q, want the no-LAN-IPv4 warn only on loopback",
						name, tc.name, c.Status, c.Summary)
				}
			}
		})
	}
}
