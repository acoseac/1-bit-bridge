package alsa

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// sysfsManifest is a manifest in testdata/sysfs.
func sysfsManifest(name string) string { return filepath.Join("testdata", "sysfs", name+".txt") }

// readSysfs builds a manifest's tree and reads its sysfs half.
func readSysfs(t *testing.T, name string) (SysfsSnapshot, string) {
	t.Helper()
	root := sysfstest.Build(t, sysfsManifest(name))
	return SysfsReader{Root: filepath.Join(root, "sys")}.Read(), root
}

// cardOf returns the identity of card n in snap.
func cardOf(t *testing.T, snap SysfsSnapshot, n int) Identity {
	t.Helper()
	for _, c := range snap.Cards {
		if c.Card == n {
			return c
		}
	}
	t.Fatalf("no card %d in %+v", n, snap.Cards)
	return Identity{}
}

// TestSysfsReadsTheGoBarOnAPi5: the GO bar is a USB card with its ids,
// its serial, the port its long name gives (xhci-hcd.0, root port 1) in
// the anchored form, and interface 0; the HDMI cards are not USB, and so
// have no identity this reader can give.
func TestSysfsReadsTheGoBarOnAPi5(t *testing.T) {
	snap, _ := readSysfs(t, "pi5-go-bar")
	if !snap.Present || len(snap.Problems) != 0 {
		t.Fatalf("present %v, problems %q", snap.Present, snap.Problems)
	}
	if len(snap.Cards) != 3 {
		t.Fatalf("cards %+v", snap.Cards)
	}
	for _, n := range []int{0, 1} {
		c := cardOf(t, snap, n)
		if c.Kind != IdentityUnsupported || c.Bus != "platform" || c.USB != nil {
			t.Errorf("card %d: %+v, want a platform card with no USB identity", n, c)
		}
	}
	c := cardOf(t, snap, 5)
	if c.Kind != IdentityUSB || c.USB == nil || len(c.Problems) != 0 {
		t.Fatalf("card 5: %+v", c)
	}
	u := c.USB
	if u.ID != (USBID{Vendor: 0x20b1, Product: 0x3009}) || !u.Serial.Equal(usbid.New("GOBAR-7Q2X-91KZ-4471")) ||
		u.Port != "platform-xhci-hcd.0/usb2-1:1.0" || u.Interface != 0 || u.PortProblem != "" {
		t.Errorf("card 5's identity: %+v", u)
	}
	if want := []string{"platform-xhci-hcd.0", "platform-xhci-hcd.1"}; !slices.Equal(snap.Controllers, want) {
		t.Errorf("controllers %q, want %q", snap.Controllers, want)
	}
	if len(snap.Devices) != 1 || snap.Devices[0].Port() != "platform-xhci-hcd.0/usb2-1" || snap.Devices[0].Hub {
		t.Errorf("devices %+v", snap.Devices)
	}
}

// TestSysfsAnchorsThePi4sVL805AsUdevDoes: the anchor names the PCIe
// controller and the platform device above its PCI run, and the chain
// carries the VL805's own hub at root port 1.
func TestSysfsAnchorsThePi4sVL805AsUdevDoes(t *testing.T) {
	snap, _ := readSysfs(t, "pi4-vl805")
	c := cardOf(t, snap, 1)
	if c.USB == nil || c.USB.Port != "platform-fd500000.pcie/pci-0000:01:00.0/usb2-1.3:1.0" {
		t.Fatalf("card 1: %+v", c)
	}
	if !slices.Equal(snap.Controllers, []string{"platform-fd500000.pcie/pci-0000:01:00.0"}) {
		t.Errorf("controllers %q", snap.Controllers)
	}
	var hubs []string
	for _, d := range snap.Devices {
		if d.Hub {
			hubs = append(hubs, d.Port())
		}
	}
	if !slices.Equal(hubs, []string{"platform-fd500000.pcie/pci-0000:01:00.0/usb2-1"}) {
		t.Errorf("hubs %q", hubs)
	}
	if k := cardOf(t, snap, 0).Kind; k != IdentityUnsupported {
		t.Errorf("HDMI card 0 is %s", k)
	}
}

// TestSysfsPutsAHubInTheChain: a DAC behind a hub is the hub's port after
// the root port.
func TestSysfsPutsAHubInTheChain(t *testing.T) {
	snap, _ := readSysfs(t, "pi5-hub")
	c := cardOf(t, snap, 3)
	if c.USB == nil || c.USB.Port != "platform-xhci-hcd.0/usb2-2.4:1.0" || c.USB.ID.String() != "1234:0b02" {
		t.Fatalf("card 3: %+v", c)
	}
	if len(snap.Devices) != 2 || !snap.Devices[0].Hub || snap.Devices[0].Port() != "platform-xhci-hcd.0/usb2-2" {
		t.Errorf("devices %+v", snap.Devices)
	}
}

// TestSysfsReadsTheInterfaceTheCardIs: interface numbers differ between
// models, and a card without a serial has none.
func TestSysfsReadsTheInterfaceTheCardIs(t *testing.T) {
	snap, _ := readSysfs(t, "pi5-interfaces")
	a, b := cardOf(t, snap, 2), cardOf(t, snap, 3)
	if a.USB == nil || a.USB.Interface != 0 || a.USB.Port != "platform-xhci-hcd.0/usb2-1:1.0" || !a.USB.Serial.Present() {
		t.Errorf("model A: %+v", a)
	}
	if b.USB == nil || b.USB.Interface != 2 || b.USB.Port != "platform-xhci-hcd.0/usb2-2:1.2" || b.USB.Serial.Present() {
		t.Errorf("model B: %+v", b)
	}
}

// oneCard is a Pi 5 tree with one USB card, 7, on root port 1, whose
// serial file holds serial exactly.
func oneCard(serial string) string {
	return `platform sys/devices/platform/axi
platform sys/devices/platform/axi/xhci-hcd.0
roothub sys/devices/platform/axi/xhci-hcd.0/usb1 speed=480
usbdev sys/devices/platform/axi/xhci-hcd.0/usb1/1-1 vendor=1234 product=0d01 serialraw=` + serial + `
usbcard 7 sys/devices/platform/axi/xhci-hcd.0/usb1/1-1/1-1:1.0
`
}

// TestSysfsTrimsTheTailOfASerial: trailing NULs and whitespace are not
// the serial, and a serial file that is only those is no serial.
func TestSysfsTrimsTheTailOfASerial(t *testing.T) {
	for raw, want := range map[string]usbid.Serial{
		`"ABC123\n"`:             usbid.New("ABC123"),
		`"ABC123\x00\x00\x00\n"`: usbid.New("ABC123"),
		`"ABC123   \n"`:          usbid.New("ABC123"),
		`"ABC123\x00 \t\x00"`:    usbid.New("ABC123"),
		`"\x00\x00\n"`:           {},
		`"   \n"`:                {},
	} {
		root := sysfstest.BuildText(t, oneCard(raw))
		c, ok := SysfsReader{Root: filepath.Join(root, "sys")}.Card(7)
		if !ok || c.USB == nil {
			t.Fatalf("%s: %+v, %v", raw, c, ok)
		}
		if want.Present() != c.USB.Serial.Present() || (want.Present() && !c.USB.Serial.Equal(want)) {
			t.Errorf("serial file %s read as %v (present %v), want %v", raw, c.USB.Serial, c.USB.Serial.Present(), want.Present())
		}
	}
}

// TestSysfsBoundsWhatItReads: a file past 4 KiB is not read, a link loop
// ends, and nothing is read through a link that leaves the root or is
// absolute; each is the card's problem, and no other card is spoiled.
func TestSysfsBoundsWhatItReads(t *testing.T) {
	big := `"` + strings.Repeat("S", 4097) + `"`
	root := sysfstest.BuildText(t, oneCard(big)+`
link sys/class/sound/card8 sys/loop/a
link sys/loop/a sys/loop/b
link sys/loop/b sys/loop/a
dir sys/devices/elsewhere
link sys/class/sound/card9 !../../../outside/sound/card9
link sys/class/sound/card10 !`+filepath.ToSlash(string(filepath.Separator))+`etc
`)
	outside := filepath.Join(root, "outside", "sound", "card9")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := SysfsReader{Root: filepath.Join(root, "sys")}.Read()
	want := map[int]string{7: "longer than 4096 bytes", 8: "links", 9: "leaves the root", 10: "absolute"}
	for n, problem := range want {
		c := cardOf(t, snap, n)
		if c.Kind != IdentityUnreadable || !strings.Contains(strings.Join(c.Problems, "; "), problem) {
			t.Errorf("card %d: kind %s, problems %q; want unreadable, %q", n, c.Kind, c.Problems, problem)
		}
	}
}

// TestSysfsAnchorsOnlyWhatTheGrammarNames: a controller under a subsystem
// the port grammar does not name leaves a USB card without a port, said
// so, and still identified by its ids and serial.
func TestSysfsAnchorsOnlyWhatTheGrammarNames(t *testing.T) {
	root := sysfstest.BuildText(t, `subsys sys/devices/virtio0 virtio
platform sys/devices/virtio0/xhci-hcd.0
roothub sys/devices/virtio0/xhci-hcd.0/usb1 speed=480
usbdev sys/devices/virtio0/xhci-hcd.0/usb1/1-1 vendor=1234 product=0d02 serial=VIRTIO-0000000001
usbcard 4 sys/devices/virtio0/xhci-hcd.0/usb1/1-1/1-1:1.0
`)
	c, ok := SysfsReader{Root: filepath.Join(root, "sys")}.Card(4)
	if !ok || c.Kind != IdentityUSB || c.USB == nil || c.USB.Port != "" ||
		!strings.Contains(c.USB.PortProblem, `"virtio"`) || !c.USB.Serial.Present() {
		t.Errorf("card 4: %+v", c)
	}
}

// TestSysfsWithoutClassSoundIsNotPresent: a tree with no class/sound (no
// sysfs, as on macOS or in a container without /sys) says so, and reads
// no card.
func TestSysfsWithoutClassSoundIsNotPresent(t *testing.T) {
	snap := SysfsReader{Root: t.TempDir()}.Read()
	if snap.Present || len(snap.Cards) != 0 {
		t.Errorf("an empty tree: %+v", snap)
	}
	if _, ok := (SysfsReader{Root: t.TempDir()}).Card(0); ok {
		t.Error("Card(0) found a card in an empty tree")
	}
}

// TestSysfsCapsTheCardsAndTheDevices: a tree that lists more than the caps
// is read up to them, and says so.
func TestSysfsCapsTheCardsAndTheDevices(t *testing.T) {
	var m strings.Builder
	m.WriteString("platform sys/devices/platform/axi\nplatform sys/devices/platform/axi/xhci-hcd.0\nroothub sys/devices/platform/axi/xhci-hcd.0/usb1 speed=480\n")
	for i := 1; i <= 6; i++ {
		dev := "sys/devices/platform/axi/xhci-hcd.0/usb1/1-" + itoa(i)
		m.WriteString("usbdev " + dev + " vendor=1234 product=0e0" + itoa(i) + "\n")
		m.WriteString("usbcard " + itoa(i) + " " + dev + "/1-" + itoa(i) + ":1.0\n")
	}
	root := sysfstest.BuildText(t, m.String())
	r := SysfsReader{Root: filepath.Join(root, "sys"), limits: &sysfsLimits{entries: 4, file: maxSysfsFile, links: maxSysfsLinks, hops: maxSysfsHops}}
	snap := r.Read()
	if len(snap.Cards) > 4 || len(snap.Devices) > 4 || !strings.Contains(strings.Join(snap.Problems, "; "), "more than 4") {
		t.Errorf("cards %d, devices %d, problems %q", len(snap.Cards), len(snap.Devices), snap.Problems)
	}
}

// TestSysfsCardReadsOneCardAgain: the check after open reads one card's
// identity again, and a card that is not there is not found.
func TestSysfsCardReadsOneCardAgain(t *testing.T) {
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	r := SysfsReader{Root: filepath.Join(root, "sys")}
	c, ok := r.Card(5)
	if !ok || c.USB == nil || c.USB.Port != "platform-xhci-hcd.0/usb2-1:1.0" {
		t.Fatalf("Card(5) = %+v, %v", c, ok)
	}
	if _, ok := r.Card(6); ok {
		t.Error("Card(6) found a card the tree does not have")
	}
	sysfstest.Apply(t, root, "unplug sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1\n")
	if _, ok := r.Card(5); ok {
		t.Error("Card(5) found the card after the unplug")
	}
}

// TestHostReadsAreWatched: reading the host's own /proc or /sys tells the
// watch first, so a test that must read no hardware can fail on it; a
// tree a test names never does.
func TestHostReadsAreWatched(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	refused := errors.New("refused by the test")
	stop := WatchHostReads(func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, path)
		return refused
	})
	defer stop()
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	_ = SysfsReader{Root: filepath.Join(root, "sys")}.Read()
	_, _ = Reader{ProcRoot: filepath.Join(root, "proc")}.Read()
	mu.Lock()
	if len(seen) != 0 {
		t.Errorf("a named tree was reported as the host's: %q", seen)
	}
	mu.Unlock()
	// The watch refuses, so nothing of this host is read here.
	if snap := (SysfsReader{}).Read(); snap.Present || len(snap.Cards) != 0 || !strings.Contains(strings.Join(snap.Problems, " "), refused.Error()) {
		t.Errorf("a refused host sysfs read: %+v", snap)
	}
	if _, ok := (SysfsReader{}).Card(0); ok {
		t.Error("a refused host sysfs read found a card")
	}
	if _, err := (Reader{}).Read(); !errors.Is(err, refused) {
		t.Errorf("a refused host /proc read: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != DefaultSysfsRoot || seen[1] != DefaultSysfsRoot || seen[2] != DefaultProcRoot {
		t.Errorf("host reads seen: %q (on %s)", seen, runtime.GOOS)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
