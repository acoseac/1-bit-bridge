//go:build linux || darwin

package alsa

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
)

// A FIFO opened for reading blocks until a writer comes, so a fake tree
// that holds one where the readers open a file or a directory would hold
// the reader for good. These tests plant one at each kind of path the two
// readers open; only a POSIX system makes a FIFO.

// fifoWatchdog is how long a read of a tree with a FIFO in it may take
// before the test calls it blocked. A read that does not open the FIFO
// returns in milliseconds.
const fifoWatchdog = 5 * time.Second

// The GO bar's USB device in pi5-go-bar, and the root hub above it.
const (
	goBarHub    = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1"
	goBarDevice = goBarHub + "/1-1"
)

// plantFIFO replaces rel, under root, with a FIFO, and returns its path.
func plantFIFO(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.RemoveAll(p); err != nil {
		return p, err
	}
	return p, syscall.Mkfifo(p, 0o600)
}

// readsPromptly runs read and fails the test when it has not returned
// within fifoWatchdog. Each of fifos is then opened for writing, which lets
// a reader blocked opening it go, so a failure leaves no goroutine behind.
func readsPromptly(t *testing.T, fifos []string, read func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		read()
	}()
	select {
	case <-done:
		return
	case <-time.After(fifoWatchdog):
	}
	for _, p := range fifos {
		if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	}
	<-done
	t.Fatalf("the read was still blocked after %v: it opened a FIFO", fifoWatchdog)
}

// TestAFIFOInTheSysfsTreeIsRefusedPromptly: a FIFO where the sysfs reader
// opens a directory (the root, class/sound, bus/usb/devices) or reads an
// attribute (idVendor, serial, a root hub's speed, bDeviceClass) is
// refused at once as a problem, and the reading returns.
func TestAFIFOInTheSysfsTreeIsRefusedPromptly(t *testing.T) {
	problems := func(snap SysfsSnapshot) string {
		all := append([]string{}, snap.Problems...)
		for _, c := range snap.Cards {
			all = append(all, c.Problems...)
			if c.USB != nil && c.USB.PortProblem != "" {
				all = append(all, c.USB.PortProblem)
			}
		}
		return strings.Join(all, "; ")
	}
	for _, tc := range []struct {
		name, at string
		ok       func(snap SysfsSnapshot) bool
	}{
		{"the root", "sys", func(snap SysfsSnapshot) bool { return !snap.Present && len(snap.Problems) == 1 }},
		{"class/sound", "sys/class/sound", func(snap SysfsSnapshot) bool {
			return !snap.Present && strings.Contains(problems(snap), "class/sound")
		}},
		{"bus/usb/devices", "sys/bus/usb/devices", func(snap SysfsSnapshot) bool {
			return snap.Present && len(snap.Controllers) == 0 && strings.Contains(problems(snap), "bus/usb/devices")
		}},
		{"idVendor", goBarDevice + "/idVendor", func(snap SysfsSnapshot) bool {
			return cardKind(snap, 5) == IdentityUnreadable && strings.Contains(problems(snap), "idVendor")
		}},
		{"serial", goBarDevice + "/serial", func(snap SysfsSnapshot) bool {
			return cardKind(snap, 5) == IdentityUnreadable && strings.Contains(problems(snap), "serial")
		}},
		{"speed", goBarHub + "/speed", func(snap SysfsSnapshot) bool { return strings.Contains(problems(snap), "speed") }},
		{"bDeviceClass", goBarDevice + "/bDeviceClass", func(snap SysfsSnapshot) bool {
			return cardKind(snap, 5) == IdentityUSB && len(snap.Devices) == 1 && !snap.Devices[0].Hub
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
			fifo, err := plantFIFO(root, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			var snap SysfsSnapshot
			readsPromptly(t, []string{fifo}, func() { snap = SysfsReader{Root: filepath.Join(root, "sys")}.Read() })
			if !tc.ok(snap) {
				t.Errorf("a FIFO at %s read as %+v", tc.at, snap)
			}
		})
	}
}

// cardKind is the kind of card n in snap, "" when it is not there.
func cardKind(snap SysfsSnapshot, n int) IdentityKind {
	for _, c := range snap.Cards {
		if c.Card == n {
			return c.Kind
		}
	}
	return ""
}

// TestAnAttributeSwappedForAFIFOAfterItsCheckIsRefused: the type that
// decides is the opened descriptor's. An attribute that is a regular file
// when the reader looks at it and a FIFO when it opens it is refused, at
// once, and never read.
func TestAnAttributeSwappedForAFIFOAfterItsCheckIsRefused(t *testing.T) {
	root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
	rel := strings.TrimPrefix(goBarDevice, "sys/") + "/idVendor"
	fifo := filepath.Join(root, "sys", filepath.FromSlash(rel))
	var once sync.Once
	var swapErr error
	r := SysfsReader{Root: filepath.Join(root, "sys"), beforeOpen: func(at string) {
		if at == rel {
			once.Do(func() { _, swapErr = plantFIFO(root, goBarDevice+"/idVendor") })
		}
	}}
	var c Identity
	var ok bool
	readsPromptly(t, []string{fifo}, func() { c, ok = r.Card(5) })
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if !ok || c.Kind != IdentityUnreadable || !strings.Contains(strings.Join(c.Problems, "; "), "idVendor: not a regular file") {
		t.Errorf("card 5 with its idVendor swapped for a FIFO: %+v, %v", c, ok)
	}
}

// TestAFIFOInTheProcTreeIsRefusedPromptly: the /proc reader too. A FIFO at
// the cards file is the reading's error; one at a card's usbid, its
// directory, a playback device's directory or a substream's status is the
// card's problem; and the reading returns.
func TestAFIFOInTheProcTreeIsRefusedPromptly(t *testing.T) {
	for _, tc := range []struct {
		name, at string
		// problem is what card 5's problems say, "" for nothing; fails is
		// the reading's own error, for the cards file.
		problem string
		fails   bool
	}{
		{name: "the cards file", at: "proc/asound/cards", fails: true},
		{name: "a usbid", at: "proc/asound/card5/usbid", problem: "card5/usbid"},
		{name: "a card's directory", at: "proc/asound/card5", problem: "card5"},
		{name: "a playback device's directory", at: "proc/asound/card5/pcm0p"},
		{name: "a substream's status", at: "proc/asound/card5/pcm0p/sub0/status", problem: "card5/pcm0p/sub0/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sysfstest.Build(t, sysfsManifest("pi5-go-bar"))
			fifo, err := plantFIFO(root, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			var snap Snapshot
			var readErr error
			readsPromptly(t, []string{fifo}, func() { snap, readErr = Reader{ProcRoot: filepath.Join(root, "proc")}.Read() })
			if (readErr != nil) != tc.fails {
				t.Fatalf("a FIFO at %s: the reading's error is %v", tc.at, readErr)
			}
			for _, c := range snap.Cards {
				if c.Index == 5 && !strings.Contains(strings.Join(c.Problems, "; "), tc.problem) {
					t.Errorf("card 5 with a FIFO at %s: problems %q", tc.at, c.Problems)
				}
			}
		})
	}
}
