package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// playerSerial is the unit serial the player tests bind. Its tail is easy
// to find in anything that leaked it.
const playerSerial = "GOBAR-7Q2X-91KZ-4471"

// numericSerial is a unit serial of digits alone, which a whole-number
// field decodes as a number.
const numericSerial = "202311050044"

// playerLeaks reports whether out carries the serial, or six bytes of it
// in a row.
func playerLeaks(out string) bool { return leaksSerial(out, playerSerial) }

// leaksSerial reports whether out carries serial, or six bytes of it in a
// row.
func leaksSerial(out, serial string) bool {
	for i := 0; i+6 <= len(serial); i++ {
		if strings.Contains(out, serial[i:i+6]) {
			return true
		}
	}
	return false
}

// writePlayerConfig writes a loopback bridge.yaml whose output block is
// output (indented under "output:"), and returns its path.
func writePlayerConfig(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	music := filepath.Join(dir, "music")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\nlibraryName: Kitchen\nlibraryRoots: [" + yamlStr(music) + "]\n" +
		"dataDir: " + yamlStr(filepath.Join(dir, "data")) + "\noutput:\n  mpd: auto\n" + output
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fullPlayerBlock is an output.player block that sets every field the
// design names, at values this release accepts.
const fullPlayerBlock = `  player:
    enabled: true
    room: "Listening Room"
    pauseReleaseSeconds: 45
    device:
      transport: usb
      binding: serial
      vendorId: "0x20B1"
      productId: "3009"
      serial: "` + playerSerial + `"
      pcmDevice: 0
      subdevice: 0
    dsdMode: off
    allowDoP: false
    mixer:
      type: none
      control: null
`

// TestThePlayerIsOffByDefault: no output.player block is a player that is
// off, with the design's defaults, and Mode stays MPD's alone.
func TestThePlayerIsOffByDefault(t *testing.T) {
	cfg, err := Load(writePlayerConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Output.Player
	if on, why := cfg.PlayerEnabled("linux"); on || why != nil {
		t.Errorf("PlayerEnabled = %v, %v; want off with no reason", on, why)
	}
	if p.PauseRelease() != 30*time.Second || p.EffectiveDSDMode() != "off" || p.EffectiveMixer() != "none" || p.AllowsDoP() {
		t.Errorf("defaults: release %v, dsd %q, mixer %q, dop %v", p.PauseRelease(), p.EffectiveDSDMode(), p.EffectiveMixer(), p.AllowsDoP())
	}
	if p.Device.Configured() || p.Problem() != nil {
		t.Errorf("device %+v configured, problem %v", p.Device, p.Problem())
	}
	if cfg.Output.Mode() != "auto" {
		t.Errorf("Mode = %q", cfg.Output.Mode())
	}
}

// TestThePlayerBlockLoadsAndRoundTrips: every field reads, the ids come
// back canonical, the serial is kept, and a Save writes the file 0600 with
// the serial whole, which loads back the same.
func TestThePlayerBlockLoadsAndRoundTrips(t *testing.T) {
	path := writePlayerConfig(t, fullPlayerBlock)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Output.Player
	d := p.Device
	if !p.Enabled || p.Room != "Listening Room" || p.PauseRelease() != 45*time.Second || d.Transport != "usb" || d.Binding != BindingSerial ||
		d.VendorID != "20b1" || d.ProductID != "3009" || !d.Serial.Equal(usbid.New(playerSerial)) || d.PCMDevice != 0 || d.Subdevice != 0 ||
		p.EffectiveDSDMode() != "off" || p.AllowsDoP() || p.EffectiveMixer() != "none" {
		t.Fatalf("loaded %+v", p)
	}
	if err := p.Problem(); err != nil {
		t.Fatalf("problem: %v", err)
	}
	if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
		t.Fatalf("PlayerEnabled(linux) = %v, %v", on, why)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), playerSerial) || !strings.Contains(string(b), "vendorId: 20b1") {
		t.Errorf("saved file lacks the serial or the canonical vendor id:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("saved mode %v, %v; want 0600", st.Mode().Perm(), err)
		}
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Output.Player.Device.Serial.Equal(d.Serial) || again.Output.Player.Room != p.Room || again.Output.Player.PauseRelease() != p.PauseRelease() {
		t.Errorf("reloaded %+v", again.Output.Player)
	}
	// Every JSON form of what holds the serial carries the redaction, and
	// every printed one too.
	for _, v := range []any{again, again.Output, again.Output.Player, again.Output.Player.Device, Clone(again)} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		if playerLeaks(string(b)) || !strings.Contains(string(b), `"[redacted]"`) {
			t.Errorf("JSON of %T: %s", v, b)
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if out := fmt.Sprintf(verb, v); playerLeaks(out) {
				t.Errorf("%s of %T prints the serial: %s", verb, v, out)
			}
		}
	}
}

// TestASaveKeepsWhatWasNotWritten: a block that names only some fields is
// saved naming only those, so a default stays a default.
func TestASaveKeepsWhatWasNotWritten(t *testing.T) {
	path := writePlayerConfig(t, "  player:\n    enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, absent := range []string{"pauseReleaseSeconds", "dsdMode", "allowDoP", "mixer", "device"} {
		if strings.Contains(string(b), absent) {
			t.Errorf("a save wrote %s, which the block did not name:\n%s", absent, b)
		}
	}
	if !strings.Contains(string(b), "player:\n        enabled: true") && !strings.Contains(string(b), "enabled: true") {
		t.Errorf("a save dropped the block:\n%s", b)
	}
}

// TestAnInvalidPlayerBlockTurnsOffThePlayerAndNothingElse: a block that
// does not read, or reads and is refused, still loads the bridge (MPD,
// the library and the server are untouched), turns the player off with a
// reason that names the field and never the serial, and a Save writes the
// block back as it was written.
func TestAnInvalidPlayerBlockTurnsOffThePlayerAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name, block, reason string
	}{
		{"unknown key", "    enabled: true\n    pauseReleaseSecond: 30\n",
			"that key is not a player setting (enabled, room, pauseReleaseSeconds, device, dsdMode, allowDoP or mixer)"},
		{"unknown key holding the serial", "    enabled: true\n    serail: " + playerSerial + "\n", "that key is not a player setting"},
		{"a wrong type", "    enabled: maybe\n", "output.player.enabled (line"},
		{"the serial where a number goes", "    enabled: true\n    device:\n      pcmDevice: " + playerSerial + "\n", "output.player.device.pcmDevice (line"},
		{"a scalar mixer", "    enabled: true\n    mixer: none\n", "mixer is an object"},
		{"a mixer control", "    enabled: true\n    mixer:\n      type: none\n      control: {iface: MIXER, name: PCM}\n", "not in this release"},
		{"resampling", "    enabled: true\n    resample: soxr\n", "never resamples"},
		{"a forced rate", "    enabled: true\n    forceRate: 96000\n", "never resamples"},
		{"software volume", "    enabled: true\n    softwareVolume: true\n", "never resamples"},
		{"a card number", "    enabled: true\n    device:\n      card: 5\n", "renumbers"},
		{"a device number", "    enabled: true\n    device:\n      devnum: 3\n", "renumbers"},
		{"a repeated key", "    enabled: true\n    room: a\n    room: b\n", "set twice"},
		{"not a mapping", "    - enabled\n", "is a mapping"},
		{"a merge key", "    <<: {enabled: true}\n", "merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePlayerConfig(t, "  player:\n"+tc.block)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("an invalid player block stopped the load: %v", err)
			}
			if len(cfg.LibraryRoots) != 1 || cfg.LibraryName != "Kitchen" || cfg.Output.Mode() != "auto" {
				t.Errorf("the rest of the config did not load: %+v", cfg)
			}
			on, why := cfg.PlayerEnabled("linux")
			if on || why == nil || !strings.Contains(why.Error(), tc.reason) {
				t.Fatalf("PlayerEnabled = %v, %v; want off, naming %q", on, why, tc.reason)
			}
			if playerLeaks(why.Error()) {
				t.Errorf("the reason quotes the serial: %v", why)
			}
			written, _ := os.ReadFile(path)
			if err := cfg.Save(path); err != nil {
				t.Fatal(err)
			}
			saved, _ := os.ReadFile(path)
			if strings.Contains(string(written), playerSerial) != strings.Contains(string(saved), playerSerial) {
				t.Errorf("a save changed whether the file holds the serial:\n%s", saved)
			}
			back, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			// The save writes the whole file again, so the block's lines
			// move; the refusal must not.
			unlined := regexp.MustCompile(` \(line \d+\)`)
			if on2, why2 := back.PlayerEnabled("linux"); on2 || why2 == nil ||
				unlined.ReplaceAllString(why2.Error(), "") != unlined.ReplaceAllString(why.Error(), "") {
				t.Errorf("after a save: %v, %v; want the same refusal %v", on2, why2, why)
			}
		})
	}
}

// TestASerialInTheWrongFieldIsNeverQuoted: a serial pasted into a field
// that takes a fixed word (binding, transport, dsdMode, mixer.type),
// written as a key, or, all digits, into a whole-number field, is refused
// as "that value" or "that key", never quoted: not in PlayerEnabled's
// reason, not in Problem, and not in Validate's log line, on the load and
// on the load after a save. A word a refusal quotes is one the field
// knows.
func TestASerialInTheWrongFieldIsNeverQuoted(t *testing.T) {
	const model = "      vendorId: 20b1\n      productId: \"3009\"\n"
	for _, tc := range []struct {
		name, block, reason string
	}{
		{"binding", "    device:\n      binding: " + playerSerial + "\n" + model,
			"output.player.device.binding: that value is not serial, uniqueModel or port"},
		{"transport", "    device:\n      transport: " + playerSerial + "\n      binding: uniqueModel\n" + model,
			"output.player.device.transport: that value is not in this release: only usb is"},
		{"dsdMode", "    dsdMode: " + playerSerial + "\n", "output.player.dsdMode: that value is not a DSD mode"},
		{"mixer type", "    mixer:\n      type: " + playerSerial + "\n", "output.player.mixer.type: that value is not a mixer type"},
		{"a player key", "    " + playerSerial + ": true\n", "that key is not a player setting"},
		{"a device key", "    device:\n      " + playerSerial + ": 1\n", "that key is not a device setting"},
		{"a mixer key", "    mixer:\n      " + playerSerial + ": none\n", "that key is not a mixer setting"},
		{"pauseReleaseSeconds", "    pauseReleaseSeconds: " + numericSerial + "\n", "output.player.pauseReleaseSeconds: must be from 1 to 3600"},
		{"pcmDevice", "    device:\n      binding: uniqueModel\n" + model + "      pcmDevice: " + numericSerial + "\n",
			"output.player.device.pcmDevice: must be from 0 to 255"},
		{"subdevice", "    device:\n      binding: uniqueModel\n" + model + "      subdevice: " + numericSerial + "\n",
			"output.player.device.subdevice: must be from 0 to 255"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaks := func(out string) bool { return playerLeaks(out) || leaksSerial(out, numericSerial) }
			rec := loggingtest.Record(t)
			path := writePlayerConfig(t, "  player:\n    enabled: true\n"+tc.block)
			for _, pass := range []string{"load", "load after a save"} {
				cfg, err := Load(path)
				if err != nil {
					t.Fatalf("%s: %v", pass, err)
				}
				on, why := cfg.PlayerEnabled("linux")
				if on || why == nil || !strings.Contains(why.Error(), tc.reason) {
					t.Fatalf("%s: PlayerEnabled = %v, %v; want off, naming %q", pass, on, why, tc.reason)
				}
				if leaks(why.Error()) {
					t.Errorf("%s: the reason quotes the serial: %v", pass, why)
				}
				if err := cfg.Output.Player.Problem(); err == nil || leaks(err.Error()) {
					t.Errorf("%s: Problem = %v", pass, err)
				}
				logged := rec.Failures(playerOffMessage)
				if len(logged) == 0 || !strings.Contains(logged[len(logged)-1], tc.reason) {
					t.Errorf("%s: Validate logged %q, want the reason %q", pass, logged, tc.reason)
				}
				for _, l := range rec.All() {
					if leaks(l) {
						t.Errorf("%s: a log line quotes the serial: %s", pass, l)
					}
				}
				if err := cfg.Save(path); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// writeConfigBody writes a loopback bridge.yaml whose top-level keys after
// listenAddress are top (libraryName among them), and whose output block is
// output, and returns its path.
func writeConfigBody(t *testing.T, top, output string) string {
	t.Helper()
	dir := t.TempDir()
	music := filepath.Join(dir, "music")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\n" + top + "libraryRoots: [" + yamlStr(music) + "]\n" +
		"dataDir: " + yamlStr(filepath.Join(dir, "data")) + "\noutput:\n" + output
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAnUnreadBlockWithAnAliasSavesAFileThatLoads: a block that does not
// read is written back as the file held it, and the rest of the file is
// written from the config's fields, which keep no anchor. So an alias in
// the block to an anchor outside it (libraryName: &room Kitchen, then
// player.room: *room) is saved as a copy of what it names, and the file
// loads again, and again after a second save, with the same refusal; an
// alias to an anchor inside the block is saved as it is.
func TestAnUnreadBlockWithAnAliasSavesAFileThatLoads(t *testing.T) {
	unlined := regexp.MustCompile(` \(line \d+\)`)
	for _, tc := range []struct {
		name, top, output string
		// saved is what the saved file must hold.
		saved []string
	}{
		{"an alias to libraryName", "libraryName: &room Kitchen\n", "  player:\n    room: *room\n    bogus: 1\n",
			[]string{"room: Kitchen"}},
		{"an alias as a key", "libraryName: &k Kitchen\n", "  player:\n    enabled: true\n    *k : 1\n", []string{"Kitchen: 1"}},
		{"an alias inside a value", "libraryName: &room Kitchen\n", "  player:\n    bogus: [*room, *room]\n", []string{"Kitchen, Kitchen"}},
		{"an anchor inside the block", "libraryName: Kitchen\n", "  player:\n    room: &r Study\n    bogus: *r\n",
			[]string{"&r Study", "bogus: *r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigBody(t, tc.top, tc.output)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			_, why := cfg.PlayerEnabled("linux")
			if why == nil || !strings.Contains(why.Error(), "is not a player setting") {
				t.Fatalf("PlayerEnabled's reason = %v", why)
			}
			for i := 0; i < 2; i++ {
				if err := cfg.Save(path); err != nil {
					t.Fatalf("save %d: %v", i+1, err)
				}
				saved, _ := os.ReadFile(path)
				if cfg, err = Load(path); err != nil {
					t.Fatalf("the file save %d wrote does not load: %v\n%s", i+1, err, saved)
				}
				if _, again := cfg.PlayerEnabled("linux"); again == nil ||
					unlined.ReplaceAllString(again.Error(), "") != unlined.ReplaceAllString(why.Error(), "") {
					t.Errorf("after save %d: %v; want the same refusal %v", i+1, again, why)
				}
				for _, s := range tc.saved {
					if !strings.Contains(string(saved), s) {
						t.Errorf("save %d does not hold %q:\n%s", i+1, s, saved)
					}
				}
			}
		})
	}
}

// TestAnUnreadBlockWhoseAliasesCannotBeCopiedIsNotKept: an alias that names
// the block from inside it (room: *o, where &o is the output mapping that
// holds the block) would copy without end, and copies past the bound are an
// alias bomb. Such a block is not kept: its reason says a save drops it,
// and the file a save writes loads, with no player block.
func TestAnUnreadBlockWhoseAliasesCannotBeCopiedIsNotKept(t *testing.T) {
	bomb := strings.TrimSuffix(strings.Repeat("*n, ", 1100), ", ")
	for _, tc := range []struct{ name, top, output string }{
		{"an alias to the block's own parent", "libraryName: Kitchen\n", "  &o\n  mpd: auto\n  player:\n    room: *o\n"},
		{"an alias bomb", "libraryName: &n Kitchen\n", "  player:\n    bogus: [" + bomb + "]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigBody(t, tc.top, tc.output)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, why := cfg.PlayerEnabled("linux"); why == nil || !strings.Contains(why.Error(), "a save drops it") {
				t.Fatalf("PlayerEnabled's reason = %v", why)
			}
			if err := cfg.Save(path); err != nil {
				t.Fatal(err)
			}
			saved, _ := os.ReadFile(path)
			back, err := Load(path)
			if err != nil {
				t.Fatalf("the file the save wrote does not load: %v\n%s", err, saved)
			}
			if strings.Contains(string(saved), "player:") || back.Output.Player.Unread() != nil || !back.Output.Player.IsZero() {
				t.Errorf("the save kept a block it could not write back:\n%s", saved)
			}
		})
	}
}

// fullPlayer is fullPlayerBlock in memory: every field set to a value
// this release takes.
func fullPlayer() PlayerConfig {
	release, dop := 45, false
	return PlayerConfig{
		Enabled: true, Room: "Listening Room", PauseReleaseSeconds: &release, DSDMode: "off", AllowDoP: &dop,
		Mixer: PlayerMixer{Type: "none"},
		Device: PlayerDevice{Transport: "usb", Binding: BindingSerial, VendorID: "20b1", ProductID: "3009",
			Serial: usbid.New(playerSerial)},
	}
}

// TestEveryPlayerRefusal: each value the block refuses, named by its field,
// and the neighbours it accepts.
func TestEveryPlayerRefusal(t *testing.T) {
	intp := func(n int) *int { return &n }
	boolp := func(b bool) *bool { return &b }
	for _, tc := range []struct {
		name   string
		edit   func(*PlayerConfig)
		reason string // "" accepts
	}{
		{"the full block", func(*PlayerConfig) {}, ""},
		{"release 1", func(p *PlayerConfig) { p.PauseReleaseSeconds = intp(1) }, ""},
		{"release an hour", func(p *PlayerConfig) { p.PauseReleaseSeconds = intp(3600) }, ""},
		{"release 0", func(p *PlayerConfig) { p.PauseReleaseSeconds = intp(0) }, "pauseReleaseSeconds"},
		{"release negative", func(p *PlayerConfig) { p.PauseReleaseSeconds = intp(-5) }, "pauseReleaseSeconds"},
		{"release past an hour", func(p *PlayerConfig) { p.PauseReleaseSeconds = intp(3601) }, "pauseReleaseSeconds"},
		{"dsd auto", func(p *PlayerConfig) { p.DSDMode = "auto" }, `dsdMode: "auto" is not in this release`},
		{"dsd native", func(p *PlayerConfig) { p.DSDMode = "native" }, `dsdMode: "native" is not in this release`},
		{"dsd dop", func(p *PlayerConfig) { p.DSDMode = "dop" }, `dsdMode: "dop" is not in this release`},
		{"dsd to pcm", func(p *PlayerConfig) { p.DSDMode = "pcm" }, `dsdMode: that value is not a DSD mode`},
		{"dsd off", func(p *PlayerConfig) { p.DSDMode = "off" }, ""},
		{"dop on", func(p *PlayerConfig) { p.AllowDoP = boolp(true) }, "allowDoP: true is not in this release"},
		{"hardware mixer", func(p *PlayerConfig) { p.Mixer.Type = "hardware" }, `mixer.type: "hardware" is not in this release`},
		{"software mixer", func(p *PlayerConfig) { p.Mixer.Type = "software" }, `mixer.type: that value is not a mixer type`},
		{"room too long", func(p *PlayerConfig) { p.Room = strings.Repeat("r", 65) }, "room"},
		{"room with a control", func(p *PlayerConfig) { p.Room = "a\x1bb" }, "room"},
		{"room not utf8", func(p *PlayerConfig) { p.Room = "caf\xe9" }, "room"},
		{"no transport", func(p *PlayerConfig) { p.Device.Transport = "" }, ""},
		{"another transport", func(p *PlayerConfig) { p.Device.Transport = "i2s" }, `device.transport: that value is not in this release`},
		{"no binding with a unit", func(p *PlayerConfig) { p.Device.Binding = "" }, "device.binding"},
		{"a card binding", func(p *PlayerConfig) { p.Device.Binding = "card" }, `device.binding: that value is not serial, uniqueModel or port`},
		{"serial with no serial", func(p *PlayerConfig) { p.Device.Serial = usbid.Serial{} }, "device.serial"},
		{"serial with no vendor", func(p *PlayerConfig) { p.Device.VendorID = "" }, "device.vendorId"},
		{"serial with no product", func(p *PlayerConfig) { p.Device.ProductID = "" }, "device.productId"},
		{"serial with a port", func(p *PlayerConfig) { p.Device.Port = "platform-xhci-hcd.0/usb2-1:1.0" }, "device.port"},
		{"uniqueModel with a serial", func(p *PlayerConfig) { p.Device.Binding = BindingUniqueModel }, "a uniqueModel binding names a model, not a unit"},
		{"uniqueModel", func(p *PlayerConfig) { p.Device.Binding, p.Device.Serial = BindingUniqueModel, usbid.Serial{} }, ""},
		{"uniqueModel with a port", func(p *PlayerConfig) {
			p.Device.Binding, p.Device.Serial, p.Device.Port = BindingUniqueModel, usbid.Serial{}, "platform-xhci-hcd.0/usb2-1:1.0"
		}, "device.port"},
		{"port", func(p *PlayerConfig) {
			p.Device = PlayerDevice{Transport: "usb", Binding: BindingPort, Port: "platform-xhci-hcd.0/usb2-1:1.0"}
		}, ""},
		{"port with a serial", func(p *PlayerConfig) {
			p.Device = PlayerDevice{Binding: BindingPort, Port: "platform-xhci-hcd.0/usb2-1:1.0", Serial: usbid.New(playerSerial)}
		}, "a port binding names a port, not a unit"},
		{"port with a model", func(p *PlayerConfig) {
			p.Device = PlayerDevice{Binding: BindingPort, Port: "platform-xhci-hcd.0/usb2-1:1.0", VendorID: "20b1", ProductID: "3009"}
		}, "a port binding names a port, not a model"},
		{"port with no port", func(p *PlayerConfig) { p.Device = PlayerDevice{Binding: BindingPort} }, "device.port"},
		{"port unanchored", func(p *PlayerConfig) { p.Device = PlayerDevice{Binding: BindingPort, Port: "1-1.3:1.0"} }, "device.port"},
		{"vendor upper", func(p *PlayerConfig) { p.Device.VendorID = "20B1" }, ""},
		{"vendor 0x", func(p *PlayerConfig) { p.Device.VendorID = "0x20b1" }, ""},
		{"vendor three digits", func(p *PlayerConfig) { p.Device.VendorID = "2b1" }, "device.vendorId"},
		{"vendor 0X", func(p *PlayerConfig) { p.Device.VendorID = "0X20B1" }, "device.vendorId"},
		{"product five digits", func(p *PlayerConfig) { p.Device.ProductID = "03009" }, "device.productId"},
		{"pcm device negative", func(p *PlayerConfig) { p.Device.PCMDevice = -1 }, "device.pcmDevice"},
		{"pcm device past 255", func(p *PlayerConfig) { p.Device.PCMDevice = 256 }, "device.pcmDevice"},
		{"subdevice negative", func(p *PlayerConfig) { p.Device.Subdevice = -1 }, "device.subdevice"},
		{"subdevice past 255", func(p *PlayerConfig) { p.Device.Subdevice = 256 }, "device.subdevice"},
		{"endpoint with no binding", func(p *PlayerConfig) { p.Device = PlayerDevice{PCMDevice: 1} }, "device.binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fullPlayer()
			tc.edit(&p)
			err := p.Problem()
			switch {
			case tc.reason == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.reason != "" && (err == nil || !strings.Contains(err.Error(), tc.reason)):
				t.Fatalf("Problem = %v, want a refusal naming %q", err, tc.reason)
			case err != nil && playerLeaks(err.Error()):
				t.Fatalf("the refusal quotes the serial: %v", err)
			}
			cfg := &Config{Output: OutputConfig{Player: p}}
			on, why := cfg.PlayerEnabled("linux")
			if (err == nil) != on || (err == nil) != (why == nil) {
				t.Errorf("PlayerEnabled = %v, %v beside Problem %v", on, why, err)
			}
		})
	}
}

// TestThePlayerIsLinuxOnlyAndNeverPublic: a block that passes everything
// else is refused off Linux and on a public bridge, and turned off rather
// than refused when enabled is false.
func TestThePlayerIsLinuxOnlyAndNeverPublic(t *testing.T) {
	cfg := &Config{Output: OutputConfig{Player: fullPlayer()}}
	for _, goos := range []string{"darwin", "windows", "freebsd"} {
		on, why := cfg.PlayerEnabled(goos)
		if on || why == nil || !strings.Contains(why.Error(), "Linux only, and this is "+goos) {
			t.Errorf("%s: %v, %v", goos, on, why)
		}
	}
	cfg.Deployment.Mode = string(DeploymentModePublic)
	on, why := cfg.PlayerEnabled("linux")
	if on || why == nil || !strings.Contains(why.Error(), "public") {
		t.Errorf("public: %v, %v", on, why)
	}
	cfg.Output.Player.Enabled = false
	if on, why := cfg.PlayerEnabled("linux"); on || why != nil {
		t.Errorf("disabled on a public bridge: %v, %v; want off with no reason", on, why)
	}
}

// TestValidateTurnsThePlayerOffWithALoggedReason: Validate never fails a
// config over its player block; it logs why the player is off, through
// the GOOS it is given, and the line carries no serial.
func TestValidateTurnsThePlayerOffWithALoggedReason(t *testing.T) {
	rec := loggingtest.Record(t)
	cfg := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"},
		Output: OutputConfig{Player: fullPlayer()}}
	cfg.Output.Player.Device.Binding = BindingUniqueModel
	cfg.playerGOOS = "linux"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate refused the config over its player block: %v", err)
	}
	cfg.Output.Player.Device.Binding = BindingSerial
	cfg.playerGOOS = "darwin"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate refused a Linux-only player off Linux: %v", err)
	}
	lines := rec.Failures(playerOffMessage)
	if len(lines) != 2 || !strings.Contains(lines[0], "names a model, not a unit") || !strings.Contains(lines[1], "Linux only, and this is darwin") {
		t.Fatalf("logged %q", lines)
	}
	for _, l := range rec.All() {
		if playerLeaks(l) {
			t.Errorf("a log line carries the serial: %s", l)
		}
	}
	cfg.playerGOOS = "linux"
	before := len(rec.All())
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := rec.Failures(playerOffMessage); len(got) != 2 || len(rec.All()) != before {
		t.Errorf("a player that runs was logged: %q", rec.All()[before:])
	}
}

// TestThePlayerOffWarningIsLoggedOnceForEachReason: Validate runs on every
// Load and every settings save (a clone, mutated, NormalizeAndValidate), and
// it wrote the player-off warning each time, the same line for a block that
// had not changed. It writes the line once each time the reason changes: two
// loads and two saves with one reason write one line, a changed reason
// writes again, and so does a reason that went away (the player runs, or is
// simply off) and came back, which is a change too.
func TestThePlayerOffWarningIsLoggedOnceForEachReason(t *testing.T) {
	// A config whose player is simply off has no reason: whatever another
	// test left as the reason last logged is gone, here and after this test.
	quiet := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"}}
	if err := quiet.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = quiet.Validate() })
	rec := loggingtest.Record(t)
	loadAndSave := func(t *testing.T, path string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			next := Clone(cfg)
			next.LibraryName = fmt.Sprintf("Study %d", i)
			if err := next.NormalizeAndValidate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantLines := func(t *testing.T, reasons ...string) {
		t.Helper()
		got := rec.Failures(playerOffMessage)
		if len(got) != len(reasons) {
			t.Fatalf("logged %d player-off lines, want %d:\n%s", len(got), len(reasons), strings.Join(got, "\n"))
		}
		for i, r := range reasons {
			if !strings.Contains(got[i], r) {
				t.Errorf("line %d is %q, want the reason %q", i+1, got[i], r)
			}
		}
	}
	native := writePlayerConfig(t, "  player:\n    enabled: true\n    dsdMode: native\n")
	loadAndSave(t, native)
	wantLines(t, `dsdMode: "native" is not in this release`)

	loadAndSave(t, writePlayerConfig(t, "  player:\n    enabled: true\n    allowDoP: true\n"))
	wantLines(t, `dsdMode: "native" is not in this release`, "allowDoP: true is not in this release")

	runs := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"},
		Output: OutputConfig{Player: fullPlayer()}, playerGOOS: "linux"}
	if err := runs.Validate(); err != nil {
		t.Fatal(err)
	}
	loadAndSave(t, native)
	wantLines(t, `dsdMode: "native" is not in this release`, "allowDoP: true is not in this release", `dsdMode: "native" is not in this release`)

	// Saves at once, from many goroutines, write a changed reason once. The
	// config is built in memory, so these saves are the first to see its
	// reason.
	allow := true
	dop := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"},
		Output: OutputConfig{Player: PlayerConfig{Enabled: true, AllowDoP: &allow}}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Clone(dop).NormalizeAndValidate()
		}()
	}
	wg.Wait()
	wantLines(t, `dsdMode: "native" is not in this release`, "allowDoP: true is not in this release", `dsdMode: "native" is not in this release`,
		"allowDoP: true is not in this release")
}

// TestValidateReadsNoHardware: the player's checks read the block and
// nothing else. With the readers' tripwire refusing any read of the host's
// /proc or /sys, and the PCM guard refusing any open, a Load and a
// Validate of an enabled, bound player, and its predicate, touch neither.
func TestValidateReadsNoHardware(t *testing.T) {
	var mu sync.Mutex
	var touched []string
	stopReads := alsa.WatchHostReads(func(root string) error {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "read "+root)
		return errors.New("the config read the host's " + root)
	})
	defer stopReads()
	stopOpens := alsapcm.GuardNodeOpens(func(path string) {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "open "+path)
	})
	defer stopOpens()
	cfg, err := Load(writePlayerConfig(t, fullPlayerBlock))
	if err != nil {
		t.Fatal(err)
	}
	cfg.playerGOOS = "linux"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		_, _ = cfg.PlayerEnabled(goos)
	}
	_ = cfg.Output.Player.Problem()
	mu.Lock()
	defer mu.Unlock()
	if len(touched) != 0 {
		t.Errorf("the config's checks touched the hardware: %q", touched)
	}
}

// TestThePlayerIsEnvSettableButItsSerialIsNot: the scalar fields take an
// override as every config leaf does; the serial does not, since an
// environment variable is no place for a password-class value.
func TestThePlayerIsEnvSettableButItsSerialIsNot(t *testing.T) {
	t.Setenv("BRIDGE_OUTPUT_PLAYER_ENABLED", "true")
	t.Setenv("BRIDGE_OUTPUT_PLAYER_PAUSE_RELEASE_SECONDS", "90")
	t.Setenv("BRIDGE_OUTPUT_PLAYER_DEVICE_VENDOR_ID", "0x20B1")
	cfg, err := Load(writePlayerConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Output.Player
	if !p.Enabled || p.PauseRelease() != 90*time.Second || p.Device.VendorID != "0x20B1" {
		t.Errorf("overrides not applied: %+v", p)
	}
	for _, b := range envBindings() {
		if strings.Contains(strings.ToLower(b.YAMLPath), "serial") {
			t.Errorf("%s (%s) sets a serial from the environment", b.EnvVar, b.YAMLPath)
		}
	}
}

// uuidV4 is the form EnsurePlayerUUID writes: a random (version 4, RFC 4122
// variant) UUID in lowercase.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestThePlayerUUIDIsMadeOnceAndKept: the UUID is random, made the first
// time it is asked for, kept in the data directory's player-uuid, and the
// same every time after, whatever the config or the binding become; two
// data directories never share one, so it is not derived from anything a
// config holds.
func TestThePlayerUUIDIsMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPlayerUUID(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadPlayerUUID on a fresh data dir = %v, want fs.ErrNotExist", err)
	}
	id, created, err := EnsurePlayerUUID(dir)
	if err != nil || !created || !uuidV4.MatchString(id) {
		t.Fatalf("EnsurePlayerUUID = %q, %v, %v", id, created, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, PlayerUUIDFile))
	if err != nil || strings.TrimSpace(string(b)) != id {
		t.Fatalf("player-uuid holds %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, PlayerUUIDFile)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("player-uuid mode %v, %v", st.Mode().Perm(), err)
		}
	}
	for i := 0; i < 3; i++ {
		again, created, err := EnsurePlayerUUID(dir)
		if err != nil || created || again != id {
			t.Fatalf("EnsurePlayerUUID again = %q, %v, %v; want %q kept", again, created, err, id)
		}
	}
	if got, err := ReadPlayerUUID(dir); err != nil || got != id {
		t.Fatalf("ReadPlayerUUID = %q, %v", got, err)
	}
	other, _, err := EnsurePlayerUUID(t.TempDir())
	if err != nil || other == id {
		t.Fatalf("a second data dir got %q, %v", other, err)
	}
	for _, garbage := range []string{"", "not-a-uuid\n", id + id, strings.ToUpper(id)} {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, PlayerUUIDFile), []byte(garbage), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadPlayerUUID(bad); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("player-uuid %q read as %q, %v; want a refusal", garbage, got, err)
		}
		if got, _, err := EnsurePlayerUUID(bad); err == nil {
			t.Errorf("EnsurePlayerUUID replaced a player-uuid it could not read with %q", got)
		}
	}
}

// TestWritePlayerUUIDFileWritesOneUUIDAndNothingElse: the identity file's
// writer, which EnsurePlayerUUID makes the file with and a restore writes a
// snapshot's UUID through, writes the UUID and a newline, 0600, over what
// was there; and it refuses to write anything but one lowercase UUID,
// leaving the file as it was and nothing staged beside it.
func TestWritePlayerUUIDFileWritesOneUUIDAndNothingElse(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	dir := t.TempDir()
	path := filepath.Join(dir, PlayerUUIDFile)
	if err := os.WriteFile(path, []byte("7c9e6679-7425-40de-944b-e07fc1f90ae7\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePlayerUUIDFile(path, id); err != nil {
		t.Fatalf("WritePlayerUUIDFile: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != id+"\n" {
		t.Fatalf("player-uuid holds %q, %v; want the UUID and a newline", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("player-uuid mode %v, %v; want 0600", st.Mode().Perm(), err)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", strings.ToUpper(id), id + "\n", " " + id, id + id, id[:35]} {
		if err := WritePlayerUUIDFile(path, bad); err == nil {
			t.Errorf("WritePlayerUUIDFile wrote %q", bad)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != id+"\n" {
			t.Fatalf("a refused write of %q left player-uuid holding %q, %v", bad, b, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != PlayerUUIDFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the data dir holds %v; want player-uuid alone", names)
	}
	if got, err := ReadPlayerUUIDFile(path); err != nil || got != id {
		t.Fatalf("ReadPlayerUUIDFile = %q, %v", got, err)
	}
}

// TestThePlayerUUIDReadsPastTrailingWhiteSpaceAndNothingElse: an editor or
// a shell can leave a CR LF, spaces, tabs or more newlines after the UUID
// it saved, and the file still holds one UUID, so it reads; anything else
// beside the UUID (before it, after it but not white space, a second line)
// is still not one UUID, and is refused, never replaced.
func TestThePlayerUUIDReadsPastTrailingWhiteSpaceAndNothingElse(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, PlayerUUIDFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, body := range []string{id, id + "\n", id + "\r\n", id + " \t\r\n\n", id + "   "} {
		if got, err := ReadPlayerUUID(write(t, body)); err != nil || got != id {
			t.Errorf("player-uuid %q read as %q, %v; want %s", body, got, err, id)
		}
	}
	for _, body := range []string{" " + id + "\n", "\n" + id, "\xef\xbb\xbf" + id + "\n", id + " x\n", id + "\n" + id + "\n",
		id + "\x00\n", strings.ToUpper(id) + "\r\n", id[:35] + " \n"} {
		dir := write(t, body)
		if got, err := ReadPlayerUUID(dir); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("player-uuid %q read as %q, %v; want a refusal", body, got, err)
		}
		if got, _, err := EnsurePlayerUUID(dir); err == nil {
			t.Errorf("EnsurePlayerUUID replaced player-uuid %q with %q", body, got)
		}
	}
}
