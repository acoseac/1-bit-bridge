package dacprofile

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

// NewKey is the key of a model: iface and product are nil where the reading
// has none.
func NewKey(vendorID, productID uint16, iface *int, product *string) (Key, error) {
	k := Key{vendorID: vendorID, productID: productID}
	if iface != nil {
		k.iface, k.hasIface = *iface, true
	}
	if product != nil {
		k.product, k.hasProduct = *product, true
	}
	return k, nil
}

// VendorID and ProductID are the model's USB IDs.
func (k Key) VendorID() uint16  { return k.vendorID }
func (k Key) ProductID() uint16 { return k.productID }

// Interface is the USB interface the card is, and whether the key has one.
func (k Key) Interface() (int, bool) { return k.iface, k.hasIface }

// Product is the model's product string, and whether the key has one.
func (k Key) Product() (string, bool) { return k.product, k.hasProduct }

// Canonical is the key's canonical form, five lines of UTF-8 joined by LF
// with no LF after the last.
func (k Key) Canonical() string {
	return ""
}

// DACKey is the key as the wire names it: "d1-" and the first eight bytes of
// the SHA-256 of the canonical form, in lowercase hexadecimal.
func (k Key) DACKey() string {
	return ""
}

// Equal reports whether k and o are one model's key.
func (k Key) Equal(o Key) bool {
	return false
}
