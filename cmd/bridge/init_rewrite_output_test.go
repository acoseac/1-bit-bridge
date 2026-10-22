package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// rewriteSerial and rewriteMPDPassword are the two values of the output
// block a rewrite must keep and never show.
const (
	rewriteSerial      = "GOBAR-7Q2X-91KZ-4471"
	rewriteMPDPassword = "mpd-kitchen-pw-8812"
)

// keptOutputBlock is an output block an operator writes over the months:
// MPD switched off with its overrides and its password, the loopback
// route's port moved, and the player enabled and bound by serial, which
// `bridge output dac bind` writes from the unit and never prints.
const keptOutputBlock = `output:
    mpd: "off"
    endpoint: 127.0.0.1:6601
    conf: /etc/mpd-kitchen.conf
    password: ` + rewriteMPDPassword + `
    loopbackPort: 7795
    player:
        enabled: true
        room: Kitchen
        pauseReleaseSeconds: 45
        device:
            transport: usb
            binding: serial
            vendorId: "20b1"
            productId: "3009"
            serial: "` + rewriteSerial + `"
`

// assertKeptOutputBlock fails unless the install's saved config holds
// keptOutputBlock, and the run listed it without showing the password or
// the serial.
func assertKeptOutputBlock(t *testing.T, cfgDir, out string) {
	t.Helper()
	o := loadInstallConfig(t, cfgDir).Output
	if o.Mode() != "off" || o.Endpoint != "127.0.0.1:6601" || o.Conf != "/etc/mpd-kitchen.conf" || o.Password != rewriteMPDPassword ||
		o.EffectiveLoopbackPort() != 7795 {
		t.Errorf("the rewrite did not keep MPD's settings: mpd %q, endpoint %q, conf %q, password kept %v, loopbackPort %d",
			o.MPD, o.Endpoint, o.Conf, o.Password == rewriteMPDPassword, o.EffectiveLoopbackPort())
	}
	p := o.Player
	d := p.Device
	if !p.Enabled || p.Room != "Kitchen" || p.PauseReleaseSeconds == nil || *p.PauseReleaseSeconds != 45 ||
		d.Binding != config.BindingSerial || d.VendorID != "20b1" || d.ProductID != "3009" || !d.Serial.Equal(usbid.New(rewriteSerial)) {
		t.Errorf("the rewrite did not keep the player's binding: %+v", p)
	}
	if !strings.Contains(readConfigFile(t, cfgDir), rewriteSerial) {
		t.Error("the saved file does not hold the serial, so the binding cannot resolve")
	}
	if v, ok := keptValue(out, "output.mpd"); !ok || v != "off" {
		t.Errorf("the run lists output.mpd as %q, %v; want it listed as kept, off", v, ok)
	}
	if v, ok := keptValue(out, "output.player"); !ok || !strings.Contains(v, "bound by serial") {
		t.Errorf("the run lists output.player as %q, %v; want it listed as kept, bound by serial", v, ok)
	}
	for _, secret := range []string{rewriteSerial, rewriteSerial[len(rewriteSerial)-9:], rewriteMPDPassword} {
		if strings.Contains(out, secret) {
			t.Errorf("the run's output shows %q", secret)
		}
	}
}

// TestInitRewriteKeepsTheOutputBlock: init asks for nothing in output, and
// a rewrite (--force, or "Overwrite?" answered y) built bridge.yaml without
// it, so the player lost its binding, which only the unit can give back
// (bind reads the serial from it and never prints it), and MPD lost its
// password, its overrides, its opt-out (off became auto, which reports MPD
// and serves it the loopback route) and the loopback port its queue keeps
// URLs on. A rewrite keeps the block in either posture, a change of posture
// included, and lists it as kept.
func TestInitRewriteKeepsTheOutputBlock(t *testing.T) {
	for _, posture := range rewritePostures {
		t.Run(posture.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := posture.setUp(t, cfgDir)
			appendToConfig(t, cfgDir, keptOutputBlock)
			loadInstallConfig(t, cfgDir)

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			assertKeptOutputBlock(t, cfgDir, out)
		})
	}
	t.Run("public over loopback", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		if code, out := loopbackInit(t, cfgDir, testLibrary(t), "First"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		appendToConfig(t, cfgDir, keptOutputBlock)

		code, out := publicInit(t, cfgDir, pickPublicInitPorts(t), "Rewritten", "--force")
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		assertKeptOutputBlock(t, cfgDir, out)
	})
}

// TestInitRewriteKeepsAPlayerBlockThatDoesNotReadAsWritten: a player block
// this release cannot read turns the player off and is kept as the file
// holds it (config.PlayerConfig), and a rewrite keeps it so too: the
// operator's text is not lost to a rewrite any more than to a save.
func TestInitRewriteKeepsAPlayerBlockThatDoesNotReadAsWritten(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	rewrite := rewritePostures[0].setUp(t, cfgDir)
	appendToConfig(t, cfgDir, "output:\n    player:\n        enabled: true\n        pauseReleaseSecond: 30\n")
	if _, why := loadInstallConfig(t, cfgDir).PlayerEnabled("linux"); why == nil {
		t.Fatal("the fixture's player block reads")
	}

	code, out := rewrite()
	defer logRunOnFailure(t, out)

	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	if !strings.Contains(readConfigFile(t, cfgDir), "pauseReleaseSecond: 30") {
		t.Errorf("the rewrite dropped the player block it could not read:\n%s", readConfigFile(t, cfgDir))
	}
	_, why := loadInstallConfig(t, cfgDir).PlayerEnabled("linux")
	if why == nil || !strings.Contains(why.Error(), "that key is not a player setting") {
		t.Errorf("after the rewrite the player's refusal is %v", why)
	}
	if v, ok := keptValue(out, "output.player"); !ok || !strings.Contains(v, "does not read") {
		t.Errorf("the run lists output.player as %q, %v; want it listed as kept as written", v, ok)
	}
}

// TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo: an output
// setting that does not read, or that the config's own check refuses, is
// one the config did not load with either, which is often why init runs
// again. The rewrite cannot keep it, so it writes the rest of the block,
// the player's binding included, and says on its warning line which setting
// it did not keep and why, never quoting a value the field does not know.
func TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo(t *testing.T) {
	const player = "    player:\n        enabled: true\n        device:\n            binding: uniqueModel\n" +
		"            vendorId: \"20b1\"\n            productId: \"3009\"\n"
	for _, tc := range []struct {
		name, block, says, absent string
		keepsPlayer               bool
	}{
		{"mpd that is not a mode", "output:\n    mpd: maybe\n" + player, `output.mpd: "maybe" is not auto, on or off`, "maybe", true},
		{"a loopback port that is not a number", "output:\n    loopbackPort: " + rewriteMPDPassword + "\n" + player,
			"output.loopbackPort (line", rewriteMPDPassword, true},
		{"a loopback port that is not a port", "output:\n    loopbackPort: 70000\n" + player,
			"output.loopbackPort: 70000 is not a port", "70000", true},
		{"a password that is not text", "output:\n    password: [a, b]\n" + player, "output.password (line", "password", true},
		{"an output that is not a block", "output: \"off\"\n", "output (line", "output:", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			appendToConfig(t, cfgDir, tc.block)
			if _, err := config.Load(filepath.Join(cfgDir, "bridge.yaml")); err == nil {
				t.Fatal("the fixture's config loads")
			}

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			saved := loadInstallConfig(t, cfgDir)
			if tc.keepsPlayer && (!saved.Output.Player.Enabled || saved.Output.Player.Device.Binding != config.BindingUniqueModel) {
				t.Errorf("the rewrite dropped the player's binding beside a setting it could not keep: %+v", saved.Output.Player)
			}
			if file := readConfigFile(t, cfgDir); strings.Contains(file, tc.absent) {
				t.Errorf("the saved config still holds %q:\n%s", tc.absent, file)
			}
			warning := outputNotKeptWarning(out)
			if !strings.Contains(warning, tc.says) {
				t.Errorf("the rewrite's warning does not say %q:\n%s", tc.says, warning)
			}
			if strings.Contains(warning, rewriteMPDPassword) {
				t.Errorf("the warning quotes a value its field does not know:\n%s", warning)
			}
		})
	}
}

// outputNotKeptWarning is the run's warning about the output settings it
// did not keep, from its first line to the end of the run's output.
func outputNotKeptWarning(out string) string {
	i := strings.Index(out, "warning: output")
	if i < 0 {
		return ""
	}
	return out[i:]
}
