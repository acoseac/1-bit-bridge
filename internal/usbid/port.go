package usbid

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ParseHex16 reads a USB vendor or product id the way the player's config
// takes one: four hexadecimal digits in either case ("0A12", "0a12"), or
// the same four after a lowercase "0x" ("0x0a12"). Anything else is
// refused: fewer or more digits, a sign, a space, "0X". Hex16 is the
// canonical form. The refusal does not quote the value.
func ParseHex16(s string) (uint16, error) {
	digits := strings.TrimPrefix(s, "0x")
	if len(digits) != 4 {
		return 0, errors.New("must be four hexadecimal digits, alone or after 0x")
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := digits[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, errors.New("must be four hexadecimal digits, alone or after 0x")
		}
		v = v<<4 | uint16(d)
	}
	return v, nil
}

// Hex16 is an id's canonical form: four lowercase hexadecimal digits, as
// sysfs's idVendor and idProduct print it.
func Hex16(v uint16) string { return fmt.Sprintf("%04x", v) }

// Port is the anchored path of the USB interface a sound card is: the
// controller the bus hangs from, the root hub's USB generation, the chain
// of ports from the root hub down to the unit, and the configuration and
// interface the card sits on. It names a place on the box and never a
// unit, so a port binding means "this port". The bus number, the device
// number and a bus-relative "<bus>-<chain>" alone are never part of it:
// the kernel numbers buses in the order controllers probe and devices in
// the order they enumerate.
//
// Its string form, the device.port a port binding stores, is
//
//	port      = anchor "/" usb
//	anchor    = segment *3( "/" segment )      ; outermost first
//	segment   = "pci-" pciaddr / "platform-" name
//	usb       = "usb" gen "-" chain ":" config "." interface
//	gen       = "1" / "2" / "3"                ; the root hub's speed: up to 12 Mbit/s, 480, 5000 and up
//	chain     = number *6( "." number )        ; the root hub's port first, then each hub's
//	config    = number
//	interface = "0" / number
//	number    = 1 to 255 in decimal, no leading zero
//	pciaddr   = 4*8hex ":" 2hex ":" 2hex "." %x30-37   ; lowercase, as sysfs names a PCI device
//	name      = 1*64( ALPHA / DIGIT / "." / "_" / "-" ) ; a platform device's sysfs name
//
// The anchor is what udev's path_id names a USB controller by: the
// nearest PCI device, then the nearest platform device above that run, and
// so on outward, each run of one kind named once by its innermost member.
// A Raspberry Pi 5's first controller, port 1, interface 0, is
// platform-xhci-hcd.0/usb2-1:1.0; a Pi 4's VL805, behind its internal hub,
// port 3, is platform-fd500000.pcie/pci-0000:01:00.0/usb2-1.3:1.0. A
// controller under anything else (ACPI, virtio, a name with a colon) has no
// anchor this grammar names, and a card under it cannot be bound by port.
type Port struct {
	// Segments are the anchor's segments, outermost first.
	Segments []string
	// Gen is the root hub's USB generation: 1, 2 or 3.
	Gen int
	// Chain is the port numbers from the root hub down.
	Chain []int
	// Config and Interface are the configuration and interface number
	// of the card's interface.
	Config, Interface int
}

// Grammar bounds.
const (
	maxAnchorSegments = 4
	maxChain          = 7
	maxPortLen        = 512
)

var (
	pciSegment      = regexp.MustCompile(`^pci-[0-9a-f]{4,8}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	platformSegment = regexp.MustCompile(`^platform-[A-Za-z0-9._-]{1,64}$`)
	usbSegment      = regexp.MustCompile(`^usb([123])-([0-9.]+):([0-9]+)\.([0-9]+)$`)
)

// errPort is every refusal of a port's form. It does not quote the value.
var errPort = errors.New("not an anchored port path (controller segments, then usbG-CHAIN:CONFIG.INTERFACE, as bridge output dac bind writes it)")

// ParsePort reads a port's string form, refusing anything the grammar on
// Port does not produce.
func ParsePort(s string) (Port, error) {
	if len(s) == 0 || len(s) > maxPortLen {
		return Port{}, errPort
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > maxAnchorSegments+1 {
		return Port{}, errPort
	}
	var p Port
	for _, seg := range parts[:len(parts)-1] {
		if !pciSegment.MatchString(seg) && !platformSegment.MatchString(seg) {
			return Port{}, errPort
		}
		p.Segments = append(p.Segments, seg)
	}
	m := usbSegment.FindStringSubmatch(parts[len(parts)-1])
	if m == nil {
		return Port{}, errPort
	}
	p.Gen = int(m[1][0] - '0')
	links := strings.Split(m[2], ".")
	if len(links) > maxChain {
		return Port{}, errPort
	}
	for _, l := range links {
		n, ok := portNumber(l, 1)
		if !ok {
			return Port{}, errPort
		}
		p.Chain = append(p.Chain, n)
	}
	var ok bool
	if p.Config, ok = portNumber(m[3], 1); !ok {
		return Port{}, errPort
	}
	if p.Interface, ok = portNumber(m[4], 0); !ok {
		return Port{}, errPort
	}
	return p, nil
}

// portNumber reads one of the grammar's numbers: decimal, no leading zero,
// from min to 255.
func portNumber(s string, min int) (int, bool) {
	if s == "" || len(s) > 3 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > 255 {
		return 0, false
	}
	return n, true
}

// Anchor is the port's controller anchor, its segments joined by "/".
func (p Port) Anchor() string { return strings.Join(p.Segments, "/") }

// ChainString is the port chain as sysfs writes it after the bus number:
// "1.3".
func (p Port) ChainString() string {
	links := make([]string, len(p.Chain))
	for i, n := range p.Chain {
		links[i] = strconv.Itoa(n)
	}
	return strings.Join(links, ".")
}

// String is the port's canonical form, which ParsePort reads back.
func (p Port) String() string {
	return fmt.Sprintf("%s/usb%d-%s:%d.%d", p.Anchor(), p.Gen, p.ChainString(), p.Config, p.Interface)
}
