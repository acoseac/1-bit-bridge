package transcode

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPoolFailureMessagesCarryNoAbsolutePath pins the privacy page's promise
// on the one pool exit that reaches the app: a failed job's message is
// stored on its batch row, which GET /v1/upscale/batches serves, and it
// used to carry err.Error() whole. An fsync failure names the sidecar by its
// absolute path under the variants directory, and a recovered panic can
// quote any path the job had in hand. Both are now redacted like sox's
// stderr: the variants directory and the source's absolute path go, and
// what remains is library-relative.
func TestPoolFailureMessagesCarryNoAbsolutePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(p *Pool, spec JobSpec)
		want  string
	}{
		{
			name: "fsync failure",
			setup: func(p *Pool, spec JobSpec) {
				p.runner = func(context.Context, JobSpec) (RunResult, error) { return RunResult{SizeBytes: 1}, nil }
				p.fsyncFn = func(path string) error { return &os.PathError{Op: "sync", Path: path, Err: syscall.EIO} }
			},
			want: "fsync sidecar: ",
		},
		{
			name: "recovered panic",
			setup: func(p *Pool, spec JobSpec) {
				p.runner = func(_ context.Context, s JobSpec) (RunResult, error) {
					panic("open " + s.SourceAbsPath + ": input/output error")
				}
			},
			want: "panic recovered in worker: open Music/Album/private.flac: input/output error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTempStoreForPool(t)
			t.Cleanup(func() { _ = store.Close() })
			seedTrackForPool(t, store, "Music/Album/private.flac")
			p := NewPool(store, 1, 4)
			t.Cleanup(p.Stop)
			outDir := t.TempDir()
			spec := JobSpec{
				SourceLibraryRel: "Music/Album/private.flac",
				SourceAbsPath:    "/Users/operator/Music/Album/private.flac",
				TargetSampleRate: 176400,
				TargetBits:       24,
				Quality:          QualityVeryHigh,
				OutputDir:        outDir,
			}
			tc.setup(p, spec)
			var fired atomic.Int64
			var msg atomic.Value
			p.SetOnJobFailed(func(_, _, errMsg string, _ float64, _ uuid.UUID, _ time.Time) {
				msg.Store(errMsg)
				fired.Add(1)
			})
			if err := p.Enqueue(spec); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for fired.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			got, _ := msg.Load().(string)
			if got == "" {
				t.Fatal("the failure was never announced")
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("errMsg = %q, want it to start %q", got, tc.want)
			}
			for _, abs := range []string{outDir, spec.SourceAbsPath, "/Users/operator"} {
				if strings.Contains(got, abs) {
					t.Errorf("errMsg %q carries the absolute path %q", got, abs)
				}
			}
		})
	}
}
