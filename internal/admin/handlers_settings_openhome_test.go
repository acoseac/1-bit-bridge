package admin

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// settingsCallbacks counts the hot-apply callbacks the settings PATCH runs
// once a save is stored (apiSettingsPatch, after its update).
type settingsCallbacks struct {
	mu                                          sync.Mutex
	mdns, tailscale, duplicates, cadence, sweep int
}

// wire hands srv the counting callbacks.
func (c *settingsCallbacks) wire(srv *Server) {
	count := func(n *int) {
		c.mu.Lock()
		defer c.mu.Unlock()
		*n++
	}
	srv.deps.MDNSToggle = func(bool) { count(&c.mdns) }
	srv.deps.TailscaleDisable = func() { count(&c.tailscale) }
	srv.deps.TriggerDuplicatesPass = func() bool { count(&c.duplicates); return true }
	srv.deps.TriggerCadenceRearm = func() { count(&c.cadence) }
	srv.deps.TriggerAutoOptimizeSweep = func() bool { count(&c.sweep); return true }
}

// counts are the callbacks run so far: mDNS, Tailscale, duplicates, cadence
// and the auto-optimize sweep.
func (c *settingsCallbacks) counts() [5]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return [5]int{c.mdns, c.tailscale, c.duplicates, c.cadence, c.sweep}
}

// TestASettingsSaveTheNextStartWouldRefuseIsRefused pins the settings PATCH
// against a listener address on the OpenHome surface's port while
// output.player.openhome is on (B612's second round): the next start would
// turn the player off over it (config.PlayerEnabled), so the save is
// refused, 409 openhome-port-clash naming the setting and the surface's
// port setting and never the port, whatever output.player.enabled says, and
// whole beside settings that would apply at once. Nothing is written: the
// file's bytes, the live config, what the Player page's status is read from
// (its pending entries are the provider's, from the live config it is
// handed) and the hot-apply callbacks after a save are as they were. The
// check is asked of the config the save would write, never of what serve
// binds now. With the opt-in off, or a port of the surface's own, the same
// address is saved, the callbacks beside it run.
func TestASettingsSaveTheNextStartWouldRefuseIsRefused(t *testing.T) {
	const besideLive = `"libraryName":"Renamed","mdnsEnabled":false,"scanIntervalSec":60,`
	for _, tc := range []struct {
		name, block, body, setting string
		// callbacks are the ones a save runs: mDNS and the cadence beside the
		// address, none for it alone.
		callbacks [5]int
	}{
		{"the API's address, the player on", "    enabled: true\n    openhome: true\n", `{"listenAddress":":7792"}`, "listenAddress", [5]int{}},
		{"the console's address, the player on", "    enabled: true\n    openhome: true\n", `{"adminAddress":"127.0.0.1:7792"}`, "adminAddress", [5]int{}},
		{"the API's address, the player off", "    openhome: true\n", `{"listenAddress":":7792"}`, "listenAddress", [5]int{}},
		{"the console's address, the player off", "    openhome: true\n", `{"adminAddress":"127.0.0.1:7792"}`, "adminAddress", [5]int{}},
		{"beside settings that apply at once", "    openhome: true\n", `{` + besideLive + `"listenAddress":":7792"}`, "listenAddress", [5]int{}},
		{"the opt-in off", "    enabled: true\n", `{"listenAddress":":7792"}`, "", [5]int{}},
		{"the opt-in off, the console's address", "    enabled: true\n", `{"adminAddress":"127.0.0.1:7792"}`, "", [5]int{}},
		{"the opt-in off, beside settings that apply at once", "    enabled: true\n", `{` + besideLive + `"listenAddress":":7792"}`, "", [5]int{1, 0, 0, 1, 0}},
		{"a port of the surface's own", "    openhome: true\n    openhomePort: 7800\n", `{"listenAddress":":7792"}`, "", [5]int{}},
		{"a port the kernel picks for the surface", "    openhome: true\n    openhomePort: 0\n", `{"adminAddress":"127.0.0.1:7792"}`, "", [5]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, cfgPath := newPlayerTestServer(t)
			writePlayerConfig(t, srv, cfgPath, "output:\n  player:\n"+tc.block)
			var calls settingsCallbacks
			calls.wire(srv)
			h := srv.Handler()
			want, held := readFileOrNil(cfgPath), srv.deps.CfgHolder.Load()

			code, body := playerRequest(t, h, http.MethodPatch, "/api/settings", tc.body, nil)
			if tc.setting == "" {
				if code != http.StatusOK {
					t.Fatalf("PATCH %s = %d: %s; want it saved", tc.body, code, body)
				}
				saved, err := config.Load(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				if saved.ListenAddress != ":7792" && saved.AdminAddress != "127.0.0.1:7792" {
					t.Errorf("PATCH %s answered 200 and the file holds %q and %q", tc.body, saved.ListenAddress, saved.AdminAddress)
				}
				if got := calls.counts(); got != tc.callbacks {
					t.Errorf("PATCH %s ran the callbacks %v; want %v", tc.body, got, tc.callbacks)
				}
				return
			}
			if code != http.StatusConflict || errorCode(body) != "openhome-port-clash" || !strings.Contains(body, tc.setting) ||
				!strings.Contains(body, "output.player.openhomePort") || strings.Contains(body, "7792") {
				t.Fatalf("PATCH %s = %d: %s; want 409 openhome-port-clash naming %s and the surface's port setting, not the port", tc.body, code, body, tc.setting)
			}
			if got := readFileOrNil(cfgPath); !bytes.Equal(got, want) {
				t.Errorf("a refused save wrote the file:\n%s", got)
			}
			if srv.deps.CfgHolder.Load() != held {
				t.Errorf("a refused save changed the live config")
			}
			if got := calls.counts(); got != tc.callbacks {
				t.Errorf("a refused save ran the callbacks %v", got)
			}
			var handed []*config.Config
			fake.eachReport(func(live *config.Config) { handed = append(handed, live) })
			if code, body := playerRequest(t, h, http.MethodGet, "/api/output/player", "", nil); code != http.StatusOK {
				t.Fatalf("GET /api/output/player = %d: %s", code, body)
			}
			if len(handed) != 1 || handed[0] != held {
				t.Errorf("after a refused save the Player page's status was read from another config than the one before it")
			}
		})
	}
}
