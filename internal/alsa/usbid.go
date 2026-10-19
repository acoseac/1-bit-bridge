package alsa

import (
	"fmt"
	"strconv"
	"strings"
)

// USBID is a USB device's vendor and product id, as a USB card's
// /proc/asound/cardN/usbid gives them.
type USBID struct {
	Vendor  uint16
	Product uint16
}

// String renders the id the way the kernel prints it and lsusb shows it:
// "20b1:3009".
func (u USBID) String() string { return fmt.Sprintf("%04x:%04x", u.Vendor, u.Product) }

// MarshalText makes the id a JSON string in its printed form.
func (u USBID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText reads the printed form back, so a reading's JSON decodes
// into the type it was encoded from.
func (u *USBID) UnmarshalText(b []byte) error {
	id, err := ParseUSBID(b)
	if err != nil {
		return err
	}
	*u = id
	return nil
}

// ParseUSBID parses cardN/usbid, which the kernel prints as "%04x:%04x".
// Anything else is an error: the id names the hardware, and a part of one
// is not an id.
func ParseUSBID(b []byte) (USBID, error) {
	s := strings.TrimSpace(string(b))
	v, p, ok := strings.Cut(s, ":")
	if !ok || len(v) != 4 || len(p) != 4 {
		return USBID{}, fmt.Errorf("usbid %s is not vvvv:pppp", quote(s))
	}
	vendor, err := strconv.ParseUint(v, 16, 16)
	if err != nil {
		return USBID{}, fmt.Errorf("usbid vendor %s is not hexadecimal", quote(v))
	}
	product, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return USBID{}, fmt.Errorf("usbid product %s is not hexadecimal", quote(p))
	}
	return USBID{Vendor: uint16(vendor), Product: uint16(product)}, nil
}
