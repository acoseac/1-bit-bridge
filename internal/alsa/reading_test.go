package alsa

import (
	"context"
	"encoding/json"
	"errors"
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

// TestAStoppedReadingOpensNothingMore: once its context is done, each
// reader finishes the file it is reading and opens nothing after it; the
// sysfs reading says it is partial, and the /proc reading returns the
// context's error.
func TestAStoppedReadingOpensNothingMore(t *testing.T) {
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opened []string
	sys := SysfsReader{Root: filepath.Join(root, "sys"), beforeOpen: func(rel string) {
		opened = append(opened, rel)
		cancel()
	}}.ReadContext(ctx)
	if len(opened) != 1 || !sys.Partial || !strings.Contains(strings.Join(sys.Problems, "; "), "stopped") {
		t.Errorf("the sysfs reading after a stop at %q: opened %q, partial %v, problems %q", opened, opened, sys.Partial, sys.Problems)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	opened = nil
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
