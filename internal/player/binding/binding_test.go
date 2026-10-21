package binding

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// TestMain refuses, and fails the binary on, any read of the host's own
// /proc or /sys and any open of a PCM node: every test here reads a tree
// it laid out and opens nothing.
func TestMain(m *testing.M) {
	var mu sync.Mutex
	var touched []string
	stopReads := alsa.WatchHostReads(func(root string) error {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "read "+root)
		return errors.New("a test read the host's " + root)
	})
	stopOpens := alsapcm.GuardNodeOpens(func(path string) {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "open "+path)
	})
	code := m.Run()
	stopReads()
	stopOpens()
	if len(touched) > 0 {
		fmt.Fprintf(os.Stderr, "binding tests touched the host's hardware: %q\n", touched)
		code = 1
	}
	os.Exit(code)
}

// The Pi 5 tree's places (pi5-go-bar).
const (
	usb1        = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"
	goBar       = usb1 + "/1-1"
	goBarSerial = "GOBAR-7Q2X-91KZ-4471"
	goBarPort   = "platform-xhci-hcd.0/usb2-1:1.0"
)

// pi5 lays out pi5-go-bar: the GO bar is card 5 on root port 1.
func pi5(t *testing.T) string {
	t.Helper()
	return sysfstest.Build(t, filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
}

// plug is the manifest of a unit at dir (a USB device directory under
// usb1) whose interface 0 is sound card n.
func plug(dir string, n int, vidpid, serial string) string {
	vid, pid, _ := strings.Cut(vidpid, ":")
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	base := filepath.Base(dir)
	return fmt.Sprintf("usbdev %s vendor=%s product=%s%s\nusbcard %d %s/%s:1.0\nproccard %d DAC%d \"Synthetic DAC\" usb=%s pcm=0.0\n",
		dir, vid, pid, s, n, dir, base, n, n, vidpid)
}

// observe reads root's two trees.
func observe(t *testing.T, root string) Inventory {
	t.Helper()
	inv, err := Observer{
		Proc:  alsa.Reader{ProcRoot: filepath.Join(root, "proc")},
		Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")},
	}.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return inv
}

// bySerial, byModel and byPort are bindings of the GO bar.
func bySerial(serial string) config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009", Serial: usbid.New(serial)}
}

func byModel(vid, pid string) config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingUniqueModel, VendorID: vid, ProductID: pid}
}

func byPort(port string) config.PlayerDevice {
	return config.PlayerDevice{Transport: "usb", Binding: config.BindingPort, Port: port}
}

// want checks a resolution's state and card, and that its reason carries
// no serial.
func want(t *testing.T, r Resolution, state State, card int) {
	t.Helper()
	if r.State != state || r.Card != card {
		t.Fatalf("resolution %s on card %d (%s), want %s on card %d", r.State, r.Card, r.Reason, state, card)
	}
	for _, s := range []string{goBarSerial, "0000000000000000", "OTHER-UNIT-SERIAL-9"} {
		if strings.Contains(r.Reason, s[4:]) {
			t.Fatalf("the reason carries a serial: %s", r.Reason)
		}
	}
}

// TestTheGoBarResolvesByEachBinding: by serial, by model and by port, the
// GO bar is card 5, its playback endpoint 0/0, and the identity the check
// after open compares with is the one sysfs gives.
func TestTheGoBarResolvesByEachBinding(t *testing.T) {
	inv := observe(t, pi5(t))
	for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPort)} {
		r := Resolve(dev, inv)
		want(t, r, StateAvailable, 5)
		if r.Endpoint != (Endpoint{}) || r.Identity.ID.String() != "20b1:3009" || !r.Identity.Serial.Equal(usbid.New(goBarSerial)) ||
			r.Identity.Port != goBarPort {
			t.Errorf("%s: %+v", dev.Binding, r)
		}
	}
	if c, ok := inv.Card(0); !ok || c.State != CardUnsupported {
		t.Errorf("HDMI card 0: %+v", c)
	}
}

// TestARenumberedCardStillResolvesToTheSameUnit: replugged in the other
// order the GO bar is card 6 and another DAC card 5; the serial and model
// bindings follow the unit, the port binding its port, which the GO bar is
// still in, and the other unit's serial binding follows it.
func TestARenumberedCardStillResolvesToTheSameUnit(t *testing.T) {
	root := pi5(t)
	other := usb1 + "/1-2"
	sysfstest.Apply(t, root, plug(other, 6, "1234:0f02", "OTHER-UNIT-SERIAL-9"))
	inv := observe(t, root)
	want(t, Resolve(bySerial(goBarSerial), inv), StateAvailable, 5)

	sysfstest.Apply(t, root, "unplug "+goBar+"\nunplug "+other+"\n")
	sysfstest.Apply(t, root, plug(other, 5, "1234:0f02", "OTHER-UNIT-SERIAL-9")+plug(goBar, 6, "20b1:3009", goBarSerial))
	inv = observe(t, root)
	for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPort)} {
		want(t, Resolve(dev, inv), StateAvailable, 6)
	}
	otherBySerial := config.PlayerDevice{Binding: config.BindingSerial, VendorID: "1234", ProductID: "0f02", Serial: usbid.New("OTHER-UNIT-SERIAL-9")}
	want(t, Resolve(otherBySerial, inv), StateAvailable, 5)
}

// TestUnplugIsMissingAndReplugIsAvailable: the bound unit gone is
// device_missing, and back, under any card number, it is available again.
func TestUnplugIsMissingAndReplugIsAvailable(t *testing.T) {
	root := pi5(t)
	dev := bySerial(goBarSerial)
	want(t, Resolve(dev, observe(t, root)), StateAvailable, 5)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n")
	r := Resolve(dev, observe(t, root))
	want(t, r, StateDeviceMissing, -1)
	if !strings.Contains(r.Reason, "20b1:3009") {
		t.Errorf("the reason does not name the model: %s", r.Reason)
	}
	sysfstest.Apply(t, root, plug(goBar, 2, "20b1:3009", goBarSerial))
	want(t, Resolve(dev, observe(t, root)), StateAvailable, 2)
}

// TestTwoSerialLessUnitsOfOneModelAreAmbiguous: a uniqueModel binding with
// two units of its model resolves to neither, never the first, and the
// ports still tell them apart; bind refuses the serial and the model.
func TestTwoSerialLessUnitsOfOneModelAreAmbiguous(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, plug(usb1+"/1-2", 6, "1234:0f01", "")+plug(usb1+"/1-3", 7, "1234:0f01", ""))
	inv := observe(t, root)
	r := Resolve(byModel("1234", "0f01"), inv)
	want(t, r, StateDeviceAmbiguous, -1)
	if !slices.Equal(r.Matches, []int{6, 7}) {
		t.Errorf("matches %v", r.Matches)
	}
	want(t, Resolve(byPort("platform-xhci-hcd.0/usb2-3:1.0"), inv), StateAvailable, 7)
	if _, err := For(inv, 6, config.BindingSerial, Endpoint{}); err == nil || !strings.Contains(err.Error(), "no serial") {
		t.Errorf("bind by serial of a unit with none: %v", err)
	}
	if _, err := For(inv, 6, config.BindingUniqueModel, Endpoint{}); err == nil || !strings.Contains(err.Error(), "device_ambiguous") {
		t.Errorf("bind by model with two units: %v", err)
	}
	if dev, err := For(inv, 6, config.BindingPort, Endpoint{}); err != nil || dev.Port != "platform-xhci-hcd.0/usb2-2:1.0" {
		t.Errorf("bind by port: %+v, %v", dev, err)
	}
}

// TestASerialTwoUnitsShareIsAmbiguous: a placeholder serial, which XMOS
// and C-Media boards ship, matches two units: neither is chosen, and bind
// refuses it.
func TestASerialTwoUnitsShareIsAmbiguous(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+
		plug(goBar, 5, "20b1:3009", "0000000000000000")+plug(usb1+"/1-2", 6, "20b1:3009", "0000000000000000"))
	inv := observe(t, root)
	r := Resolve(bySerial("0000000000000000"), inv)
	want(t, r, StateDeviceAmbiguous, -1)
	if !slices.Equal(r.Matches, []int{5, 6}) {
		t.Errorf("matches %v", r.Matches)
	}
	if _, err := For(inv, 5, config.BindingSerial, Endpoint{}); err == nil || !strings.Contains(err.Error(), "device_ambiguous") {
		t.Errorf("bind by a shared serial: %v", err)
	}
}

// TestIdsMatchInEverySpellingTheConfigTakes: 20B1 and 0x20b1 are the
// GO bar's vendor id as 20b1 is.
func TestIdsMatchInEverySpellingTheConfigTakes(t *testing.T) {
	inv := observe(t, pi5(t))
	for _, vid := range []string{"20b1", "20B1", "0x20b1", "0x20B1"} {
		dev := bySerial(goBarSerial)
		dev.VendorID = vid
		want(t, Resolve(dev, inv), StateAvailable, 5)
		want(t, Resolve(byModel(vid, "3009"), inv), StateAvailable, 5)
	}
}

// TestAPortBindingFollowsThePortNotTheUnit: another model in the bound
// port is the player's DAC, and the bound unit in another port is not.
func TestAPortBindingFollowsThePortNotTheUnit(t *testing.T) {
	root := pi5(t)
	dev := byPort(goBarPort)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(goBar, 5, "1234:0f03", ""))
	r := Resolve(dev, observe(t, root))
	want(t, r, StateAvailable, 5)
	if r.Identity.ID.String() != "1234:0f03" {
		t.Errorf("the port's DAC is %s", r.Identity.ID)
	}
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(usb1+"/1-2", 5, "20b1:3009", goBarSerial))
	r = Resolve(dev, observe(t, root))
	want(t, r, StateDeviceMissing, -1)
	if !strings.Contains(r.Reason, goBarPort) {
		t.Errorf("the reason does not name the port: %s", r.Reason)
	}
}

// TestAHubOrAGoneControllerInvalidatesAPortBinding: a hub in the bound
// port, or a controller the binding names that is not there, fails closed
// as binding_invalid, which an empty port is not; a serial binding follows
// the unit through the hub.
func TestAHubOrAGoneControllerInvalidatesAPortBinding(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\nusbdev "+goBar+" vendor=05e3 product=0610 class=09\n"+plug(goBar+"/1-1.3", 5, "20b1:3009", goBarSerial))
	inv := observe(t, root)
	r := Resolve(byPort(goBarPort), inv)
	want(t, r, StateBindingInvalid, -1)
	if !strings.Contains(r.Reason, "hub") {
		t.Errorf("reason %s", r.Reason)
	}
	want(t, Resolve(bySerial(goBarSerial), inv), StateAvailable, 5)
	if got := Resolve(bySerial(goBarSerial), inv).Identity.Port; got != "platform-xhci-hcd.0/usb2-1.3:1.0" {
		t.Errorf("through the hub the port is %s", got)
	}

	r = Resolve(byPort("platform-xhci-hcd.7/usb2-1:1.0"), inv)
	want(t, r, StateBindingInvalid, -1)
	if !strings.Contains(r.Reason, "platform-xhci-hcd.7") || !strings.Contains(r.Reason, "rebind") {
		t.Errorf("reason %s", r.Reason)
	}
	want(t, Resolve(byPort("platform-xhci-hcd.0/usb2-2:1.0"), inv), StateDeviceMissing, -1)
}

// TestTheCheckAfterOpenRefusesAnotherDACAtTheBoundCard: the check passes
// the node Resolve named, and refuses, naming why, the same card number
// once another DAC has taken it (a swap between the resolve and the open),
// a node of another device or substream, a capture node, and a card that
// went away.
func TestTheCheckAfterOpenRefusesAnotherDACAtTheBoundCard(t *testing.T) {
	root := pi5(t)
	r := Resolve(bySerial(goBarSerial), observe(t, root))
	want(t, r, StateAvailable, 5)
	c := Checker{Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")}, Want: r}
	node := alsapcm.NodeInfo{Card: 5, Device: 0, Subdevice: 0, Playback: true}
	if err := c.Check(node); err != nil {
		t.Fatalf("the bound node: %v", err)
	}
	for _, n := range []alsapcm.NodeInfo{
		{Card: 5, Device: 1, Playback: true},
		{Card: 5, Device: 0, Subdevice: 1, Playback: true},
		{Card: 5, Device: 0},
	} {
		if err := c.Check(n); !errors.Is(err, ErrWrongDevice) {
			t.Errorf("node %+v: %v", n, err)
		}
	}
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(usb1+"/1-2", 5, "1234:0f04", "SWAPPED-IN-UNIT-0001"))
	err := c.Check(node)
	if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "1234:0f04") || !strings.Contains(err.Error(), "20b1:3009") {
		t.Fatalf("a swapped DAC at card 5: %v", err)
	}
	if strings.Contains(err.Error(), "SWAPPED-IN-UNIT-0001") || strings.Contains(err.Error(), goBarSerial) {
		t.Fatalf("the refusal carries a serial: %v", err)
	}
	sysfstest.Apply(t, root, "unplug "+usb1+"/1-2\n")
	if err := c.Check(node); !errors.Is(err, ErrWrongDevice) {
		t.Errorf("a card that went away: %v", err)
	}
	if err := (Checker{Sysfs: c.Sysfs, Want: Resolution{State: StateDeviceMissing, Card: -1}}).Check(node); !errors.Is(err, ErrWrongDevice) {
		t.Errorf("a check against no resolution: %v", err)
	}
}

// TestACardOneTreeListsAloneIsTransient: a card in /proc/asound that sysfs
// lacks, one in sysfs that /proc lacks, one whose two readings name
// different units, and one whose files vanished mid-read are transient,
// and the binding resolves to identity_transient, not latched: once the
// trees agree it resolves.
func TestACardOneTreeListsAloneIsTransient(t *testing.T) {
	dev := bySerial(goBarSerial)
	for _, tc := range []struct {
		name, step, settle string
	}{
		{"in /proc alone", "proccard 7 New \"Arriving DAC\" usb=1234:0f05 pcm=0.0\n", plug(usb1+"/1-2", 7, "1234:0f05", "")},
		{"in sysfs alone", "usbdev " + usb1 + "/1-2 vendor=1234 product=0f05\nusbcard 7 " + usb1 + "/1-2/1-2:1.0\n",
			"proccard 7 New \"Arriving DAC\" usb=1234:0f05 pcm=0.0\n"},
		{"two units", plug(usb1+"/1-2", 7, "1234:0f05", "") + "file proc/asound/card7/usbid 1234:0f06\n",
			"file proc/asound/card7/usbid 1234:0f05\n"},
		{"vanished", "link sys/class/sound/card7 sys/devices/gone/sound/card7\nproccard 7 New \"Leaving DAC\" usb=1234:0f05 pcm=0.0\n",
			"procremove 7\nremove sys/class/sound/card7\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv := observe(t, root)
			if c, ok := inv.Card(7); !ok || c.State != CardTransient {
				t.Fatalf("card 7: %+v, %v", c, ok)
			}
			r := Resolve(dev, inv)
			want(t, r, StateIdentityTransient, -1)
			if !strings.Contains(r.Reason, "card 7") {
				t.Errorf("reason %s", r.Reason)
			}
			sysfstest.Apply(t, root, tc.settle)
			want(t, Resolve(dev, observe(t, root)), StateAvailable, 5)
		})
	}
}

// TestAnUnreadableCardIsNeverRuledOut: a USB card whose identity cannot be
// read may be a second unit of the bound one, so a match beside it is
// ambiguous, never the first.
func TestAnUnreadableCardIsNeverRuledOut(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, plug(usb1+"/1-2", 6, "20b1:3009", "")+"file "+usb1+"/1-2/idVendor \""+strings.Repeat("2", 5000)+"\"\n")
	inv := observe(t, root)
	if c, _ := inv.Card(6); c.State != CardUnreadable {
		t.Fatalf("card 6: %+v", c)
	}
	r := Resolve(bySerial(goBarSerial), inv)
	want(t, r, StateDeviceAmbiguous, -1)
	if !strings.Contains(r.Reason, "card 6") {
		t.Errorf("reason %s", r.Reason)
	}
	want(t, Resolve(byPort(goBarPort), inv), StateAvailable, 5)
}

// TestTheEndpointMustBeThere: the unit present with no playback stream at
// the bound device and substream is endpoint_missing, on its card.
func TestTheEndpointMustBeThere(t *testing.T) {
	inv := observe(t, pi5(t))
	dev := bySerial(goBarSerial)
	dev.PCMDevice = 1
	want(t, Resolve(dev, inv), StateEndpointMissing, 5)
	dev.PCMDevice, dev.Subdevice = 0, 1
	want(t, Resolve(dev, inv), StateEndpointMissing, 5)
	if _, err := For(inv, 5, config.BindingSerial, Endpoint{Device: 1}); err == nil || !strings.Contains(err.Error(), "endpoint_missing") {
		t.Errorf("bind to an endpoint the card lacks: %v", err)
	}
}

// TestWhatCannotBeBound: an HDMI card has no identity a binding names, a
// box without sysfs names none, no binding is not_configured, and an
// invalid one is refused by the config's own check.
func TestWhatCannotBeBound(t *testing.T) {
	root := pi5(t)
	inv := observe(t, root)
	if _, err := For(inv, 0, config.BindingUniqueModel, Endpoint{}); err == nil || !strings.Contains(err.Error(), "identity_unsupported") {
		t.Errorf("bind an HDMI card: %v", err)
	}
	if _, err := For(inv, 9, config.BindingSerial, Endpoint{}); err == nil {
		t.Error("bound a card the box does not have")
	}
	noSysfs, err := Observer{Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}, Sysfs: alsa.SysfsReader{Root: t.TempDir()}}.Observe()
	if err != nil {
		t.Fatal(err)
	}
	want(t, Resolve(bySerial(goBarSerial), noSysfs), StateIdentityUnsupported, -1)
	want(t, Resolve(config.PlayerDevice{}, inv), StateNotConfigured, -1)
	bad := bySerial(goBarSerial)
	bad.Port = goBarPort
	r := Resolve(bad, inv)
	want(t, r, StateBindingInvalid, -1)
	if !strings.Contains(r.Reason, "device.port") {
		t.Errorf("reason %s", r.Reason)
	}
}

// TestForWritesOnlyTheBindingsOwnFields: each binding For builds carries
// its own fields, the serial whole, and resolves to the card it was built
// from.
func TestForWritesOnlyTheBindingsOwnFields(t *testing.T) {
	inv := observe(t, pi5(t))
	s, err := For(inv, 5, config.BindingSerial, Endpoint{})
	if err != nil || s.Transport != "usb" || s.VendorID != "20b1" || s.ProductID != "3009" || !s.Serial.Equal(usbid.New(goBarSerial)) || s.Port != "" {
		t.Errorf("serial: %+v, %v", s, err)
	}
	m, err := For(inv, 5, config.BindingUniqueModel, Endpoint{})
	if err != nil || m.Serial.Present() || m.Port != "" || m.VendorID != "20b1" {
		t.Errorf("uniqueModel: %+v, %v", m, err)
	}
	p, err := For(inv, 5, config.BindingPort, Endpoint{})
	if err != nil || p.Serial.Present() || p.VendorID != "" || p.Port != goBarPort {
		t.Errorf("port: %+v, %v", p, err)
	}
	for _, dev := range []config.PlayerDevice{s, m, p} {
		if err := dev.Problem(); err != nil {
			t.Errorf("%s: %v", dev.Binding, err)
		}
		want(t, Resolve(dev, inv), StateAvailable, 5)
	}
	if _, err := For(inv, 5, "card", Endpoint{}); err == nil {
		t.Error("For took a binding kind that is none")
	}
}

// TestWhatResolveReturnsLogsNoSerial: an inventory, a card, a resolution
// and a check's refusal, logged through slog's text and JSON handlers as a
// watcher or the engine would log them, never carry the serial; each holds
// it as a usbid.Serial, which logs as its redaction.
func TestWhatResolveReturnsLogsNoSerial(t *testing.T) {
	root := pi5(t)
	inv := observe(t, root)
	r := Resolve(bySerial(goBarSerial), inv)
	c, _ := inv.Card(5)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(goBar, 5, "1234:0f04", "SWAPPED-IN-UNIT-0001"))
	refusal := Checker{Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")}, Want: r}.Check(alsapcm.NodeInfo{Card: 5, Playback: true})
	var buf strings.Builder
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		l := slog.New(h)
		l.Info("resolved", "resolution", r, "identity", r.Identity, "card", c, "inventory", inv, "refusal", refusal)
		l.Warn("cards", slog.Any("cards", inv.Cards), slog.Any("usb", c.USB))
	}
	for _, s := range []string{goBarSerial, "SWAPPED-IN-UNIT-0001"} {
		for i := 0; i+6 <= len(s); i++ {
			if strings.Contains(buf.String(), s[i:i+6]) {
				t.Fatalf("a log line carries a serial:\n%s", buf.String())
			}
		}
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Errorf("the logs carry no redaction, so the serial was not logged at all:\n%s", buf.String())
	}
}

// TestStatusIsDisabledBeforeItResolves: the player's state is disabled,
// with the config's reason, until PlayerEnabled says it runs, and the
// binding's resolution after.
func TestStatusIsDisabledBeforeItResolves(t *testing.T) {
	inv := observe(t, pi5(t))
	cfg := &config.Config{Output: config.OutputConfig{Player: config.PlayerConfig{Device: bySerial(goBarSerial)}}}
	if r := Status(cfg, "linux", inv); r.State != StateDisabled || r.Reason == "" {
		t.Errorf("off: %+v", r)
	}
	cfg.Output.Player.Enabled = true
	if r := Status(cfg, "darwin", inv); r.State != StateDisabled || !strings.Contains(r.Reason, "Linux only") {
		t.Errorf("off Linux: %+v", r)
	}
	want(t, Status(cfg, "linux", inv), StateAvailable, 5)
}
