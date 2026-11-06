package dsdstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// unitOf is a unit as the box reads its card: a serial, a port, a card
// number and a device number each of its own, none of which the state may
// hold.
type unitOf struct {
	vendor, product uint16
	name, serial    string
	port            string
	card, devnum    int
}

func (u unitOf) bound(t *testing.T) Bound {
	t.Helper()
	card := alsa.Identity{Card: u.card, Kind: alsa.IdentityUSB, Bus: "usb", USB: &alsa.USBIdentity{
		ID: alsa.USBID{Vendor: u.vendor, Product: u.product}, Serial: usbid.New(u.serial), Port: u.port, Interface: 1, DevNum: u.devnum}}
	q := alsa.QualificationIdentity{Manufacturer: "iFi", Product: u.name, BCDDevice: "0123", Port: u.port, DevNum: u.devnum, Interface: 1}
	b, err := BoundOf(card, q, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNoDSDStateHoldsASerialOrAPort reads the state file's bytes after a
// migration that wrote for two DACs, each bound from the box's readings of
// its card (BoundOf) with a serial, a port, a card number and a device
// number of its own, values found nowhere else, and the owner's record made
// at a port of its own: none of them is in the file, and no key of it names
// a serial, a port, a bus, a card or a device number. A DAC is its model's
// key alone, the vendor and product IDs, the interface and the product
// string.
func TestNoDSDStateHoldsASerialOrAPort(t *testing.T) {
	durable(t)
	goBarUnit := unitOf{vendor: 0x20b1, product: 0x3009, name: "GO bar", serial: "QZ7X-SERIAL-4471-WQ", port: "platform-xhci-hcd.7.auto-usb2-4.6", card: 4093, devnum: 6151}
	otherUnit := unitOf{vendor: 0x152a, product: 0x85de, name: "Brooklyn DAC+", serial: "MYTK-SERIAL-9128-KP", port: "pci-0000:03:00.0-usb3-2.5", card: 4094, devnum: 6152}
	recordPort := "platform-1f00300000.usb-usb2-9.9.9"
	recordDir, _ := recordFixture(t, recordSpec{identity: qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: "iFi", Product: "GO bar", BCDDevice: "0123", Port: recordPort, Interface: 1}})
	data := t.TempDir()
	s := openAt(t, data, options{})
	l := Legacy{DSDMode: ModeNative, DSDQualification: "approved.json"}
	for _, u := range []unitOf{otherUnit, goBarUnit} {
		b := u.bound(t)
		if _, err := s.Migrate(recordDir, l, &b); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(data, FileName))
	if err != nil {
		t.Fatal(err)
	}
	st := fileState(t, data)
	if len(st.DACs) != 2 || len(st.Warnings) != 1 || st.MigratedFrom.RecordSHA256 == "" {
		t.Fatalf("the migration wrote %s: want both DACs, the other one's warning and the record taken", raw)
	}
	for _, u := range []unitOf{goBarUnit, otherUnit} {
		for what, v := range map[string]string{"serial": u.serial, "serial's label": usbid.New(u.serial).Label(), "port": u.port,
			"card number": strconv.Itoa(u.card), "device number": strconv.Itoa(u.devnum)} {
			if strings.Contains(string(raw), v) {
				t.Errorf("the state file holds a unit's %s (%s):\n%s", what, v, raw)
			}
		}
	}
	if strings.Contains(string(raw), recordPort) {
		t.Errorf("the state file holds the record's port:\n%s", raw)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"serial", "port", "bus", "busnum", "card", "devnum", "devNum", "device", "subdevice"}
	walkKeys(doc, func(path, key string, v any) {
		if slices.Contains(forbidden, key) {
			t.Errorf("the state file names %s at %s", key, path)
		}
		if key == "key" || key == "dac" {
			obj, _ := v.(map[string]any)
			var fields []string
			for k := range obj {
				fields = append(fields, k)
			}
			slices.Sort(fields)
			if !slices.Equal(fields, []string{"interface", "product", "productId", "vendorId"}) {
				t.Errorf("a DAC at %s is named by %v, want its model's key alone", path, fields)
			}
		}
	})
}

// walkKeys calls fn with every key of every object in v, decoded JSON.
func walkKeys(v any, fn func(path, key string, v any)) {
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				fn(path, k, e)
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		}
	}
	walk("", v)
}
