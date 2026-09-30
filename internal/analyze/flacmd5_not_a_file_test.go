//go:build unix

package analyze

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestVerifyFLACAudioMD5RefusesANamedPipeWithoutWaiting: the audio-MD5 check
// reads STREAMINFO itself, in this process, from a path the job took from the
// manifest, and a file replaced by a named pipe since its scan answers at once,
// as a read that could not ask (retryable). A plain open of the pipe waited
// for a writer, and nothing can cancel that wait, so the analysis worker, and
// its place in the pool, stayed until something wrote to the pipe.
func TestVerifyFLACAudioMD5RefusesANamedPipeWithoutWaiting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "01.flac")
	fsutiltest.MakeFIFO(t, p)
	var (
		state     string
		retryable bool
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		state, retryable = verifyFLACAudioMD5(context.Background(), p, decoderSox)
	}()
	fsutiltest.AwaitPastFIFOs(t, "verifyFLACAudioMD5", fsutiltest.ServeBound, done, p)
	if state != "" || !retryable {
		t.Errorf("state %q, retryable %v; want no state and a retry, as for a read that could not ask", state, retryable)
	}
}
