package playback

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

const goBar = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"

// goBarLocator is a BindingLocator of the Pi 5 tree's GO bar, bound by
// serial, over a tree the test lays out.
func goBarLocator(t *testing.T) (BindingLocator, string) {
	t.Helper()
	root := sysfstest.Build(t, filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	return BindingLocator{
		Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009",
			Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")},
		Observer: binding.Observer{Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}, Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")}},
	}, root
}

// TestBindingLocatorNamesTheBoundUnitAndItsCheck pins the locator: on the
// box's trees, a binding that resolves gives its card's endpoint, a label
// with no serial in it, and 4a's check after open, which admits that card's
// playback node and refuses any other.
func TestBindingLocatorNamesTheBoundUnitAndItsCheck(t *testing.T) {
	loc, _ := goBarLocator(t)
	u, err := loc.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Endpoint != (alsapcm.Endpoint{Card: 5, Device: 0}) {
		t.Errorf("the endpoint %v, want card 5 device 0", u.Endpoint)
	}
	if err := u.Admit(alsapcm.NodeInfo{Card: 5, Device: 0, Subdevice: 0, Playback: true}); err != nil {
		t.Errorf("the check refused the bound node: %v", err)
	}
	for _, n := range []alsapcm.NodeInfo{
		{Card: 0, Device: 0, Subdevice: 0, Playback: true},
		{Card: 5, Device: 0, Subdevice: 1, Playback: true},
		{Card: 5, Device: 0, Subdevice: 0, Playback: false},
	} {
		if err := u.Admit(n); !errors.Is(err, binding.ErrWrongDevice) {
			t.Errorf("the check of %+v: %v, want ErrWrongDevice", n, err)
		}
	}
	if u.Label == "" || strings.Contains(u.Label, "GOBAR-7Q2X-91KZ-4471") {
		t.Errorf("the label %q: the unit named, never by its serial", u.Label)
	}
}

// TestBindingLocatorRefusesWhatDoesNotResolve pins a binding that names no
// unit now: an unplugged DAC is a UnitError with the binding's state,
// device_missing, and so is a second unit of a uniqueModel, ambiguous.
func TestBindingLocatorRefusesWhatDoesNotResolve(t *testing.T) {
	loc, root := goBarLocator(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n")
	_, err := loc.Locate(context.Background())
	var ue *UnitError
	if !errors.As(err, &ue) || ue.State != binding.StateDeviceMissing {
		t.Fatalf("Locate with the DAC unplugged: %v, want device_missing", err)
	}

	loc, root = goBarLocator(t)
	loc.Device = config.PlayerDevice{Transport: "usb", Binding: config.BindingUniqueModel, VendorID: "20b1", ProductID: "3009"}
	other := "sys/devices/platform/axi/1000120000.pcie/1f00300000.usb/xhci-hcd.1/usb3/3-1"
	sysfstest.Apply(t, root, "usbdev "+other+" vendor=20b1 product=3009 serial=OTHER-0001 devnum=2\n"+
		"usbcard 6 "+other+"/3-1:1.0\nproccard 6 Audio_1 \"iFi (by AMR) HD USB Audio\" usb=20b1:3009 pcm=0.0\n")
	if _, err := loc.Locate(context.Background()); !errors.As(err, &ue) || ue.State != binding.StateDeviceAmbiguous {
		t.Fatalf("Locate with two units of a uniqueModel: %v, want device_ambiguous", err)
	}
}

// TestNewRequiresItsSeams pins New's refusals: no resolver, no locator.
func TestNewRequiresItsSeams(t *testing.T) {
	if _, err := New(Config{Locator: BindingLocator{}}); err == nil {
		t.Error("New with no Resolver")
	}
	if _, err := New(Config{Resolver: nopResolver{}}); err == nil {
		t.Error("New with no Locator")
	}
}

type nopResolver struct{}

func (nopResolver) Open(context.Context, engine.Reference) (Content, error) {
	return Content{}, errors.New("unused")
}

// TestThePackingsAreTheDesignsTable pins B470's §1 table and its order: the
// container as wide as the samples first.
func TestThePackingsAreTheDesignsTable(t *testing.T) {
	want := map[int][]format.ALSAFormat{16: {format.S16LE}, 24: {format.S243LE, format.S24LE, format.S32LE}, 32: {format.S32LE}}
	if len(packings) != len(want) {
		t.Fatalf("packings %v", packings)
	}
	for bits, fs := range want {
		if !slices.Equal(packings[bits], fs) {
			t.Errorf("%d bits: %v, want %v", bits, packings[bits], fs)
		}
	}
}

// TestANoteNamesTheKernelsStatusFile pins the path the owner reads to see
// the device closed.
func TestANoteNamesTheKernelsStatusFile(t *testing.T) {
	n := Note{Params: alsapcm.Params{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}, Subdevice: 0}}
	if got := n.StatusPath(); got != "/proc/asound/card5/pcm0p/sub0/status" {
		t.Errorf("status path %q", got)
	}
}
