package alsa

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// UnitModel is what the bridge player's effective DSD policy reads of a
// card's USB unit (B728 B7): the unit as Card reads it, its USB strings and
// firmware, which key its model and its firmware, and what the DoP gain
// check parses, its descriptors and the active configuration. It holds no
// serial and needs no anchored port: the policy keys a DAC by its model.
type UnitModel struct {
	// USB is the card's unit as Card reads it: its IDs, the interface the
	// card is, its port and its enumeration (DevNum), which the policy
	// compares with the binding's resolution and never stores.
	USB USBIdentity
	// Manufacturer and Product are the unit's USB strings, "" where the unit
	// has none.
	Manufacturer, Product string
	// BCDDevice is the unit's descriptor firmware code, four hexadecimal
	// digits as sysfs gives them, "" where the unit has none.
	BCDDevice string
	// Descriptors are the unit's USB descriptors as sysfs gives them (its
	// device descriptor, then each configuration's descriptors), nil when
	// they did not read: the DoP gain check then refuses, failing closed.
	Descriptors []byte
	// Config is the unit's active configuration, its bConfigurationValue,
	// which the card's interface's sysfs name carries
	// (<bus>-<port>:<config>.<interface>); 0 when that name does not say.
	Config int
}

// maxDescriptorBytes bounds the descriptors attribute: the device
// descriptor and every configuration's, each of which USB bounds at 65,535
// bytes. A unit's real descriptors are a few kilobytes; one longer than this
// is not read, and the gain check refuses.
const maxDescriptorBytes = 256 << 10

// UnitModel reads card's unit for the effective DSD policy, from one opening
// of the tree, every read confined to it and bounded: the card's identity as
// Card reads it, which must be a readable USB unit; its manufacturer,
// product and bcdDevice attributes, each "" where the unit has none, and an
// error where one is there and does not read, or holds a control character,
// or, for the firmware, is no four hexadecimal digits; its descriptors, nil
// where they do not read; and the configuration its interface's name
// carries. Unlike Qualification it needs no anchored port, which the policy
// never keys a DAC by.
func (r SysfsReader) UnitModel(card int) (UnitModel, error) {
	t, err := r.open()
	if err != nil {
		return UnitModel{}, err
	}
	defer t.close()
	id := t.card(card)
	if id.Kind != IdentityUSB || id.USB == nil {
		return UnitModel{}, errors.New("the card is not a USB unit whose identity reads")
	}
	iface, err := t.resolve("class/sound/card" + strconv.Itoa(card) + "/device")
	if err != nil {
		return UnitModel{}, err
	}
	m := UnitModel{USB: *id.USB}
	unit := path.Dir(iface)
	for _, f := range []struct {
		name string
		dst  *string
	}{{"manufacturer", &m.Manufacturer}, {"product", &m.Product}, {"bcdDevice", &m.BCDDevice}} {
		b, err := t.attr(unit, f.name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return UnitModel{}, fileError{f.name, err}
		}
		v := strings.TrimSpace(string(b))
		if strings.ContainsAny(v, "\x00\r\n") {
			return UnitModel{}, fmt.Errorf("%s holds a control character", f.name)
		}
		*f.dst = v
	}
	if m.BCDDevice != "" {
		if _, err := usbid.ParseHex16(m.BCDDevice); err != nil {
			return UnitModel{}, errors.New("bcdDevice is not four hexadecimal digits")
		}
	}
	if b, err := t.attrCapped(unit, "descriptors", maxDescriptorBytes); err == nil && len(b) > 0 {
		m.Descriptors = b
	}
	if sm := usbInterfaceName.FindStringSubmatch(path.Base(iface)); sm != nil {
		if c, err := strconv.Atoi(sm[3]); err == nil && c > 0 && c <= 255 {
			m.Config = c
		}
	}
	return m, nil
}

// cardFile reads one file of card's directory in the ALSA tree, confined to
// the tree and bounded at maxFileBytes, as a reading reads a card's files: a
// file that is not there answers fs.ErrNotExist.
func (r Reader) cardFile(card int, name string) ([]byte, error) {
	procRoot, asoundRoot := r.roots()
	for _, root := range []string{procRoot, asoundRoot} {
		if err := checkHostRead(root, DefaultProcRoot); err != nil {
			return nil, err
		}
	}
	t, err := openTree(asoundRoot, maxLinks, maxHops)
	if err != nil {
		return nil, err
	}
	defer t.close()
	rel := "card" + strconv.Itoa(card) + "/" + name
	var before func()
	if r.beforeOpen != nil {
		before = func() { r.beforeOpen("asound/" + rel) }
	}
	b, err := t.readFile(rel, maxFileBytes, before)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("read %s: %w", filepath.ToSlash(filepath.Join("asound", rel)), err)
	}
	return b, nil
}

// USBMixer is card's usbmixer file, the kernel's table of the controls its
// USB audio driver made of each unit (/proc/asound/card<N>/usbmixer), as it
// is: the DoP gain check maps a feature unit to its controls by it
// (policy.ParseUSBMixer). A card the USB audio driver does not drive has
// none, which answers fs.ErrNotExist.
func (r Reader) USBMixer(card int) ([]byte, error) { return r.cardFile(card, "usbmixer") }

// PlaybackAltsets are the playback altsets of card's PCM device device, from
// its stream file (/proc/asound/card<N>/stream<D>), read whole: a stream file
// that does not parse without a problem, or holds no playback direction, is
// an error, never a shorter list, since a prefix of a stream is not the
// device's capabilities.
func (r Reader) PlaybackAltsets(card, device int) ([]Altset, error) {
	b, err := r.cardFile(card, "stream"+strconv.Itoa(device))
	if err != nil {
		return nil, err
	}
	st, probs := ParseStream(b)
	switch {
	case len(probs) != 0:
		return nil, fmt.Errorf("the stream file of card %d, device %d does not parse whole: %s", card, device, strings.Join(probs, "; "))
	case st.Playback == nil || len(st.Playback.Altsets) == 0:
		return nil, fmt.Errorf("the stream file of card %d, device %d lists no playback altset", card, device)
	}
	return st.Playback.Altsets, nil
}
