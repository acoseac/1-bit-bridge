package alsa

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
)

// TestSysfsReadsWhichEnumerationAUnitIs: a USB unit's devnum, which the
// kernel gives each enumeration anew, and the card's directory are read and
// kept in memory: a unit unplugged and plugged back into its port is the
// same identity at another devnum. Neither is encoded: devnum is never to be
// persisted, and the JSON shape is the reading's diagnostic one.
func TestSysfsReadsWhichEnumerationAUnitIs(t *testing.T) {
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	r := SysfsReader{Root: filepath.Join(root, "sys")}
	snap := r.Read()
	c := cardOf(t, snap, 5)
	if c.USB == nil || c.USB.DevNum != 2 || c.Dir != strings.TrimPrefix(goBarDevice, "sys/")+"/1-1:1.0/sound/card5" {
		t.Fatalf("card 5: %+v, dir %q", c.USB, c.Dir)
	}
	if hdmi := cardOf(t, snap, 0); hdmi.Dir != "devices/platform/axi/107c701400.hdmi/sound/card0" {
		t.Errorf("the HDMI card's directory: %q", hdmi.Dir)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "devnum") || strings.Contains(string(b), "sound/card5") {
		t.Errorf("the reading's JSON carries the enumeration or the directory: %s", b)
	}
	sysfstest.Apply(t, root, "unplug "+goBarDevice+"\n"+
		"usbdev "+goBarDevice+" vendor=20b1 product=3009 serial=GOBAR-7Q2X-91KZ-4471 devnum=3\n"+
		"usbcard 5 "+goBarDevice+"/1-1:1.0\n")
	again := cardOf(t, r.Read(), 5)
	if again.USB == nil || again.USB.DevNum != 3 || again.USB.ID != c.USB.ID || !again.USB.Serial.Equal(c.USB.Serial) ||
		again.USB.Port != c.USB.Port || again.Dir != c.Dir {
		t.Errorf("the GO bar plugged back: %+v, dir %q", again.USB, again.Dir)
	}
}

// TestSysfsSaysWhenItsReadingIsNotWhole: a root that cannot be opened, a
// listing a cap cut, and a root hub that could not be read each make the
// reading Partial: what it lacks is not absent from the box. A whole tree,
// and a tree with no class/sound (no sound cards to identify), are not.
func TestSysfsSaysWhenItsReadingIsNotWhole(t *testing.T) {
	whole, _ := readSysfs(t, "pi5-go-bar")
	if whole.Partial {
		t.Fatalf("a whole tree read as partial: %q", whole.Problems)
	}
	if snap := (SysfsReader{Root: t.TempDir()}).Read(); snap.Partial || snap.Present {
		t.Errorf("a tree with no class/sound: %+v", snap)
	}
	if snap := (SysfsReader{Root: filepath.Join(t.TempDir(), "gone")}).Read(); !snap.Partial || snap.Present {
		t.Errorf("a root that is not there: %+v", snap)
	}
	limited := func(root string) SysfsSnapshot {
		return SysfsReader{Root: filepath.Join(root, "sys"), limits: &sysfsLimits{entries: 2, file: maxSysfsFile, links: maxLinks, hops: maxHops}}.Read()
	}
	if snap := limited(sysfstest.Build(t, sysfsManifest("pi5-go-bar"))); !snap.Partial {
		t.Errorf("listings cut at two entries: %+v", snap)
	}
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	sysfstest.Apply(t, root, "remove "+goBarHub+"/speed\ndir "+goBarHub+"/speed\n")
	snap := SysfsReader{Root: filepath.Join(root, "sys")}.Read()
	if !snap.Partial || !strings.Contains(strings.Join(snap.Problems, "; "), "usb1/speed") {
		t.Errorf("a root hub whose speed cannot be read: partial %v, problems %q", snap.Partial, snap.Problems)
	}
}

// TestAProcCardSaysWhenItsDevicesCannotAllBeListed: a card whose directory
// or a pcmNp directory cannot be listed, or whose pcmNp or subM entry's
// type cannot be read, is Partial, and its problems say which: its devices
// are not all known. One that is not there, gone between the listing that
// named it and its own, is the card or the device going, never Partial
// (a pcmNp the card's listing named is still a problem, as before). The
// other cards read whole (B470 PR 4a2 round 2).
func TestAProcCardSaysWhenItsDevicesCannotAllBeListed(t *testing.T) {
	const card5 = "proc/asound/card5"
	for _, tc := range []struct {
		name string
		// step changes the Pi 5's tree; gone is a path the reader is about
		// to list, removed just before it does.
		step, gone string
		partial    bool
		problem    string
	}{
		{"a card directory whose link loops", "remove " + card5 + "\nlink " + card5 + " !card5\n", "", true, "card5: too many links"},
		{"a pcmNp directory whose link leaves the tree", "remove " + card5 + "/pcm0p\nlink " + card5 + "/pcm0p !../../../outside\n", "", true, "card5/pcm0p: a link leaves the root"},
		{"a pcmNp entry whose link loops", "remove " + card5 + "/pcm0p\nlink " + card5 + "/pcm0p !pcm0p\n", "", true, "card5/pcm0p: too many links"},
		{"a subM entry whose link loops", "remove " + card5 + "/pcm0p/sub0\nlink " + card5 + "/pcm0p/sub0 !sub0\n", "", true, "card5/pcm0p/sub0: too many links"},
		{"a pcmNp directory gone before its listing", "", "asound/card5/pcm0p", false, "card5/pcm0p: "},
		{"a card directory gone before its listing", "", "asound/card5", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
			sysfstest.Apply(t, root, tc.step)
			snap, err := Reader{ProcRoot: filepath.Join(root, "proc"), beforeOpen: func(path string) {
				if path == tc.gone {
					if err := os.RemoveAll(filepath.Join(root, "proc", filepath.FromSlash(path))); err != nil {
						t.Error(err)
					}
				}
			}}.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Cards) != 3 {
				t.Fatalf("cards %+v", snap.Cards)
			}
			for _, c := range snap.Cards {
				if c.Index != 5 {
					if c.Partial || len(c.Problems) != 0 || len(c.Devices) != 1 {
						t.Errorf("card %d did not read whole: partial %v, problems %q, %d devices", c.Index, c.Partial, c.Problems, len(c.Devices))
					}
					continue
				}
				problems := strings.Join(c.Problems, "; ")
				if c.Partial != tc.partial || !strings.Contains(problems, tc.problem) || (tc.problem == "") != (problems == "") {
					t.Errorf("card 5: partial %v, problems %q; want partial %v, a problem naming %q", c.Partial, c.Problems, tc.partial, tc.problem)
				}
				if len(c.Devices) != 0 && (len(c.Devices) != 1 || len(c.Devices[0].Substreams) != 0) {
					t.Errorf("card 5 has endpoints it could not list: %+v", c.Devices)
				}
			}
		})
	}
}

// TestAStoppedReadingOpensNothingMore: once its context is done, each
// reader finishes the file it is reading and opens nothing after it. The
// sysfs reading says it is partial wherever the stop lands, and each case
// here leaves something of its kind unread after it, which a reading that
// only looked whole would report as gone: card 0 of 0, 1 and 5, a root hub
// before three more, a USB device before another. Until round 2 the mark
// was set in a deferred function on an unnamed result, after the result
// was copied, so a stop anywhere but the last card returned the reading
// whole; the old test stopped at card 5, the last, and its Partial came
// from the USB listing the stop refused. A stop before the last card also
// leaves the reading's cards short of its listing, which CardsComplete says,
// as a cut listing does; one in the USB loops has read every card (round 4:
// the port-binding fix's CardsComplete stayed true on a stop at card 0). The
// /proc reading returns the context's error.
func TestAStoppedReadingOpensNothingMore(t *testing.T) {
	// card0Unit is a USB DAC on the Pi 5's second controller, put in for
	// the first HDMI card as card 0, so the stop can land inside card 0.
	const card0Unit = "sys/devices/platform/axi/1000120000.pcie/1f00300000.usb/xhci-hcd.1/usb3/3-1"
	for _, tc := range []struct {
		name string
		// step changes the Pi 5's tree; the stop lands on the first open
		// whose path begins with at; read says the reading stopped there,
		// and complete whether it read every card its listing names.
		step, at string
		read     func(SysfsSnapshot) bool
		complete bool
	}{
		{"card 0 of 0, 1 and 5", "usbdev " + card0Unit + " vendor=1234 product=0f05 devnum=2\nusbcard 0 " + card0Unit + "/3-1:1.0\n",
			strings.TrimPrefix(card0Unit, "sys/") + "/",
			func(s SysfsSnapshot) bool { return len(s.Cards) == 1 && s.Cards[0].Card == 0 }, false},
		{"a root hub before three more", "unplug " + goBarDevice + "\n",
			strings.TrimPrefix(goBarHub, "sys/") + "/speed",
			func(s SysfsSnapshot) bool { return len(s.Cards) == 2 && len(s.Controllers) == 0 }, true},
		{"a USB device before another", "usbdev " + goBarHub + "/1-4 vendor=046d product=c31c devnum=7\n",
			strings.TrimPrefix(goBarDevice, "sys/") + "/bDeviceClass",
			func(s SysfsSnapshot) bool { return len(s.Cards) == 3 && len(s.Devices) == 1 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
			sysfstest.Apply(t, root, tc.step)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var opened []string
			stop := -1
			sys := SysfsReader{Root: filepath.Join(root, "sys"), beforeOpen: func(rel string) {
				opened = append(opened, rel)
				if stop < 0 && strings.HasPrefix(rel, tc.at) {
					stop = len(opened) - 1
					cancel()
				}
			}}.ReadContext(ctx)
			if stop < 0 {
				t.Fatalf("the reading never opened %s: %q", tc.at, opened)
			}
			if after := opened[stop+1:]; len(after) > 0 {
				t.Errorf("opened %q after the stop at %s", after, opened[stop])
			}
			if !sys.Partial || !strings.Contains(strings.Join(sys.Problems, "; "), "stopped") {
				t.Errorf("the reading stopped at %s: partial %v, problems %q", opened[stop], sys.Partial, sys.Problems)
			}
			if !tc.read(sys) {
				t.Errorf("the reading did not stop where the case says: %d cards, controllers %q, %d devices",
					len(sys.Cards), sys.Controllers, len(sys.Devices))
			}
			if sys.CardsComplete != tc.complete {
				t.Errorf("the reading stopped at %s says its cards are complete: %v, with %d cards read", opened[stop], sys.CardsComplete, len(sys.Cards))
			}
		})
	}

	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opened []string
	proc, err := Reader{ProcRoot: filepath.Join(root, "proc"), beforeOpen: func(path string) {
		opened = append(opened, path)
		if path == "asound/card0" {
			cancel()
		}
	}}.ReadContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the /proc reading stopped at card 0: error %v", err)
	}
	if last := opened[len(opened)-1]; last != "asound/card0" || len(proc.Cards) > 1 {
		t.Errorf("the /proc reading opened %q after its stop, and read %d cards", opened, len(proc.Cards))
	}
}
