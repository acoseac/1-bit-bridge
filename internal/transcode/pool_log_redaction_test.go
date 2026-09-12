package transcode

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe for the worker goroutine to write and
// the test goroutine to read.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestPoolLogsTheRedactedFailure pins the journal half of the redaction
// contract. The variant-failure ROW and the SSE frame carried the redacted
// message while the `pool: sox failed` log line logged the raw error — and
// sox's stderr quotes the absolute source path (`can't open input file
// '/mnt/music/…'`), so every failed job put a host path into the journal
// beside a `path` attribute that already named the file in the
// library-relative form the privacy page promises. A DSD render adds the
// scratch directory to the same line. Drives the REAL pool with a runner
// whose error is shaped like sox's, capturing slog's default handler, which
// `logging.Component` resolves at log time.
func TestPoolLogsTheRedactedFailure(t *testing.T) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	const absSource = "/mnt/music/Artist/Album/01.dsf"
	const tempDir = "/home/operator/bridge-data/tmp"
	scratch := renderScratchDir(tempDir)
	done := make(chan struct{})
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		defer close(done)
		return RunResult{}, errors.New("sox: exit status 2 (sox stderr: sox FAIL formats: can't open input file `" +
			absSource + "': No such file) (ffmpeg stderr: Output " + scratch + "/7f3a9c.raw: No space left on device)")
	}
	spec := JobSpec{
		SourceLibraryRel: "Artist/Album/01.dsf",
		SourceAbsPath:    absSource,
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
		TempDir:          tempDir,
		Kind:             JobKindPCMRender,
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never invoked")
	}
	// The log line is written after the runner returns; wait for it rather
	// than for a fixed interval.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "pool: sox failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no failure log line within the deadline; log so far:\n%s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	for _, leak := range []string{absSource, tempDir, "/mnt/music"} {
		if strings.Contains(out, leak) {
			t.Errorf("the failure log line carries the absolute host path %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "Artist/Album/01.dsf") {
		t.Errorf("the library-relative form should remain:\n%s", out)
	}
}
