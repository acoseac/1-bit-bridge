package transcode

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
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
// what remains is library-relative. A root-level source's sidecar sits
// directly in the variants directory, so a failed parent-directory fsync
// names that directory bare, and it reads as a placeholder (CodeRabbit on
// #1055).
func TestPoolFailureMessagesCarryNoAbsolutePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rel   string
		posix bool // the failure exists only where syncDir syncs
		setup func(p *Pool)
		want  string
	}{
		{
			name: "fsync failure",
			rel:  "Music/Album/private.flac",
			setup: func(p *Pool) {
				p.runner = succeedingRunner
				p.fsyncFn = func(path string) error { return &os.PathError{Op: "sync", Path: path, Err: syscall.EIO} }
			},
			want: "fsync sidecar: ",
		},
		{
			name:  "a root-level source's parent-directory fsync failure",
			rel:   "private.flac",
			posix: true,
			setup: func(p *Pool) {
				p.runner = succeedingRunner
				p.fsyncFn = parentDirFsyncFailure
			},
			want: `fsync sidecar: fsync parent dir: open dir "<variantsDir>": open <variantsDir>: permission denied`,
		},
		{
			name: "recovered panic",
			rel:  "Music/Album/private.flac",
			setup: func(p *Pool) {
				p.runner = func(_ context.Context, s JobSpec) (RunResult, error) {
					panic("open " + s.SourceAbsPath + ": input/output error")
				}
			},
			want: "panic recovered in worker: open Music/Album/private.flac: input/output error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.posix && runtime.GOOS == "windows" {
				t.Skip("syncDir is a no-op on Windows, so no parent-directory fsync can fail there")
			}
			got, spec := failOneJob(t, tc.rel, tc.setup)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("errMsg = %q, want it to start %q", got, tc.want)
			}
			for _, abs := range []string{spec.OutputDir, spec.SourceAbsPath, "/Users/operator"} {
				if strings.Contains(got, abs) {
					t.Errorf("errMsg %q carries the absolute path %q", got, abs)
				}
			}
		})
	}
}

// succeedingRunner stands in for sox: the job gets as far as the fsync.
func succeedingRunner(context.Context, JobSpec) (RunResult, error) {
	return RunResult{SizeBytes: 1}, nil
}

// parentDirFsyncFailure returns the error fsyncFileAndParent gives when
// syncDir cannot open the sidecar's directory, in the POSIX syncDir's words.
func parentDirFsyncFailure(sidecar string) error {
	dir := filepath.Dir(sidecar)
	return fmt.Errorf("fsync parent dir: %w",
		fmt.Errorf("open dir %q: %w", dir, &os.PathError{Op: "open", Path: dir, Err: syscall.EACCES}))
}

// failOneJob runs one job for rel through a real pool whose runner and fsync
// setup has replaced, and returns the message its failure was announced
// with, and the spec it ran.
func failOneJob(t *testing.T, rel string, setup func(p *Pool)) (string, JobSpec) {
	t.Helper()
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	seedTrackForPool(t, store, rel)
	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)
	spec := JobSpec{
		SourceLibraryRel: rel,
		SourceAbsPath:    path.Join("/Users/operator/Music", rel),
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	setup(p)
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
	return got, spec
}
