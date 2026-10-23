package playback

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

// TestMain fails the binary if a test read the host's /proc or /sys, or
// opened a PCM node: these tests read the trees they lay out, and the
// backend's tests that play run over the fake kernel, in internal/alsapcm.
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
		fmt.Fprintf(os.Stderr, "playback tests touched the host's hardware: %q\n", touched)
		code = 1
	}
	os.Exit(code)
}
