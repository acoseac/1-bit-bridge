package binding

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// The transient cards these tests put beside the GO bar: card 7 in
// /proc/asound alone, with a usbid of model, or with none (a driver that
// writes no usbid), and card 7 in sysfs alone, a unit of model with serial.
func procOnly(model string) string {
	usb := ""
	if model != "" {
		usb = " usb=" + model
	} else {
		usb = " driver=snd-usb-hiface"
	}
	return "proccard 7 New \"Arriving DAC\"" + usb + " pcm=0.0\n"
}

func sysfsOnly(model, serial string) string {
	vid, pid, _ := strings.Cut(model, ":")
	s := ""
	if serial != "" {
		s = " serial=" + serial
	}
	return "usbdev " + usb1 + "/1-2 vendor=" + vid + " product=" + pid + s + "\nusbcard 7 " + usb1 + "/1-2/1-2:1.0\n"
}

// settled checks Settled's answer and whether it is settled.
func settled(t *testing.T, dev config.PlayerDevice, inv Inventory, state State, card int, isSettled bool) Resolution {
	t.Helper()
	r, ok := Settled(dev, inv)
	want(t, r, state, card)
	if ok != isSettled {
		t.Fatalf("%s binding: settled %v, want %v (%s)", dev.Binding, ok, isSettled, r.Reason)
	}
	return r
}

// TestSettledSetsAsideACardThatCannotBeTheBoundUnit: a transient card that
// its usbid, or its sysfs identity, shows to be another model, or another
// unit of the bound model, cannot be the bound unit, so the GO bar's own
// answer stands and is settled: available beside it, and device_missing at
// once when the GO bar is unplugged. Resolve still says identity_transient
// for the whole box.
func TestSettledSetsAsideACardThatCannotBeTheBoundUnit(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		devs       []config.PlayerDevice
	}{
		{"another model in /proc alone", procOnly("1234:0f05"), []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009")}},
		{"another model in sysfs alone", sysfsOnly("1234:0f05", ""), []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009"), byPort(goBarPort)}},
		{"another unit of the model in sysfs alone", sysfsOnly("20b1:3009", "OTHER-UNIT-SERIAL-9"), []config.PlayerDevice{bySerial(goBarSerial), byPort(goBarPort)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv := observe(t, root)
			for _, dev := range tc.devs {
				want(t, Resolve(dev, inv), StateIdentityTransient, -1)
				settled(t, dev, inv, StateAvailable, 5, true)
			}
			sysfstest.Apply(t, root, "unplug "+goBar+"\n")
			inv = observe(t, root)
			for _, dev := range tc.devs {
				settled(t, dev, inv, StateDeviceMissing, -1, true)
			}
		})
	}
}

// TestSettledWaitsOnACardThatMayBeTheBoundUnit: a transient card of the
// bound model, one whose model nothing said, and, for a port binding with
// the port empty, one whose port nothing said, may be the bound unit (or a
// second unit of it), so the answer is identity_transient, naming that
// card, and unsettled.
func TestSettledWaitsOnACardThatMayBeTheBoundUnit(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		unplug     bool
		devs       []config.PlayerDevice
	}{
		{"the bound model in /proc alone", procOnly("20b1:3009"), false, []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009")}},
		{"no usbid in /proc alone", procOnly(""), false, []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009")}},
		{"the bound model in sysfs alone", sysfsOnly("20b1:3009", ""), false, []config.PlayerDevice{byModel("20b1", "3009")}},
		{"the bound serial in sysfs alone", sysfsOnly("20b1:3009", goBarSerial), false, []config.PlayerDevice{bySerial(goBarSerial)}},
		{"an empty port beside a card in /proc alone", procOnly("1234:0f05"), true, []config.PlayerDevice{byPort(goBarPort)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			if tc.unplug {
				sysfstest.Apply(t, root, "unplug "+goBar+"\n")
			}
			sysfstest.Apply(t, root, tc.step)
			inv := observe(t, root)
			for _, dev := range tc.devs {
				r := settled(t, dev, inv, StateIdentityTransient, -1, false)
				if !strings.Contains(r.Reason, "card 7") {
					t.Errorf("%s binding: the reason does not name card 7: %s", dev.Binding, r.Reason)
				}
			}
		})
	}
}

// TestSettledHoldsWhatNoCardCanChange: an ambiguity stands beside a
// transient card that may be a third unit, a port whose card is there
// stands beside a card whose port nothing said, and a binding the config
// refuses is refused whatever the cards are; each is settled.
func TestSettledHoldsWhatNoCardCanChange(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, plug(usb1+"/1-3", 6, "20b1:3009", "")+procOnly("20b1:3009"))
	inv := observe(t, root)
	settled(t, byModel("20b1", "3009"), inv, StateDeviceAmbiguous, -1, true)
	settled(t, byPort(goBarPort), inv, StateAvailable, 5, true)
	bad := bySerial(goBarSerial)
	bad.Port = goBarPort
	settled(t, bad, inv, StateBindingInvalid, -1, true)
	settled(t, config.PlayerDevice{}, inv, StateNotConfigured, -1, true)
}

// TestObserveKeepsWhatATransientCardsTreesSaid: a transient card carries
// what each tree said of its unit, and every card its sysfs directory; a
// card that is not transient carries no glimpse.
func TestObserveKeepsWhatATransientCardsTreesSaid(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, procOnly("1234:0f05")+
		"usbdev "+usb1+"/1-3 vendor=1234 product=0f06 serial=SYSFS-ONLY-UNIT-0001\nusbcard 8 "+usb1+"/1-3/1-3:1.0\n")
	inv := observe(t, root)
	c7, _ := inv.Card(7)
	if c7.State != CardTransient || c7.ProcUSBID == nil || c7.ProcUSBID.String() != "1234:0f05" || c7.SysfsUSB != nil || c7.Dir != "" {
		t.Errorf("card 7, in /proc alone: %+v", c7)
	}
	c8, _ := inv.Card(8)
	if c8.State != CardTransient || c8.ProcUSBID != nil || c8.SysfsUSB == nil || c8.SysfsUSB.ID.String() != "1234:0f06" ||
		!strings.HasSuffix(c8.Dir, "/1-3/1-3:1.0/sound/card8") {
		t.Errorf("card 8, in sysfs alone: %+v", c8)
	}
	c5, _ := inv.Card(5)
	if c5.State != CardUSB || c5.ProcUSBID != nil || c5.SysfsUSB != nil || !strings.HasSuffix(c5.Dir, "/1-1/1-1:1.0/sound/card5") || c5.USB.DevNum != 2 {
		t.Errorf("card 5, the GO bar: %+v", c5)
	}
	if inv.Partial {
		t.Errorf("a whole reading is partial: %q", inv.Problems)
	}
}
