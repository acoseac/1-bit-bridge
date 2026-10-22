package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
// it did not keep, where and why, with no value in it: what is refused is
// often something pasted onto the wrong line, MPD's password or the DAC's
// serial on mpd's, a serial of digits on the port's (B505's round-2 review:
// the warning quoted a refused mpd and printed a refused port). The value
// reaches neither the run's output nor the saved config.
func TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo(t *testing.T) {
	const player = "    player:\n        enabled: true\n        device:\n            binding: uniqueModel\n" +
		"            vendorId: \"20b1\"\n            productId: \"3009\"\n"
	notAMode := []string{"output.mpd (line ", "): that value is not auto, on or off"}
	notAPort := []string{"output.loopbackPort (line ", "): that value is not a port (0 to 65535)"}
	for _, tc := range []struct {
		name, block, secret string
		says                []string
		keepsPlayer         bool
	}{
		{"mpd that is not a mode", "output:\n    mpd: maybe\n" + player, "maybe", notAMode, true},
		{"MPD's password on mpd's line", "output:\n    mpd: " + rewriteMPDPassword + "\n" + player, rewriteMPDPassword, notAMode, true},
		{"the DAC's serial on mpd's line", "output:\n    mpd: \"" + rewriteSerial + "\"\n" + player, rewriteSerial[len(rewriteSerial)-6:], notAMode, true},
		{"MPD's password on mpd's line in a merged block", "output:\n    <<: {mpd: " + rewriteMPDPassword + "}\n" + player,
			rewriteMPDPassword, notAMode, true},
		{"a loopback port that is not a number", "output:\n    loopbackPort: " + rewriteMPDPassword + "\n" + player,
			rewriteMPDPassword, []string{"output.loopbackPort (line ", "): it does not read as that setting"}, true},
		{"a serial of digits as the loopback port", "output:\n    loopbackPort: 202311050044\n" + player, "202311050044", notAPort, true},
		{"a loopback port that is not a port", "output:\n    loopbackPort: 70000\n" + player, "70000", notAPort, true},
		{"a password that is not text", "output:\n    password: [" + rewriteMPDPassword + "]\n" + player, rewriteMPDPassword,
			[]string{"output.password (line ", "): it does not read as that setting"}, true},
		{"an output that is not a block", "output: " + rewriteMPDPassword + "\n", rewriteMPDPassword,
			[]string{"output (line ", "): it is not a block of output settings"}, false},
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
			warning := outputNotKeptWarning(out)
			for _, s := range tc.says {
				if !strings.Contains(warning, s) {
					t.Errorf("the rewrite's warning does not say %q:\n%s", s, warning)
				}
			}
			if strings.Contains(out, tc.secret) {
				t.Errorf("the run's output carries the value %q", tc.secret)
			}
			if file := readConfigFile(t, cfgDir); strings.Contains(file, tc.secret) {
				t.Errorf("the saved config still holds %q:\n%s", tc.secret, file)
			}
		})
	}
}

// sameOutput fails unless got and want are one output block: the same YAML
// written out, the serial whole included (config.PlayerConfig.MarshalYAML).
func sameOutput(t *testing.T, got, want config.OutputConfig) {
	t.Helper()
	g, err := yaml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(g) != string(w) {
		t.Errorf("the output block after the rewrite:\n%s\nwant:\n%s", g, w)
	}
}

// mergeTestPlayer is a player block bound by serial, and mergeOtherPlayer
// one bound by port, for the merge cases.
const (
	mergeTestPlayer  = `{enabled: true, room: Kitchen, device: {binding: serial, vendorId: "20b1", productId: "3009", serial: "` + rewriteSerial + `"}}`
	mergeOtherPlayer = `{enabled: true, room: Study, device: {binding: port, port: "platform-xhci-hcd.0/usb2-1:1.0"}}`
)

// TestInitRewriteKeepsWhatTheOutputBlockLoadedWith: a rewrite keeps every
// output setting with the value Load gave it. A merge key (<<) yields to
// the keys its block writes, before it or after it, and an explicit empty
// or null value is a value: the rewrite processed the block's pairs in the
// file's order and kept only values that were not zero, so a merge written
// last overwrote the key beside it, the player's binding included, and an
// explicit empty password let a merged one through (Codex on B505's round
// 1). Each case's config loads, and the block after the rewrite is the
// block Load read before it, with no warning.
func TestInitRewriteKeepsWhatTheOutputBlockLoadedWith(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"a key written before a merge that sets it too", "output:\n    mpd: \"off\"\n    <<: {mpd: \"on\"}\n"},
		{"a key written after a merge that sets it too", "output:\n    <<: {mpd: \"on\"}\n    mpd: \"off\"\n"},
		{"an empty password after a merged one", "output:\n    <<: {password: merged-pw}\n    password: \"\"\n"},
		{"an empty password before a merged one", "output:\n    password: \"\"\n    <<: {password: merged-pw}\n"},
		{"a null port over a merged one", "output:\n    <<: {loopbackPort: 7795}\n    loopbackPort: ~\n"},
		{"the first block of a merge sequence", "output:\n    <<: [{mpd: \"on\"}, {mpd: \"off\", password: listed-pw}]\n"},
		{"a merged block's own merge after its keys", "output:\n    <<: {mpd: \"on\", <<: {mpd: \"off\", conf: /etc/deep.conf}}\n"},
		{"an anchored block merged beside a key", "output:\n    <<: &base {mpd: \"on\", loopbackPort: 7799}\n    mpd: \"off\"\n"},
		{"the player written beside a merged one", "output:\n    player: " + mergeTestPlayer + "\n    <<: {player: " + mergeOtherPlayer + "}\n"},
		{"the player written after a merged one", "output:\n    <<: {player: " + mergeOtherPlayer + "}\n    player: " + mergeTestPlayer + "\n"},
		{"a key written as an alias", "output:\n    conf: &k mpd\n    *k : \"off\"\n"},
		{"an output block a top-level merge names", "<<: {output: {mpd: \"off\", password: top-pw}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			appendToConfig(t, cfgDir, tc.block)
			before := loadInstallConfig(t, cfgDir).Output

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			sameOutput(t, loadInstallConfig(t, cfgDir).Output, before)
			if w := outputNotKeptWarning(out); w != "" {
				t.Errorf("the rewrite of a block that loads warns:\n%s", w)
			}
		})
	}
}

// TestPriorOutputResolvesMergeKeysAsLoadDoes holds resolveOutput to the
// decoder it mirrors: for each block, a config that loads, what priorOutput
// keeps is exactly the output block config.Load read from the same bytes,
// with nothing left out. The shapes go past the rewrite tests': merges
// inside a sequence's blocks, an alias inside a sequence, empty merges, null
// values over merged ones, and a port of 0.
func TestPriorOutputResolvesMergeKeysAsLoadDoes(t *testing.T) {
	for _, block := range []string{
		`{mpd: "off", <<: {mpd: "on"}}`,
		`{<<: {mpd: "on"}, mpd: "off"}`,
		`{<<: [{mpd: "on", conf: /a}, {mpd: "off", conf: /b, endpoint: "127.0.0.1:6601"}]}`,
		`{<<: [{<<: {mpd: "off"}, conf: /a}, {mpd: "on", conf: /b}]}`,
		`{<<: [&a {conf: /a, password: a-pw}, *a, {mpd: "on"}], mpd: "off"}`,
		`{<<: {<<: {<<: {password: deep-pw, mpd: "on"}, mpd: "off"}, conf: /c}, conf: /top}`,
		`{password: "", <<: {password: merged-pw}}`,
		`{password: ~, <<: {password: merged-pw}}`,
		`{loopbackPort: ~, <<: {loopbackPort: 7795}}`,
		`{<<: {loopbackPort: 0}}`,
		`{<<: {}, mpd: "on"}`,
		`{<<: [], mpd: "on"}`,
		`{player: {enabled: true}, <<: {player: {enabled: false, room: Study}}}`,
		`{<<: {player: {enabled: true, room: Study}}, player: ~}`,
		`{<<: {player: ` + mergeTestPlayer + `}, mpd: "off"}`,
		`{endpoint: &e "127.0.0.1:6601", <<: {endpoint: other}, conf: *e}`,
	} {
		t.Run(block, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			body := "libraryRoots:\n  - " + dir + "\ndataDir: " + filepath.Join(dir, "data") + "\noutput: " + block + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.Load(path)
			if err != nil {
				t.Fatalf("the fixture's config does not load: %v", err)
			}
			kept, notKept := priorOutput([]byte(body))
			if len(notKept) != 0 {
				t.Errorf("priorOutput did not keep %q", notKept)
			}
			sameOutput(t, kept, loaded.Output)
		})
	}
}

// TestInitRewriteResolvesMergesBesideASettingItCannotKeep: when a setting
// does not read, the rest of the block is still resolved as Load resolves
// it. A key the block writes hides a merged value for its setting even when
// the key is the one refused (the operator overrode the merged value, so
// the rewrite keeps neither); a merged setting that is refused costs that
// setting alone; and what makes Load refuse the whole block (a key written
// twice in one block, two merge keys in one block, a merge key that names
// no block) costs that key alone, named in the warning. kept is the block
// the saved config must hold.
func TestInitRewriteResolvesMergesBesideASettingItCannotKeep(t *testing.T) {
	const player = "    player: " + mergeTestPlayer + "\n"
	for _, tc := range []struct {
		name, block, kept string
		says              []string
	}{
		{"an empty password beside a refused mode",
			"output:\n    mpd: maybe\n    <<: {password: merged-pw}\n    password: \"\"\n    endpoint: 127.0.0.1:6601\n" + player,
			"output:\n    endpoint: 127.0.0.1:6601\n" + player, []string{"output.mpd (line "}},
		{"a refused key hides the merged value it overrode",
			"output:\n    <<: {mpd: \"on\", conf: /etc/merged.conf}\n    mpd: maybe\n" + player,
			"output:\n    conf: /etc/merged.conf\n" + player, []string{"output.mpd (line ", "): that value is not auto, on or off"}},
		{"a refused merged setting costs itself alone",
			"output:\n    <<: {loopbackPort: 70000, conf: /etc/merged.conf}\n    mpd: \"off\"\n" + player,
			"output:\n    mpd: \"off\"\n    conf: /etc/merged.conf\n" + player, []string{"output.loopbackPort (line ", "): that value is not a port"}},
		{"a key written twice",
			"output:\n    mpd: \"on\"\n    mpd: \"off\"\n" + player,
			"output:\n" + player, []string{"output.mpd (line ", "): it is written twice in one block"}},
		{"two merge keys in one block",
			"output:\n    <<: {mpd: \"on\"}\n    <<: {conf: /etc/merged.conf}\n    password: kept-pw\n" + player,
			"output:\n    password: kept-pw\n" + player, []string{"a merge key of output (line ", "): it is written twice in one block"}},
		{"a merge key that names no block",
			"output:\n    <<: 5\n    mpd: \"off\"\n" + player,
			"output:\n    mpd: \"off\"\n" + player, []string{"a merge key of output (line ", "): it names no block of settings"}},
		{"a merge sequence with something but blocks in it",
			"output:\n    <<: [{conf: /etc/merged.conf}, 5]\n    mpd: \"off\"\n" + player,
			"output:\n    mpd: \"off\"\n" + player, []string{"a merge key of output (line ", "): it names no block of settings"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantDir := filepath.Join(t.TempDir(), "want")
			rewritePostures[0].setUp(t, wantDir)
			appendToConfig(t, wantDir, tc.kept)
			want := loadInstallConfig(t, wantDir).Output

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
			sameOutput(t, loadInstallConfig(t, cfgDir).Output, want)
			warning := outputNotKeptWarning(out)
			for _, s := range tc.says {
				if !strings.Contains(warning, s) {
					t.Errorf("the rewrite's warning does not say %q:\n%s", s, warning)
				}
			}
			for _, secret := range []string{"maybe", "70000", "merged-pw", rewriteSerial[len(rewriteSerial)-6:]} {
				if strings.Contains(warning, secret) {
					t.Errorf("the warning carries %q:\n%s", secret, warning)
				}
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
