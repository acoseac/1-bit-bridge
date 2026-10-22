package hotplug

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uevent builds a kernel uevent message as kobject_uevent_env lays one out:
// "ACTION@DEVPATH", a NUL, then each KEY=VALUE record and a NUL after it,
// ACTION, DEVPATH and SUBSYSTEM first and SEQNUM last.
func uevent(action, devpath, subsystem string, seqnum string, extra ...string) []byte {
	var b bytes.Buffer
	b.WriteString(action + "@" + devpath + "\x00")
	for _, r := range append(append([]string{"ACTION=" + action, "DEVPATH=" + devpath, "SUBSYSTEM=" + subsystem}, extra...), "SEQNUM="+seqnum) {
		b.WriteString(r + "\x00")
	}
	return b.Bytes()
}

// TestParseUeventReadsTheCapturedMessages: every message in
// testdata/uevent parses into what it says. They are reconstructions of the
// GO bar's events on a Pi 5, laid out as the kernel lays them out
// (testdata/uevent/reconstruct.py), until TestCaptureKernelUevents's capture
// on the Pi replaces them; the test reads whatever .bin files are there.
func TestParseUeventReadsTheCapturedMessages(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "uevent", "*.bin"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no messages in testdata/uevent: %v", err)
	}
	got := map[string]Uevent{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		u, err := ParseUevent(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		got[filepath.Base(f)] = u
	}
	const goBar = "/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"
	const iface = goBar + "/1-1:1.0"
	const card = iface + "/sound/card5"
	known := map[string]Uevent{
		"gobar-usb-device-add.bin":      {Action: "add", DevPath: goBar, Subsystem: "usb", DevType: "usb_device", Seqnum: 5219},
		"gobar-usb-interface-add.bin":   {Action: "add", DevPath: iface, Subsystem: "usb", DevType: "usb_interface", Seqnum: 5220},
		"gobar-usb-device-bind.bin":     {Action: "bind", DevPath: goBar, Subsystem: "usb", DevType: "usb_device", Driver: "usb", Seqnum: 5231},
		"gobar-interface-bind.bin":      {Action: "bind", DevPath: iface, Subsystem: "usb", DevType: "usb_interface", Driver: "snd-usb-audio", Seqnum: 5232},
		"gobar-sound-card-add.bin":      {Action: "add", DevPath: card, Subsystem: "sound", Seqnum: 5233},
		"gobar-control-add.bin":         {Action: "add", DevPath: card + "/controlC5", Subsystem: "sound", Seqnum: 5234},
		"gobar-pcm-playback-add.bin":    {Action: "add", DevPath: card + "/pcmC5D0p", Subsystem: "sound", Seqnum: 5235},
		"gobar-pcm-playback-remove.bin": {Action: "remove", DevPath: card + "/pcmC5D0p", Subsystem: "sound", Seqnum: 5297},
		"gobar-sound-card-remove.bin":   {Action: "remove", DevPath: card, Subsystem: "sound", Seqnum: 5299},
		"gobar-interface-unbind.bin":    {Action: "unbind", DevPath: iface, Subsystem: "usb", DevType: "usb_interface", Seqnum: 5301},
		"gobar-interface-remove.bin":    {Action: "remove", DevPath: iface, Subsystem: "usb", DevType: "usb_interface", Seqnum: 5302},
		"gobar-usb-device-unbind.bin":   {Action: "unbind", DevPath: goBar, Subsystem: "usb", DevType: "usb_device", Seqnum: 5317},
		"gobar-usb-device-remove.bin":   {Action: "remove", DevPath: goBar, Subsystem: "usb", DevType: "usb_device", Seqnum: 5318},
		"hdmi-sound-card-change.bin":    {Action: "change", DevPath: "/devices/platform/axi/107c701400.hdmi/sound/card0", Subsystem: "sound", Seqnum: 4102},
		"block-device-add.bin":          {Action: "add", DevPath: "/devices/virtual/block/loop7", Subsystem: "block", DevType: "disk", Seqnum: 6010},
	}
	for name, g := range got {
		want, ok := known[name]
		if !ok {
			// A capture the owner added: it parses, which is checked above.
			continue
		}
		if g != want {
			t.Errorf("%s: %+v, want %+v", name, g, want)
		}
		if g.Wakes() != (g.Subsystem != "block") {
			t.Errorf("%s wakes %v", name, g.Wakes())
		}
	}
}

// TestOnlySoundAndUSBWakeTheWatcher: add, remove, change, bind and unbind
// of a sound or usb device wake the watcher; any other subsystem or action
// does not.
func TestOnlySoundAndUSBWakeTheWatcher(t *testing.T) {
	for _, tc := range []struct {
		action, subsystem string
		wakes             bool
	}{
		{"add", "usb", true}, {"remove", "usb", true}, {"change", "usb", true}, {"bind", "usb", true}, {"unbind", "usb", true},
		{"add", "sound", true}, {"remove", "sound", true}, {"change", "sound", true}, {"bind", "sound", true}, {"unbind", "sound", true},
		{"move", "usb", false}, {"online", "sound", false}, {"offline", "usb", false},
		{"add", "input", false}, {"remove", "block", false}, {"add", "hidraw", false}, {"add", "usbmisc", false},
	} {
		u := Uevent{Action: tc.action, DevPath: "/devices/x", Subsystem: tc.subsystem, Seqnum: 1}
		if u.Wakes() != tc.wakes {
			t.Errorf("%s of %s wakes %v, want %v", tc.action, tc.subsystem, u.Wakes(), tc.wakes)
		}
	}
}

// TestParseUeventDropsWhatTheKernelNeverSends: a message the kernel does
// not lay out is malformed, and no partial event comes out of it.
func TestParseUeventDropsWhatTheKernelNeverSends(t *testing.T) {
	const dev = "/devices/platform/x/usb1/1-1"
	good := uevent("add", dev, "usb", "7")
	if _, err := ParseUevent(good); err != nil {
		t.Fatalf("a well-formed message: %v", err)
	}
	many := make([]string, maxUeventRecords)
	for i := range many {
		many[i] = "K" + strings.Repeat("X", i%5) + "=v"
	}
	for name, msg := range map[string][]byte{
		"empty":                    {},
		"no NUL after the header":  []byte("add@" + dev),
		"no @ in the header":       []byte("add" + dev + "\x00ACTION=add\x00DEVPATH=" + dev + "\x00SUBSYSTEM=usb\x00SEQNUM=1\x00"),
		"no action":                uevent("", dev, "usb", "7"),
		"a devpath not from root":  uevent("add", "devices/x", "usb", "7"),
		"udev's own message":       append([]byte("libudev\x00"), good...),
		"no SEQNUM":                []byte("add@" + dev + "\x00ACTION=add\x00DEVPATH=" + dev + "\x00SUBSYSTEM=usb\x00"),
		"no SUBSYSTEM":             []byte("add@" + dev + "\x00ACTION=add\x00DEVPATH=" + dev + "\x00SEQNUM=1\x00"),
		"an empty SUBSYSTEM":       uevent("add", dev, "", "7"),
		"a SEQNUM that is no number": uevent("add", dev, "usb", "seven"),
		"a negative SEQNUM":        uevent("add", dev, "usb", "-7"),
		"ACTION unlike the header": []byte("add@" + dev + "\x00ACTION=remove\x00DEVPATH=" + dev + "\x00SUBSYSTEM=usb\x00SEQNUM=1\x00"),
		"DEVPATH unlike the header": []byte("add@" + dev + "\x00ACTION=add\x00DEVPATH=/devices/y\x00SUBSYSTEM=usb\x00SEQNUM=1\x00"),
		"a second SUBSYSTEM":       uevent("add", dev, "usb", "7", "SUBSYSTEM=sound"),
		"a second SEQNUM":          append(uevent("add", dev, "usb", "7"), []byte("SEQNUM=8\x00")...),
		"a record with no =":       uevent("add", dev, "usb", "7", "DEVTYPE"),
		"a record with no key":     uevent("add", dev, "usb", "7", "=usb_device"),
		"an empty record inside":   uevent("add", dev, "usb", "7", ""),
		"too many records":         uevent("add", dev, "usb", "7", many...),
		"longer than the cap":      uevent("add", dev, "usb", "7", "PAD="+strings.Repeat("p", maxUeventBytes)),
	} {
		if u, err := ParseUevent(msg); !errors.Is(err, errMalformedUevent) || u != (Uevent{}) {
			t.Errorf("%s: %+v, %v; want malformed and nothing else", name, u, err)
		}
	}
}

// TestParseUeventTakesTheKernelsTrailingNULOrNone: the kernel ends every
// record with a NUL, so a message ends with one; a message whose last
// record has none still reads, and the optional fields read when present.
func TestParseUeventTakesTheKernelsTrailingNULOrNone(t *testing.T) {
	const dev = "/devices/platform/x/usb1/1-1/1-1:1.0"
	msg := uevent("bind", dev, "usb", "18446744073709551615", "DEVTYPE=usb_interface", "DRIVER=snd-usb-audio", "INTERFACE=1/1/32")
	for _, m := range [][]byte{msg, msg[:len(msg)-1]} {
		u, err := ParseUevent(m)
		want := Uevent{Action: "bind", DevPath: dev, Subsystem: "usb", DevType: "usb_interface", Driver: "snd-usb-audio", Seqnum: 18446744073709551615}
		if err != nil || u != want {
			t.Errorf("%q: %+v, %v", m, u, err)
		}
	}
}
