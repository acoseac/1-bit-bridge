package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
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
	return dacPlugOn(dir, 0, n, vid, pid, serial)
}

// dacPlugOn is dacPlug with the card on interface iface (the Hugo 2's is
// interface 1).
func dacPlugOn(dir string, iface, n int, vid, pid, serial string) string {
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	base := filepath.Base(dir)
	return "usbdev " + dir + " vendor=" + vid + " product=" + pid + s + "\nusbcard " + strconv.Itoa(n) + " " + dir + "/" + base + ":1." + strconv.Itoa(iface) + "\n" +
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
// TestOutputDACBindSaysTheBindingAppliesAtTheNextStart: bind writes the
// config file only while no bridge serves the data dir, and a serve reads
// output.player when it starts (B470 PR 4a2: nothing reloads the file), so
// bind says the binding applies when bridge serve next starts, whether the
// player is on or off.
func TestOutputDACBindSaysTheBindingAppliesAtTheNextStart(t *testing.T) {
	root := dacTree(t)
	for _, block := range []string{"  player:\n    room: Study\n", "  player:\n    enabled: true\n"} {
		cfgPath := dacConfig(t, block)
		out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "uniqueModel", "--config", cfgPath}, treeFlags(root)...)...)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		if !strings.Contains(out, "the binding applies when bridge serve next starts") {
			t.Errorf("bind with %q does not say when the binding applies:\n%s", block, out)
		}
	}
}

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

// TestOutputDACBindSavesTheConfigItReadUnderTheLock: bind reads the config
// again once it holds the data dir's lock, so what another writer committed
// between bind's first read and the lock (a restore, a serve's last
// settings save before it stopped) is kept beside the binding, never
// written over with the copy read before it. A data dir that moved in
// between, whose lock bind does not hold, and a player block that no longer
// reads are refused, and the file is left as the other writer left it.
func TestOutputDACBindSavesTheConfigItReadUnderTheLock(t *testing.T) {
	root := dacTree(t)
	for _, tc := range []struct {
		name string
		// rewrite is what the other writer does to the file's text.
		rewrite func(t *testing.T, text string) string
		// says is bind's refusal, "" when the bind goes through.
		says string
	}{
		{"a restore", func(t *testing.T, text string) string {
			return strings.Replace(text, "room: Study", "room: Kitchen", 1) + "libraryName: Restored\n"
		}, ""},
		{"a data dir moved", func(t *testing.T, text string) string {
			return regexpDataDir.ReplaceAllLiteralString(text, "dataDir: '"+filepath.Join(t.TempDir(), "moved")+"'")
		}, "data dir"},
		{"a block that no longer reads", func(t *testing.T, text string) string {
			return strings.Replace(text, "room: Study", "room: Study\n    resample: soxr", 1)
		}, "does not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := dacConfig(t, "  player:\n    room: Study\n")
			var landed []byte
			env := dacEnv()
			env.beforeLock = func() {
				b, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Error(err)
					return
				}
				landed = []byte(tc.rewrite(t, string(b)))
				if err := os.WriteFile(cfgPath, landed, 0o600); err != nil {
					t.Error(err)
				}
			}
			_, errOut, code := runDAC(t, env, append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			after, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.says != "" {
				if code != 1 || !strings.Contains(errOut, tc.says) || !bytes.Equal(after, landed) {
					t.Errorf("exit %d, stderr %q; the file is\n%s\nwant a refusal naming %q and the file as the other writer left it", code, errOut, after, tc.says)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LibraryName != "Restored" || cfg.Output.Player.Room != "Kitchen" || cfg.Output.Player.Device.Binding != config.BindingSerial {
				t.Errorf("bind wrote over what landed before its lock: libraryName %q, room %q, binding %q",
					cfg.LibraryName, cfg.Output.Player.Room, cfg.Output.Player.Device.Binding)
			}
		})
	}
}

// TestOutputDACBindNeverWritesOverAnUpdateAfterItsRead: library add and
// remove and init save the config without the data dir's lock, so one of
// them can save between bind's read under the lock and its own save. bind
// replaces the file only while it still holds the bytes bind read: an
// update, one of the same size whose modification time was put back (which
// a look at the size and the time would miss), and a file removed are each
// refused, and the file is left as the other writer left it.
func TestOutputDACBindNeverWritesOverAnUpdateAfterItsRead(t *testing.T) {
	root := dacTree(t)
	for _, tc := range []struct {
		name string
		// land is what the other writer does to the file at path, and
		// returns what it left there, nil for no file.
		land func(t *testing.T, path string) []byte
	}{
		{"an update", func(t *testing.T, path string) []byte {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, "libraryName: Added\n"...)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			return b
		}},
		{"an update of the same size, its modification time put back", func(t *testing.T, path string) []byte {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b = bytes.Replace(b, []byte("room: Study"), []byte("room: Stody"), 1)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
				t.Fatal(err)
			}
			if again, err := os.Stat(path); err != nil || again.Size() != st.Size() || !again.ModTime().Equal(st.ModTime()) {
				t.Fatalf("premise: the size and the time are not as they were: %v", err)
			}
			return b
		}},
		{"the file removed", func(t *testing.T, path string) []byte {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := dacConfig(t, "  player:\n    room: Study\n")
			var landed []byte
			env := dacEnv()
			env.beforeSave = func() { landed = tc.land(t, cfgPath) }
			out, errOut, code := runDAC(t, env, append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
			if code != 1 || !strings.Contains(errOut, "changed after bind read it") || !strings.Contains(errOut, "run bind again") {
				t.Errorf("exit %d, stdout %q, stderr %q; want a refusal saying the file changed", code, out, errOut)
			}
			after, err := os.ReadFile(cfgPath)
			switch {
			case landed == nil && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("the removed config is back: %v\n%s", err, after)
			case landed != nil && !bytes.Equal(after, landed):
				t.Errorf("bind wrote over the update; the file is\n%s", after)
			}
			if dacLeaks(out+errOut, dacSerial) {
				t.Errorf("the refusal printed the serial:\n%s%s", out, errOut)
			}
		})
	}
}

// regexpDataDir is the dataDir line dacConfig writes.
var regexpDataDir = regexp.MustCompile(`(?m)^dataDir: '[^']*'`)

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

// TestOutputDACVerifyCardFollowsAPortBinding is the hardware gate's step
// with the CLI: bind by port writes the port alone and says so; with
// another model in that port, its card on interface 1, inspect resolves the
// binding to it and --verify-card passes its card, as it does for the form
// B470 PR 4a's bind wrote, and the bound unit, now in another port, is
// refused, naming the port; no serial is printed. A unit that carries two
// cards is bound with the card's configuration and interface after the
// port, which bind says it records.
func TestOutputDACVerifyCardFollowsAPortBinding(t *testing.T) {
	root := dacTree(t)
	cfgPath := dacConfig(t, "")
	out, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "port", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "bound card 5 by port platform-xhci-hcd.0/usb2-1, whatever unit is in it: now 20b1:3009 (…4471) at platform-xhci-hcd.0/usb2-1:1.0, device 0 subdevice 0") {
		t.Fatalf("bind: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Output.Player.Device; d.Binding != config.BindingPort || d.Port != "platform-xhci-hcd.0/usb2-1" || d.Serial.Present() || d.VendorID != "" {
		t.Errorf("bind wrote %+v; want the port alone", d)
	}
	older := dacConfig(t, "  player:\n    device:\n      binding: port\n      port: platform-xhci-hcd.0/usb2-1:1.0\n")

	const hugo = "HUGO2-UNIT-0000000042"
	sysfstest.Apply(t, root, "unplug "+dacUSB1+"/1-1\n"+dacPlugOn(dacUSB1+"/1-1", 1, 3, "1234", "0c12", hugo)+dacPlug(dacUSB1+"/1-2", 5, "20b1", "3009", dacSerial))
	for _, path := range []string{cfgPath, older} {
		out, errOut, code = runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "3", "--config", path}, treeFlags(root)...)...)
		if code != 0 || !strings.Contains(out, "resolves: available: card 3 is at platform-xhci-hcd.0/usb2-1, device 0 subdevice 0\n") ||
			!strings.Contains(out, "verify card 3: passes: card 3 is at the bound port, 1234:0c12 (…0042) at platform-xhci-hcd.0/usb2-1:1.1\n") {
			t.Errorf("%s: the other model: exit %d, stdout %q, stderr %q", path, code, out, errOut)
		}
		out, errOut, code = runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "5", "--config", path}, treeFlags(root)...)...)
		if code != 1 || !strings.Contains(out, "verify card 5: refused:") || !strings.Contains(out, "not at the bound port platform-xhci-hcd.0/usb2-1") {
			t.Errorf("%s: the bound unit in another port: exit %d, stdout %q, stderr %q", path, code, out, errOut)
		}
		if dacLeaks(out+errOut, dacSerial) || dacLeaks(out+errOut, hugo) {
			t.Errorf("%s: inspect printed a serial:\n%s%s", path, out, errOut)
		}
	}

	sysfstest.Apply(t, root, "unplug "+dacUSB1+"/1-1\nusbdev "+dacUSB1+"/1-1 vendor=1234 product=0c20\n"+
		"usbcard 3 "+dacUSB1+"/1-1/1-1:1.0\nproccard 3 DAC3 \"Synthetic composite DAC\" usb=1234:0c20 pcm=0.0\n"+
		"usbcard 4 "+dacUSB1+"/1-1/1-1:1.2\nproccard 4 Other4 \"Synthetic composite DAC, second card\" driver=snd-synthetic pcm=0.0\n")
	out, errOut, code = runDAC(t, dacEnv(), append([]string{"bind", "4", "--binding", "port", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "bound card 4 by port platform-xhci-hcd.0/usb2-1:1.2,") ||
		!strings.Contains(out, "carries more than one card") {
		t.Fatalf("bind a unit with two cards: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if cfg, err := config.Load(cfgPath); err != nil || cfg.Output.Player.Device.Port != "platform-xhci-hcd.0/usb2-1:1.2" {
		t.Errorf("bind of a unit with two cards wrote %+v, %v", cfg.Output.Player.Device, err)
	}
	out, _, code = runDAC(t, dacEnv(), append([]string{"inspect", "--verify-card", "4", "--config", cfgPath}, treeFlags(root)...)...)
	if code != 0 || !strings.Contains(out, "resolves: available: card 4 is at platform-xhci-hcd.0/usb2-1, device 0 subdevice 0\n") {
		t.Errorf("the card bound on a unit with two: exit %d\n%s", code, out)
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
