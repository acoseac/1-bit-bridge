package binding

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// What a reading of the Pi 5 tree may fail to place (B510). usbUnlisted is
// a USB listing, bus/usb/devices, that cannot be listed; rootHubsUnread
// makes the two root hubs of the GO bar's controller links that leave the
// tree, which the reader refuses: two root hubs it could not read, never two
// that went. second is card 6 on the GO bar's interface 2, a second card of
// its unit, which another driver made, so /proc/asound has no usbid for it.
const (
	usbUnlisted    = "remove sys/bus/usb/devices\nfile sys/bus/usb/devices not a directory\n"
	rootHubsUnread = "link sys/bus/usb/devices/usb1 !../../../../outside\nlink sys/bus/usb/devices/usb2 !../../../../outside\n"
	second         = "usbcard 6 " + goBar + "/1-1:1.2\nproccard 6 Other6 \"Synthetic DAC, second card\" driver=snd-synthetic pcm=0.0\n"
)

// readTrees reads root's two trees as Observe does, the sysfs reading
// through edit when it is set, and returns the inventory with the readers
// the check after open takes, so that the check reads what Resolve read.
func readTrees(t *testing.T, root string, edit func(alsa.SysfsSnapshot) alsa.SysfsSnapshot) (Inventory, alsa.Reader, SysfsReading) {
	t.Helper()
	proc := alsa.Reader{ProcRoot: filepath.Join(root, "proc")}
	var sys SysfsReading = alsa.SysfsReader{Root: filepath.Join(root, "sys")}
	if edit != nil {
		sys = readingAs{SysfsReader: alsa.SysfsReader{Root: filepath.Join(root, "sys")}, edit: edit}
	}
	snap, err := proc.Read()
	if err != nil {
		t.Fatalf("/proc/asound: %v", err)
	}
	return inventory(snap, sys.Read()), proc, sys
}

// portUnreadable is a sysfs reading in which the port path of each of cards
// could not be read, as Read returns one when a read on that card's way up
// to its controller fails (its root hub's speed, say) and the rest of the
// reading does not: the card's identity read, its Port empty and its
// PortProblem saying why.
func portUnreadable(cards ...int) func(alsa.SysfsSnapshot) alsa.SysfsSnapshot {
	return func(s alsa.SysfsSnapshot) alsa.SysfsSnapshot {
		ids := append([]alsa.Identity{}, s.Cards...)
		for i, id := range ids {
			if slices.Contains(cards, id.Card) && id.USB != nil {
				u := *id.USB
				u.Port, u.PortProblem = "", "usb1/speed: input/output error"
				ids[i].USB = &u
			}
		}
		s.Cards = ids
		return s
	}
}

// says fails t unless s holds every one of in and none of out.
func says(t *testing.T, what, s string, in, out []string) {
	t.Helper()
	for _, x := range in {
		if !strings.Contains(s, x) {
			t.Errorf("%s does not say %q: %s", what, x, s)
		}
	}
	for _, x := range out {
		if strings.Contains(s, x) {
			t.Errorf("%s says %q: %s", what, x, s)
		}
	}
}

// TestAUSBListingThatCannotBeReadWholeIsNoTopologyChange: under a port
// binding, a USB listing (bus/usb/devices) that could not be listed, whose
// root hubs of the bound controller could not be read, or that was cut at
// the reader's cap before that controller, lacks the controller in the
// reading alone. Resolve answers identity_incomplete, saying the reading
// failed and to resolve again, never a topology change to rebind for, and
// bind refuses the port binding with that state. Serial and uniqueModel
// bindings never read the listing for their answer: they resolve, and bind,
// as on a whole reading (B510).
func TestAUSBListingThatCannotBeReadWholeIsNoTopologyChange(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		edit       func(alsa.SysfsSnapshot) alsa.SysfsSnapshot
	}{
		{"a listing that cannot be listed", usbUnlisted, nil},
		{"the bound controller's root hubs unreadable", rootHubsUnread, nil},
		{"a listing cut at the cap before the bound controller", "", cutDevicesAtTheCap(goBarAnchor)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv, _, _ := readTrees(t, root, tc.edit)
			if contains(inv.Controllers, goBarAnchor) || !inv.Partial {
				t.Fatalf("premise: a reading that is not whole and lacks %s: controllers %q, partial %v (problems %q)",
					goBarAnchor, inv.Controllers, inv.Partial, inv.Problems)
			}
			if _, err := For(inv, 5, config.BindingPort, Endpoint{}); err == nil {
				t.Error("bind card 5 by port took a USB listing that could not be read whole")
			} else {
				says(t, "bind by port", err.Error(), []string{string(StateIdentityIncomplete), "could not be read whole"},
					[]string{"topology change", "rebind"})
			}
			for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel} {
				dev, err := For(inv, 5, kind, Endpoint{})
				if err != nil {
					t.Errorf("bind card 5 by %s: %v", kind, err)
					continue
				}
				want(t, Resolve(dev, inv), StateAvailable, 5)
			}
			for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009")} {
				want(t, Resolve(dev, inv), StateAvailable, 5)
			}
			for _, port := range []string{goBarPlace, goBarPort} {
				r := Resolve(byPort(port), inv)
				want(t, r, StateIdentityIncomplete, -1)
				says(t, "by "+port+", the reason", r.Reason, []string{goBarAnchor, "could not be read whole", "resolve again"},
					[]string{"topology change", "rebind"})
			}
		})
	}
}

// TestARootHubWithAnUnknownSpeedIsNoTopologyChange: the bound controller's
// only root hub has a speed that reads but is not a USB speed. Its omission
// from the reading is identity_incomplete for a port binding, while a serial
// binding on that same reading still resolves (B510).
func TestARootHubWithAnUnknownSpeedIsNoTopologyChange(t *testing.T) {
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+usb2+"\nfile "+usb1+"/speed 999\n")
	inv := observe(t, root)
	if contains(inv.Controllers, goBarAnchor) {
		t.Fatalf("premise: the controller with an unknown root-hub speed is listed: %q", inv.Controllers)
	}
	says(t, "the reading's problems", strings.Join(inv.Problems, "; "), []string{"usb1/speed", "is not a USB speed"}, nil)
	want(t, Resolve(bySerial(goBarSerial), inv), StateAvailable, 5)
	r := Resolve(byPort(goBarPlace), inv)
	want(t, r, StateIdentityIncomplete, -1)
	says(t, "the reason", r.Reason, []string{goBarAnchor, "could not be read whole", "resolve again"},
		[]string{"topology change", "rebind"})
}

// TestAControllerGoneFromAWholeUSBListingIsATopologyChange: a USB listing
// read whole that does not name the bound controller is a controller that
// is not on the box: binding_invalid, a topology change to rebind for, as
// before B510. So is a box whose sysfs has no USB listing at all (no
// bus/usb/devices: a box without USB, which the reader does not call
// partial), and a listing whose root hubs of the bound controller went as
// they were read (links to nothing), which the reader takes for the box as
// it is.
func TestAControllerGoneFromAWholeUSBListingIsATopologyChange(t *testing.T) {
	for _, tc := range []struct{ name, step, port string }{
		{"a controller the box does not have", "", "platform-xhci-hcd.7/usb2-1"},
		{"no USB listing at all", "remove sys/bus/usb/devices\n", goBarPlace},
		{"the bound controller's root hubs gone as they were read", "link sys/bus/usb/devices/usb1 !gone\nlink sys/bus/usb/devices/usb2 !gone\n", goBarPlace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv := observe(t, root)
			if inv.Partial {
				t.Fatalf("premise: the reading is partial: %q", inv.Problems)
			}
			r := Resolve(byPort(tc.port), inv)
			want(t, r, StateBindingInvalid, -1)
			says(t, "the reason", r.Reason, []string{"is not on the box", "topology change", "rebind"}, []string{"could not be read whole"})
			for _, dev := range []config.PlayerDevice{bySerial(goBarSerial), byModel("20b1", "3009")} {
				want(t, Resolve(dev, inv), StateAvailable, 5)
			}
		})
	}
}

// TestACardOfTheBoundUnitWhosePortPathCannotBeReadIsNotElsewhere: a USB
// card whose port path cannot be read sits at no port the reading can name,
// which is not another port, and one port holds one unit: a card of the
// unit in the bound port whose port path failed to read is still there. So
// a port binding's one card at its port, beside a card whose port path
// cannot be read and whose model and serial do not tell it from that card's
// unit, is device_ambiguous, as it is beside a card whose identity cannot be
// read, and bind refuses the port alone there; the check after open refuses
// the card the binding chose before. The card on the interface the binding
// records is chosen, and admitted, whatever else the unit carries. A card
// whose model, or whose serial, differs from that card's cannot be another
// card of its unit, and changes nothing; one with no serial to compare may
// be (B510).
func TestACardOfTheBoundUnitWhosePortPathCannotBeReadIsNotElsewhere(t *testing.T) {
	base := observe(t, pi5(t))
	node := alsapcm.NodeInfo{Card: 5, Playback: true}
	place, recorded, other := byPort(goBarPlace), byPort(goBarPort), byPort(goBarPlace+":1.2")
	made := map[string]Resolution{}
	for _, dev := range []config.PlayerDevice{place, recorded, other} {
		made[dev.Port] = Resolve(dev, base)
		want(t, made[dev.Port], StateAvailable, 5)
	}
	for _, tc := range []struct {
		name, step string
		unplaced   int
		// ambiguous says the card may be another card of the GO bar's unit.
		ambiguous bool
	}{
		{"a second card of the unit", second, 6, true},
		{"another unit of the model with no serial", plug(usb1+"/1-2", 7, "20b1:3009", ""), 7, true},
		{"another model", plug(usb1+"/1-2", 7, "1234:0f02", "OTHER-UNIT-SERIAL-9"), 7, false},
		{"another unit of the model with its own serial", plug(usb1+"/1-2", 7, "20b1:3009", "OTHER-UNIT-SERIAL-9"), 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv, proc, sys := readTrees(t, root, portUnreadable(tc.unplaced))
			if c, ok := inv.Card(tc.unplaced); !ok || c.State != CardUSB || c.USB.Port != "" {
				t.Fatalf("premise: card %d is not a USB card whose port path could not be read: %+v, %v", tc.unplaced, c, ok)
			}
			// The check after open, on the resolution each binding made before.
			check := func(r Resolution) error { return Checker{Proc: proc, Sysfs: sys, Want: r}.Check(node) }
			if err := check(made[recorded.Port]); err != nil {
				t.Errorf("the card on the recorded interface: %v", err)
			}
			for _, dev := range []config.PlayerDevice{place, other} {
				err := check(made[dev.Port])
				switch {
				case !tc.ambiguous && err != nil:
					t.Errorf("by %s, the check: %v", dev.Port, err)
				case tc.ambiguous && !errors.Is(err, ErrWrongDevice):
					t.Errorf("by %s, the check admitted card 5 beside card %d: %v", dev.Port, tc.unplaced, err)
				case tc.ambiguous:
					says(t, "by "+dev.Port+", the check", err.Error(), []string{"card " + strconv.Itoa(tc.unplaced) + "'s port path cannot be read now",
						"may be another card of the unit at the bound port " + goBarPlace, "resolve again"}, nil)
				}
			}
			_, err := For(inv, 5, config.BindingPort, Endpoint{})
			switch {
			case tc.ambiguous && (err == nil || !strings.Contains(err.Error(), string(StateDeviceAmbiguous))):
				t.Errorf("bind card 5 by port beside card %d: %v", tc.unplaced, err)
			case !tc.ambiguous && err != nil:
				t.Errorf("bind card 5 by port beside card %d: %v", tc.unplaced, err)
			}
			// Resolve: the card on the recorded interface is chosen whatever.
			want(t, Resolve(recorded, inv), StateAvailable, 5)
			for _, dev := range []config.PlayerDevice{place, other} {
				r := Resolve(dev, inv)
				if !tc.ambiguous {
					want(t, r, StateAvailable, 5)
					continue
				}
				want(t, r, StateDeviceAmbiguous, -1)
				says(t, "by "+dev.Port+", the reason", r.Reason, []string{"card 5 is at " + goBarPlace,
					"the port path of card " + strconv.Itoa(tc.unplaced) + " could not be read", "another card of the unit in that port", "neither is chosen"}, nil)
			}
		})
	}
}

// TestNoCardAtTheBoundPortBesideACardWhosePortPathCannotBeReadIsNotMissing:
// with no card at the bound port, a USB card whose port path cannot be read
// may be the card there, so the answer is not that the DAC is absent
// (device_missing): it is identity_incomplete, naming the card and saying to
// resolve again; and the check after open, on the card the binding chose
// before, says that card's port path cannot be read, never that it was
// moved. Gone, the GO bar's root hub's speed leaves the GO bar's port path
// unreadable in a reading the reader still calls whole (it takes the missing
// file for a root hub that went), the controller listed through its other
// root hub, so the hotplug watcher publishes the answer. A port with no card
// and no such card beside it is device_missing, as before (B510).
func TestNoCardAtTheBoundPortBesideACardWhosePortPathCannotBeReadIsNotMissing(t *testing.T) {
	made := Resolve(byPort(goBarPlace), observe(t, pi5(t)))
	want(t, made, StateAvailable, 5)
	for _, tc := range []struct {
		name, step string
		edit       func(alsa.SysfsSnapshot) alsa.SysfsSnapshot
	}{
		{"the GO bar's root hub's speed gone", "remove " + usb1 + "/speed\n", nil},
		{"a read on the GO bar's way up that failed", "", portUnreadable(5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			sysfstest.Apply(t, root, tc.step)
			inv, proc, sys := readTrees(t, root, tc.edit)
			if c, ok := inv.Card(5); !ok || c.State != CardUSB || c.USB.Port != "" || inv.Partial {
				t.Fatalf("premise: card 5 a USB card whose port path could not be read in a whole reading: %+v, %v, partial %v (%q)",
					c, ok, inv.Partial, inv.Problems)
			}
			if err := (Checker{Proc: proc, Sysfs: sys, Want: made}).Check(alsapcm.NodeInfo{Card: 5, Playback: true}); !errors.Is(err, ErrWrongDevice) {
				t.Errorf("the check: %v", err)
			} else {
				says(t, "the check", err.Error(), []string{"card 5's port path cannot be read now", "bound port " + goBarPlace, "resolve again"},
					[]string{"renumbered or moved"})
			}
			for _, port := range []string{goBarPlace, goBarPort} {
				r := Resolve(byPort(port), inv)
				want(t, r, StateIdentityIncomplete, -1)
				says(t, "by "+port+", the reason", r.Reason, []string{"the port path of card 5 could not be read",
					"may be the card at " + goBarPlace, "resolve again"}, []string{"no card is at"})
			}
		})
	}
	root := pi5(t)
	sysfstest.Apply(t, root, "unplug "+goBar+"\n")
	r := Resolve(byPort(goBarPlace), observe(t, root))
	want(t, r, StateDeviceMissing, -1)
	says(t, "an empty port", r.Reason, []string{"no card is at " + goBarPlace}, nil)
}

// TestSettledAnswersAUSBListingThatCannotBeReadWholeAtOnce: Resolve answers
// a port binding whose USB listing could not be read whole, and lacks its
// controller, before it looks at any card, as it answers a card listing cut
// short, so Settled answers it at once, settled, beside a transient card
// that may be at the bound port: no card can change it on this reading. No
// card at the bound port beside a card whose port path cannot be read is
// decided among the cards, as an empty port is: a transient card that may be
// at the port holds the answer, since once it settles it may be the card
// there, and one at another port does not (B510).
func TestSettledAnswersAUSBListingThatCannotBeReadWholeAtOnce(t *testing.T) {
	t.Run("a USB listing that cannot be read beside a card in /proc alone", func(t *testing.T) {
		root := pi5(t)
		sysfstest.Apply(t, root, usbUnlisted+procOnly("1234:0f05"))
		inv := observe(t, root)
		for _, port := range []string{goBarPlace, goBarPort} {
			settled(t, byPort(port), inv, StateIdentityIncomplete, -1, true)
			want(t, Resolve(byPort(port), inv), StateIdentityIncomplete, -1)
		}
	})
	t.Run("an unreadable port path beside a card in /proc alone", func(t *testing.T) {
		root := pi5(t)
		sysfstest.Apply(t, root, "remove "+usb1+"/speed\n"+procOnly("1234:0f05"))
		if r := settled(t, byPort(goBarPlace), observe(t, root), StateIdentityTransient, -1, false); !strings.Contains(r.Reason, "card 7") {
			t.Errorf("the reason does not name card 7: %s", r.Reason)
		}
	})
	t.Run("an unreadable port path beside a card in sysfs alone at another port", func(t *testing.T) {
		// Card 7 on the other controller, whose port path reads.
		root := pi5(t)
		sysfstest.Apply(t, root, "remove "+usb1+"/speed\nusbdev "+usb3+"/3-1 vendor=1234 product=0f05\nusbcard 7 "+usb3+"/3-1/3-1:1.0\n")
		settled(t, byPort(goBarPlace), observe(t, root), StateIdentityIncomplete, -1, true)
	})
}
