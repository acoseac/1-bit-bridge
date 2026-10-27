package alsa

import (
	"errors"
	"path"
	"strconv"
)

// QualificationGain reads USB Audio feature-unit presence from descriptors.
// An unreadable or malformed descriptor stream is unknown, never no gain.
func (r SysfsReader) QualificationGain(card int) (bool, error) {
	t, e := r.open()
	if e != nil {
		return false, e
	}
	defer t.close()
	iface, e := t.resolve("class/sound/card" + strconv.Itoa(card) + "/device")
	if e != nil {
		return false, e
	}
	b, e := t.attr(path.Dir(iface), "descriptors")
	if e != nil {
		return false, e
	}
	return descriptorGain(b)
}
func descriptorGain(b []byte) (bool, error) {
	if len(b) == 0 {
		return false, errors.New("USB descriptors unavailable")
	}
	audio := false
	gain := false
	for len(b) > 0 {
		if len(b) < 2 || int(b[0]) < 2 || int(b[0]) > len(b) {
			return false, errors.New("malformed USB descriptors")
		}
		d := b[:int(b[0])]
		if d[1] == 4 {
			if len(d) < 9 {
				return false, errors.New("short USB interface")
			}
			audio = d[5] == 1 && d[6] == 1
			if audio && d[7] != 0 && d[7] != 0x20 {
				return false, errors.New("unsupported USB Audio protocol")
			}
		}
		if audio && d[1] == 0x24 {
			if len(d) < 3 {
				return false, errors.New("short audio descriptor")
			}
			if d[2] == 6 {
				gain = true
			}
		}
		b = b[len(d):]
	}
	return gain, nil
}
