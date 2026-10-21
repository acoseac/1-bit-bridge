package usbid

import (
	"strings"
	"testing"
)

// TestParseHex16TakesTheThreeSpellings: four hexadecimal digits in either
// case, or after a lowercase 0x; the canonical form is four lowercase
// digits. Anything else is refused, without quoting it.
func TestParseHex16TakesTheThreeSpellings(t *testing.T) {
	for in, want := range map[string]uint16{
		"0A12": 0x0a12, "0a12": 0x0a12, "0x0a12": 0x0a12, "0x0A12": 0x0a12,
		"20b1": 0x20b1, "FFFF": 0xffff, "0000": 0, "1234": 0x1234,
	} {
		got, err := ParseHex16(in)
		if err != nil || got != want {
			t.Errorf("ParseHex16(%q) = %#04x, %v; want %#04x", in, got, err, want)
		}
		if Hex16(got) != strings.ToLower(strings.TrimPrefix(in, "0x")) {
			t.Errorf("Hex16(%#04x) = %q", got, Hex16(got))
		}
	}
	for _, in := range []string{"", "a12", "00a12", "0X0a12", "0x", "0xa12", "0x00a12", " 0a12", "0a12 ", "+0a1", "-0a1", "0g12", "x0a12", "0b0101", "١٢٣٤", "0a\x0012"} {
		if v, err := ParseHex16(in); err == nil {
			t.Errorf("ParseHex16(%q) = %#04x, want a refusal", in, v)
		} else if len(in) >= 4 && strings.Contains(err.Error(), in) {
			t.Errorf("the refusal of %q quotes it: %v", in, err)
		}
	}
}

// TestPortRoundTripsItsCanonicalForm: each topology the grammar names
// parses into its parts and prints back byte for byte.
func TestPortRoundTripsItsCanonicalForm(t *testing.T) {
	for _, tc := range []struct {
		in        string
		anchor    string
		gen       int
		chain     []int
		config    int
		iface     int
		chainText string
	}{
		{"platform-xhci-hcd.0/usb2-1:1.0", "platform-xhci-hcd.0", 2, []int{1}, 1, 0, "1"},
		{"platform-fd500000.pcie/pci-0000:01:00.0/usb2-1.3:1.0", "platform-fd500000.pcie/pci-0000:01:00.0", 2, []int{1, 3}, 1, 0, "1.3"},
		{"pci-0000:00:14.0/usb2-2.1.4:1.2", "pci-0000:00:14.0", 2, []int{2, 1, 4}, 1, 2, "2.1.4"},
		{"pci-10000:e1:00.3/usb3-10:2.255", "pci-10000:e1:00.3", 3, []int{10}, 2, 255, "10"},
		{"platform-ci_hdrc.0/usb1-1.2.3.4.5.6.7:255.0", "platform-ci_hdrc.0", 1, []int{1, 2, 3, 4, 5, 6, 7}, 255, 0, "1.2.3.4.5.6.7"},
		{"platform-a/pci-0000:00:00.0/platform-b.usb/pci-0000:01:00.0/usb2-255:1.3", "platform-a/pci-0000:00:00.0/platform-b.usb/pci-0000:01:00.0", 2, []int{255}, 1, 3, "255"},
	} {
		p, err := ParsePort(tc.in)
		if err != nil {
			t.Errorf("ParsePort(%q): %v", tc.in, err)
			continue
		}
		if p.String() != tc.in || p.Anchor() != tc.anchor || p.Gen != tc.gen || p.ChainString() != tc.chainText ||
			p.Config != tc.config || p.Interface != tc.iface || len(p.Chain) != len(tc.chain) {
			t.Errorf("ParsePort(%q) = %+v (%q, anchor %q)", tc.in, p, p.String(), p.Anchor())
		}
		for i := range tc.chain {
			if i < len(p.Chain) && p.Chain[i] != tc.chain[i] {
				t.Errorf("%q: chain %v, want %v", tc.in, p.Chain, tc.chain)
			}
		}
	}
}

// TestParsePortRefusesWhatTheGrammarDoesNot: a bus number, a device
// number or an unanchored bus-port suffix never parses, nor anything the
// grammar's ranges, digits or characters leave out.
func TestParsePortRefusesWhatTheGrammarDoesNot(t *testing.T) {
	for _, in := range []string{
		"",
		"usb2-1:1.0",                                // no anchor
		"1-1.3:1.0",                                 // a bus-relative sysfs name
		"1-1.3",                                     // a bus number and a chain
		"pci-0000:01:00.0",                          // no usb segment
		"pci-0000:01:00.0/usb2-1.3",                 // no interface
		"pci-0000:01:00.0/usb2-1.3:1",               // no interface
		"pci-0000:01:00.0/usb4-1:1.0",               // no generation 4
		"pci-0000:01:00.0/usb-1:1.0",                // no generation
		"pci-0000:01:00.0/usb2-0:1.0",               // port 0
		"pci-0000:01:00.0/usb2-01:1.0",              // a leading zero
		"pci-0000:01:00.0/usb2-256:1.0",             // past 255
		"pci-0000:01:00.0/usb2-1:0.0",               // configuration 0
		"pci-0000:01:00.0/usb2-1:1.256",             // interface past 255
		"pci-0000:01:00.0/usb2-1:1.01",              // a leading zero
		"pci-0000:01:00.0/usb2-1..2:1.0",            // an empty port
		"pci-0000:01:00.0/usb2-1.2.3.4.5.6.7.8:1.0", // past seven ports
		"pci-0000:01:00.8/usb2-1:1.0",               // function 8
		"pci-0000:1:00.0/usb2-1:1.0",                // a one-digit bus
		"pci-0000:01:00.0:x/usb2-1:1.0",             // trailing junk
		"pci-0000:01:00.0/usb2-1:1.0/",              // trailing slash
		"/pci-0000:01:00.0/usb2-1:1.0",              // leading slash
		"PCI-0000:01:00.0/usb2-1:1.0",               // case
		"pci-0000:01:0A.0/usb2-1:1.0",               // upper-case hex
		"platform-/usb2-1:1.0",                      // an empty name
		"platform-soc:usb/usb2-1:1.0",               // a colon in a name
		"platform-a b/usb2-1:1.0",                   // a space in a name
		"platform-" + strings.Repeat("a", 65) + "/usb2-1:1.0",
		"devnum-5/usb2-1:1.0",
		"platform-a/platform-b/platform-c/platform-d/platform-e/usb2-1:1.0", // five anchors
		"platform-a//usb2-1:1.0",
		"platform-a/usb2-1:1.0\n",
		"platform-a/usb2-1:1.0 ",
		"platform-a/usb2-１:1.0",
	} {
		if p, err := ParsePort(in); err == nil {
			t.Errorf("ParsePort(%q) = %+v, want a refusal", in, p)
		}
	}
}
