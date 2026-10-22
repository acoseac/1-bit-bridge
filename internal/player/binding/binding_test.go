package binding

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
// recorded, or another, neither. A card the check's re-read of sysfs cannot
// identify (unreadable, or vanished as it was read) may be another card of
// the unit in the port, so it refuses there as Resolve does, and so does
// sysfs that cannot be read again; a card that is not USB, or a readable
// one at another port, refuses nothing.
func TestTheCheckAfterOpenFollowsAPortBinding(t *testing.T) {
	root := pi5(t)
	sys := alsa.SysfsReader{Root: filepath.Join(root, "sys")}
	inv := observe(t, root)
	node := alsapcm.NodeInfo{Card: 5, Playback: true}
	place, older, other, serial := Resolve(byPort(goBarPlace), inv), Resolve(byPort(goBarPort), inv),
		Resolve(byPort(goBarPlace+":1.2"), inv), Resolve(bySerial(goBarSerial), inv)
	for _, r := range []Resolution{place, older, other, serial} {
		want(t, r, StateAvailable, 5)
		if err := (Checker{Sysfs: sys, Want: r}).Check(node); err != nil {
			t.Fatalf("the bound node: %v", err)
		}
	}
	check := func(r Resolution) error { return Checker{Sysfs: sys, Want: r}.Check(node) }

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

	// A unit swapped in whose other card the re-read cannot identify:
	// registered on the unit itself rather than on an interface (unreadable),
	// or losing its device link as it goes (vanished). Resolve on the same
	// tree answers device_ambiguous or identity_transient, so the check
	// after open refuses card 5 too, unless it is on the interface the
	// binding records, which no other card of that unit can be.
	for _, tc := range []struct {
		name, layout, then, says string
		resolves                 State
	}{
		{"unreadable", plug(goBar, 5, "1234:0c20", "") + "card 6 " + goBar + "\nproccard 6 Other6 \"Synthetic DAC, second card\" driver=snd-synthetic pcm=0.0\n", "",
			"card 6 could not be read", StateDeviceAmbiguous},
		{"vanished", composite(goBar, 5, 6, "1234:0c20"), "remove " + goBar + "/1-1:1.2/sound/card6/device\n",
			"card 6 is being added or removed", StateIdentityTransient},
	} {
		sysfstest.Apply(t, root, "unplug "+goBar+"\n"+tc.layout)
		if tc.then != "" {
			sysfstest.Apply(t, root, tc.then)
		}
		want(t, Resolve(byPort(goBarPlace), observe(t, root)), tc.resolves, -1)
		for name, r := range map[string]Resolution{"no interface recorded": place, "another interface recorded": other} {
			err := check(r)
			if !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), tc.says) || !strings.Contains(err.Error(), goBarPlace) {
				t.Errorf("%s, beside card 5 a card that is %s: %v", name, tc.name, err)
			}
		}
		if err := check(older); err != nil {
			t.Errorf("the card on the recorded interface, beside a card that is %s: %v", tc.name, err)
		}
	}

	// A card that is not USB (the HDMI cards) and a readable card at another
	// port are ruled out. A reading that cannot list the cards rules out
	// nothing, as it does not for Resolve: the opened card is not in it.
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+plugOn(goBar, 1, 5, "1234:0c12", "HUGO2-UNIT-0000000042")+plug(usb1+"/1-2", 7, "20b1:3009", goBarSerial))
	if err := check(place); err != nil {
		t.Errorf("a unit with one card in the port, another at port 2, and the HDMI cards: %v", err)
	}
	const notThere = "card 5 is not a USB card whose identity sysfs gives now"
	t.Run("sysfs that cannot list its cards", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a POSIX permission bit a non-root reader honours")
		}
		classSound := filepath.Join(root, "sys", "class", "sound")
		if err := os.Chmod(classSound, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(classSound, 0o755) })
		if sys.Read().Present {
			t.Fatal("premise: class/sound still lists")
		}
		if err := check(place); !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), notThere) {
			t.Errorf("sysfs that cannot list its cards: %v", err)
		}
	})
	sysfstest.Apply(t, root, "unplug "+goBar+"\n")
	if err := check(place); !errors.Is(err, ErrWrongDevice) || !strings.Contains(err.Error(), notThere) {
		t.Errorf("card 5 gone from the port: %v", err)
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
