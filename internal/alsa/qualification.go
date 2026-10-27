package alsa

import (
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
	"path"
	"strconv"
	"strings"
)

// QualificationIdentity is separate evidence, never a PCM binding condition.
type QualificationIdentity struct {
	Manufacturer, Product, BCDDevice, Port string
	DevNum, Interface                      int
}

// Qualification reads additional USB descriptors without weakening Card.
func (r SysfsReader) Qualification(card int) (QualificationIdentity, error) {
	var q QualificationIdentity
	t, e := r.open()
	if e != nil {
		return q, e
	}
	defer t.close()
	id := t.card(card)
	if id.Kind != IdentityUSB || id.USB == nil {
		return q, errors.New("qualification requires readable USB identity")
	}
	iface, e := t.resolve("class/sound/card" + strconv.Itoa(card) + "/device")
	if e != nil {
		return q, e
	}
	unit := path.Dir(iface)
	for _, f := range []struct {
		name string
		dst  *string
	}{{"manufacturer", &q.Manufacturer}, {"product", &q.Product}, {"bcdDevice", &q.BCDDevice}} {
		b, e := t.attr(unit, f.name)
		if e != nil {
			return q, fmt.Errorf("qualification %s unavailable", f.name)
		}
		v := strings.TrimSpace(string(b))
		if v == "" || strings.ContainsAny(v, "\x00\r\n") {
			return q, fmt.Errorf("qualification %s malformed", f.name)
		}
		*f.dst = v
	}
	if _, e := usbid.ParseHex16(q.BCDDevice); e != nil {
		return q, errors.New("qualification bcdDevice malformed")
	}
	q.Port, q.DevNum, q.Interface = id.USB.Port, id.USB.DevNum, id.USB.Interface
	if q.Port == "" || q.DevNum == 0 {
		return q, errors.New("qualification attachment unknown")
	}
	return q, nil
}
