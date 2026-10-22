package alsa

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// DefaultSysfsRoot is the host's sysfs, which a zero SysfsReader reads.
const DefaultSysfsRoot = "/sys"

// SysfsReader reads what sysfs says about the hardware behind each sound
// card, for the player's binding (B470 PR 4a): for a USB card, the unit
// (its vendor and product ids and its serial), the anchored port it sits
// on (usbid.Port) and the interface the card is. A card index and an ALSA
// id are observations here, as in Reader, and never an identity.
//
// It is passive and bounded. It opens files under Root and nothing else:
// every symlink is resolved component by component inside Root, an
// absolute link or one that climbs out of Root is refused, a chain of more
// than 40 links is a loop, a walk up a device's ancestors stops after 32,
// a file is read to 4 KiB at most, and a directory is listed to 1024
// entries at most. Like Reader it carries no build tag, so its tests read
// fake trees (internal/alsa/sysfstest) on every OS.
type SysfsReader struct {
	// Root is the sysfs tree; "" means DefaultSysfsRoot.
	Root string
	// limits replaces the bounds, for the tests that reach them.
	limits *sysfsLimits
	// beforeOpen, when set, is called with an attribute's path after the
	// attribute is checked and before it is opened, and with "." just
	// before the root itself is opened: the windows a test swaps a FIFO in.
	beforeOpen func(rel string)
}

// The reader's bounds; links and hops are maxLinks and maxHops.
const (
	maxSysfsFile    = 4096
	maxSysfsEntries = 1024
	// maxSysfsProblems bounds the problems one snapshot keeps.
	maxSysfsProblems = 16
)

// sysfsLimits are SysfsReader's bounds.
type sysfsLimits struct {
	entries, file, links, hops int
}

// IdentityKind is what sysfs lets the reader say about a card's hardware.
type IdentityKind string

const (
	// IdentityUSB is a USB card whose unit and port were read.
	IdentityUSB IdentityKind = "usb"
	// IdentityUnsupported is a card that is not USB (HDMI, an I2S HAT, a
	// PCI card), which has no identity a binding names, so it cannot be
	// bound.
	IdentityUnsupported IdentityKind = "identity_unsupported"
	// IdentityUnreadable is a card whose files do not say who it is: a
	// link that does not resolve, an attribute that cannot be read, a USB
	// card that is not under an interface. Its problems say which.
	IdentityUnreadable IdentityKind = "unreadable"
)

// Identity is what sysfs says about one card.
type Identity struct {
	// Card is the N of sysfs's class/sound/cardN.
	Card int          `json:"card"`
	Kind IdentityKind `json:"kind"`
	// Bus is the subsystem of the card's device: "usb", "platform", "pci",
	// "virtual" for a card with no device (one under devices/virtual), or
	// "" when it could not be read.
	Bus string `json:"bus"`
	// USB is the unit's identity, for a USB card.
	USB *USBIdentity `json:"usb"`
	// Vanished says a link or a file the card had went while it was read:
	// a card the kernel is removing (or adding), whose reading is not an
	// answer. A device link a virtual card never has is not one.
	Vanished bool `json:"vanished"`
	// Problems are what could not be read, by file, never quoting a
	// serial.
	Problems []string `json:"problems"`
}

// USBIdentity is a USB card's unit and place.
type USBIdentity struct {
	// ID is the unit's idVendor and idProduct.
	ID USBID `json:"id"`
	// Serial is the unit's serial, trailing NULs and whitespace trimmed;
	// absent when the unit has none. It prints and encodes redacted.
	Serial usbid.Serial `json:"serial"`
	// Port is the card's anchored port path, "" when the controller is
	// under something the port grammar does not name (PortProblem says
	// what): such a card can be bound by serial or by model, not by port.
	Port        string `json:"port"`
	PortProblem string `json:"portProblem"`
	// Interface is the number of the USB interface the card is.
	Interface int `json:"interface"`
}

// USBDevice is one USB device sysfs lists (bus/usb/devices), root hubs and
// interfaces aside: where it sits, and whether it is a hub. A port
// binding's check that a hub has taken its port reads these.
type USBDevice struct {
	Anchor string `json:"anchor"`
	Gen    int    `json:"gen"`
	Chain  string `json:"chain"`
	// Hub is a device whose class is 09.
	Hub bool `json:"hub"`
}

// Port is the device's place as a port path without the configuration
// and interface: "platform-xhci-hcd.0/usb2-1".
func (d USBDevice) Port() string { return d.Anchor + "/usb" + strconv.Itoa(d.Gen) + "-" + d.Chain }

// SysfsSnapshot is one reading of every sound card's identity, and of the
// USB controllers and devices a port binding is judged against.
type SysfsSnapshot struct {
	Root string `json:"root"`
	// Present says the tree has class/sound: a host with no sysfs (macOS,
	// a container without /sys) has none, and nothing here says who a
	// card is.
	Present bool `json:"present"`
	// CardsComplete says Cards holds every card class/sound lists: it is
	// Present and its listing was not cut at the entry cap. A reading cut
	// short may leave a card out, which only a problem would otherwise say.
	CardsComplete bool       `json:"cardsComplete"`
	Cards         []Identity `json:"cards"`
	// Controllers are the anchors of every USB controller with a root hub,
	// sorted, each once.
	Controllers []string    `json:"controllers"`
	Devices     []USBDevice `json:"devices"`
	Problems    []string    `json:"problems"`
}

// hostWatch is WatchHostReads' function, nil while nothing watches.
var hostWatch atomic.Pointer[func(string) error]

// WatchHostReads calls f before Reader or SysfsReader reads anything under
// the host's own trees (DefaultProcRoot and DefaultSysfsRoot), with that
// root, until stop is called. When f returns an error the read does not
// happen and returns it. It is a tripwire for a test binary that must read
// no hardware (one that fails, and refuses, on the call); a tree a test
// names never reaches it.
func WatchHostReads(f func(root string) error) (stop func()) {
	hostWatch.Store(&f)
	return func() { hostWatch.CompareAndSwap(&f, nil) }
}

// checkHostRead asks the watch, when there is one and root is one of the
// host's own trees.
func checkHostRead(root, hostRoot string) error {
	w := hostWatch.Load()
	if w == nil {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(root))
	if clean != hostRoot && !strings.HasPrefix(clean, hostRoot+"/") {
		return nil
	}
	return (*w)(hostRoot)
}

// Read takes one reading. It never fails: what cannot be read is a problem,
// on the card it is about or on the snapshot.
func (r SysfsReader) Read() SysfsSnapshot {
	snap := SysfsSnapshot{Root: r.root(), Cards: []Identity{}, Controllers: []string{}, Devices: []USBDevice{}, Problems: []string{}}
	t, err := r.open()
	if err != nil {
		snap.addProblem("%s: %s", snap.Root, errText(err))
		return snap
	}
	defer t.close()
	names, more, err := t.list("class/sound")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			snap.addProblem("class/sound: %s", errText(err))
		}
		return snap
	}
	snap.Present, snap.CardsComplete = true, !more
	if more {
		snap.addProblem("class/sound lists more than %d entries; the rest are not read", t.lim.entries)
	}
	var cards []int
	for _, name := range names {
		if n, ok := numbered(cardName, "card", name); ok {
			cards = append(cards, n)
		}
	}
	sort.Ints(cards)
	for _, n := range cards {
		snap.Cards = append(snap.Cards, t.card(n))
	}
	t.usb(&snap)
	return snap
}

// Card reads one card's identity again: the check after open reads the
// card the kernel says it opened. It is false when sysfs has no such card.
func (r SysfsReader) Card(n int) (Identity, bool) {
	t, err := r.open()
	if err != nil {
		return Identity{}, false
	}
	defer t.close()
	if _, err := t.root.Lstat(filepath.FromSlash("class/sound/card" + strconv.Itoa(n))); err != nil {
		return Identity{}, false
	}
	return t.card(n), true
}

func (r SysfsReader) root() string {
	if r.Root == "" {
		return DefaultSysfsRoot
	}
	return r.Root
}

// open opens the tree's root (openTree), after asking the watch when it is
// the host's.
func (r SysfsReader) open() (*sysfsTree, error) {
	if err := checkHostRead(r.root(), DefaultSysfsRoot); err != nil {
		return nil, err
	}
	lim := sysfsLimits{entries: maxSysfsEntries, file: maxSysfsFile, links: maxLinks, hops: maxHops}
	if r.limits != nil {
		lim = *r.limits
	}
	var before func()
	if r.beforeOpen != nil {
		before = func() { r.beforeOpen(".") }
	}
	tr, err := openTree(r.root(), lim.links, lim.hops, before)
	if err != nil {
		return nil, err
	}
	return &sysfsTree{tree: tr, lim: lim, beforeOpen: r.beforeOpen}, nil
}

// addProblem records a snapshot problem, at most maxSysfsProblems.
func (s *SysfsSnapshot) addProblem(format string, args ...any) {
	if len(s.Problems) < maxSysfsProblems {
		s.Problems = append(s.Problems, fmt.Sprintf(format, args...))
	}
}

// cardName matches class/sound's card entries; usbInterfaceName and
// usbDeviceName a USB interface's and a USB device's sysfs names.
var (
	cardName         = regexp.MustCompile(`^card(\d+)$`)
	usbInterfaceName = regexp.MustCompile(`^(\d+)-(\d+(?:\.\d+)*):(\d+)\.(\d+)$`)
	usbDeviceName    = regexp.MustCompile(`^(\d+)-(\d+(?:\.\d+)*)$`)
	rootHubName      = regexp.MustCompile(`^usb(\d+)$`)
)

// fileError is a file that could not be read, printed without the path a
// *fs.PathError repeats, and still errors.Is what it wraps.
type fileError struct {
	file string
	err  error
}

func (e fileError) Error() string { return e.file + ": " + errText(e.err) }

func (e fileError) Unwrap() error { return e.err }

// sysfsTree is one reading's open root (tree) and its bounds.
type sysfsTree struct {
	*tree
	lim        sysfsLimits
	beforeOpen func(rel string)
}

// list names a directory's entries, at most the cap, sorted; more is true
// when the directory holds more than that. It opens the directory
// directory-only (openDirectory), so a FIFO in its place is refused at once.
func (t *sysfsTree) list(rel string) (names []string, more bool, err error) {
	dir, err := t.resolve(rel)
	if err != nil {
		return nil, false, err
	}
	f, err := openDirectory(t.opener(dir))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	names, err = f.Readdirnames(t.lim.entries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(names) > t.lim.entries {
		names, more = names[:t.lim.entries], true
	}
	sort.Strings(names)
	return names, more, nil
}

// attr reads one attribute file in a directory resolve returned, to the
// cap: a regular file, never a link. The type that decides is the opened
// descriptor's (openRegular), so a FIFO swapped in after the look is
// refused, never read.
func (t *sysfsTree) attr(dir, name string) ([]byte, error) {
	rel := path.Join(dir, name)
	fi, err := t.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotAFile
	}
	if t.beforeOpen != nil {
		t.beforeOpen(rel)
	}
	f, err := openRegular(t.opener(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAllCapped(f, int64(t.lim.file))
}

// subsystem is the name of the subsystem dir's subsystem link points at
// ("usb", "pci", "platform"), or "" when it has none.
func (t *sysfsTree) subsystem(dir string) string {
	text, err := t.root.Readlink(filepath.FromSlash(path.Join(dir, "subsystem")))
	if err != nil {
		return ""
	}
	return path.Base(filepath.ToSlash(text))
}

// virtualDevices is where the kernel places a class device that has no
// parent device (get_device_parent): a sound card there has no device link,
// and never had one.
const virtualDevices = "devices/virtual/"

// card reads card n's identity. What it needed and could not find makes
// the card Vanished: the card class/sound listed, gone before its link
// resolved, or a link or attribute the card had and lost. The kernel
// removes a card's device link first when it removes the card
// (device_remove_class_symlinks), and makes it before the class/sound link
// that lists the card, so a missing device link is a card going, unless the
// card is under devices/virtual, which never has one: such a card is not
// USB, and nothing vanished.
func (t *sysfsTree) card(n int) Identity {
	id := Identity{Card: n, Kind: IdentityUnreadable, Problems: []string{}}
	name := "card" + strconv.Itoa(n)
	fail := func(err error, format string, args ...any) Identity {
		if errors.Is(err, fs.ErrNotExist) {
			id.Vanished = true
		}
		id.Problems = append(id.Problems, fmt.Sprintf(format, args...))
		return id
	}
	cardDir, err := t.resolve("class/sound/" + name)
	if err != nil {
		return fail(err, "class/sound/%s: %s", name, errText(err))
	}
	devDir, err := t.resolve(cardDir + "/device")
	switch {
	case errors.Is(err, fs.ErrNotExist) && strings.HasPrefix(cardDir, virtualDevices):
		id.Kind, id.Bus = IdentityUnsupported, "virtual"
		return id
	case err != nil:
		return fail(err, "%s/device: %s", name, errText(err))
	}
	id.Bus = t.subsystem(devDir)
	switch id.Bus {
	case "usb":
	case "":
		return fail(nil, "%s/device: no subsystem says what bus the card is on", name)
	default:
		id.Kind = IdentityUnsupported
		return id
	}
	usb, err := t.usbIdentity(devDir)
	if err != nil {
		return fail(err, "%s: %s", name, err)
	}
	id.Kind, id.USB = IdentityUSB, usb
	return id
}

// usbIdentity reads the unit and the place of the USB interface iface, a
// sound card's device.
func (t *sysfsTree) usbIdentity(iface string) (*USBIdentity, error) {
	m := usbInterfaceName.FindStringSubmatch(path.Base(iface))
	if m == nil {
		return nil, fmt.Errorf("the card's USB device %s is not an interface", path.Base(iface))
	}
	unit := path.Dir(iface)
	if path.Base(unit) != m[1]+"-"+m[2] {
		return nil, fmt.Errorf("the interface %s is not under its device", path.Base(iface))
	}
	u := &USBIdentity{}
	for _, f := range []struct {
		name string
		into *uint16
	}{{"idVendor", &u.ID.Vendor}, {"idProduct", &u.ID.Product}} {
		b, err := t.attr(unit, f.name)
		if err != nil {
			return nil, fileError{f.name, err}
		}
		v, err := usbid.ParseHex16(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: %s", f.name, err)
		}
		*f.into = v
	}
	b, err := t.attr(unit, "serial")
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fileError{"serial", err}
	default:
		u.Serial = usbid.New(string(b))
	}
	iface8, ok := portNum(m[4], 0)
	if !ok {
		return nil, fmt.Errorf("interface %s is out of range", m[4])
	}
	u.Interface = iface8
	port, err := t.port(unit, m)
	if err != nil {
		u.PortProblem = err.Error()
	} else {
		u.Port = port
	}
	return u, nil
}

// port is the anchored port path of the interface m names (bus, chain,
// configuration, interface) on the device unit.
func (t *sysfsTree) port(unit string, m []string) (string, error) {
	hub, err := t.rootHub(unit)
	if err != nil {
		return "", err
	}
	if path.Base(hub) != "usb"+m[1] {
		return "", fmt.Errorf("the root hub above the device is %s, not usb%s", path.Base(hub), m[1])
	}
	gen, err := t.gen(hub)
	if err != nil {
		return "", err
	}
	segs, err := t.anchor(path.Dir(hub))
	if err != nil {
		return "", err
	}
	p := usbid.Port{Segments: segs, Gen: gen}
	for _, c := range strings.Split(m[2], ".") {
		n, ok := portNum(c, 1)
		if !ok {
			return "", fmt.Errorf("port %s of the chain is out of range", c)
		}
		p.Chain = append(p.Chain, n)
	}
	var ok bool
	if p.Config, ok = portNum(m[3], 1); !ok {
		return "", fmt.Errorf("configuration %s is out of range", m[3])
	}
	if p.Interface, ok = portNum(m[4], 0); !ok {
		return "", fmt.Errorf("interface %s is out of range", m[4])
	}
	s := p.String()
	if _, err := usbid.ParsePort(s); err != nil {
		return "", fmt.Errorf("the port path is outside the port grammar: %w", err)
	}
	return s, nil
}

// portNum reads a sysfs number as the port grammar takes it: decimal, from
// min to 255, which sysfs writes with no leading zero.
func portNum(s string, min int) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > 255 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

// rootHub walks up from a USB device while its parent is a USB device too,
// and returns the topmost: the root hub.
func (t *sysfsTree) rootHub(dir string) (string, error) {
	for hops := 0; ; hops++ {
		if hops > t.lim.hops {
			return "", fmt.Errorf("more than %d USB devices above the unit", t.lim.hops)
		}
		parent := path.Dir(dir)
		if parent == "." || parent == dir {
			return "", errors.New("no controller above the unit's root hub")
		}
		if t.subsystem(parent) != "usb" {
			return dir, nil
		}
		dir = parent
	}
}

// gen is a root hub's USB generation, from its speed: 1 up to 12 Mbit/s, 2
// at 480, 3 at 5000 and up.
func (t *sysfsTree) gen(hub string) (int, error) {
	b, err := t.attr(hub, "speed")
	if err != nil {
		return 0, fmt.Errorf("%s/speed: %s", path.Base(hub), errText(err))
	}
	switch s := strings.TrimSpace(string(b)); s {
	case "1.5", "12":
		return 1, nil
	case "480":
		return 2, nil
	case "5000", "10000", "20000", "40000":
		return 3, nil
	default:
		return 0, fmt.Errorf("%s/speed %s is not a USB speed", path.Base(hub), quote(s))
	}
}

// anchor names the controller dir as udev's path_id does: walking up, the
// nearest PCI device and the nearest platform device above each run of
// one kind, outermost first. A directory with no subsystem is passed over
// (pci0000:00, devices/platform); any other subsystem leaves the
// controller without an anchor the port grammar names.
func (t *sysfsTree) anchor(dir string) ([]string, error) {
	var segs []string
	for hops := 0; dir != "." && dir != "" && dir != "devices"; hops++ {
		if hops > t.lim.hops {
			return nil, fmt.Errorf("more than %d devices above the controller", t.lim.hops)
		}
		switch sub := t.subsystem(dir); sub {
		case "pci", "platform":
			segs = append([]string{sub + "-" + path.Base(dir)}, segs...)
			for {
				parent := path.Dir(dir)
				if parent == "." || parent == dir || t.subsystem(parent) != sub {
					break
				}
				dir = parent
			}
		case "":
		default:
			return nil, fmt.Errorf("the USB controller is under a %s device, which a port path does not name", quote(sub))
		}
		dir = path.Dir(dir)
	}
	if len(segs) == 0 {
		return nil, errors.New("no PCI or platform device above the USB controller")
	}
	return segs, nil
}

// usb reads the root hubs and the devices bus/usb/devices lists into snap:
// the controllers' anchors and each device's place.
func (t *sysfsTree) usb(snap *SysfsSnapshot) {
	names, more, err := t.list("bus/usb/devices")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			snap.addProblem("bus/usb/devices: %s", errText(err))
		}
		return
	}
	if more {
		snap.addProblem("bus/usb/devices lists more than %d entries; the rest are not read", t.lim.entries)
	}
	type root struct {
		anchor string
		gen    int
	}
	roots := map[string]root{}
	anchors := map[string]bool{}
	for _, name := range names {
		m := rootHubName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		dir, err := t.resolve("bus/usb/devices/" + name)
		if err != nil {
			snap.addProblem("bus/usb/devices/%s: %s", name, errText(err))
			continue
		}
		gen, err := t.gen(dir)
		if err != nil {
			snap.addProblem("%s", err)
			continue
		}
		segs, err := t.anchor(path.Dir(dir))
		if err != nil {
			snap.addProblem("%s: %s", name, err)
			continue
		}
		a := strings.Join(segs, "/")
		roots[m[1]] = root{anchor: a, gen: gen}
		anchors[a] = true
	}
	for a := range anchors {
		snap.Controllers = append(snap.Controllers, a)
	}
	sort.Strings(snap.Controllers)
	for _, name := range names {
		m := usbDeviceName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		r, ok := roots[m[1]]
		if !ok {
			continue
		}
		dir, err := t.resolve("bus/usb/devices/" + name)
		if err != nil {
			snap.addProblem("bus/usb/devices/%s: %s", name, errText(err))
			continue
		}
		class, _ := t.attr(dir, "bDeviceClass")
		snap.Devices = append(snap.Devices, USBDevice{Anchor: r.anchor, Gen: r.gen, Chain: m[2],
			Hub: strings.TrimSpace(string(class)) == "09"})
	}
	sort.Slice(snap.Devices, func(i, j int) bool { return snap.Devices[i].Port() < snap.Devices[j].Port() })
}
