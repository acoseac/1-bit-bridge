package transcode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// changedLibrary is a library on disk scanned into a store, whose files
// the test then changes without a scan: the state a batch meets between a
// retag and the scan that reads it.
type changedLibrary struct {
	store *manifest.Store
	root  string
}

// newChangedLibrary writes each file under a temp root and a row for it as
// a scan leaves one (the row's size and mtime are the file's).
func newChangedLibrary(t *testing.T, files map[string]manifest.Track) *changedLibrary {
	t.Helper()
	l := &changedLibrary{store: openTempStoreForBatch(t), root: t.TempDir()}
	t.Cleanup(func() { _ = l.store.Close() })
	for rel, tr := range files {
		abs := filepath.Join(l.root, filepath.FromSlash(rel))
		writeSource(t, abs, 4096)
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		tr := tr
		tr.Path, tr.Size, tr.ModTime = rel, fi.Size(), fi.ModTime()
		if err := l.store.UpsertTrack(context.Background(), &tr); err != nil {
			t.Fatalf("UpsertTrack %s: %v", rel, err)
		}
	}
	return l
}

func flacTrack() manifest.Track {
	rate, bits, isDSD := 96000.0, 24, false
	return manifest.Track{Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits, IsDSD: &isDSD}
}

func dsfTrack() manifest.Track {
	rate, bits, isDSD, ch, dur := 2822400.0, 1, true, 2, 3.0
	return manifest.Track{Codec: "DSF", SampleRate: &rate, BitsPerSample: &bits, IsDSD: &isDSD, Channels: &ch, Duration: &dur}
}

// TestABatchPassesOverAFileThatChangedSinceItsScan: a batch renders only a
// file its row still describes, like every other enqueuer. On main the
// walks stamped the row and never looked at the file, so a batch submitted
// between a retag and the scan rendered the new bytes under the old stamp:
// measured, `01 -> 410 variant_stale`, and a second batch after the scan
// counted the track covered (any rendition of the family counts) and
// rendered nothing, so the rendition stayed unservable.
//
// Each walk (upscale, optimize, pcm) passes the changed file over, counts it
// among the batch row's skipped files, and asks for a rescan of its
// directory through the pool, naming it by its row's path; the unchanged
// file beside it is rendered. The resolver is the production one,
// fs.Resolver.ResolveChecked, whose stat the walks compare with the row.
func TestABatchPassesOverAFileThatChangedSinceItsScan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		submit   func(c *Coordinator, out string) (*SubmitResult, error)
		rendered []string
		changed  []string
	}{
		{"upscale", func(c *Coordinator, out string) (*SubmitResult, error) {
			return c.Submit(context.Background(), "Album", 192000, 24, out)
		}, []string{"Album/02.flac"}, []string{"Album/01.flac"}},
		{"optimize", func(c *Coordinator, out string) (*SubmitResult, error) {
			return c.SubmitOptimize(context.Background(), "Album", out)
		}, []string{"Album/02.flac", "Album/04.dsf"}, []string{"Album/01.flac", "Album/03.dsf"}},
		{"pcm", func(c *Coordinator, out string) (*SubmitResult, error) {
			return c.SubmitPCMRender(context.Background(), "Album", out)
		}, []string{"Album/04.dsf"}, []string{"Album/03.dsf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lib := newChangedLibrary(t, map[string]manifest.Track{
				"Album/01.flac": flacTrack(), "Album/02.flac": flacTrack(),
				"Album/03.dsf": dsfTrack(), "Album/04.dsf": dsfTrack(),
			})
			retag(t, filepath.Join(lib.root, "Album", "01.flac"))
			retag(t, filepath.Join(lib.root, "Album", "03.dsf"))

			p := NewPool(lib.store, 1, 16)
			t.Cleanup(p.Stop)
			p.fsyncFn = noopFsync
			var mu sync.Mutex
			var ran, rescans []string
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
				mu.Lock()
				ran = append(ran, spec.SourceLibraryRel)
				mu.Unlock()
				select {
				case <-release:
				case <-ctx.Done():
				}
				return RunResult{}, ctx.Err()
			}
			p.SetSourceRescan(func(rel string) {
				mu.Lock()
				defer mu.Unlock()
				rescans = append(rescans, rel)
			})
			c, err := NewCoordinator(p, lib.store, t.TempDir(), nil, bridgefs.New([]string{lib.root}).ResolveChecked)
			if err != nil {
				t.Fatal(err)
			}
			c.WithDSDRender(capsFn(dsdCapsOn))

			res, err := tc.submit(c, t.TempDir())
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			if res.EnqueuedCount != len(tc.rendered) {
				t.Errorf("enqueued %d jobs, want %d (%v): a file that changed after its scan was queued "+
					"to render bytes its row's stamp does not describe", res.EnqueuedCount, len(tc.rendered), tc.rendered)
			}
			row := batchRow(t, lib.store, res)
			if row.SkippedFiles < len(tc.changed) {
				t.Errorf("the batch row counts %d skipped, want at least the %d changed files", row.SkippedFiles, len(tc.changed))
			}
			mu.Lock()
			gotRescans := append([]string(nil), rescans...)
			mu.Unlock()
			sort.Strings(gotRescans)
			if want := tc.changed; !slices.Equal(gotRescans, want) {
				t.Errorf("rescans asked for %v, want %v: the changed files' directories, named by their rows' paths", gotRescans, want)
			}
		})
	}
}

// batchRow is the persisted row of the batch res reports.
func batchRow(t *testing.T, s *manifest.Store, res *SubmitResult) manifest.UpscaleBatchRow {
	t.Helper()
	rows, err := s.ListUpscaleBatches(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == res.BatchID {
			return r
		}
	}
	t.Fatalf("no batch row %s", res.BatchID)
	return manifest.UpscaleBatchRow{}
}
