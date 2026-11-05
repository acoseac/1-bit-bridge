package api

import (
	"net"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestHealthAdvertisesAConvertedNameOnlyWhileMDNSIsOn is B739's second round
// on /v1/health's list, the one the pairing QR's urls= and the console's
// panel read too. A host name that is not ASCII is advertised in its IDNA
// ASCII form, which nothing on the LAN but the bridge's own mDNS responder
// answers, so its `.local` endpoint is listed only while the live config has
// mDNS on: with `mdns.enabled: false` a host named Жук listed
// `https://xn--f1ai0a.local:7788`, which no phone can resolve. A change of the
// setting is followed at the next walk. An ASCII name's `.local` endpoint is
// listed either way, as on main: the system's own responder answers it.
func TestHealthAdvertisesAConvertedNameOnlyWhileMDNSIsOn(t *testing.T) {
	flags := net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{
		{Name: "eth0", Flags: flags, Addrs: []string{"192.168.1.20"}},
	}))
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host string
		// on and off are the `.local` endpoints listed with mDNS on and off.
		on, off []string
	}{
		{"a Cyrillic name", "Жук", []string{"https://xn--f1ai0a.local:7788"}, nil},
		{"an ASCII name", "MacBook-Pro_2", []string{"https://MacBook-Pro_2.local:7788"},
			[]string{"https://MacBook-Pro_2.local:7788"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(advertise.SetHostProbeForTest(
				func() (string, error) { return tc.host, nil },
				func() bool { return false },
			))
			cfg := healthTestConfig(t)
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			on, off := true, false
			cfg.MDNS.Enabled = &off
			srv := New(cfg, store, nil, "AB:CD")
			locals := func() []string {
				var out []string
				for _, e := range srv.ReachableEndpoints() {
					if e.Class == advertise.ClassMDNSHost {
						out = append(out, e.URL)
					}
				}
				return out
			}
			if got := locals(); !slices.Equal(got, tc.off) {
				t.Errorf("with mDNS off the .local endpoints are %q, want %q", got, tc.off)
			}
			next := config.Clone(cfg)
			next.MDNS.Enabled = &on
			srv.cfgHolder.Store(next)
			if got := locals(); !slices.Equal(got, tc.on) {
				t.Errorf("with mDNS on the .local endpoints are %q, want %q", got, tc.on)
			}
		})
	}
}
