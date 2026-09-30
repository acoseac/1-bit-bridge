//go:build unix

package acoustid

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestComputeFromPrefixRefusesANamedPipeWithoutWaiting: the prefix mode opens
// the file itself, in this process, to feed fpcalc its first bytes, and a
// named pipe there (a file replaced by one since its scan) is refused at
// once as unreadable, its path left out of the error. A plain open of the pipe
// waited for a writer, and nothing can cancel that wait: the run, and the
// context it was handed, could not stop it.
func TestComputeFromPrefixRefusesANamedPipeWithoutWaiting(t *testing.T) {
	withFakeFpcalc(t, `{"duration": 45.00, "fingerprint": "AQABz0mUaEkSRZEG"}`, 0)
	p := filepath.Join(t.TempDir(), "01.flac")
	fsutiltest.MakeFIFO(t, p)
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = ComputeFromPrefix(context.Background(), p, 0, 1024)
	}()
	fsutiltest.AwaitPastFIFOs(t, "ComputeFromPrefix", fsutiltest.ServeBound, done, p)
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err %v, want ErrUnreadable", err)
	}
	if !strings.Contains(err.Error(), "named pipe") || strings.Contains(err.Error(), p) {
		t.Errorf("err %q, want it to name the named pipe and not the path", err)
	}
}
