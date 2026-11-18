package playback_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// TestAManufacturerStringThatDoesNotReadRefusesEveryRate: the bound unit's
// manufacturer string, there and not read (a NUL in it, or a directory at its
// name), is the reader's error, never an empty string, so no set is computed
// and every rate is refused for want of a reading of the DAC (dsd_dac_unread),
// through the box's own readers over its trees; the same unit whose string
// reads, or that has none, is read and its set computed.
func TestAManufacturerStringThatDoesNotReadRefusesEveryRate(t *testing.T) {
	const goBar = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"
	stream, err := os.ReadFile(filepath.Join("..", "..", "alsa", "testdata", "dsd256-native", "proc", "asound", "card5", "stream0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		lay      func(t *testing.T, at string)
		computed bool
	}{
		{"a string that reads", func(t *testing.T, at string) { writeText(t, at, "iFi\n") }, true},
		{"no string", func(*testing.T, string) {}, true},
		{"a NUL in the string", func(t *testing.T, at string) { writeText(t, at, "iFi\x00\n") }, false},
		{"a directory at its name", func(t *testing.T, at string) {
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := sysfstest.Build(t, filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
			writeText(t, filepath.Join(root, goBar, "product"), "GO bar\n")
			writeText(t, filepath.Join(root, goBar, "bcdDevice"), "0123\n")
			writeText(t, filepath.Join(root, "proc", "asound", "card5", "stream0"), string(stream))
			c.lay(t, filepath.Join(root, goBar, "manufacturer"))
			sysfs := alsa.SysfsReader{Root: filepath.Join(root, "sys")}
			id, ok := sysfs.Card(5)
			if !ok || id.USB == nil {
				t.Fatal("the GO bar's card does not read")
			}
			pol := &playback.DSDPolicy{Store: playbacktest.NewMemStore(dsdstate.Defaults()), Kernel: playbacktest.Kernel,
				Box: playback.HostBox{Sysfs: sysfs, Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}},
				Bound: func() (playback.BoundDAC, bool) {
					return playback.BoundDAC{Card: 5, Identity: *id.USB, Enumeration: "test/2"}, true
				},
				Now: func() time.Time { return playbacktest.T0 }}
			b, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: pol})
			if err != nil {
				t.Fatal(err)
			}
			b.RefreshDSD(true)
			p := b.Policy()
			if unread := p.Reason == string(playback.ReasonDACUnread); unread == c.computed {
				t.Fatalf("the set's reason is %q (tuples %v, converted %v): want a set computed %v", p.Reason, p.Tuples, p.Converted, c.computed)
			}
			if !c.computed && (len(p.Tuples) != 0 || len(p.Converted) != 0) {
				t.Errorf("a unit whose manufacturer string does not read plays %v, converted %v", p.Tuples, p.Converted)
			}
		})
	}
}

// writeText writes text at path.
func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
