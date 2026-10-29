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

// The Pi 5 tree's places (pi5-go-bar). goBarPlace is the GO bar's port,
// which a port binding names, and goBarPort its card's port path, the form
// B470 PR 4a's bind wrote.
const (
	usb1        = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"
	goBar       = usb1 + "/1-1"
	goBarSerial = "GOBAR-7Q2X-91KZ-4471"
	goBarPlace  = "platform-xhci-hcd.0/usb2-1"
	goBarPort   = goBarPlace + ":1.0"
	// usb2 is the USB 3 root hub of the GO bar's controller, xhci-hcd.0,
	// whose anchor, goBarAnchor, a port binding names before its port.
	usb2        = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb2"
	goBarAnchor = "platform-xhci-hcd.0"
	// usb3 is the USB 2 root hub of the tree's second controller,
	// xhci-hcd.1, and gatePort its port 2, where the hardware gate bound
	// the GO bar by port.
	usb3     = "sys/devices/platform/axi/1000120000.pcie/1f00300000.usb/xhci-hcd.1/usb3"
	gatePort = usb3 + "/3-2"
)

// pi5 lays out pi5-go-bar: the GO bar is card 5 on root port 1.
func pi5(t *testing.T) string {
	t.Helper()
	return sysfstest.Build(t, filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
}

// plug is the manifest of a unit at dir (a USB device directory under
// usb1) whose interface 0 is sound card n.
func plug(dir string, n int, vidpid, serial string) string { return plugOn(dir, 0, n, vidpid, serial) }

// plugOn is plug with the card on interface iface, as a model whose audio
// control interface comes after a HID or DFU one has it (the Hugo 2's is
// interface 1).
func plugOn(dir string, iface, n int, vidpid, serial string) string {
	vid, pid, _ := strings.Cut(vidpid, ":")
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	base := filepath.Base(dir)
	return fmt.Sprintf("usbdev %s vendor=%s product=%s%s\nusbcard %d %s/%s:1.%d\nproccard %d DAC%d \"Synthetic DAC\" usb=%s pcm=0.0\n",
		dir, vid, pid, s, n, dir, base, iface, n, n, vidpid)
}

// composite is the manifest of a unit at dir that carries two sound cards,
// as a unit two drivers each make a card of: card n on interface 0, which
// snd-usb-audio made, and card m on interface 2, which another driver made
// and which therefore has no usbid. Synthetic.
func composite(dir string, n, m int, vidpid string) string {
	vid, pid, _ := strings.Cut(vidpid, ":")
	base := filepath.Base(dir)
	return fmt.Sprintf("usbdev %s vendor=%s product=%s\n", dir, vid, pid) +
		fmt.Sprintf("usbcard %d %s/%s:1.0\nproccard %d DAC%d \"Synthetic composite DAC\" usb=%s pcm=0.0\n", n, dir, base, n, n, vidpid) +
		fmt.Sprintf("usbcard %d %s/%s:1.2\nproccard %d Other%d \"Synthetic composite DAC, second card\" driver=snd-synthetic pcm=0.0\n", m, dir, base, m, m)
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
	want(t, Resolve(byPort("platform-xhci-hcd.0/usb2-3"), inv), StateAvailable, 7)
	if _, err := For(inv, 6, config.BindingSerial, Endpoint{}); err == nil || !strings.Contains(err.Error(), "no serial") {
		t.Errorf("bind by serial of a unit with none: %v", err)
	}
	if _, err := For(inv, 6, config.BindingUniqueModel, Endpoint{}); err == nil || !strings.Contains(err.Error(), "device_ambiguous") {
		t.Errorf("bind by model with two units: %v", err)
	}
	if dev, err := For(inv, 6, config.BindingPort, Endpoint{}); err != nil || dev.Port != "platform-xhci-hcd.0/usb2-2" {
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
// port is the player's DAC, and the bound unit in another port is not, in
// the port binding bind writes and in the form B470 PR 4a's bind wrote.
func TestAPortBindingFollowsThePortNotTheUnit(t *testing.T) {
	for _, port := range []string{goBarPlace, goBarPort} {
		root := pi5(t)
		dev := byPort(port)
		sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(goBar, 5, "1234:0f03", ""))
		r := Resolve(dev, observe(t, root))
		want(t, r, StateAvailable, 5)
		if r.Identity.ID.String() != "1234:0f03" {
			t.Errorf("%s: the port's DAC is %s", port, r.Identity.ID)
		}
		sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(usb1+"/1-2", 5, "20b1:3009", goBarSerial))
		r = Resolve(dev, observe(t, root))
		want(t, r, StateDeviceMissing, -1)
		if !strings.Contains(r.Reason, "no card is at "+goBarPlace) {
			t.Errorf("%s: the reason does not name the port: %s", port, r.Reason)
		}
	}
}

// TestAPortBindingResolvesWhateverModelIsInThePort is B470 PR 4a's hardware
// gate on a Pi 5: the GO bar, its card on interface 0, bound by port in
// xhci-hcd.1's port 2, then a Hugo 2 (made-up ids here), whose card is on
// interface 1, in that port, where the binding 4a's bind wrote,
// platform-xhci-hcd.1/usb2-2:1.0, was device_missing. A port binding names
// the controller and the port chain alone, so bind writes
// platform-xhci-hcd.1/usb2-2, and both forms resolve to whichever model is
// in the port, on its own interface, and to device_missing once the port is
// empty, the GO bar in another port included.
func TestAPortBindingResolvesWhateverModelIsInThePort(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(gatePort, 5, "20b1:3009", goBarSerial))
	inv := observe(t, root)
	dev, err := For(inv, 5, config.BindingPort, Endpoint{})
	if err != nil || dev.Port != "platform-xhci-hcd.1/usb2-2" || dev.Problem() != nil {
		t.Fatalf("bind by port wrote %q, %v; want the port alone", dev.Port, err)
	}
	older := byPort("platform-xhci-hcd.1/usb2-2:1.0")
	for _, d := range []config.PlayerDevice{dev, older} {
		want(t, Resolve(d, inv), StateAvailable, 5)
	}

	sysfstest.Apply(t, root, "unplug "+gatePort+"\n"+plugOn(gatePort, 1, 3, "1234:0c12", "HUGO2-UNIT-0000000042"))
	inv = observe(t, root)
	for _, d := range []config.PlayerDevice{dev, older} {
		r := Resolve(d, inv)
		want(t, r, StateAvailable, 3)
		if r.Identity.ID.String() != "1234:0c12" || r.Identity.Interface != 1 || r.Identity.Port != "platform-xhci-hcd.1/usb2-2:1.1" {
			t.Errorf("%s: the port's DAC is %+v", d.Port, r.Identity)
		}
		if !strings.Contains(r.Reason, "card 3 is at platform-xhci-hcd.1/usb2-2, device 0 subdevice 0") {
			t.Errorf("%s: reason %s", d.Port, r.Reason)
		}
	}

	sysfstest.Apply(t, root, "unplug "+gatePort+"\n"+plugOn(gatePort, 2, 4, "1234:0c13", "")+plug(usb3+"/3-1", 5, "20b1:3009", goBarSerial))
	inv = observe(t, root)
	for _, d := range []config.PlayerDevice{dev, older} {
		want(t, Resolve(d, inv), StateAvailable, 4)
	}

	sysfstest.Apply(t, root, "unplug "+gatePort+"\n")
	inv = observe(t, root)
	for _, d := range []config.PlayerDevice{dev, older} {
		r := Resolve(d, inv)
		want(t, r, StateDeviceMissing, -1)
		if !strings.Contains(r.Reason, "no card is at platform-xhci-hcd.1/usb2-2") {
			t.Errorf("%s: reason %s", d.Port, r.Reason)
		}
	}
}

// TestAUnitWithTwoCardsInThePortFollowsTheRecordedInterface: a unit that
// carries two sound cards in the bound port (a composite device: card 5 on
// interface 0, card 6 on interface 2) is ambiguous to a binding that
// records no interface, never its first card; a binding that records one
// resolves to the card on it, and to neither when neither card is on it.
// bind records the card's configuration and interface for such a unit, and
// a unit with one card in the port is that card whatever either records.
func TestAUnitWithTwoCardsInThePortFollowsTheRecordedInterface(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+composite(goBar, 5, 6, "1234:0c20"))
	inv := observe(t, root)
	for _, n := range []int{5, 6} {
		if c, ok := inv.Card(n); !ok || c.State != CardUSB {
			t.Fatalf("card %d: %+v, %v", n, c, ok)
		}
	}
	r := Resolve(byPort(goBarPlace), inv)
	want(t, r, StateDeviceAmbiguous, -1)
	if !slices.Equal(r.Matches, []int{5, 6}) || !strings.Contains(r.Reason, "records no interface") {
		t.Errorf("no interface recorded: matches %v, reason %s", r.Matches, r.Reason)
	}
	want(t, Resolve(byPort(goBarPlace+":1.0"), inv), StateAvailable, 5)
	want(t, Resolve(byPort(goBarPlace+":1.2"), inv), StateAvailable, 6)
	for _, port := range []string{goBarPlace + ":1.1", goBarPlace + ":2.0", goBarPlace + ":2.2"} {
		r := Resolve(byPort(port), inv)
		want(t, r, StateDeviceAmbiguous, -1)
		if !slices.Equal(r.Matches, []int{5, 6}) || !strings.Contains(r.Reason, "neither is on the interface the binding records") {
			t.Errorf("%s: matches %v, reason %s", port, r.Matches, r.Reason)
		}
	}
	for n, port := range map[int]string{5: goBarPlace + ":1.0", 6: goBarPlace + ":1.2"} {
		dev, err := For(inv, n, config.BindingPort, Endpoint{})
		if err != nil || dev.Port != port {
			t.Errorf("bind card %d by port: %q, %v; want %q", n, dev.Port, err, port)
		}
	}

	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plugOn(goBar, 1, 7, "1234:0c21", ""))
	inv = observe(t, root)
	for _, port := range []string{goBarPlace, goBarPlace + ":1.0", goBarPlace + ":1.2"} {
		want(t, Resolve(byPort(port), inv), StateAvailable, 7)
	}
}

// TestAHubOrAGoneControllerInvalidatesAPortBinding: a hub in the bound
// port, or a controller the binding names that is not there, fails closed
// as binding_invalid, which an empty port is not, in either form; a serial
// binding follows the unit through the hub.
func TestAHubOrAGoneControllerInvalidatesAPortBinding(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\nusbdev "+goBar+" vendor=05e3 product=0610 class=09\n"+plug(goBar+"/1-1.3", 5, "20b1:3009", goBarSerial))
	inv := observe(t, root)
	for _, port := range []string{goBarPort, goBarPlace} {
		r := Resolve(byPort(port), inv)
		want(t, r, StateBindingInvalid, -1)
		if !strings.Contains(r.Reason, "hub") {
			t.Errorf("%s: reason %s", port, r.Reason)
		}
	}
	want(t, Resolve(bySerial(goBarSerial), inv), StateAvailable, 5)
	if got := Resolve(bySerial(goBarSerial), inv).Identity.Port; got != "platform-xhci-hcd.0/usb2-1.3:1.0" {
		t.Errorf("through the hub the port is %s", got)
	}

	for _, port := range []string{"platform-xhci-hcd.7/usb2-1:1.0", "platform-xhci-hcd.7/usb2-1"} {
		r := Resolve(byPort(port), inv)
		want(t, r, StateBindingInvalid, -1)
		if !strings.Contains(r.Reason, "platform-xhci-hcd.7") || !strings.Contains(r.Reason, "rebind") {
			t.Errorf("%s: reason %s", port, r.Reason)
		}
	}
	want(t, Resolve(byPort("platform-xhci-hcd.0/usb2-2"), inv), StateDeviceMissing, -1)
	want(t, Resolve(byPort("platform-xhci-hcd.0/usb2-2:1.0"), inv), StateDeviceMissing, -1)
}

// TestAPortWhoseDeviceClassCannotBeReadIsNotAnEmptyPort: a device in the
// bound port whose class (bDeviceClass) cannot be read may be a hub, so a
// port binding with no card at the port is refused (binding_invalid), as a
// hub there is, never read as an empty port (device_missing). The reading
// says the class could not be read, and a serial binding still follows the
// unit behind it.
func TestAPortWhoseDeviceClassCannotBeReadIsNotAnEmptyPort(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\nusbdev "+goBar+" vendor=05e3 product=0610 class=09\n"+
		"remove "+goBar+"/bDeviceClass\ndir "+goBar+"/bDeviceClass\n"+plug(goBar+"/1-1.3", 5, "20b1:3009", goBarSerial))
	inv := observe(t, root)
	d, ok := inv.deviceAt(goBarPort)
	if !ok || d.Hub || !d.HubUnknown {
		t.Fatalf("the device in the bound port: %+v, %v; want its hub status unknown", d, ok)
	}
	if !strings.Contains(strings.Join(inv.Problems, "; "), "bDeviceClass") {
		t.Errorf("the reading's problems do not name the class: %q", inv.Problems)
	}
	r := Resolve(byPort(goBarPort), inv)
	want(t, r, StateBindingInvalid, -1)
	if !strings.Contains(r.Reason, "class") {
		t.Errorf("reason %s", r.Reason)
	}
	want(t, Resolve(bySerial(goBarSerial), inv), StateAvailable, 5)
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

// TestTheCheckAfterOpenFollowsAPortBinding: under a port binding the check
// after open asks for the bound port and not the bound unit. Another model
// swapped into the port between the resolve and the open passes, its card
// on its own interface, in either form of the binding, where a serial
// binding's check refuses it; a card at another port is refused. On a unit
// that carries two cards the opened card must be the one the binding
// chooses there now: the card on the interface it records, and with none
// recorded, or another, neither. The check reads both trees again and
// reconciles them as Resolve does: a card unreadable in sysfs may be
// another card of the unit in the port, so it refuses there as Resolve
// does, unless card 5 is on the recorded interface; a card the kernel is
// adding or removing (vanished as it was read, or listed by /proc/asound
// and not by sysfs) refuses whatever the binding records, as Resolve's
// identity_transient does; a card that is not USB, or a readable one at
// another port, refuses nothing. A sysfs reading that cannot list the
// cards, or whose listing was cut short, and a /proc/asound that cannot be
// read are no answer: the check refuses on each with a reason of its own,
// even where card 5's own lookup still reads.
func TestTheCheckAfterOpenFollowsAPortBinding(t *testing.T) {
	root := pi5(t)
	sys := alsa.SysfsReader{Root: filepath.Join(root, "sys")}
	proc := alsa.Reader{ProcRoot: filepath.Join(root, "proc")}
	inv := observe(t, root)
	node := alsapcm.NodeInfo{Card: 5, Playback: true}
	place, older, other, serial := Resolve(byPort(goBarPlace), inv), Resolve(byPort(goBarPort), inv),
		Resolve(byPort(goBarPlace+":1.2"), inv), Resolve(bySerial(goBarSerial), inv)
	for _, r := range []Resolution{place, older, other, serial} {
		want(t, r, StateAvailable, 5)
		if err := (Checker{Proc: proc, Sysfs: sys, Want: r}).Check(node); err != nil {
			t.Fatalf("the bound node: %v", err)
		}
	}
	check := func(r Resolution) error { return Checker{Proc: proc, Sysfs: sys, Want: r}.Check(node) }

	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plugOn(goBar, 1, 5, "1234:0c12", "HUGO2-UNIT-0000000042"))
	for _, r := range []Resolution{place, older, other} {
		if err := check(r); err != nil {
			t.Errorf("another model at the bound port: %v", err)
		}
	}
	if err := check(serial); !errors.Is(err, ErrWrongDevice) {
		t.Errorf("a serial binding took another model: %v", err)
	}

	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(usb1+"/1-2", 5, "20b1:3009", goBarSerial))
	for _, r := range []Resolution{place, older, other} {
		err := check(r)
		if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "at platform-xhci-hcd.0/usb2-2:1.0, not at the bound port "+goBarPlace) {
			t.Errorf("a card at another port: %v", err)
		} else if strings.Contains(err.Error(), goBarSerial[4:]) {
			t.Errorf("the refusal carries a serial: %v", err)
		}
	}

	sysfstest.Apply(t, root, "unplug "+usb1+"/1-2\n"+composite(goBar, 5, 6, "1234:0c20"))
	if err := check(older); err != nil {
		t.Errorf("the card on the recorded interface of a unit with two: %v", err)
	}
	for name, r := range map[string]Resolution{"no interface recorded": place, "another interface recorded": other} {
		err := check(r)
		if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "cards 5 and 6") {
			t.Errorf("%s, a unit with two cards: %v", name, err)
		}
	}

	// A unit swapped in whose other card the check cannot identify. One
	// registered on the unit itself rather than on an interface is
	// unreadable: Resolve on the same tree answers device_ambiguous, so the
	// check after open refuses card 5 too, unless it is on the interface the
	// binding records, which no other card of that unit can be. One losing
	// its device link as it goes (vanished), or gone from sysfs while
	// /proc/asound still lists it, is a card the kernel is adding or
	// removing: Resolve answers identity_transient whatever the binding
	// records, so the check refuses card 5 on the recorded interface too.
	for _, tc := range []struct {
		name, layout, then, refusal, says string
		resolves                          State
		onTheInterface                    bool
	}{
		{"unreadable", plug(goBar, 5, "1234:0c20", "") + "card 6 " + goBar + "\nproccard 6 Other6 \"Synthetic DAC, second card\" driver=snd-synthetic pcm=0.0\n", "",
			"card 6 cannot be identified now", "not an interface", StateDeviceAmbiguous, true},
		{"vanished", composite(goBar, 5, 6, "1234:0c20"), "remove " + goBar + "/1-1:1.2/sound/card6/device\n",
			"card 6 is being added or removed now", "went while they were read", StateIdentityTransient, false},
		{"listed by /proc/asound alone", composite(goBar, 5, 6, "1234:0c20"), "remove sys/class/sound/card6\n",
			"card 6 is being added or removed now", "in /proc/asound and not in sysfs", StateIdentityTransient, false},
	} {
		sysfstest.Apply(t, root, "unplug "+goBar+"\nprocremove 6\n"+tc.layout)
		if tc.then != "" {
			sysfstest.Apply(t, root, tc.then)
		}
		inv := observe(t, root)
		want(t, Resolve(byPort(goBarPlace), inv), tc.resolves, -1)
		recorded := map[string]Resolution{"no interface recorded": place, "another interface recorded": other}
		if tc.onTheInterface {
			want(t, Resolve(byPort(goBarPort), inv), StateAvailable, 5)
			if err := check(older); err != nil {
				t.Errorf("the card on the recorded interface, beside a card that is %s: %v", tc.name, err)
			}
		} else {
			want(t, Resolve(byPort(goBarPort), inv), tc.resolves, -1)
			recorded["the interface recorded"] = older
		}
		for name, r := range recorded {
			err := check(r)
			if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), tc.refusal) ||
				!strings.Contains(err.Error(), tc.says) || !strings.Contains(err.Error(), goBarPlace) {
				t.Errorf("%s, beside card 5 a card that is %s: %v", name, tc.name, err)
			}
		}
	}

	// A card that is not USB (the HDMI cards) and a readable card at another
	// port, which both trees list, are ruled out.
	sysfstest.Apply(t, root, "unplug "+goBar+"\nprocremove 6\n"+plugOn(goBar, 1, 5, "1234:0c12", "HUGO2-UNIT-0000000042")+plug(usb1+"/1-2", 7, "20b1:3009", goBarSerial))
	if err := check(place); err != nil {
		t.Errorf("a unit with one card in the port, another at port 2, and the HDMI cards: %v", err)
	}

	// A sysfs reading that cannot list the cards, while card 5's own lookup
	// still reads: refused on the listing itself, which a lookup of the
	// opened card apart from the listing would never see, even where card 5
	// is on the recorded interface and no other card would matter. So is a
	// /proc/asound that cannot be read.
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plug(goBar, 5, "1234:0c20", ""))
	failed := readingAs{SysfsReader: sys, edit: unlisted}
	if id, ok := failed.Card(5); !ok || id.USB == nil || id.USB.Port != goBarPort {
		t.Fatalf("premise: card 5's own lookup does not read: %+v, %v", id, ok)
	}
	for name, r := range map[string]Resolution{"no interface recorded": place, "the interface recorded": older} {
		if err := check(r); err != nil {
			t.Fatalf("%s, premise: the whole reading passes: %v", name, err)
		}
		if err := (Checker{Proc: proc, Sysfs: failed, Want: r}).Check(node); !errors.Is(err, ErrWrongDevice) ||
			!strings.Contains(err.Error(), "sysfs could not be read") || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s, a sysfs that cannot list its cards: %v", name, err)
		}
		if err := (Checker{Proc: alsa.Reader{ProcRoot: t.TempDir()}, Sysfs: sys, Want: r}).Check(node); !errors.Is(err, ErrWrongDevice) ||
			!strings.Contains(err.Error(), "/proc/asound's cards could not be read") {
			t.Errorf("%s, a /proc/asound that cannot be read: %v", name, err)
		}
	}

	// A listing cut at the cap that keeps card 5 and cuts its sibling, card
	// 6, whether /proc/asound lists card 6 (as Resolve would see it) or not
	// yet (a card being added, which only the cut says may be there).
	cut := readingAs{SysfsReader: sys, edit: cutAtTheCap(6)}
	for _, step := range []string{"unplug " + goBar + "\n" + composite(goBar, 5, 6, "1234:0c20"), "procremove 6\n"} {
		sysfstest.Apply(t, root, step)
		for name, r := range map[string]Resolution{"no interface recorded": place, "another interface recorded": other, "the interface recorded": older} {
			err := (Checker{Proc: proc, Sysfs: cut, Want: r}).Check(node)
			if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "cut short") || !strings.Contains(err.Error(), "more than 1024") {
				t.Errorf("%s, a listing cut short after %q: %v", name, strings.TrimSpace(step), err)
			}
		}
	}

	// Card 5 itself gone from sysfs while /proc/asound lists it, then gone
	// from both.
	sysfstest.Apply(t, root, "unplug "+goBar+"\nprocremove 6\n"+plug(goBar, 5, "1234:0c20", ""))
	sysfstest.Apply(t, root, "remove sys/class/sound/card5\n")
	if err := check(place); !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "card 5 is not a USB card both trees agree on now") ||
		!strings.Contains(err.Error(), "in /proc/asound and not in sysfs") {
		t.Errorf("card 5 in /proc/asound alone: %v", err)
	}
	sysfstest.Apply(t, root, "unplug "+goBar+"\nprocremove 5\n")
	if err := check(place); !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), "card 5 is in neither /proc/asound nor sysfs now") {
		t.Errorf("card 5 gone from the port: %v", err)
	}
}

// readingAs stands in for the sysfs reader the check after open reads
// through: Card is the real reader's lookup, and Read the real reading as
// edit changes it, into one the reader could not list or cut short.
type readingAs struct {
	alsa.SysfsReader
	edit func(alsa.SysfsSnapshot) alsa.SysfsSnapshot
}

func (r readingAs) Read() alsa.SysfsSnapshot { return r.edit(r.SysfsReader.Read()) }

// unlisted is a sysfs reading that could not list class/sound, as Read
// returns one: not present, no cards, and the problem that says why.
func unlisted(s alsa.SysfsSnapshot) alsa.SysfsSnapshot {
	return alsa.SysfsSnapshot{Root: s.Root, Problems: []string{"class/sound: permission denied"}}
}

// cutAtTheCap is a sysfs reading cut short at the reader's entry cap, as
// Read returns one (CardsComplete false, and the problem it adds), with the
// cards in drop left out of it.
func cutAtTheCap(drop ...int) func(alsa.SysfsSnapshot) alsa.SysfsSnapshot {
	return func(s alsa.SysfsSnapshot) alsa.SysfsSnapshot {
		var kept []alsa.Identity
		for _, id := range s.Cards {
			if !slices.Contains(drop, id.Card) {
				kept = append(kept, id)
			}
		}
		s.Cards, s.CardsComplete = kept, false
		s.Problems = append(s.Problems, "class/sound lists more than 1024 entries; the rest are not read")
		return s
	}
}

// cutDevicesAtTheCap is a sysfs reading whose USB device listing
// (bus/usb/devices) was cut short at the reader's entry cap before the root
// hubs of the controller anchor names, as Read returns one: that controller
// and the devices on it left out, the problem it adds, and the reading not
// whole. The card listing stays whole.
func cutDevicesAtTheCap(anchor string) func(alsa.SysfsSnapshot) alsa.SysfsSnapshot {
	return func(s alsa.SysfsSnapshot) alsa.SysfsSnapshot {
		var controllers []string
		for _, a := range s.Controllers {
			if a != anchor {
				controllers = append(controllers, a)
			}
		}
		var devices []alsa.USBDevice
		for _, d := range s.Devices {
			if d.Anchor != anchor {
				devices = append(devices, d)
			}
		}
		s.Controllers, s.Devices, s.Partial = controllers, devices, true
		s.Problems = append(s.Problems, "bus/usb/devices lists more than 1024 entries; the rest are not read")
		return s
	}
}

// TestTheCheckAfterOpenAgreesWithResolveOnEachReading runs each shape a
// reading can take through Resolve and through the check after open, under
// each form of binding: by serial, by uniqueModel, by the port alone, and
// by the port with the GO bar's configuration and interface. Each check is
// made from the resolution of the GO bar alone, as an open is made from the
// resolve before it, and the box changes in between; it must admit card 5
// exactly when Resolve, on the reading the check takes, answers available
// on card 5, and refuse it whenever Resolve refuses. Under a port binding
// that holds for every shape: the check chooses the card again there, since
// a unit Resolve never saw may be in the port by then. Under a serial or
// uniqueModel binding the check chooses nothing, and where it answers
// otherwise than Resolve the table says so and why. The rows take each piece
// of the reading Resolve consults in turn: whether the card listing is
// whole, each card's state in the two trees, the USB controllers and devices
// (the controller a port binding names, a hub in the bound port), the cards'
// port paths and interfaces, and the bound playback stream.
func TestTheCheckAfterOpenAgreesWithResolveOnEachReading(t *testing.T) {
	forms := []struct {
		name string
		dev  config.PlayerDevice
	}{
		{"serial", bySerial(goBarSerial)},
		{"uniqueModel", byModel("20b1", "3009")},
		{"the port alone", byPort(goBarPlace)},
		{"the port and its interface", byPort(goBarPort)},
	}
	base := observe(t, pi5(t))
	var made [4]Resolution
	for i, f := range forms {
		made[i] = Resolve(f.dev, base)
		want(t, made[i], StateAvailable, 5)
	}

	// Why the check after open answers otherwise than Resolve on one
	// reading, under a serial or a uniqueModel binding. Under a port binding
	// nothing does.
	const (
		unitChosen = "under a serial or uniqueModel binding the check chooses no card: it asks whether the node opened is " +
			"the unit Resolve chose, by SameUnit on that card's own sysfs identity, which pins the unit's ids and serial " +
			"and the card's port and interface; a card beside it that Resolve cannot identify, a second card or unit " +
			"Resolve would have to choose among, and a listing Resolve cannot read whole are why Resolve chooses none, " +
			"and none of them makes card 5 another unit"
		unitMoved = "SameUnit compares the opened card with the identity the open was made from, its port path included, " +
			"so the bound unit at another port path now (moved, or behind a hub) or at one sysfs can no longer read is " +
			"refused, to be resolved again: here the check refuses a card Resolve would choose now, the safe way to differ"
		unitInSysfs = "under a serial or uniqueModel binding the check reads the opened card's identity from sysfs, " +
			"which is where a USB card's identity comes from (/proc/asound's usbid only cross-checks it), and requires " +
			"the unit Resolve chose: a card /proc/asound does not list yet or any more is that unit being added or " +
			"removed, and a removed card's next write is a disconnect, which ends the sink"
		unitStream = "under a serial or uniqueModel binding the check reads no /proc/asound: the stream is the one the " +
			"kernel says it opened (INFO on the open descriptor, which it answers only for a stream the card has) and " +
			"the unit is SameUnit's, so a reading that lists card 5 without that stream makes it no other unit"
	)
	byUnit := func(why string) [4]string { return [4]string{why, why, "", ""} }
	every := func(s State) [4]State { return [4]State{s, s, s, s} }
	available, ambiguous, missing, invalid := StateAvailable, StateDeviceAmbiguous, StateDeviceMissing, StateBindingInvalid
	incomplete := StateIdentityIncomplete
	for _, row := range []struct {
		name string
		// steps are laid out in turn on a new Pi 5 tree, and edit, when set,
		// makes the sysfs reading one the reader could have returned.
		steps []string
		edit  func(alsa.SysfsSnapshot) alsa.SysfsSnapshot
		// resolves is what each form resolves to on the reading (available is
		// on card 5), and differs, for a form, why the check answers
		// otherwise.
		resolves [4]State
		differs  [4]string
		// partial says the row's reading is partial (Inventory.Partial), a
		// premise the row checks.
		partial bool
	}{
		{name: "the GO bar beside the two HDMI cards, which are not USB", resolves: every(available)},
		{name: "a virtual card beside it", resolves: every(available), steps: []string{
			"dir sys/devices/virtual/sound/card3\nlink sys/class/sound/card3 sys/devices/virtual/sound/card3\n" +
				"proccard 3 Virtual \"A virtual card\" driver=Virtual pcm=0.0\n"}},
		{name: "a card at another port", resolves: every(available),
			steps: []string{plug(usb1+"/1-2", 7, "1234:0f02", "OTHER-UNIT-SERIAL-9")}},
		{name: "another model in the port as card 5", resolves: [4]State{missing, missing, available, available},
			steps: []string{"unplug " + goBar + "\n" + plugOn(goBar, 1, 5, "1234:0c12", "HUGO2-UNIT-0000000042")}},
		{name: "the GO bar in another port as card 5", resolves: [4]State{available, available, missing, missing}, differs: byUnit(unitMoved),
			steps: []string{"unplug " + goBar + "\n" + plug(usb1+"/1-2", 5, "20b1:3009", goBarSerial)}},
		{name: "card 5 unplugged", resolves: every(missing), steps: []string{"unplug " + goBar + "\n"}},
		{name: "a second card of the unit that both trees read", resolves: [4]State{ambiguous, ambiguous, ambiguous, available},
			differs: byUnit(unitChosen), steps: []string{second}},
		{name: "the listing cut short with no card missing from it", resolves: every(StateIdentityIncomplete),
			differs: byUnit(unitChosen), edit: cutAtTheCap()},
		{name: "the second card cut from the listing while /proc/asound lists it", resolves: every(StateIdentityIncomplete),
			differs: byUnit(unitChosen), steps: []string{second}, edit: cutAtTheCap(6)},
		{name: "the second card cut from the listing before /proc/asound lists it", resolves: every(StateIdentityIncomplete),
			differs: byUnit(unitChosen), steps: []string{second, "procremove 6\n"}, edit: cutAtTheCap(6)},
		{name: "the second card listed by /proc/asound and no longer by sysfs", resolves: every(StateIdentityTransient),
			differs: byUnit(unitChosen), steps: []string{second, "remove sys/class/sound/card6\n"}},
		{name: "the second card listed by sysfs and not by /proc/asound", resolves: every(StateIdentityTransient),
			differs: byUnit(unitChosen), steps: []string{second, "procremove 6\n"}},
		{name: "the second card vanished as it was read", resolves: every(StateIdentityTransient),
			differs: byUnit(unitChosen), steps: []string{second, "remove " + goBar + "/1-1:1.2/sound/card6/device\n"}},
		{name: "a second card whose identity cannot be read", resolves: [4]State{ambiguous, ambiguous, ambiguous, available},
			differs: byUnit(unitChosen), steps: []string{
				"card 6 " + goBar + "\nproccard 6 Other6 \"Synthetic DAC, second card\" driver=snd-synthetic pcm=0.0\n"}},
		{name: "a card at another port listed by sysfs and not by /proc/asound", resolves: every(StateIdentityTransient),
			differs: byUnit(unitChosen), steps: []string{"usbdev " + usb1 + "/1-2 vendor=1234 product=0f05\nusbcard 7 " + usb1 + "/1-2/1-2:1.0\n"}},
		{name: "card 5 listed by /proc/asound and no longer by sysfs", resolves: every(StateIdentityTransient),
			steps: []string{"remove sys/class/sound/card5\n"}},
		{name: "card 5 listed by sysfs and not by /proc/asound", resolves: every(StateIdentityTransient),
			differs: byUnit(unitInSysfs), steps: []string{"procremove 5\n"}},
		{name: "card 5 vanished as it was read", resolves: every(StateIdentityTransient),
			steps: []string{"remove " + goBar + "/1-1:1.0/sound/card5/device\n"}},
		{name: "a listing that cannot be read", resolves: every(StateIdentityUnsupported), differs: byUnit(unitChosen), edit: unlisted},
		// The USB listing, bus/usb/devices, gives Resolve the controllers a
		// port binding's controller must be among, and the devices it tells a
		// hub in the bound port by. A card's own port path is read from its
		// device's ancestors and not from the listing, so it still reads when
		// the listing does not. A listing that could not be read whole (not
		// listed, a root hub not read, cut at the cap) lacks the controller in
		// the reading alone, which is no topology change (B510); one whose
		// root hubs went as they were read is the box as it is.
		{name: "a USB device listing that cannot be read", resolves: [4]State{available, available, incomplete, incomplete},
			steps: []string{usbUnlisted}, partial: true},
		{name: "the root hubs of the GO bar's controller unreadable in the USB listing", resolves: [4]State{available, available, incomplete, incomplete},
			steps: []string{rootHubsUnread}, partial: true},
		{name: "the root hubs of the GO bar's controller gone from the USB listing as it was read", resolves: [4]State{available, available, invalid, invalid},
			steps: []string{"link sys/bus/usb/devices/usb1 !gone\nlink sys/bus/usb/devices/usb2 !gone\n"}},
		{name: "the USB listing cut short before the GO bar's controller", resolves: [4]State{available, available, incomplete, incomplete},
			edit: cutDevicesAtTheCap(goBarAnchor), partial: true},
		// Card 5's port path cannot be read when its root hub's speed cannot,
		// while its controller is still listed through its other root hub: no
		// card is placed at the bound port, and card 5 may be the card there
		// (B510).
		{name: "card 5's port path unreadable", resolves: [4]State{available, available, incomplete, incomplete}, differs: byUnit(unitMoved),
			steps: []string{"remove " + usb1 + "/speed\n"}},
		// A card whose port path cannot be read is unknown, never elsewhere: one
		// of the GO bar's model and serial may be a second card of its unit in
		// the bound port, and one of another model cannot be (B510).
		{name: "a second card of the unit whose port path cannot be read", resolves: [4]State{ambiguous, ambiguous, ambiguous, available},
			differs: byUnit(unitChosen), steps: []string{second}, edit: portUnreadable(6)},
		{name: "a card of another model at another port whose port path cannot be read", resolves: every(available),
			steps: []string{plug(usb1+"/1-2", 7, "1234:0f02", "OTHER-UNIT-SERIAL-9")}, edit: portUnreadable(7)},
		// Resolve reads a hub in the bound port only when no card is at it, to
		// answer binding_invalid rather than device_missing, and the check
		// refuses there too, since the opened card must be at the bound port.
		{name: "a hub in the bound port with the GO bar behind it as card 5", resolves: [4]State{available, available, invalid, invalid},
			differs: byUnit(unitMoved), steps: []string{"unplug " + goBar + "\nusbdev " + goBar + " vendor=05e3 product=0610 class=09\n" +
				plug(goBar+"/1-1.3", 5, "20b1:3009", goBarSerial)}},
		{name: "card 5 without the bound playback stream", resolves: every(StateEndpointMissing), differs: byUnit(unitStream),
			steps: []string{"remove proc/asound/card5/pcm0p\n"}},
		// A reading partial elsewhere (B470 PR 4a2: Inventory.Partial) is one
		// Resolve answers from, and the check with it: what a partial reading
		// lacks it lacks, so it can hide no card at the bound port, and every
		// piece the check's answer depends on (the card listing, /proc/asound's
		// cards, the bound controller, the opened card's stream) is asked of
		// it on its own (round 4: CodeRabbit asked the check to refuse any
		// partial inventory, which would refuse card 5 where Resolve chooses
		// it).
		{name: "another card whose PCM directory cannot be listed", resolves: every(available), partial: true,
			steps: []string{"remove proc/asound/card0/pcm0p\nlink proc/asound/card0/pcm0p !../../../outside\n"}},
		{name: "a root hub of another controller that cannot be read in the USB listing", resolves: every(available), partial: true,
			steps: []string{"link sys/bus/usb/devices/usb3 !../../../../outside\n"}},
		{name: "another model in the port as card 5 without the bound playback stream",
			resolves: [4]State{missing, missing, StateEndpointMissing, StateEndpointMissing}, steps: []string{"unplug " + goBar + "\n" +
				"usbdev " + goBar + " vendor=1234 product=0c12 serial=HUGO2-UNIT-0000000042\nusbcard 5 " + goBar + "/1-1:1.1\n" +
				"proccard 5 DAC5 \"Synthetic DAC\" usb=1234:0c12 pcm=1.0\n"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := pi5(t)
			for _, step := range row.steps {
				sysfstest.Apply(t, root, step)
			}
			proc := alsa.Reader{ProcRoot: filepath.Join(root, "proc")}
			var sys SysfsReading = alsa.SysfsReader{Root: filepath.Join(root, "sys")}
			if row.edit != nil {
				sys = readingAs{SysfsReader: alsa.SysfsReader{Root: filepath.Join(root, "sys")}, edit: row.edit}
			}
			snap, err := proc.Read()
			if err != nil {
				t.Fatalf("/proc/asound: %v", err)
			}
			// Observe's reading of the two trees, through the sysfs reading the
			// check takes.
			reading := sys.Read()
			inv := inventory(snap, reading)
			if inv.CardsComplete != reading.CardsComplete {
				t.Errorf("the inventory says its card listing is whole (%v), and the reading says %v", inv.CardsComplete, reading.CardsComplete)
			}
			if row.partial && !inv.Partial {
				t.Fatalf("premise: the reading is not partial (problems %q)", inv.Problems)
			}
			for i, f := range forms {
				r := Resolve(f.dev, inv)
				if r.State != row.resolves[i] || (r.State == StateAvailable && r.Card != 5) {
					t.Errorf("by %s: resolves %s on card %d (%s), and the table says %s", f.name, r.State, r.Card, r.Reason, row.resolves[i])
				}
				err := Checker{Proc: proc, Sysfs: sys, Want: made[i]}.Check(alsapcm.NodeInfo{Card: 5, Playback: true})
				if err != nil && !errors.Is(err, ErrWrongDevice) {
					t.Errorf("by %s: the check refused with %v, which is not ErrWrongDevice", f.name, err)
				}
				admitted, chosen := err == nil, r.State == StateAvailable && r.Card == 5
				switch why := row.differs[i]; {
				case why == "" && admitted != chosen:
					t.Errorf("by %s: Resolve answers %s (%s), and the check %s", f.name, r.State, r.Reason, verdict(err))
				case why != "" && admitted == chosen:
					t.Errorf("by %s: Resolve answers %s (%s), and the check %s, where the table says they differ: %s",
						f.name, r.State, r.Reason, verdict(err), why)
				}
				for _, s := range []string{goBarSerial, "HUGO2-UNIT-0000000042", "OTHER-UNIT-SERIAL-9"} {
					if strings.Contains(r.Reason, s[4:]) || (err != nil && strings.Contains(err.Error(), s[4:])) {
						t.Errorf("by %s: a serial in the reason or the refusal: %s; %v", f.name, r.Reason, err)
					}
				}
			}
		})
	}
}

// verdict words the check after open's answer on card 5 for a failure
// message.
func verdict(err error) string {
	if err == nil {
		return "admits card 5"
	}
	return "refuses it: " + err.Error()
}

// TestACutListingResolvesToNoCardAndBindsNone: a sysfs whose card listing
// was cut short at the reader's cap may be missing a card, the one a
// binding names or one beside it that makes it ambiguous. Resolve answers
// identity_incomplete under every binding and says why, and bind refuses
// every kind of binding with that state, whether the cut kept card 5 or
// left it to /proc/asound alone, where it reads as a card being added or
// removed, which it is not.
func TestACutListingResolvesToNoCardAndBindsNone(t *testing.T) {
	root := pi5(t)
	snap, err := alsa.Reader{ProcRoot: filepath.Join(root, "proc")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	sys := alsa.SysfsReader{Root: filepath.Join(root, "sys")}
	for _, drop := range [][]int{nil, {5}} {
		inv := inventory(snap, cutAtTheCap(drop...)(sys.Read()))
		for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPlace), byPort(goBarPort)} {
			r := Resolve(dev, inv)
			want(t, r, StateIdentityIncomplete, -1)
			if !strings.Contains(r.Reason, "card listing at "+filepath.Join(root, "sys")+" was cut short") {
				t.Errorf("cards %v cut, by %s: reason %s", drop, dev.Binding, r.Reason)
			}
		}
		for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
			if _, err := For(inv, 5, kind, Endpoint{}); err == nil || !strings.Contains(err.Error(), "identity_incomplete") ||
				!strings.Contains(err.Error(), "cut short") {
				t.Errorf("cards %v cut, bind by %s: %v", drop, kind, err)
			}
		}
	}
}

// TestTheCheckAfterOpenNeedsTheBoundControllerAndStream: under a port
// binding the check after open asks what Resolve asks of the reading before
// it chooses a card, and refuses with a reason of its own where Resolve
// answers binding_invalid, identity_incomplete or endpoint_missing. A USB
// listing that does not name the bound controller is no port to choose a
// card at, whatever the opened card's own port path says, and the refusal
// names the controller and why the listing lacks it: a listing that could
// not be read is Resolve's identity_incomplete (B510), and the check's words
// were never a topology change; a card /proc/asound lists without the bound
// playback stream is no card the binding chooses.
func TestTheCheckAfterOpenNeedsTheBoundControllerAndStream(t *testing.T) {
	base := observe(t, pi5(t))
	node := alsapcm.NodeInfo{Card: 5, Playback: true}
	for _, port := range []string{goBarPlace, goBarPort} {
		made := Resolve(byPort(port), base)
		want(t, made, StateAvailable, 5)
		for _, tc := range []struct {
			name, step string
			resolves   State
			says       []string
		}{
			{"a USB listing that cannot be read", usbUnlisted,
				StateIdentityIncomplete, []string{"the controller the port binding names, " + goBarAnchor + ", is not among the USB controllers",
					"bus/usb/devices: not a directory", "bound port " + goBarPlace}},
			{"card 5 without the bound playback stream", "remove proc/asound/card5/pcm0p\n",
				StateEndpointMissing, []string{"card 5 has no playback stream at device 0 subdevice 0", "bound port " + goBarPlace}},
		} {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			if r := Resolve(byPort(port), observe(t, root)); r.State != tc.resolves {
				t.Fatalf("%s, %s: Resolve answers %s (%s), want %s", port, tc.name, r.State, r.Reason, tc.resolves)
			}
			err := Checker{Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}, Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")},
				Want: made}.Check(node)
			if !errors.Is(err, ErrWrongDevice) {
				t.Errorf("%s, %s: %v", port, tc.name, err)
				continue
			}
			for _, s := range tc.says {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("%s, %s: the refusal does not say %q: %v", port, tc.name, s, err)
				}
			}
		}
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

// TestAUSBCardWithNoUsbidIsReadFromSysfs: snd-usb-audio alone writes
// /proc/asound/cardN/usbid, so a USB card another driver made (snd-usb-
// hiface's M2Tech hiFace, caiaq, ua101, 6fire, line6) has none, and sysfs
// gives its whole identity. Beside it the GO bar stays available by every
// binding, and the card is bound and resolved by its sysfs identity. A
// USB-Audio card with no usbid is snd-usb-audio's own window, which is
// transient: it lists a card before it writes the usbid, and the usbid
// reads empty while it disconnects one.
func TestAUSBCardWithNoUsbidIsReadFromSysfs(t *testing.T) {
	root := pi5(t)
	other := usb1 + "/1-2"
	sysfstest.Apply(t, root, "usbdev "+other+" vendor=1234 product=0f10 serial=HIFACE-LIKE-UNIT-0042\nusbcard 6 "+other+"/1-2:1.0\n"+
		"proccard 6 hiFace \"Synthetic hiFace\" driver=snd-usb-hiface pcm=0.0\n")
	inv := observe(t, root)
	c, ok := inv.Card(6)
	if !ok || c.State != CardUSB || c.USB == nil || c.USB.ID.String() != "1234:0f10" || c.USB.Port != "platform-xhci-hcd.0/usb2-2:1.0" {
		t.Fatalf("card 6: %+v, %v", c, ok)
	}
	for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPort)} {
		want(t, Resolve(dev, inv), StateAvailable, 5)
	}
	for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
		dev, err := For(inv, 6, kind, Endpoint{})
		if err != nil {
			t.Fatalf("bind card 6 by %s: %v", kind, err)
		}
		want(t, Resolve(dev, inv), StateAvailable, 6)
	}
	for _, step := range []string{"file proc/asound/card5/usbid \"\"\n", "remove proc/asound/card5/usbid\n"} {
		sysfstest.Apply(t, root, step)
		inv = observe(t, root)
		if c, _ := inv.Card(5); c.State != CardTransient {
			t.Fatalf("the GO bar after %q: %+v", step, c)
		}
		want(t, Resolve(byModel("1234", "0f10"), inv), StateIdentityTransient, -1)
	}
}

// TestAVirtualCardIsUnsupportedNotTransient: a card with no parent device
// sits under devices/virtual/sound with no device link, as the kernel
// places it. It never had a link to lose, so it is not vanishing: it is not
// USB (identity_unsupported), cannot be bound, and blocks no binding.
func TestAVirtualCardIsUnsupportedNotTransient(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "dir sys/devices/virtual/sound/card3\nlink sys/class/sound/card3 sys/devices/virtual/sound/card3\n"+
		"proccard 3 Virtual \"A virtual card\" driver=Virtual pcm=0.0\n")
	inv := observe(t, root)
	if c, ok := inv.Card(3); !ok || c.State != CardUnsupported || !strings.Contains(c.Why, "virtual") {
		t.Fatalf("card 3: %+v, %v", c, ok)
	}
	for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPort)} {
		want(t, Resolve(dev, inv), StateAvailable, 5)
	}
	if _, err := For(inv, 3, config.BindingPort, Endpoint{}); err == nil || !strings.Contains(err.Error(), "identity_unsupported") {
		t.Errorf("bind the virtual card: %v", err)
	}
}

// TestAnUnreadableCardIsNeverRuledOut: a USB card whose identity cannot be
// read may be a second unit of the bound one, so a match beside it is
// ambiguous, never the first. It may also be a second card of the unit in
// a bound port, so a port binding's match is ambiguous beside it too, and
// bind refuses that binding, unless the match is on the interface the
// binding records, which no other card of that unit can be.
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
	r = Resolve(byPort(goBarPlace), inv)
	want(t, r, StateDeviceAmbiguous, -1)
	if !strings.Contains(r.Reason, "card 6") || !strings.Contains(r.Reason, "another card of the unit") {
		t.Errorf("the port alone: reason %s", r.Reason)
	}
	_, err := For(inv, 5, config.BindingPort, Endpoint{})
	if err == nil || !strings.Contains(err.Error(), "device_ambiguous") || strings.Contains(err.Error(), "bind by port") {
		t.Errorf("bind by port beside an unreadable card: %v", err)
	}
}

// TestTheEndpointMustBeThere: the unit present with no playback stream at
// the bound device and substream is endpoint_missing, on its card, and so
// is the unit in a bound port.
func TestTheEndpointMustBeThere(t *testing.T) {
	inv := observe(t, pi5(t))
	for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byPort(goBarPlace)} {
		dev.PCMDevice = 1
		want(t, Resolve(dev, inv), StateEndpointMissing, 5)
		dev.PCMDevice, dev.Subdevice = 0, 1
		want(t, Resolve(dev, inv), StateEndpointMissing, 5)
	}
	for _, kind := range []string{config.BindingSerial, config.BindingPort} {
		if _, err := For(inv, 5, kind, Endpoint{Device: 1}); err == nil || !strings.Contains(err.Error(), "endpoint_missing") {
			t.Errorf("bind by %s to an endpoint the card lacks: %v", kind, err)
		}
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
// its own fields, the serial whole, a port binding the port alone, and
// resolves to the card it was built from.
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
	if err != nil || p.Serial.Present() || p.VendorID != "" || p.Port != goBarPlace {
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
