package main

import (
	"fmt"
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
		// No key of the block's own wrote it, so no line is certain.
		{"MPD's password on mpd's line in a merged block", "output:\n    <<: {mpd: " + rewriteMPDPassword + "}\n" + player,
			rewriteMPDPassword, []string{"output.mpd (from a merged block): that value is not auto, on or off"}, true},
		{"a loopback port that is not a number", "output:\n    loopbackPort: " + rewriteMPDPassword + "\n" + player,
			rewriteMPDPassword, []string{"output.loopbackPort (line ", "): it does not read as that setting"}, true},
		{"a serial of digits as the loopback port", "output:\n    loopbackPort: 202311050044\n" + player, "202311050044", notAPort, true},
		{"a loopback port that is not a port", "output:\n    loopbackPort: 70000\n" + player, "70000", notAPort, true},
		{"a password that is not text", "output:\n    password: [" + rewriteMPDPassword + "]\n" + player, rewriteMPDPassword,
			[]string{"output.password (line ", "): it does not read as that setting"}, true},
		{"an output that is not a block", "output: " + rewriteMPDPassword + "\n", rewriteMPDPassword,
			[]string{"output (line ", "): it is not a block of output settings"}, false},
		// Named by its line, never its text, which can be a value pasted
		// where a key goes. Round 2 dropped it without a word.
		{"a key output does not take", "output:\n    mpd: \"off\"\n    mdp: maybe\n" + player, "mdp",
			[]string{"a key of output (line ", "): it is not an output setting"}, true},
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

// TestPriorOutputResolvesMergeKeysAsLoadDoes holds priorOutput to Load: for
// each block, a config that loads, what priorOutput keeps is exactly the
// output block config.Load read from the same bytes, with nothing left out.
// The shapes go past the rewrite tests': merges inside a sequence's blocks,
// an alias inside a sequence, empty merges, null values over merged ones, a
// port of 0, and the shapes round 2's resolver read otherwise than the
// decoder (B505's round 3): a key written as !!binary, which the decoder
// reads as the text it encodes (Codex); a merge sequence past the 64 blocks
// the resolver followed, which dropped a setting a valid config holds
// (Codex); and a setting a merged block writes twice, once through an alias,
// which the decoder takes the first of and the resolver dropped (the Claude
// API); and values with a tag. And each beside a whole config
// (wholeConfigShapes, round 4): round 3 decoded the output block on its own,
// and yaml.v3 counts aliases across the document, so a block whose merges
// expand aliases tenfold a level beside 1,000 library roots, which Load
// takes, failed that decode as "excessive aliasing" and lost its password
// (Codex).
func TestPriorOutputResolvesMergeKeysAsLoadDoes(t *testing.T) {
	var shapes []outputShape
	for _, block := range []string{
		`{!!binary cGFzc3dvcmQ=: "", <<: {password: old-pw}}`,
		`{<<: [` + strings.Repeat(`{}, `, 63) + `{password: saved-pw}]}`,
		`{<<: [` + strings.Repeat(`{}, `, 200) + `{password: saved-pw, mpd: "off"}]}`,
		`{conf: &k mpd, <<: {mpd: "on", *k: "off"}}`,
		`{loopbackPort: !!int "7795", password: !!binary bXBkLXB3}`,
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
		shapes = append(shapes, outputShape{name: block, block: block})
	}
	for _, s := range append(shapes, wholeConfigShapes()...) {
		t.Run(s.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			body := "libraryRoots:\n  - " + dir + "\n" + s.roots + "dataDir: " + filepath.Join(dir, "data") + "\n" + s.others
			if s.block != "" {
				body += "output: " + s.block + "\n"
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.Load(path)
			if err != nil {
				t.Fatalf("the fixture's config does not load: %v", err)
			}
			if s.roots+s.others != "" {
				if held, err := yaml.Marshal(loaded.Output); err != nil || string(held) == "{}\n" {
					t.Fatalf("the fixture's output block loads as nothing (%v), so it pins nothing", err)
				}
			}
			kept, notKept := priorOutput([]byte(body))
			if len(notKept) != 0 {
				t.Errorf("priorOutput did not keep %q", notKept)
			}
			sameOutput(t, kept, loaded.Output)
		})
	}
}

// outputShape is an output block for TestPriorOutputResolvesMergeKeysAsLoadDoes
// and what else its config holds: roots are list entries after the fixture's
// own library root, others top-level keys before the block, and block the
// output block, none when it is empty.
type outputShape struct{ name, roots, others, block string }

// wholeConfigShapes are output blocks beside a config with much else in it,
// for TestPriorOutputResolvesMergeKeysAsLoadDoes: what the decoder decodes
// outside the block changes what it counts, so a block read on its own can
// be refused where Load takes it.
func wholeConfigShapes() []outputShape {
	var roots strings.Builder
	for i := 1; i < 1000; i++ {
		fmt.Fprintf(&roots, "  - /music/r%d\n", i)
	}
	tenfold := func(anchor string) string {
		return strings.TrimSuffix(strings.Repeat("*"+anchor+", ", 10), ", ")
	}
	var endpoints, servers strings.Builder
	endpoints.WriteString("customEndpoints:\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&endpoints, "  - https://e%d.example:7788\n", i)
	}
	servers.WriteString("upnpUpstream:\n  servers:\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&servers, "    - {name: s%d, udn: \"uuid:%d\"}\n", i, i)
	}
	return []outputShape{
		{"1,000 roots beside merges whose aliases expand tenfold a level (Codex)", roots.String(), "",
			"{password: saved-pw, <<: [&a {}, &b {<<: [" + tenfold("a") + "]}, &c {<<: [" + tenfold("b") + "]}, " +
				"&d {<<: [" + tenfold("c") + "]}, &e {<<: [" + tenfold("d") + "]}]}"},
		{"1,000 roots beside a player and merged MPD settings", roots.String(), "",
			"{<<: [{mpd: \"off\", conf: /etc/mpd-a.conf}, {password: listed-pw}], player: " + mergeTestPlayer + "}"},
		{"a whole config whose anchors the block aliases",
			"", "libraryName: &n Kitchen\nscanIntervalSec: &port 7799\n" + endpoints.String() + servers.String() +
				"update: {checkIntervalHours: 12}\nbackup: {intervalHours: 24}\nanalysis: {workers: 2}\n",
			"{conf: *n, loopbackPort: *port, <<: {mpd: \"off\"}, password: kept-pw}"},
		{"a whole config whose top-level merge names the block",
			"", "libraryName: Study\n" + endpoints.String() +
				"<<: {output: {mpd: \"off\", password: top-pw, player: " + mergeOtherPlayer + "}, scanIntervalSec: 900}\n",
			""},
	}
}

// TestInitRewriteResolvesMergesBesideASettingItCannotKeep: a block yaml.v3
// decodes, merges and all, and whose setting Validate refuses, keeps the
// rest as the decoder resolved it. A key the block writes hides a merged
// value for its setting even when the key is the one refused (the operator
// overrode the merged value, so the rewrite keeps neither), a merged
// setting that is refused costs that setting alone, and the warning names
// the line of the block's own key or says the value was merged. A block
// yaml.v3 does not decode is one Load refuses, and the rewrite keeps of it
// only what it can name with certainty (B505's round 3): with plain keys,
// each setting but one written twice; with a merge key, an alias, a tag
// among its keys, nothing, since which setting a value is for is the
// decoder's to say (round 2's resolver kept what it resolved on its own,
// and two of these shapes it resolved otherwise without a word, the Claude
// API); and a block written twice, nothing. kept is the block the saved
// config must hold.
func TestInitRewriteResolvesMergesBesideASettingItCannotKeep(t *testing.T) {
	const player = "    player: " + mergeTestPlayer + "\n"
	notCertain := []string{"output (line ", "): it does not load, and a key of it is a merge key (<<), an alias, tagged or not plain text",
		"none of it is kept"}
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
			"output:\n    mpd: \"off\"\n    conf: /etc/merged.conf\n" + player,
			[]string{"output.loopbackPort (from a merged block): that value is not a port"}},
		{"a refused setting whose key is an alias",
			"output:\n    conf: &k mpd\n    *k : maybe\n" + player,
			"output:\n    conf: mpd\n" + player, []string{"output.mpd (line ", "): that value is not auto, on or off"}},
		{"a key written twice",
			"output:\n    mpd: \"on\"\n    mpd: \"off\"\n" + player,
			"output:\n" + player, []string{"output.mpd (lines ", "): it is written more than once in one block, so no value of it is kept"}},
		{"two merge keys in one block",
			"output:\n    <<: {mpd: \"on\"}\n    <<: {conf: /etc/merged.conf}\n    password: kept-pw\n" + player,
			"output:\n", notCertain},
		{"a merge key that names no block",
			"output:\n    <<: 5\n    mpd: \"off\"\n" + player,
			"output:\n", notCertain},
		{"a merge sequence with something but blocks in it",
			"output:\n    <<: [{conf: /etc/merged.conf}, 5]\n    mpd: \"off\"\n" + player,
			"output:\n", notCertain},
		{"a merged block that writes a key twice",
			"output:\n    mpd: \"off\"\n    <<: {mpd: \"on\", mpd: auto}\n" + player,
			"output:\n", notCertain},
		{"a merge key beside a quoted one",
			"output:\n    \"<<\": {mpd: \"on\"}\n    <<: {conf: /etc/merged.conf}\n    mpd: \"off\"\n" + player,
			"output:\n", notCertain},
		{"a key written as an alias in a block that does not load",
			"output:\n    conf: &k mpd\n    *k : \"off\"\n    loopbackPort: maybe\n" + player,
			"output:\n", notCertain},
		{"a key with a tag in a block that does not load",
			"output:\n    !!str mpd: \"off\"\n    loopbackPort: maybe\n" + player,
			"output:\n", notCertain},
		{"a block written twice, once as !!binary",
			"output:\n    mpd: \"off\"\n" + player + "!!binary b3V0cHV0:\n    mpd: \"on\"\n",
			"output:\n", []string{"output (lines ", "): it is written twice, which Load refuses, so none of it is kept"}},
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
