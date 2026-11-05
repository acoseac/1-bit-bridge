package dacprofile

import (
	"strings"
	"testing"
)

// TestTheDACKeyMatchesItsVectors: a key's canonical form is the design's
// five lines (section 2.2), "dacKey/1", the vendor and product IDs as four
// lowercase hexadecimal digits, the interface as "=" and its decimal number
// or "-" when there is none, the product string as "=" and the string as the
// reader returns it or "-" when there is none, joined by LF with no LF after
// the last; and its dacKey is "d1-" and the first eight bytes of the form's
// SHA-256 in lowercase hexadecimal. The vectors were computed apart from this
// package (Python's hashlib over the forms written out, two of them again with
// coreutils' sha256sum): an absent interface and interface 0, an absent
// product and a product of "-", and two spellings of one character under
// Unicode and two cases of a name, which nothing folds, each hash apart.
func TestTheDACKeyMatchesItsVectors(t *testing.T) {
	for _, v := range []struct {
		name                string
		vendorID, productID uint16
		iface               *int
		productString       *string
		form, dacKey        string
	}{
		{"the Hugo 2", 0x245f, 0x0815, ptr(1), ptr("Hugo2"), "dacKey/1\n245f\n0815\n=1\n=Hugo2", "d1-ed3dc7cedf69d4df"},
		{"no interface", 0x245f, 0x0815, nil, ptr("Hugo2"), "dacKey/1\n245f\n0815\n-\n=Hugo2", "d1-a72ceecb6bd8464f"},
		{"no product string", 0x245f, 0x0815, ptr(1), nil, "dacKey/1\n245f\n0815\n=1\n-", "d1-1b888551f23d3ac3"},
		{"a product string of -", 0x245f, 0x0815, ptr(1), ptr("-"), "dacKey/1\n245f\n0815\n=1\n=-", "d1-e74237462b8b0ad2"},
		{"neither", 0x245f, 0x0815, nil, nil, "dacKey/1\n245f\n0815\n-\n-", "d1-e429f510b47ae450"},
		{"interface 0", 0x20b1, 0x0010, ptr(0), ptr("iFi (by AMR) HD USB Audio"), "dacKey/1\n20b1\n0010\n=0\n=iFi (by AMR) HD USB Audio", "d1-1297a8ed562e3d0d"},
		{"interface 255", 0x25ce, 0x0025, ptr(255), ptr("Brooklyn DAC+"), "dacKey/1\n25ce\n0025\n=255\n=Brooklyn DAC+", "d1-c280b84f214fcea5"},
		{"a composed character", 0x1511, 0x0062, ptr(0), ptr("Ü"), "dacKey/1\n1511\n0062\n=0\n=Ü", "d1-6100d891196cb6e3"},
		{"the same character decomposed", 0x1511, 0x0062, ptr(0), ptr("Ü"), "dacKey/1\n1511\n0062\n=0\n=Ü", "d1-b9ec9e6d9df05eec"},
		{"a name in capitals", 0x245f, 0x0815, ptr(1), ptr("HUGO2"), "dacKey/1\n245f\n0815\n=1\n=HUGO2", "d1-875feb9fbbcfe10e"},
	} {
		k, err := NewKey(v.vendorID, v.productID, v.iface, v.productString)
		if err != nil {
			t.Errorf("%s: NewKey: %v", v.name, err)
			continue
		}
		if got := k.Canonical(); got != v.form {
			t.Errorf("%s: canonical form %q, want %q", v.name, got, v.form)
		}
		if got := k.DACKey(); got != v.dacKey {
			t.Errorf("%s: dacKey %s, want %s", v.name, got, v.dacKey)
		}
	}
}

// TestAKeyHoldsOnlyWhatTheReaderReturns: NewKey refuses an interface that is
// not a USB interface number and a product string the sysfs reader never
// returns (empty, with white space around it, holding a NUL, a CR or an LF,
// or not UTF-8), so no two spellings of one reading make two keys and no
// field can run into the next line of the form; and a key's accessors give
// back what it was made of.
func TestAKeyHoldsOnlyWhatTheReaderReturns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		iface   *int
		product *string
	}{
		{"interface -1", ptr(-1), nil},
		{"interface 256", ptr(256), nil},
		{"an empty product string", nil, ptr("")},
		{"a leading space", nil, ptr(" Hugo2")},
		{"a trailing space", nil, ptr("Hugo2 ")},
		{"a trailing tab", nil, ptr("Hugo2\t")},
		{"an LF", nil, ptr("Hu\ngo2")},
		{"a CR", nil, ptr("Hu\rgo2")},
		{"a NUL", nil, ptr("Hu\x00go2")},
		{"bytes that are not UTF-8", nil, ptr("Hugo\xff")},
	} {
		if k, err := NewKey(0x245f, 0x0815, tc.iface, tc.product); err == nil {
			t.Errorf("NewKey takes %s: %q", tc.name, k.Canonical())
		}
	}
	k, err := NewKey(0x245f, 0x0815, ptr(1), ptr("Hugo 2 inside"))
	if err != nil {
		t.Fatal(err)
	}
	iface, hasIface := k.Interface()
	product, hasProduct := k.Product()
	if k.VendorID() != 0x245f || k.ProductID() != 0x0815 || iface != 1 || !hasIface || product != "Hugo 2 inside" || !hasProduct {
		t.Errorf("the key gives back %04x:%04x %d %v %q %v", k.VendorID(), k.ProductID(), iface, hasIface, product, hasProduct)
	}
	if _, has := (Key{}).Interface(); has {
		t.Error("the zero key has an interface")
	}
}

// TestKeysCompareByTheirCanonicalForm: two keys of one reading are Equal;
// keys that differ in any field, or in a field's presence (an absent
// interface and interface 0, an absent product string and one of "-"), are
// not, and their dacKeys differ.
func TestKeysCompareByTheirCanonicalForm(t *testing.T) {
	key := func(iface *int, product *string) Key {
		k, err := NewKey(0x245f, 0x0815, iface, product)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a, b := key(ptr(1), ptr("Hugo2")), key(ptr(1), ptr("Hugo2"))
	if !a.Equal(b) || a.DACKey() != b.DACKey() {
		t.Errorf("two keys of one reading differ: %q, %q", a.Canonical(), b.Canonical())
	}
	for _, tc := range []struct {
		name string
		a, b Key
	}{
		{"an absent interface and interface 0", key(nil, ptr("Hugo2")), key(ptr(0), ptr("Hugo2"))},
		{"an absent product string and one of -", key(ptr(1), nil), key(ptr(1), ptr("-"))},
		{"two interfaces", key(ptr(1), ptr("Hugo2")), key(ptr(2), ptr("Hugo2"))},
		{"two product strings", key(ptr(1), ptr("Hugo2")), key(ptr(1), ptr("Hugo 2"))},
	} {
		if tc.a.Equal(tc.b) || tc.a.DACKey() == tc.b.DACKey() {
			t.Errorf("%s compare equal: %q, %q", tc.name, tc.a.Canonical(), tc.b.Canonical())
		}
	}
	if !strings.HasPrefix(a.DACKey(), "d1-") || len(a.DACKey()) != len("d1-")+16 {
		t.Errorf("a dacKey is %q", a.DACKey())
	}
}
