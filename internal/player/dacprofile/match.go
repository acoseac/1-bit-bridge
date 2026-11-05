package dacprofile

import (
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Unit is a unit's model identity and firmware, as the sysfs reader returns
// them: what Match is asked of.
type Unit struct {
	Key Key
	// BCDDevice is the unit's descriptor firmware code, four hexadecimal
	// digits as usbid.ParseHex16 reads them, or "" where it was not read,
	// which matches no alternative that lists firmware.
	BCDDevice string
}

// Match is the profile that knows u, a copy that shares nothing with the
// database, and whether exactly one profile does: a unit no profile matches,
// or two do, is unknown (the design's section 2.2; two is a lint failure,
// and Match does not assume the lint ran). An alternative matches on every
// field it sets, compared exactly, the product string byte for byte; an
// alternative on a shared reference ID matches only with its product string.
// No name is ever compared.
func (db *Database) Match(u Unit) (Profile, bool) {
	bcd, err := usbid.ParseHex16(u.BCDDevice)
	hasBCD := err == nil
	shared := map[[2]uint16]bool{}
	for _, s := range db.SharedReferenceIDs {
		v, e1 := usbid.ParseHex16(s.VendorID)
		p, e2 := usbid.ParseHex16(s.ProductID)
		if e1 == nil && e2 == nil {
			shared[[2]uint16{v, p}] = true
		}
	}
	var found *Profile
	for i := range db.Profiles {
		p := &db.Profiles[i]
		if !slices.ContainsFunc(p.Match, func(m Match) bool { return m.matches(u, bcd, hasBCD, shared) }) {
			continue
		}
		if found != nil {
			return Profile{}, false
		}
		found = p
	}
	if found == nil {
		return Profile{}, false
	}
	return found.clone(), true
}

// matches reports whether u matches the alternative m.
func (m Match) matches(u Unit, bcd uint16, hasBCD bool, shared map[[2]uint16]bool) bool {
	v, e1 := usbid.ParseHex16(m.VendorID)
	p, e2 := usbid.ParseHex16(m.ProductID)
	if e1 != nil || e2 != nil || v != u.Key.VendorID() || p != u.Key.ProductID() {
		return false
	}
	if m.Interface != nil {
		if iface, ok := u.Key.Interface(); !ok || iface != *m.Interface {
			return false
		}
	}
	if m.Product != nil {
		if product, ok := u.Key.Product(); !ok || product != *m.Product {
			return false
		}
	} else if shared[[2]uint16{v, p}] {
		return false
	}
	if m.BCDDevice != nil {
		return hasBCD && slices.ContainsFunc(m.BCDDevice, func(b string) bool {
			x, err := usbid.ParseHex16(b)
			return err == nil && x == bcd
		})
	}
	return true
}

// clone is a copy of p that shares no slice or pointer with it.
func (p Profile) clone() Profile {
	c := p
	c.Match = slices.Clone(p.Match)
	for i, m := range c.Match {
		if m.Interface != nil {
			v := *m.Interface
			c.Match[i].Interface = &v
		}
		if m.Product != nil {
			v := *m.Product
			c.Match[i].Product = &v
		}
		c.Match[i].BCDDevice = slices.Clone(m.BCDDevice)
	}
	if p.Native != nil {
		n := *p.Native
		n.Formats = slices.Clone(n.Formats)
		c.Native = &n
	}
	if p.DoP != nil {
		d := *p.DoP
		d.RefusedBitRates = slices.Clone(d.RefusedBitRates)
		d.AnalogVolumeControls = slices.Clone(d.AnalogVolumeControls)
		d.UnityAtRawMax = slices.Clone(d.UnityAtRawMax)
		for i := range d.UnityAtRawMax {
			d.UnityAtRawMax[i].BCDDevice = slices.Clone(d.UnityAtRawMax[i].BCDDevice)
		}
		c.DoP = &d
	}
	if p.Timing != nil {
		t := *p.Timing
		c.Timing = &t
	}
	c.Caveats = slices.Clone(p.Caveats)
	c.Provenance = slices.Clone(p.Provenance)
	return c
}
