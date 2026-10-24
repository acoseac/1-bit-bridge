package config

import (
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestTheOpenHomeSurfaceIsOffByDefault: a player block that names neither
// output.player.openhome nor openhomePort is a player whose OpenHome
// surface is off, on port 7792 were it on: the port after the API's
// (7788), the console's (7789), the DLNA listener's (7790) and the
// loopback track route's (7791).
func TestTheOpenHomeSurfaceIsOffByDefault(t *testing.T) {
	cfg, err := Load(writePlayerConfig(t, "  player:\n    enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Output.Player
	if p.OpenHome || p.EffectiveOpenHomePort() != 7792 || DefaultOpenHomePort != 7792 {
		t.Errorf("openhome %v on port %d (default %d); want off, 7792", p.OpenHome, p.EffectiveOpenHomePort(), DefaultOpenHomePort)
	}
	for _, other := range []int{7788, 7789, 7790, 7791} {
		if DefaultOpenHomePort == other {
			t.Errorf("the default OpenHome port is %d, which serve binds already", other)
		}
	}
	if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
		t.Errorf("PlayerEnabled = %v, %v", on, why)
	}
}

// TestTheOpenHomeOptInLoadsAndRoundTrips: openhome and openhomePort read,
// a save writes them back as they were written, and a block that names
// neither is saved naming neither. A clone shares no port with the config
// it copies.
func TestTheOpenHomeOptInLoadsAndRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		on          bool
		port        int
		saved       []string
	}{
		{"on, the default port", "    openhome: true\n", true, 7792, []string{"openhome: true"}},
		{"on, a port of its own", "    openhome: true\n    openhomePort: 7800\n", true, 7800, []string{"openhome: true", "openhomePort: 7800"}},
		{"on, a port the kernel picks", "    openhome: true\n    openhomePort: 0\n", true, 0, []string{"openhome: true", "openhomePort: 0"}},
		{"off, a port written", "    openhome: false\n    openhomePort: 7800\n", false, 7800, []string{"openhomePort: 7800"}},
		{"neither", "", false, 7792, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePlayerConfig(t, "  player:\n    enabled: true\n"+tc.block)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Output.Player
			if p.OpenHome != tc.on || p.EffectiveOpenHomePort() != tc.port {
				t.Fatalf("loaded openhome %v on port %d, want %v on %d", p.OpenHome, p.EffectiveOpenHomePort(), tc.on, tc.port)
			}
			if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
				t.Fatalf("PlayerEnabled = %v, %v", on, why)
			}
			if err := saveAt(cfg, path); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.saved {
				if !strings.Contains(string(b), s) {
					t.Errorf("the save lacks %q:\n%s", s, b)
				}
			}
			if tc.saved == nil && strings.Contains(string(b), "openhome") {
				t.Errorf("a save wrote an OpenHome setting the block did not name:\n%s", b)
			}
			again, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if q := again.Output.Player; q.OpenHome != tc.on || q.EffectiveOpenHomePort() != tc.port {
				t.Errorf("after a save: openhome %v on port %d", q.OpenHome, q.EffectiveOpenHomePort())
			}
			clone := Clone(cfg)
			if clone.Output.Player.OpenHomePort != nil {
				*clone.Output.Player.OpenHomePort = 9999
				if cfg.Output.Player.EffectiveOpenHomePort() != tc.port {
					t.Errorf("a clone's port is the config's: changing it moved the config's to %d", cfg.Output.Player.EffectiveOpenHomePort())
				}
			}
		})
	}
}

// TestAnOpenHomeSettingIsValidatedLikeItsNeighbours: a value that does not
// read (a word where the switch or the port goes, a list) leaves the block
// unread, and a port outside 0 to 65535 is refused by name; either turns
// the player off, with a reason that names the setting and quotes no value,
// and never fails the load. (A fraction reads as its whole part, as it does
// on every whole-number line of the block: yaml.v3 truncates a float into an
// int.)
func TestAnOpenHomeSettingIsValidatedLikeItsNeighbours(t *testing.T) {
	for _, tc := range []struct {
		name, block, reason string
	}{
		{"a word for the switch", "    openhome: maybe\n", "output.player.openhome (line"},
		{"a word for the port", "    openhome: true\n    openhomePort: " + pastedWord + "\n", "output.player.openhomePort (line"},
		{"a list for the port", "    openhome: true\n    openhomePort: [7792]\n", "output.player.openhomePort (line"},
		{"a negative port", "    openhome: true\n    openhomePort: -1\n", "output.player.openhomePort: must be a port"},
		{"a port past 65535", "    openhome: true\n    openhomePort: 65536\n", "output.player.openhomePort: must be a port"},
		{"a serial of digits for the port", "    openhome: true\n    openhomePort: " + pastedDigits + "\n", "output.player.openhomePort: must be a port"},
		{"a port out of range with the surface off", "    openhomePort: 65536\n", "output.player.openhomePort: must be a port"},
		{"port 1", "    openhome: true\n    openhomePort: 1\n", ""},
		{"port 65535", "    openhome: true\n    openhomePort: 65535\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writePlayerConfig(t, "  player:\n    enabled: true\n"+tc.block))
			if err != nil {
				t.Fatalf("an OpenHome setting stopped the load: %v", err)
			}
			on, why := cfg.PlayerEnabled("linux")
			if tc.reason == "" {
				if !on || why != nil {
					t.Fatalf("PlayerEnabled = %v, %v; want on", on, why)
				}
				return
			}
			if on || why == nil || !strings.Contains(why.Error(), tc.reason) {
				t.Fatalf("PlayerEnabled = %v, %v; want off, naming %q", on, why, tc.reason)
			}
			for _, value := range []string{pastedWord, pastedDigits, "65536", "maybe"} {
				if strings.Contains(why.Error(), value) {
					t.Errorf("the reason quotes the value %s: %v", value, why)
				}
			}
		})
	}
}

// TestAnOpenHomePortServeBindsElsewhereTurnsThePlayerOff: with the surface
// on, a port another listener of serve binds is refused, naming that
// listener's setting and never the port: the API's (listenAddress), the
// console's (adminAddress), the DLNA listener's (dlna.listenAddress, its
// default included and whether or not DLNA is on) and the loopback track
// route's (output.loopbackPort, its default included). Port 0, a port the
// kernel picks, clashes with nothing, and with the surface off the port is
// never bound, so it is no refusal. Validate logs the refusal once.
func TestAnOpenHomePortServeBindsElsewhereTurnsThePlayerOff(t *testing.T) {
	intp := func(n int) *int { return &n }
	base := func() *Config {
		return &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"},
			Output: OutputConfig{Player: fullPlayer()}}
	}
	for _, tc := range []struct {
		name   string
		edit   func(*Config)
		clash  string // the setting the reason names, "" for none
		number string // the port, which the reason must not quote
	}{
		{"the default port", func(c *Config) {}, "", ""},
		{"the API's port", func(c *Config) { c.Output.Player.OpenHomePort = intp(7788) }, "for listenAddress", "7788"},
		{"the console's port", func(c *Config) { c.Output.Player.OpenHomePort = intp(7789) }, "for adminAddress", "7789"},
		{"the DLNA listener's default port", func(c *Config) { c.Output.Player.OpenHomePort = intp(7790) }, "for dlna.listenAddress", "7790"},
		{"a pinned DLNA port, with DLNA off", func(c *Config) {
			c.DLNA.ListenAddress = "192.168.1.5:7801"
			c.Output.Player.OpenHomePort = intp(7801)
		}, "for dlna.listenAddress", "7801"},
		{"the loopback route's default port", func(c *Config) { c.Output.Player.OpenHomePort = intp(7791) }, "for output.loopbackPort", "7791"},
		{"the loopback route's own port", func(c *Config) {
			c.Output.LoopbackPort = intp(7802)
			c.Output.Player.OpenHomePort = intp(7802)
		}, "for output.loopbackPort", "7802"},
		{"the API's port with the surface off", func(c *Config) {
			c.Output.Player.OpenHome = false
			c.Output.Player.OpenHomePort = intp(7788)
		}, "", ""},
		{"port 0 beside an API on port 0", func(c *Config) {
			c.ListenAddress = ":0"
			c.Output.Player.OpenHomePort = intp(0)
		}, "", ""},
		{"an API moved off the default port", func(c *Config) {
			c.ListenAddress = ":7792"
		}, "for listenAddress", "7792"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Output.Player.OpenHome = true
			tc.edit(cfg)
			on, why := cfg.PlayerEnabled("linux")
			if tc.clash == "" {
				if !on || why != nil {
					t.Fatalf("PlayerEnabled = %v, %v; want on", on, why)
				}
				return
			}
			if on || why == nil || !strings.Contains(why.Error(), "output.player.openhomePort") || !strings.Contains(why.Error(), tc.clash) {
				t.Fatalf("PlayerEnabled = %v, %v; want off, naming openhomePort and %q", on, why, tc.clash)
			}
			if strings.Contains(why.Error(), tc.number) {
				t.Errorf("the reason quotes the port: %v", why)
			}
		})
	}

	forgetPlayerOffReason(t)
	rec := loggingtest.Record(t)
	cfg := base()
	cfg.Output.Player.OpenHome, cfg.Output.Player.OpenHomePort = true, intp(7790)
	cfg.playerGOOS = "linux"
	for i := 0; i < 3; i++ {
		if err := Clone(cfg).Validate(); err != nil {
			t.Fatalf("Validate refused the config over the OpenHome port: %v", err)
		}
	}
	if lines := rec.Failures(playerOffMessage); len(lines) != 1 || !strings.Contains(lines[0], "for dlna.listenAddress") {
		t.Errorf("logged %q, want one line naming dlna.listenAddress", lines)
	}
}
