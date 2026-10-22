package binding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
)

// TestAReadingThatCannotListACardsDevicesIsPartial: a card's /proc/asound
// directory, or a PCM directory in it, that cannot be listed leaves the
// card's playback endpoints unknown, so the inventory is partial and its
// problems say which, as a sysfs listing that could not be read is; the
// hotplug watcher then publishes nothing from it. Until round 2 the
// failure was a problem on the /proc card alone, which the inventory
// dropped, and the card read as having no endpoint: endpoint_missing. A
// card or a PCM directory that is not there is the box as it is (the card
// or the device going), never a partial reading.
func TestAReadingThatCannotListACardsDevicesIsPartial(t *testing.T) {
	const card5 = "proc/asound/card5"
	// unreadable takes the read permission from dir and gives it back
	// before the tree is removed. os.Root opens each directory on a file's
	// way for reading, on every Unix, so nothing below dir opens either: a
	// card directory hides the card's usbid too, which makes a USB-Audio
	// card transient, so that case's card is one whose driver writes none.
	unreadable := func(t *testing.T, root, dir string) {
		t.Helper()
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a POSIX permission bit a non-root reader honours")
		}
		p := filepath.Join(root, filepath.FromSlash(dir))
		if err := os.Chmod(p, 0o100); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
	for _, tc := range []struct {
		name string
		// change makes the listing fail, or the entry go.
		change  func(t *testing.T, root string)
		partial bool
		// state and card are what a serial binding of the GO bar resolves
		// to from what was read.
		state State
		card  int
	}{
		{"a PCM directory that cannot be listed", func(t *testing.T, root string) { unreadable(t, root, card5+"/pcm0p") },
			true, StateEndpointMissing, 5},
		{"a PCM directory whose link leaves the tree", func(t *testing.T, root string) {
			sysfstest.Apply(t, root, "remove "+card5+"/pcm0p\nlink "+card5+"/pcm0p !../../../outside\n")
		}, true, StateEndpointMissing, 5},
		{"a card directory that cannot be listed", func(t *testing.T, root string) {
			sysfstest.Apply(t, root, "procremove 5\nproccard 5 Audio \"iFi (by AMR) HD USB Audio\" driver=snd-usb-hiface pcm=0.0\n")
			unreadable(t, root, card5)
		}, true, StateEndpointMissing, 5},
		{"a PCM directory that is not there", func(t *testing.T, root string) { sysfstest.Apply(t, root, "remove "+card5+"/pcm0p\n") },
			false, StateEndpointMissing, 5},
		{"a card directory that is not there", func(t *testing.T, root string) { sysfstest.Apply(t, root, "remove "+card5+"\n") },
			false, StateIdentityTransient, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pi5(t)
			tc.change(t, root)
			inv := observe(t, root)
			if inv.Partial != tc.partial {
				t.Errorf("partial %v, want %v (problems %q)", inv.Partial, tc.partial, inv.Problems)
			}
			if tc.partial && !strings.Contains(strings.Join(inv.Problems, "; "), "card5") {
				t.Errorf("the problems do not say which card could not be listed: %q", inv.Problems)
			}
			// Resolve answers from what was read, as it does for a partial
			// sysfs reading: not acting on it is its caller's.
			want(t, Resolve(bySerial(goBarSerial), inv), tc.state, tc.card)
		})
	}
}
