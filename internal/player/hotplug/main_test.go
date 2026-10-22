package hotplug

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

// TestMain refuses, and fails the binary on, any read of the host's own
// /proc or /sys and any open of a PCM node: the watcher reads the trees it
// is handed and opens nothing under /dev/snd, ever (4a's seams pin it).
func TestMain(m *testing.M) {
	var mu sync.Mutex
	var touched []string
	stopReads := alsa.WatchHostReads(func(root string) error {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "read "+root)
		return errors.New("a test read the host's " + root)
	})
	stopOpens := alsapcm.GuardNodeOpens(func(path string) {
		mu.Lock()
		defer mu.Unlock()
		touched = append(touched, "open "+path)
	})
	code := m.Run()
	stopReads()
	stopOpens()
	if len(touched) > 0 {
		fmt.Fprintf(os.Stderr, "hotplug tests touched the host's hardware: %q\n", touched)
		code = 1
	}
	os.Exit(code)
}
