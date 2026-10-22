package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// dacSerial is the GO bar's serial in the pi5-go-bar tree.
const dacSerial = "GOBAR-7Q2X-91KZ-4471"

// dacUSB1 is the Pi 5 tree's first USB 2 bus.
const dacUSB1 = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"

// dacLeaks reports whether out carries the serial, or six bytes of it.
func dacLeaks(out, serial string) bool {
	for i := 0; i+6 <= len(serial); i++ {
		if strings.Contains(out, serial[i:i+6]) {
			return true
		}
	}
	return false
}

// dacTree lays out pi5-go-bar and returns its root.
func dacTree(t *testing.T) string {
	t.Helper()
	return sysfstest.Build(t, filepath.Join(repoRootForCitations(t), "internal", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
}

// dacPlug is the manifest of a unit at dir whose interface 0 is card n.
func dacPlug(dir string, n int, vid, pid, serial string) string {
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	base := filepath.Base(dir)
	return "usbdev " + dir + " vendor=" + vid + " product=" + pid + s + "\nusbcard " + strconv.Itoa(n) + " " + dir + "/" + base + ":1.0\n" +
		"proccard " + strconv.Itoa(n) + " DAC" + strconv.Itoa(n) + " \"Synthetic DAC\" usb=" + vid + ":" + pid + " pcm=0.0\n"
}

// dacConfig writes a loopback bridge.yaml with an ephemeral admin port (so
// no probe dials one) and output written under output:, and returns its
// path.
func dacConfig(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	music := filepath.Join(dir, "music")
	if err := os.Mkdir(music, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "listenAddress: \":7788\"\nadminAddress: 127.0.0.1:0\nscanIntervalSec: 3600\nlibraryRoots: ['" + music + "']\n" +
		"dataDir: '" + filepath.Join(dir, "data") + "'\n"
	if output != "" {
		body += "output:\n" + output
	}
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// dacEnv is the env the tests run the commands on: Linux, the fixed clock,
// and a LAN of eth0 at 192.168.1.20.
func dacEnv() outputBindEnv {
	return outputBindEnv{goos: "linux", now: func() time.Time { return outputTestNow }, lan: func() ([]lan.Interface, error) {
		return []lan.Interface{{Name: "eth0", Flags: net.FlagUp | net.FlagRunning | net.FlagMulticast,
			Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.20/24")}}}, nil
	}}
}

// runDAC runs `bridge output dac` with args on env.
func runDAC(t *testing.T, env outputBindEnv, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	code = runOutputDACBinding(context.Background(), args, env, &so, &se)
	return so.String(), se.String(), code
}

// treeFlags are the flags that read root's two trees.
func treeFlags(root string) []string {
	return []string{"--proc-root", filepath.Join(root, "proc"), "--sysfs-root", filepath.Join(root, "sys")}
}

// TestOutputDACInspectShowsEachCardAndHowTheBindingResolves: inspect prints
// each card's unit, its serial's label and its port, the player's state,
// the LAN and the resolution, and never the serial, as text and as JSON.
func TestOutputDACInspectShowsEachCardAndHowTheBindingResolves(t *testing.T) {
	root := dacTree(t)
	cfg := dacConfig(t, "  player:\n    enabled: true\n    device:\n      binding: serial\n      vendorId: 20b1\n      productId: \"3009\"\n      serial: "+dacSerial+"\n")
	args := append([]string{"inspect", "--config", cfg}, treeFlags(root)...)
	out, errOut, code := runDAC(t, dacEnv(), args...)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, line := range []string{
		"player: on\n",
		"LAN: ready on eth0 (192.168.1.20)\n",
		"binding: serial 20b1:3009 (…4471), device 0 subdevice 0\n",
		"resolves: available: card 5 is 20b1:3009 with the bound serial (…4471), device 0 subdevice 0\n",
		"card 5: Audio [iFi (by AMR) HD USB Audio]\n  identity:  usb 20b1:3009, serial …4471\n  port:      platform-xhci-hcd.0/usb2-1:1.0, interface 0\n  playback:  device 0 subdevice 0\n",
		"card 0: vc4hdmi0 [vc4-hdmi-0]\n  identity:  identity_unsupported: a platform card, not USB",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("inspect lacks %q:\n%s", line, out)
		}
	}
	if dacLeaks(out+errOut, dacSerial) {
		t.Errorf("inspect printed the serial:\n%s", out)
	}
	out, errOut, code = runDAC(t, dacEnv(), append(args, "--json")...)
	if code != 0 {
		t.Fatalf("json: exit %d, stderr %q", code, errOut)
	}
	var rep struct {
		Player struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		} `json:"player"`
		LAN struct {
			State string `json:"state"`
		} `json:"lan"`
		Binding struct {
			Binding string `json:"binding"`
			Serial  string `json:"serial"`
		} `json:"binding"`
		Resolution struct {
			State string `json:"state"`
			Card  *int   `json:"card"`
		} `json:"resolution"`
		Cards []struct {
			Index  int     `json:"index"`
			State  string  `json:"state"`
			Serial *string `json:"serial"`
			Port   string  `json:"port"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !rep.Player.Enabled || rep.LAN.State != "ready" || rep.Binding.Binding != "serial" || rep.Binding.Serial != "…4471" ||
		rep.Resolution.State != "available" || rep.Resolution.Card == nil || *rep.Resolution.Card != 5 || len(rep.Cards) != 3 {
		t.Errorf("json report: %+v", rep)
	}
	if c := rep.Cards[2]; c.Index != 5 || c.State != "usb" || c.Serial == nil || *c.Serial != "…4471" || c.Port != "platform-xhci-hcd.0/usb2-1:1.0" {
		t.Errorf("card 5 in json: %+v", c)
	}
	if dacLeaks(out, dacSerial) {
		t.Errorf("--json printed the serial:\n%s", out)
	}
}

// TestOutputDACInspectNeverQuotesASerialInTheWrongField: a serial pasted
// into the binding, the transport, dsdMode, mixer.type, an id or the port,
// written as a key, or, all digits, into an endpoint number, reaches
// inspect as the field's refusal, in the player's reason and the
// resolution's, and is printed nowhere, as text or as JSON, whether the
// player is enabled or not: the binding's own line shows such a field as
// "that value".
func TestOutputDACInspectNeverQuotesASerialInTheWrongField(t *testing.T) {
	root := dacTree(t)
	const model = "      vendorId: 20b1\n      productId: \"3009\"\n"
	const digits = "202311050044"
	for _, tc := range []struct {
		name, block, says string
		// whenOff: the refusal shows with the player off too, as the
		// resolution's reason (a binding that is refused, or a block
		// that does not read); a refusal of the player's own fields is
		// its reason only when it is enabled.
		whenOff bool
	}{
		{"binding", "    device:\n      binding: " + dacSerial + "\n" + model, "device.binding: that value", true},
		{"transport", "    device:\n      transport: " + dacSerial + "\n      binding: uniqueModel\n" + model, "device.transport: that value", true},
		{"dsdMode", "    dsdMode: " + dacSerial + "\n    device:\n      binding: uniqueModel\n" + model, "dsdMode: that value", false},
		{"mixer type", "    mixer:\n      type: " + dacSerial + "\n    device:\n      binding: uniqueModel\n" + model, "mixer.type: that value", false},
		{"vendorId", "    device:\n      binding: uniqueModel\n      vendorId: " + dacSerial + "\n      productId: \"3009\"\n",
			"binding: uniqueModel that value:3009", true},
		{"productId", "    device:\n      binding: uniqueModel\n      vendorId: 20b1\n      productId: " + dacSerial + "\n",
			"binding: uniqueModel 20b1:that value", true},
		{"port", "    device:\n      binding: port\n      port: " + dacSerial + "\n", "binding: port that value", true},
		{"pcmDevice", "    device:\n      binding: uniqueModel\n" + model + "      pcmDevice: " + digits + "\n",
			"binding: uniqueModel 20b1:3009, an endpoint out of range", true},
		{"a key", "    " + dacSerial + ": true\n", "that key is not a player setting", true},
		{"a device key", "    device:\n      " + dacSerial + ": 1\n", "that key is not a device setting", true},
	} {
		for _, enabled := range []string{"", "    enabled: true\n"} {
			cfgPath := dacConfig(t, "  player:\n"+enabled+tc.block)
			for _, format := range [][]string{nil, {"--json"}} {
				args := append(append([]string{"inspect", "--config", cfgPath}, treeFlags(root)...), format...)
				out, errOut, code := runDAC(t, dacEnv(), args...)
				name := tc.name + " " + strings.TrimSpace(enabled) + " " + strings.Join(format, "")
				if code != 0 {
					t.Fatalf("%s: exit %d, stderr %q", name, code, errOut)
				}
				if dacLeaks(out+errOut, dacSerial) || dacLeaks(out+errOut, digits) {
					t.Errorf("%s: inspect quotes the serial:\n%s%s", name, out, errOut)
				}
				// The binding's own line is the text form's.
				if format != nil && strings.HasPrefix(tc.says, "binding: ") {
					continue
				}
				if (enabled != "" || tc.whenOff) && !strings.Contains(out, tc.says) {
					t.Errorf("%s: inspect does not say %q:\n%s", name, tc.says, out)
				}
			}
		}
	}
}

// TestOutputDACInspectWorksWithoutAConfigOrABinding: with no config the
// cards are still read, and a config with no binding says how to make one.
func TestOutputDACInspectWorksWithoutAConfigOrABinding(t *testing.T) {
	root := dacTree(t)
	out, errOut, code := runDAC(t, dacEnv(), append([]string{"inspect", "--config", filepath.Join(t.TempDir(), "none.yaml")}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "config: ") || !strings.Contains(out, "card 5: Audio") {
		t.Errorf("no config: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, _, code = runDAC(t, dacEnv(), append([]string{"inspect", "--config", dacConfig(t, "")}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "resolves: not_configured: no device is bound: bridge output dac bind <card>") ||
		!strings.Contains(out, "player: off (output.player.enabled is not true)") {
		t.Errorf("no binding: exit %d:\n%s", code, out)
	}
}

// TestOutputDACBindWritesTheSerialToTheFileAndPrintsNone: bind writes the
// card's unit into the config file, 0600, the serial whole there and
// nowhere on the terminal, keeps the block's other fields, and the binding
// it wrote resolves to that card.
func TestOutputDACBindWritesTheSerialToTheFileAndPrintsNone(t *testing.T) {
	root := dacTree(t)
	cfgPath := dacConfig(t, "  player:\n    room: Study\n")
	out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "bound card 5 by serial: 20b1:3009 (…4471) at platform-xhci-hcd.0/usb2-1:1.0, device 0 subdevice 0") {
		t.Errorf("stdout %q", out)
	}
	if dacLeaks(out+errOut, dacSerial) {
		t.Errorf("bind printed the serial:\n%s%s", out, errOut)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), dacSerial) {
		t.Errorf("the file lacks the serial:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(cfgPath); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("the config's mode is %v, %v; want 0600", st.Mode().Perm(), err)
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Output.Player.Device
	if cfg.Output.Player.Room != "Study" || d.Binding != config.BindingSerial || d.VendorID != "20b1" || d.ProductID != "3009" ||
		!d.Serial.Equal(usbid.New(dacSerial)) || d.Port != "" || d.Transport != "usb" {
		t.Errorf("the config holds %+v", cfg.Output.Player)
	}
	out, _, code = runDAC(t, dacEnv(), append([]string{"inspect", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "resolves: available: card 5") {
		t.Errorf("after bind, inspect: exit %d\n%s", code, out)
	}
	for _, kind := range []string{"uniqueModel", "port"} {
		if _, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", kind, "--config", cfgPath}, treeFlags(root)...)...); code != 0 {
			t.Fatalf("bind by %s: exit %d, %s", kind, code, errOut)
		}
		b, _ := os.ReadFile(cfgPath)
		if strings.Contains(string(b), dacSerial) {
			t.Errorf("a %s binding kept the serial:\n%s", kind, b)
		}
	}
}

// TestOutputDACBindRefusesWhatWouldNotResolve: an ambiguous binding, a card
// that cannot be bound and a config whose player block does not read are
// refused, and the file is left as it was.
func TestOutputDACBindRefusesWhatWouldNotResolve(t *testing.T) {
	root := dacTree(t)
	sysfstest.Apply(t, root, dacPlug(dacUSB1+"/1-2", 6, "1234", "0f01", "")+dacPlug(dacUSB1+"/1-3", 7, "1234", "0f01", ""))
	cfgPath := dacConfig(t, "")
	unread := dacConfig(t, "  player:\n    enabled: true\n    resample: soxr\n")
	for _, tc := range []struct {
		name string
		cfg  string
		args []string
		code int
		says string
	}{
		{"two units of a model", cfgPath, []string{"bind", "6", "--binding", "uniqueModel"}, 1, "device_ambiguous"},
		{"a unit with no serial", cfgPath, []string{"bind", "6", "--binding", "serial"}, 1, "has no serial"},
		{"an HDMI card", cfgPath, []string{"bind", "0", "--binding", "port"}, 1, "identity_unsupported"},
		{"no such card", cfgPath, []string{"bind", "9", "--binding", "serial"}, 1, "there is no card 9"},
		{"no such endpoint", cfgPath, []string{"bind", "5", "--binding", "serial", "--pcm-device", "1"}, 1, "endpoint_missing"},
		{"a block that does not read", unread, []string{"bind", "5", "--binding", "serial"}, 1, "does not read"},
		{"no binding kind", cfgPath, []string{"bind", "5"}, 2, "--binding is serial, uniqueModel or port"},
		{"another binding kind", cfgPath, []string{"bind", "5", "--binding", "card"}, 2, "--binding is serial, uniqueModel or port"},
		{"no card", cfgPath, []string{"bind", "--binding", "serial"}, 2, "the card comes first"},
		{"a card that is not a number", cfgPath, []string{"bind", "Audio", "--binding", "serial"}, 2, "a card number"},
		{"an argument after the flags", cfgPath, []string{"bind", "5", "--binding", "serial", "extra"}, 2, "unexpected argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := os.ReadFile(tc.cfg)
			args := append(append(tc.args, "--config", tc.cfg), treeFlags(root)...)
			_, errOut, code := runDAC(t, dacEnv(), args...)
			if code != tc.code || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stderr %q; want %d and %q", code, errOut, tc.code, tc.says)
			}
			after, _ := os.ReadFile(tc.cfg)
			if !bytes.Equal(before, after) {
				t.Errorf("a refused bind changed the file:\n%s", after)
			}
		})
	}
}

// TestOutputDACBindRefusesWhileABridgeServesTheDataDir: a running serve
// holds the data dir's lock, and would write its own copy of the config
// over the binding at its next settings save, so bind refuses until it
// stops.
func TestOutputDACBindRefusesWhileABridgeServesTheDataDir(t *testing.T) {
	root := dacTree(t)
	cfgPath := dacConfig(t, "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockServeDataDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)
	_, errOut, code := runDAC(t, dacEnv(), args...)
	if code != 1 || !strings.Contains(errOut, "already running on the data dir") {
		t.Errorf("bind beside a serve: exit %d, stderr %q", code, errOut)
	}
	release()
	if _, errOut, code := runDAC(t, dacEnv(), args...); code != 0 {
		t.Errorf("bind once the serve stopped: exit %d, %s", code, errOut)
	}
}

// TestOutputDACVerifyCardRefusesAnotherDAC: --verify-card runs the check
// after open against a card's sysfs reading without opening its PCM: the
// bound card passes, and another DAC's card is refused, naming both units
// and neither serial.
func TestOutputDACVerifyCardRefusesAnotherDAC(t *testing.T) {
	root := dacTree(t)
	sysfstest.Apply(t, root, dacPlug(dacUSB1+"/1-2", 6, "1234", "0f04", "ANOTHER-DAC-0000017"))
	cfgPath := dacConfig(t, "")
	if _, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...); code != 0 {
		t.Fatalf("bind: %d %s", code, errOut)
	}
	out, errOut, code := runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "6", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 1 || !strings.Contains(out, "verify card 6: refused:") || !strings.Contains(out, "1234:0f04") || !strings.Contains(out, "20b1:3009") {
		t.Errorf("another DAC: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if dacLeaks(out+errOut, dacSerial) || dacLeaks(out+errOut, "ANOTHER-DAC-0000017") {
		t.Errorf("the refusal printed a serial:\n%s", out)
	}
	out, _, code = runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "5", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "verify card 5: passes: card 5 is the bound unit") {
		t.Errorf("the bound card: exit %d\n%s", code, out)
	}
	sysfstest.Apply(t, root, "unplug "+dacUSB1+"/1-1\n")
	out, _, code = runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "5", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 1 || !strings.Contains(out, "verify card 5: the binding does not resolve (device_missing)") {
		t.Errorf("an unplugged unit: exit %d\n%s", code, out)
	}
}

// TestOutputDACBindKeepsThePlayersUUID: a bind on an enabled player makes
// the UUID once; a rebind to another card and a config rewrite keep it.
func TestOutputDACBindKeepsThePlayersUUID(t *testing.T) {
	root := dacTree(t)
	sysfstest.Apply(t, root, dacPlug(dacUSB1+"/1-2", 6, "1234", "0f04", "ANOTHER-DAC-0000017"))
	cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
	out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "player uuid: ") || !strings.Contains(out, "(made now, kept in player-uuid)") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := config.ReadPlayerUUID(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, code := runDAC(t, dacEnv(), append([]string{"bind", "6", "--binding", "port", "--config", cfgPath}, treeFlags(root)...)...); code != 0 {
		t.Fatal("rebind failed")
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if again, err := config.ReadPlayerUUID(cfg.DataDir); err != nil || again != id {
		t.Errorf("the UUID moved from %s to %s, %v", id, again, err)
	}
	if out, _, _ := runDAC(t, dacEnv(), append([]string{"inspect", "--config", cfgPath}, treeFlags(root)...)...); !strings.Contains(out, "player uuid: "+id) {
		t.Errorf("inspect does not show the UUID:\n%s", out)
	}
}

// TestOutputDACInspectReportsTheLANAsAState: no eligible interface yet is
// lan_unavailable, a state inspect reports, not a failure.
func TestOutputDACInspectReportsTheLANAsAState(t *testing.T) {
	root := dacTree(t)
	env := dacEnv()
	env.lan = func() ([]lan.Interface, error) {
		return []lan.Interface{{Name: "eth0", Flags: net.FlagUp | net.FlagRunning | net.FlagMulticast,
			Addrs: []netip.Prefix{netip.MustParsePrefix("fe80::1/64")}}}, nil
	}
	out, _, code := runDAC(t, env, append([]string{"inspect", "--config", dacConfig(t, "")}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "LAN: lan_unavailable: no LAN interface yet: eth0 has no private or link-local IPv4 address yet") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// TestOutputDACBindingRefusesTheHostsTreesOffLinux: off Linux, inspect and
// bind refuse before reading anything unless both trees are named.
func TestOutputDACBindingRefusesTheHostsTreesOffLinux(t *testing.T) {
	env := dacEnv()
	env.goos = "darwin"
	for _, args := range [][]string{{"inspect"}, {"bind", "5", "--binding", "serial"}, {"inspect", "--proc-root", t.TempDir()}} {
		_, errOut, code := runDAC(t, env, append(args, "--config", dacConfig(t, ""))...)
		if code != 1 || !strings.Contains(errOut, "ALSA is Linux only, and this is darwin") {
			t.Errorf("%q: exit %d, stderr %q", args, code, errOut)
		}
	}
	root := dacTree(t)
	if _, errOut, code := runDAC(t, env, append([]string{"inspect", "--config", dacConfig(t, "")}, treeFlags(root)...)...); code != 0 {
		t.Errorf("with both trees named: exit %d, %s", code, errOut)
	}
}
