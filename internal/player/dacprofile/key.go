package dacprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Key is a DAC model's identity (the design's section 2.2): the vendor and
// product IDs, the USB interface the card is, and the product string, either
// of the last two absent. bcdDevice is not part of it: it narrows a match and
// is recorded with evidence, and a choice outlives a firmware update. A Key
// holds no serial, port, card number or device number, and is compared by
// its canonical form, never field by field.
type Key struct {
	vendorID, productID uint16
	iface               int
	hasIface            bool
	product             string
	hasProduct          bool
}

// keyForm is the canonical form's first line, its version: the form changes
// only with a new one, and a new dacKey prefix with it.
const keyForm = "dacKey/1"

// dacKeyPrefix is a dacKey's prefix, the form's version.
const dacKeyPrefix = "d1-"

// NewKey is the key of a model: iface and product are nil where the reading
// has none. It refuses an interface that is not a USB interface number (0 to
// 255) and a product string the sysfs reader never returns: empty, with
// white space around it, holding a NUL, a CR or an LF (alsa.Qualification
// trims the first and refuses the rest), or not UTF-8. So no two spellings of
// one reading make two keys, and no field runs into the next line of the
// canonical form. Neither refusal quotes the string.
func NewKey(vendorID, productID uint16, iface *int, product *string) (Key, error) {
	k := Key{vendorID: vendorID, productID: productID}
	if iface != nil {
		if *iface < 0 || *iface > 255 {
			return Key{}, errors.New("dacprofile: a USB interface number is 0 to 255")
		}
		k.iface, k.hasIface = *iface, true
	}
	if product != nil {
		if err := checkProduct(*product); err != nil {
			return Key{}, err
		}
		k.product, k.hasProduct = *product, true
	}
	return k, nil
}

// checkProduct refuses a product string the sysfs reader never returns.
func checkProduct(p string) error {
	switch {
	case p == "":
		return errors.New("dacprofile: a product string is never empty")
	case strings.TrimSpace(p) != p:
		return errors.New("dacprofile: a product string has no white space around it")
	case strings.ContainsAny(p, "\x00\r\n"):
		return errors.New("dacprofile: a product string holds no NUL, CR or LF")
	case !utf8.ValidString(p):
		return errors.New("dacprofile: a product string is UTF-8")
	}
	return nil
}

// VendorID and ProductID are the model's USB IDs.
func (k Key) VendorID() uint16  { return k.vendorID }
func (k Key) ProductID() uint16 { return k.productID }

// Interface is the USB interface the card is, and whether the key has one.
func (k Key) Interface() (int, bool) { return k.iface, k.hasIface }

// Product is the model's product string, and whether the key has one.
func (k Key) Product() (string, bool) { return k.product, k.hasProduct }

// Canonical is the key's canonical form, five lines of UTF-8 joined by LF
// with no LF after the last: "dacKey/1"; the vendor ID and the product ID,
// each four lowercase hexadecimal digits (usbid.Hex16); the interface, "="
// and its decimal number (no sign, no leading zero, "=0" for zero) or a lone
// "-" when the key has none; the product string, "=" and the string as it is
// or a lone "-" when the key has none. The "=" keeps a present value apart
// from absence, so a product named "-" is "=-"; nothing is folded or
// normalized.
func (k Key) Canonical() string {
	var b strings.Builder
	b.WriteString(keyForm)
	b.WriteByte('\n')
	b.WriteString(usbid.Hex16(k.vendorID))
	b.WriteByte('\n')
	b.WriteString(usbid.Hex16(k.productID))
	b.WriteByte('\n')
	if k.hasIface {
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(k.iface))
	} else {
		b.WriteByte('-')
	}
	b.WriteByte('\n')
	if k.hasProduct {
		b.WriteByte('=')
		b.WriteString(k.product)
	} else {
		b.WriteByte('-')
	}
	return b.String()
}

// DACKey is the key as the wire names it: "d1-" and the first eight bytes of
// the SHA-256 of the canonical form, in lowercase hexadecimal.
func (k Key) DACKey() string {
	sum := sha256.Sum256([]byte(k.Canonical()))
	return dacKeyPrefix + hex.EncodeToString(sum[:8])
}

// Equal reports whether k and o are one model's key, by their canonical
// forms.
func (k Key) Equal(o Key) bool {
	return k.Canonical() == o.Canonical()
}
